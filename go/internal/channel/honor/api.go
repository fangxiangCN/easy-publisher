package honor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

const uploadBoundary = "----EasyPublisherHonorBoundary"

// API 是荣耀开放平台的接口封装。无状态：凭据由构造入参传入。
type API struct {
	client *http.Client

	// baseURL 与 tokenURL 可注入。测试里都指向 httptest.Server ——
	// Kotlin 版把这两个域名都写成常量，导致渠道流程完全无法集成测试。
	baseURL  string
	tokenURL string
}

// NewAPI 构造接口封装。baseURL / tokenURL 为空时用生产域名。
func NewAPI(client *http.Client, baseURL, tokenURL string) *API {
	if baseURL == "" {
		baseURL = BaseURL
	}
	if tokenURL == "" {
		tokenURL = TokenURL
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		client:   client,
		baseURL:  strings.TrimSuffix(baseURL, "/") + "/",
		tokenURL: tokenURL,
	}
}

// GetToken 获取 access_token。
//
// 两个刻意保留的细节：
//  1. 它是 POST + application/x-www-form-urlencoded，不是 JSON body ——
//     荣耀 IAM 只接受表单。改成 JSON 会直接失败。
//  2. 用的是**绝对地址**，域名（iam.developer.honor.com）与其余接口的 baseURL
//     （appmarket-openapi-drcn.cloud.honor.com）不同。
func (a *API) GetToken(ctx context.Context, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", eperr.ConfigurationError("构造荣耀 token 请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return "", err
	}
	resp, err := unmarshal[tokenResp](body, "获取token")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(resp.Token)
	if token == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindCredential,
			Channel: ID,
			Msg:     "荣耀未返回 access_token，请检查 client_id 与 client_secret 是否正确",
		}
	}
	return token, nil
}

// GetAppID 通过包名获取 appId。
func (a *API) GetAppID(ctx context.Context, token, packageName string) (string, error) {
	rawURL := a.url(pathGetAppID, map[string]string{"pkgName": packageName})
	body, err := a.get(ctx, rawURL, token)
	if err != nil {
		return "", err
	}
	entries, err := unmarshal[result[[]appIDEntry]](body, "获取AppId")
	if err != nil {
		return "", err
	}
	list, err := entries.requireData("获取AppId")
	if err != nil {
		return "", err
	}
	for _, e := range list {
		if id := strings.TrimSpace(e.AppID); id != "" {
			return id, nil
		}
	}
	return "", eperr.PreconditionError(ID,
		"荣耀应用市场中找不到包名 %s 对应的应用，请确认应用已创建且账号有权限", packageName)
}

// GetAppInfo 获取应用详情。
//
// 注意这一步在发布流程里不是「查状态」，而是取语言信息 ——
// 后续 update-language-info 要求回填 appName / intro，必须先在拿到。
// 查审核状态用的是另一个接口（GetCurrentRelease）。
func (a *API) GetAppInfo(ctx context.Context, token, appID string) (AppInfo, error) {
	rawURL := a.url(pathGetAppDetail, map[string]string{"appId": appID})
	body, err := a.get(ctx, rawURL, token)
	if err != nil {
		return AppInfo{}, err
	}
	res, err := unmarshal[result[AppInfo]](body, "获取App信息")
	if err != nil {
		return AppInfo{}, err
	}
	return res.requireData("获取App信息")
}

// GetCurrentRelease 获取当前版本的审核状态。
func (a *API) GetCurrentRelease(ctx context.Context, token, appID string) (ReviewState, error) {
	rawURL := a.url(pathGetCurrentRelease, map[string]string{"appId": appID})
	body, err := a.get(ctx, rawURL, token)
	if err != nil {
		return ReviewState{}, err
	}
	res, err := unmarshal[result[ReviewState]](body, "获取审核状态")
	if err != nil {
		return ReviewState{}, err
	}
	return res.requireData("获取审核状态")
}

// GetUploadURL 申请上传地址。
//
// fileType=100 是荣耀约定的 APK 安装包类型，fileSha256 由服务端校验。
func (a *API) GetUploadURL(
	ctx context.Context,
	token, appID, artifactPath string,
) (UploadURL, error) {
	sum, err := fileSHA256(artifactPath)
	if err != nil {
		return UploadURL{}, err
	}
	size, err := fileSize(artifactPath)
	if err != nil {
		return UploadURL{}, err
	}

	payload := []uploadFile{{
		FileName:   baseName(artifactPath),
		FileType:   APKFileType,
		FileSize:   size,
		FileSha256: sum,
	}}
	body, err := a.postJSON(ctx, a.url(pathGetUploadURL, map[string]string{"appId": appID}),
		token, payload)
	if err != nil {
		return UploadURL{}, err
	}
	res, err := unmarshal[result[[]UploadURL]](body, "获取Apk上传地址")
	if err != nil {
		return UploadURL{}, err
	}
	list, err := res.requireData("获取Apk上传地址")
	if err != nil {
		return UploadURL{}, err
	}
	if len(list) == 0 {
		return UploadURL{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "荣耀未返回 APK 上传地址，渠道接口可能已变更",
		}
	}
	return list[0], nil
}

// UploadFile 上传 APK 到服务端下发的地址。
//
// 上传地址由接口响应给出，必须先校验是 https —— 否则一旦响应被篡改成 http，
// APK 与 Authorization 头会一起明文发出。
func (a *API) UploadFile(
	ctx context.Context,
	token string,
	target UploadURL,
	artifactPath string,
	onProgress func(float64),
) error {
	rawURL := strings.TrimSpace(target.URL)
	if rawURL == "" {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "荣耀返回的上传地址为空",
		}
	}
	if _, err := httpx.RequireHTTPS(rawURL, ID); err != nil {
		return err
	}

	bodyReader, contentLength, err := multipartFileBody(artifactPath, onProgress)
	if err != nil {
		return err
	}
	if closer, ok := bodyReader.(io.Closer); ok {
		defer closer.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bodyReader)
	if err != nil {
		return eperr.ConfigurationError("构造荣耀上传请求失败：%v", err)
	}
	req.ContentLength = contentLength
	// 强制 form-data：MultipartBody 默认是 mixed，荣耀只接受 form-data
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+uploadBoundary)
	req.Header.Set("Authorization", token)

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	// 上传接口返回裸 JSON，不是 result 包装体，但同样以 code == 0 表示成功。
	// 套用通用解析会拿不到 code
	ack, err := unmarshal[uploadAck](body, "上传APK文件")
	if err != nil {
		return err
	}
	if ack.Code == nil || *ack.Code != 0 {
		code := ""
		if ack.Code != nil {
			code = fmt.Sprint(*ack.Code)
		}
		msg := strings.TrimSpace(ack.Msg)
		if msg == "" {
			msg = "渠道未返回错误描述"
		}
		return &eperr.Error{
			Kind:    eperr.KindChannelRejected,
			Channel: ID,
			Code:    code,
			Msg:     "上传APK文件失败：" + msg,
			Raw:     truncate(body),
		}
	}
	return nil
}

// BindApkFile 把已上传的文件绑定到版本上。
func (a *API) BindApkFile(ctx context.Context, token, appID string, target UploadURL) error {
	if target.ObjectID == nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "荣耀未返回上传文件的 objectId，无法绑定 APK",
		}
	}
	payload := bindApkFile{Items: []bindItem{{ObjectID: *target.ObjectID}}}
	body, err := a.postJSON(ctx, a.url(pathUpdateFileInfo, map[string]string{"appId": appID}),
		token, payload)
	if err != nil {
		return err
	}
	res, err := unmarshal[result[any]](body, "绑定已上传的apk文件")
	if err != nil {
		return err
	}
	return res.checkSuccess("绑定已上传的apk文件")
}

// UpdateVersionDesc 更新版本说明。
//
// appName / intro 必填，所以用线上语言信息原样回填；荣耀缺字段时用空串而不是
// 中断流程 —— 后台已有应用必然有名称，真为空时由渠道自己报错更准确。
func (a *API) UpdateVersionDesc(
	ctx context.Context,
	token, appID, updateDesc string,
	lang LanguageInfo,
) error {
	item := versionDescItem{
		AppName:    lang.AppName,
		Intro:      lang.Intro,
		BriefIntro: nil,
		NewFeature: updateDesc,
		LanguageID: "zh-CN",
	}
	if strings.TrimSpace(lang.LanguageID) != "" {
		item.LanguageID = lang.LanguageID
	}
	if strings.TrimSpace(lang.BriefIntro) != "" {
		brief := lang.BriefIntro
		item.BriefIntro = &brief
	}

	payload := versionDesc{List: []versionDescItem{item}}
	body, err := a.postJSON(ctx, a.url(pathUpdateLanguage, map[string]string{"appId": appID}),
		token, payload)
	if err != nil {
		return err
	}
	res, err := unmarshal[result[any]](body, "修改新版本更新描述")
	if err != nil {
		return err
	}
	return res.checkSuccess("修改新版本更新描述")
}

// Submit 提交审核。
//
// releaseType：1 全网发布，2 指定时间发布（此时 releaseTime 必填）。
func (a *API) Submit(ctx context.Context, token, appID string, onlineTime int64) error {
	payload := submitParam{ReleaseType: 1}
	if onlineTime > 0 {
		// 荣耀要求 yyyy-MM-dd'T'HH:mm:ssZZ，形如 2024-01-01T01:01:01+0800。
		// Go 的 "Z07:00" 布局产出 +08:00（带冒号），荣耀要的是 +0800（不带冒号），
		// 因此必须用 "-0700" 布局。写错会被判为时间格式有误
		formatted := time.UnixMilli(onlineTime).Format("2006-01-02T15:04:05-0700")
		payload.ReleaseType = 2
		payload.ReleaseTime = &formatted
	}

	body, err := a.postJSON(ctx, a.url(pathSubmitAudit, map[string]string{"appId": appID}),
		token, payload)
	if err != nil {
		return err
	}
	res, err := unmarshal[result[any]](body, "提交审核")
	if err != nil {
		return err
	}
	return res.checkSuccess("提交审核")
}

// ---- 内部 ----

func (a *API) url(path string, query map[string]string) string {
	u := a.baseURL + path
	if len(query) == 0 {
		return u
	}
	values := url.Values{}
	for k, v := range query {
		values.Set(k, v)
	}
	return u + "?" + values.Encode()
}

func (a *API) get(ctx context.Context, rawURL, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", eperr.ConfigurationError("构造荣耀请求失败：%v", err)
	}
	req.Header.Set("Authorization", token)
	return httpx.Do(a.client, req, ID)
}

func (a *API) postJSON(
	ctx context.Context,
	rawURL, token string,
	payload any,
) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", eperr.ConfigurationError("序列化荣耀请求体失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", eperr.ConfigurationError("构造荣耀请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	return httpx.Do(a.client, req, ID)
}

// multipartFileBody 构造只含一个 file 字段的 multipart 请求体。
//
// 头尾手工拼装并预先算出 ContentLength：APK 动辄上百兆，不整体读进内存，
// 且服务端不应收到 chunked 编码的请求。
func multipartFileBody(path string, onProgress func(float64)) (io.Reader, int64, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, 0, err
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

	head := "--" + uploadBoundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="` + escapeQuotes(baseName(path)) + `"` + "\r\n" +
		"Content-Type: " + APKMediaType + "\r\n\r\n"
	tail := "\r\n--" + uploadBoundary + "--\r\n"

	total := int64(len(head)) + st.Size() + int64(len(tail))
	progress := &progressReader{r: f, total: st.Size(), onProgress: onProgress}
	return io.MultiReader(strings.NewReader(head), progress, strings.NewReader(tail)), total, nil
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

// ---- 文件辅助 ----

func openFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, eperr.LocalFileError("无法打开待上传文件：%s（%v）", path, err)
	}
	return f, nil
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, eperr.LocalFileError("无法读取文件大小：%s（%v）", path, err)
	}
	return st.Size(), nil
}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
