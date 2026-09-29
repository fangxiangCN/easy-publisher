package huawei

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

// API 是华为 AppGallery Connect 的接口封装。无状态：凭据由方法入参传入。
type API struct {
	client *http.Client
	// baseURL 可注入，测试里指向 httptest.Server
	baseURL string
}

// NewAPI 构造接口封装。baseURL 为空时用生产域名。
func NewAPI(client *http.Client, baseURL string) *API {
	if baseURL == "" {
		baseURL = BaseURL
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		client:  client,
		baseURL: strings.TrimSuffix(baseURL, "/") + "/",
	}
}

// GetToken 取 access_token。
//
// 华为**成功时不返回 ret**，所以这里用 checkSuccessIfPresent 而不是 checkSuccess ——
// 与上游行为一致。要求 ret 必须存在会把这个正常响应判成协议错误。
func (a *API) GetToken(ctx context.Context, clientID, clientSecret string) (string, error) {
	payload := tokenReq{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		GrantType:    "client_credentials",
	}
	// token 请求不带鉴权头，用 nil 表示
	body, err := a.postJSON(ctx, a.url(pathToken, nil), nil, payload)
	if err != nil {
		return "", err
	}
	r, err := unmarshal[tokenResp](body, "获取token")
	if err != nil {
		return "", err
	}
	if err := r.Ret.checkSuccessIfPresent("获取token"); err != nil {
		return "", err
	}
	token := strings.TrimSpace(r.AccessToken)
	if token == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindCredential,
			Channel: ID,
			Msg:     "华为未返回 access_token，请检查 client_id 与 client_secret 是否正确",
		}
	}
	return token, nil
}

// GetAppID 通过包名取 appId。
func (a *API) GetAppID(ctx context.Context, clientID, token, packageName string) (string, error) {
	rawURL := a.url(pathAppIDList, map[string]string{"packageName": packageName})
	body, err := a.get(ctx, rawURL, clientID, token)
	if err != nil {
		return "", err
	}
	r, err := unmarshal[appIDResp](body, "获取AppId")
	if err != nil {
		return "", err
	}
	if err := r.Ret.checkSuccess("获取AppId"); err != nil {
		return "", err
	}
	for _, e := range r.AppIDs {
		if id := strings.TrimSpace(e.ID); id != "" {
			return id, nil
		}
	}
	return "", eperr.PreconditionError(ID,
		"华为账号下未找到包名 %s 对应的应用，请确认应用已在 AppGallery Connect 创建", packageName)
}

// GetAppInfo 取应用信息。
func (a *API) GetAppInfo(ctx context.Context, clientID, token, appID string) (AppInfo, error) {
	type appInfoResp struct {
		Ret     *ret     `json:"ret"`
		AppInfo *AppInfo `json:"appInfo"`
	}
	rawURL := a.url(pathAppInfo, map[string]string{"appId": appID})
	body, err := a.get(ctx, rawURL, clientID, token)
	if err != nil {
		return AppInfo{}, err
	}
	wrapper, err := unmarshal[appInfoResp](body, "获取App信息")
	if err != nil {
		return AppInfo{}, err
	}
	if err := wrapper.Ret.checkSuccess("获取App信息"); err != nil {
		return AppInfo{}, err
	}
	if wrapper.AppInfo == nil {
		return AppInfo{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "华为未返回 appInfo，无法判断应用状态",
			Raw:     truncate(body),
		}
	}
	return *wrapper.AppInfo, nil
}

// GetUploadURL 取文件上传地址。
func (a *API) GetUploadURL(
	ctx context.Context,
	clientID, token, appID, fileName string,
	contentLength int64,
) (UploadURL, error) {
	rawURL := a.url(pathUploadURL, map[string]string{
		"appId":         appID,
		"fileName":      fileName,
		"contentLength": fmt.Sprint(contentLength),
	})
	body, err := a.get(ctx, rawURL, clientID, token)
	if err != nil {
		return UploadURL{}, err
	}
	r, err := unmarshal[uploadURLResp](body, "获取Apk上传地址")
	if err != nil {
		return UploadURL{}, err
	}
	if err := r.Ret.checkSuccess("获取Apk上传地址"); err != nil {
		return UploadURL{}, err
	}
	if r.Info == nil {
		return UploadURL{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "华为未返回上传地址（urlInfo）",
			Raw:     truncate(body),
		}
	}
	return *r.Info, nil
}

// UploadFile 上传 APK 到华为返回的 OBS 地址。
//
// 两点必须保持原样：
//   - PUT + application/octet-stream
//   - 原样透传华为下发的 headers（含签名），不可增删
//
// 上传地址来自接口响应，必须先校验是 https —— 否则响应被篡改后
// APK 与签名头会明文发出。
func (a *API) UploadFile(
	ctx context.Context,
	target UploadURL,
	artifactPath string,
	onProgress func(float64),
) error {
	rawURL := strings.TrimSpace(target.URL)
	if rawURL == "" {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "华为返回的上传地址为空",
		}
	}
	if _, err := httpx.RequireHTTPS(rawURL, ID); err != nil {
		return err
	}

	reader, contentLength, err := progressFileBody(artifactPath, onProgress)
	if err != nil {
		return err
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, reader)
	if err != nil {
		return eperr.ConfigurationError("构造华为上传请求失败：%v", err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", APKMediaType)
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	// httpx.Do 内部 use 关闭响应。上游的 client.newCall(request).execute()
	// 没有 .use，每次上传泄漏一个连接与响应体
	_, err = httpx.Do(a.client, req, ID)
	return err
}

// BindApk 绑定已上传的文件，返回用于查询编译状态的 pkgId。
func (a *API) BindApk(
	ctx context.Context,
	clientID, token, appID, fileName, objectID string,
) (string, error) {
	payload := refreshApk{
		FileType: APKFileType,
		Files:    []fileEntry{{FileName: fileName, FileDestURL: objectID}},
	}
	rawURL := a.url(pathAppFileInfo, map[string]string{"appId": appID})
	body, err := a.putJSON(ctx, rawURL, clientID, token, payload)
	if err != nil {
		return "", err
	}
	r, err := unmarshal[bindFileResp](body, "绑定Apk文件")
	if err != nil {
		return "", err
	}
	if err := r.Ret.checkSuccess("绑定Apk文件"); err != nil {
		return "", err
	}
	for _, id := range r.PkgVersion {
		if strings.TrimSpace(id) != "" {
			return id, nil
		}
	}
	return "", &eperr.Error{
		Kind:    eperr.KindProtocolMismatch,
		Channel: ID,
		Msg:     "绑定 APK 成功但未返回 pkgVersion，无法查询编译状态",
		Raw:     truncate(body),
	}
}

// GetCompileState 查询 APK 编译状态。
//
// 返回 (是否成功, 是否拿到状态)。没拿到状态（pkgStateList 为空）时调用方应继续等待 ——
// 上游用 first()，空数组会抛 NoSuchElementException，用户看到毫无信息量的堆栈。
func (a *API) GetCompileState(
	ctx context.Context,
	clientID, token, appID, pkgID string,
) (success bool, known bool, err error) {
	rawURL := a.url(pathCompileStatus, map[string]string{"appId": appID, "pkgIds": pkgID})
	body, err := a.get(ctx, rawURL, clientID, token)
	if err != nil {
		return false, false, err
	}
	r, err := unmarshal[apkState](body, "检测Apk编译状态")
	if err != nil {
		return false, false, err
	}
	if err := r.Ret.checkSuccess("检测Apk编译状态"); err != nil {
		return false, false, err
	}
	if len(r.PkgStateList) == 0 {
		return false, false, nil
	}
	return r.PkgStateList[0].isSuccess(), true, nil
}

// UpdateVersionDesc 更新版本描述。
func (a *API) UpdateVersionDesc(ctx context.Context, clientID, token, appID, desc string) error {
	payload := versionDesc{NewFeatures: desc, Lang: DefaultLang}
	rawURL := a.url(pathAppLanguage, map[string]string{"appId": appID})
	body, err := a.putJSON(ctx, rawURL, clientID, token, payload)
	if err != nil {
		return err
	}
	r, err := unmarshal[resp](body, "修改新版本更新描述")
	if err != nil {
		return err
	}
	return r.Ret.checkSuccess("修改新版本更新描述")
}

// Submit 提交审核。
//
// releaseTime 为空时不拼该 query，等价于「审核通过后立即发布」。
func (a *API) Submit(ctx context.Context, clientID, token, appID string, onlineTime int64) error {
	query := map[string]string{"appId": appID}
	if onlineTime > 0 {
		// 华为要求 yyyy-MM-dd'T'HH:mm:ssZZ，形如 2015-01-01T01:01:01+0800。
		// Go 的 "Z07:00" 布局产出 +08:00（带冒号），华为要的是 +0800（不带），
		// 因此必须用 "-0700" 布局
		query["releaseTime"] = time.UnixMilli(onlineTime).Format("2006-01-02T15:04:05-0700")
	}

	rawURL := a.url(pathAppSubmit, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, nil)
	if err != nil {
		return eperr.ConfigurationError("构造华为送审请求失败：%v", err)
	}
	req.Header.Set("client_id", clientID)
	req.Header.Set("Authorization", token)

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	r, err := unmarshal[resp](body, "提交审核")
	if err != nil {
		return err
	}
	return r.Ret.checkSuccess("提交审核")
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

func (a *API) get(ctx context.Context, rawURL, clientID, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", eperr.ConfigurationError("构造华为请求失败：%v", err)
	}
	a.setAuth(req, clientID, token)
	return httpx.Do(a.client, req, ID)
}

// apiAuth 是请求的鉴权信息。nil 表示不带鉴权（仅取 token 那一步）。
type apiAuth struct {
	clientID string
	token    string
}

func (a *API) postJSON(
	ctx context.Context,
	rawURL string,
	auth *apiAuth,
	payload any,
) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", eperr.ConfigurationError("序列化华为请求体失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", eperr.ConfigurationError("构造华为请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != nil {
		a.setAuth(req, auth.clientID, auth.token)
	}
	return httpx.Do(a.client, req, ID)
}

func (a *API) putJSON(
	ctx context.Context,
	rawURL, clientID, token string,
	payload any,
) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", eperr.ConfigurationError("序列化华为请求体失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", eperr.ConfigurationError("构造华为请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	a.setAuth(req, clientID, token)
	return httpx.Do(a.client, req, ID)
}

func (a *API) setAuth(req *http.Request, clientID, token string) {
	req.Header.Set("client_id", clientID)
	req.Header.Set("Authorization", token)
}

func unmarshal[T any](body, action string) (T, error) {
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return out, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 的响应无法解析，渠道接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return out, nil
}

// progressFileBody 返回带进度回调的文件读取器。
//
// 不整体读进内存：APK 动辄上百兆。调用方设置 ContentLength 后，
// 服务端不会收到 chunked 编码的请求。
type progressFile struct {
	f          *os.File
	total      int64
	read       int64
	onProgress func(float64)
	lastPct    int
	lastReport time.Time
}

func progressFileBody(path string, onProgress func(float64)) (io.Reader, int64, error) {
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
	return &progressFile{f: f, total: st.Size(), onProgress: onProgress}, st.Size(), nil
}

func (p *progressFile) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if n > 0 {
		p.read += int64(n)
		p.report(false)
	}
	if err == io.EOF {
		p.report(true)
	}
	return n, err
}

func (p *progressFile) Close() error { return p.f.Close() }

func (p *progressFile) report(final bool) {
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

// artifactSize 返回待上传文件的字节数。
func artifactSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, eperr.LocalFileError("无法读取文件大小：%s（%v）", path, err)
	}
	return st.Size(), nil
}

// fileNameOf 返回文件名（不含目录）。
func fileNameOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}
