package mi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

const boundary = "----EasyPublisherMiBoundary"

// API 是小米应用市场自动发布接口。
//
// 无状态：凭据与超时都通过方法入参传入，可安全并发复用（上游的 MiMarketClient
// 把账号与密钥存在字段里，并发场景下会串台）。
type API struct {
	client *http.Client
	// baseURL 可注入，测试里指向 httptest.Server
	baseURL string
}

// NewAPI 构造接口封装。baseURL 为空时用生产域名。
func NewAPI(client *http.Client, baseURL string) *API {
	if baseURL == "" {
		baseURL = Domain
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		client:  client,
		baseURL: strings.TrimSuffix(baseURL, "/"),
	}
}

// GetAppInfo 查询应用信息。
//
// certificate 是小米下发的 X.509 公钥证书文本；password 是开放平台的「私钥」
// （接口字段名就叫 password）。
func (a *API) GetAppInfo(
	ctx context.Context,
	account, certificate, password, packageName string,
) (AppInfoResp, error) {
	// RequestData 必须「序列化一次」：表单里发的和参与 MD5 的必须是同一份字符串，
	// 否则服务端算出的 hash 与我们给的不一致，直接鉴权失败
	requestData, err := BuildQueryRequestData(account, packageName)
	if err != nil {
		return AppInfoResp{}, err
	}
	sigPlain, err := BuildSig(password, []SigItem{
		{Name: RequestDataField, Hash: MD5Hex(requestData)},
	})
	if err != nil {
		return AppInfoResp{}, err
	}
	sig, err := Encrypt(sigPlain, certificate)
	if err != nil {
		return AppInfoResp{}, err
	}

	form := make(map[string]string, 2)
	form[RequestDataField] = requestData
	form[SIGField] = sig

	body, err := a.postForm(ctx, a.baseURL+PathQuery, form)
	if err != nil {
		return AppInfoResp{}, err
	}
	resp, err := unmarshal[AppInfoResp](body, "获取应用信息")
	if err != nil {
		return AppInfoResp{}, err
	}
	if err := checkMiResult(resp.Result, resp.Message, "获取应用信息", body); err != nil {
		return AppInfoResp{}, err
	}
	return resp, nil
}

// UploadApk 上传 APK 并提交审核。
//
// 小米的 dev/push 把「上传 APK」和「提交审核」合并成一次请求，因此整个调用都落在
// 不可撤销区间内，无法像其他渠道那样只保护最后一步。这一点由调用方用
// eperr.AtSubmissionPoint 包裹。
func (a *API) UploadApk(
	ctx context.Context,
	account, certificate, password string,
	artifactPath string,
	info PackageInfo,
	updateDesc string,
	onlineTime int64,
	onProgress func(float64),
) error {
	requestData, err := BuildPushRequestData(account, info, updateDesc, onlineTime)
	if err != nil {
		return err
	}
	apkMD5, err := fileMD5(artifactPath)
	if err != nil {
		return err
	}
	sigPlain, err := BuildSig(password, []SigItem{
		{Name: RequestDataField, Hash: MD5Hex(requestData)},
		// apk 的 MD5 同样是小米规定的完整性字段，不是安全签名
		{Name: APKPartName, Hash: apkMD5},
	})
	if err != nil {
		return err
	}
	sig, err := Encrypt(sigPlain, certificate)
	if err != nil {
		return err
	}

	bodyReader, contentLength, err := multipartBody(artifactPath, requestData, sig, onProgress)
	if err != nil {
		return err
	}
	if closer, ok := bodyReader.(io.Closer); ok {
		defer closer.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+PathPush, bodyReader)
	if err != nil {
		return eperr.ConfigurationError("构造小米上传请求失败：%v", err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	resp, err := unmarshal[CommonResp](body, "上传并提交审核")
	if err != nil {
		return err
	}
	return checkMiResult(resp.Result, resp.Message, "上传并提交审核", body)
}

// postForm 提交表单。
func (a *API) postForm(ctx context.Context, rawURL string, fields map[string]string) (string, error) {
	// 按固定顺序拼接，避免 map 遍历顺序导致请求体不一致
	var b strings.Builder
	for _, key := range []string{RequestDataField, SIGField} {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(key))
		b.WriteByte('=')
		b.WriteString(urlEncode(fields[key]))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL,
		strings.NewReader(b.String()))
	if err != nil {
		return "", eperr.ConfigurationError("构造小米请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return httpx.Do(a.client, req, ID)
}

// multipartBody 构造上传请求体：apk + RequestData + SIG 三个字段，按此顺序。
//
// 头尾手工拼装并预先算出 ContentLength：APK 动辄上百兆，不整体读进内存，
// 且服务端不应收到 chunked 编码的请求。
func multipartBody(
	artifactPath, requestData, sig string,
	onProgress func(float64),
) (io.Reader, int64, error) {
	f, err := os.Open(artifactPath)
	if err != nil {
		return nil, 0, eperr.LocalFileError("无法打开待上传文件：%s（%v）", artifactPath, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, eperr.LocalFileError("无法读取文件大小：%s（%v）", artifactPath, err)
	}
	if st.Size() == 0 {
		f.Close()
		return nil, 0, eperr.LocalFileError("待上传文件为空：%s", artifactPath)
	}

	var head bytes.Buffer
	// apk 的 part 文件名是**空串**：小米服务端按 part 名 "apk" 取文件，
	// 填入真实文件名未经验证。看着像 bug，但必须保持原样
	fmt.Fprintf(&head, "--%s\r\n", boundary)
	fmt.Fprintf(&head, "Content-Disposition: form-data; name=%q; filename=\"\"\r\n", APKPartName)
	fmt.Fprintf(&head, "Content-Type: %s\r\n\r\n", APKMediaType)

	var tail bytes.Buffer
	fmt.Fprintf(&tail, "\r\n--%s\r\n", boundary)
	fmt.Fprintf(&tail, "Content-Disposition: form-data; name=%q\r\n\r\n", RequestDataField)
	tail.WriteString(requestData)
	fmt.Fprintf(&tail, "\r\n--%s\r\n", boundary)
	fmt.Fprintf(&tail, "Content-Disposition: form-data; name=%q\r\n\r\n", SIGField)
	tail.WriteString(sig)
	fmt.Fprintf(&tail, "\r\n--%s--\r\n", boundary)

	total := int64(head.Len()) + st.Size() + int64(tail.Len())
	progress := &progressReader{r: f, total: st.Size(), onProgress: onProgress}
	return io.MultiReader(bytes.NewReader(head.Bytes()), progress, bytes.NewReader(tail.Bytes())), total, nil
}

type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	onProgress func(float64)
	lastPct    int
	lastReport time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		p.report(false)
	}
	if err == io.EOF {
		p.report(true)
	}
	return n, err
}

func (p *progressReader) Close() error {
	if c, ok := p.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *progressReader) report(final bool) {
	if p.onProgress == nil || p.total <= 0 {
		return
	}
	fraction := float64(p.read) / float64(p.total)
	if fraction > 1 {
		fraction = 1
	}
	pct := int(fraction * 100)
	now := time.Now()
	if pct == p.lastPct && !final {
		return
	}
	if !final && pct != 100 && now.Sub(p.lastReport) < 200*time.Millisecond {
		return
	}
	p.lastPct = pct
	p.lastReport = now
	p.onProgress(fraction)
}

func unmarshal[T any](body, action string) (T, error) {
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return out, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 的响应无法解析，接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return out, nil
}

// urlEncode 做表单字段的百分号编码。
//
// 自己实现而不是用 net/url.Values.Encode：后者会按 key 排序，
// 而小米的表单字段顺序是 RequestData 在前、SIG 在后，保持固定顺序更稳妥。
func urlEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
		}
	}
	return b.String()
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", eperr.LocalFileError("无法打开文件以计算 MD5：%s（%v）", path, err)
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", eperr.LocalFileError("计算文件 MD5 失败：%s（%v）", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
