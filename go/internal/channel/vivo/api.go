package vivo

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// API 是 vivo 开放平台的接口封装。
//
// 无状态：凭据由构造入参传入，实例随单次请求创建。
//
// 超时不在此处决定 —— Client 由调用方按 UploadRequest.Timeouts 构造，
// 这样 issue #17（60 秒不足以上传大包）可以由用户直接用超时参数解决，
// 而不需要改代码里的常量。
type API struct {
	accessKey    string
	accessSecret string
	client       *http.Client

	// baseURL 可注入。Kotlin 版把 DOMAIN 写成 const val，导致渠道流程
	// 完全无法集成测试 —— 这是重写时最值得改的一处：测试里指向 httptest.Server，
	// 就能在没有真实凭据的情况下断言完整的请求形状。
	baseURL string
}

// NewAPI 构造接口封装。baseURL 为空时用生产域名。
func NewAPI(accessKey, accessSecret string, client *http.Client, baseURL string) *API {
	if baseURL == "" {
		baseURL = ReleaseDomain
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		accessKey:    accessKey,
		accessSecret: accessSecret,
		client:       client,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
	}
}

// GetAppInfo 查询应用详情。
func (a *API) GetAppInfo(ctx context.Context, packageName string) (appInfo, error) {
	req, err := a.signedRequest(ctx, http.MethodGet, methodGetAppInfo,
		map[string]string{"packageName": packageName}, nil)
	if err != nil {
		return appInfo{}, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return appInfo{}, err
	}

	var env envelope
	if err := unmarshal(body, &env); err != nil {
		return appInfo{}, err
	}
	if err := env.ensureSuccess("查询应用详情", body); err != nil {
		return appInfo{}, err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return appInfo{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg: fmt.Sprintf("vivo 查询应用详情成功但未返回 data，"+
				"请确认包名 %s 是否属于该开发者账号", packageName),
			Raw: truncate(body),
		}
	}
	var info appInfo
	if err := json.Unmarshal(env.Data, &info); err != nil {
		return appInfo{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "解析 vivo 应用详情失败，接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return info, nil
}

// UploadAPK 上传安装包，返回流水号等提交时需要的信息。
func (a *API) UploadAPK(
	ctx context.Context,
	artifactPath, packageName string,
	onProgress func(float64),
) (UploadResult, error) {
	md5sum, err := fileMD5(artifactPath)
	if err != nil {
		return UploadResult{}, err
	}

	// fileMd5 参与签名，因此必须在构造 URL 之前算好
	params := map[string]string{
		"packageName": packageName,
		"fileMd5":     md5sum,
	}
	bodyReader, contentLength, err := multipartFileBody(artifactPath, onProgress)
	if err != nil {
		return UploadResult{}, err
	}
	if closer, ok := bodyReader.(io.Closer); ok {
		defer closer.Close()
	}

	req, err := a.signedRequest(ctx, http.MethodPost, methodUploadAPK, params, bodyReader)
	if err != nil {
		return UploadResult{}, err
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", multipartContentType(boundary))

	respBody, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return UploadResult{}, err
	}
	var env envelope
	if err := unmarshal(respBody, &env); err != nil {
		return UploadResult{}, err
	}
	if err := env.ensureSuccess("上传 APK", respBody); err != nil {
		return UploadResult{}, err
	}
	var result apkResult
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &result); err != nil {
			return UploadResult{}, &eperr.Error{
				Kind:    eperr.KindProtocolMismatch,
				Channel: ID,
				Msg:     "解析 vivo 上传结果失败，接口可能已变更：" + err.Error(),
				Raw:     truncate(respBody),
				Err:     err,
			}
		}
	}
	return result.requireUploadResult(respBody)
}

// Submit 提交更新。
//
// 保持 GET —— 全部业务参数都在 query 上（见 signedRequest），
// 换成 POST 表单会改变待签串与服务端的取参方式。
func (a *API) Submit(ctx context.Context, result UploadResult, release ReleaseParams) error {
	// 立即上架:1，定时上架:2
	onlineType := "1"
	if release.OnlineTime > 0 {
		onlineType = "2"
	}
	params := map[string]string{
		"packageName": result.PackageName,
		"versionCode": strconv.FormatInt(result.VersionCode, 10),
		"apk":         result.Serialnumber,
		"fileMd5":     result.FileMd5,
		"onlineType":  onlineType,
		"updateDesc":  release.UpdateDesc,
	}
	if release.OnlineTime > 0 {
		// onlineType = 2 时上架时间必填，格式固定 yyyy-MM-dd HH:mm:ss。
		// 用 UTC 之外的本地时区：Kotlin 版用 Locale.ROOT + 系统默认时区，
		// 这里同样用本地时区，但显式指定格式化布局，避免 Go 的时间格式
		// 在非英文环境下产生本地化数字（Go 不会，但布局写错会）
		params["scheOnlineTime"] = time.UnixMilli(release.OnlineTime).
			Format("2006-01-02 15:04:05")
	}

	req, err := a.signedRequest(ctx, http.MethodGet, methodSubmit, params, nil)
	if err != nil {
		return err
	}
	respBody, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	var env envelope
	if err := unmarshal(respBody, &env); err != nil {
		return err
	}
	return env.ensureSuccess("提交更新", respBody)
}

// ReleaseParams 是提交更新需要的发布参数。
//
// 单独定义而不复用 channel.ReleaseParams，是为了让 API 层不依赖 channel 包 ——
// 依赖方向是 channel → api，反过来会成环。
type ReleaseParams struct {
	UpdateDesc string
	OnlineTime int64
}

// signedRequest 构造带签名的请求。
//
// vivo 用的是 router/rest 风格的网关：**method、access_key、timestamp、sign
// 等签名参数以及全部业务参数都必须放在 query string 上**，连上传 APK 这种
// multipart 请求也一样（body 里只有文件）。这不是移植时的偷懒，而是该网关的
// 强制要求，改成放进 body 或 header 会直接鉴权失败。
//
// 副作用是签名与凭据会出现在 URL 里，所以这个 URL 绝不能整体进日志。
func (a *API) signedRequest(
	ctx context.Context,
	method, apiMethod string,
	params map[string]string,
	body io.Reader,
) (*http.Request, error) {
	signed := Sign(a.accessKey, a.accessSecret, apiMethod, params)

	u, err := url.Parse(a.baseURL)
	if err != nil {
		return nil, eperr.ConfigurationError("vivo 接口地址不合法：%v", err)
	}
	q := u.Query()
	for k, v := range signed {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, eperr.ConfigurationError("构造 vivo 请求失败：%v", err)
	}
	return req, nil
}

func unmarshal(body string, target any) error {
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "vivo 响应不是合法 JSON，接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return nil
}

// ---- multipart 构造 ----

// boundary 固定即可：请求体由我们自己拼装，不需要随机化来防止内容冲突。
// 但仍要确保文件名里不会出现它。
const boundary = "----EasyPublisherVivoBoundary"

func multipartContentType(b string) string {
	return "multipart/form-data; boundary=" + b
}

// multipartFileBody 构造只含一个 file 字段的 multipart 请求体。
//
// 不整体读进内存：APK 动辄上百兆。做法是手工拼出头尾两段，
// 中间用文件流，并**预先算出 ContentLength** —— 这样服务端不会收到
// chunked 编码的请求，与 OkHttp 的 MultipartBody 行为一致。
func multipartFileBody(path string, onProgress func(float64)) (io.Reader, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, eperr.LocalFileError("无法打开待上传文件：%s（%v）", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, eperr.LocalFileError("无法读取文件大小：%s（%v）", path, err)
	}
	if st.Size() == 0 {
		f.Close()
		return nil, 0, eperr.LocalFileError("待上传文件为空：%s", path)
	}

	// 头部格式必须与 mime/multipart 的 CreateFormFile 一致，
	// 否则服务端按它自己的解析器读不出来
	head := fmt.Sprintf("--%s\r\n%s: form-data; name=\"file\"; filename=\"%s\"\r\n%s: application/octet-stream\r\n\r\n",
		boundary, "Content-Disposition", escapeQuotes(fileBaseName(path)), "Content-Type")
	tail := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	total := int64(len(head)) + st.Size() + int64(len(tail))
	progress := &progressReader{r: f, total: st.Size(), onProgress: onProgress}
	return io.MultiReader(strings.NewReader(head), progress, strings.NewReader(tail)), total, nil
}

// progressReader 只在文件字节上报告进度，头尾的几百字节不计入。
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

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func fileBaseName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// fileMD5 计算文件的 MD5。
//
// MD5 在这里是 vivo 接口规定的文件指纹字段（fileMd5 参与签名），不是安全签名，
// 因此沿用 MD5 是可接受的 —— 不要「升级」成 SHA-256，那会让签名对不上。
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
