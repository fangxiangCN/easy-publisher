package oppo

import (
	"bytes"
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

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
)

// ID 是渠道标识。
const ID = "oppo"

const (
	// Domain 是 OPPO 开放平台地址
	Domain = "https://oop-openapi-cn.heytapmobi.com"

	pathToken     = "/developer/v1/token"
	pathAppInfo   = "/resource/v1/app/info"
	pathUploadURL = "/resource/v1/upload/get-upload-url"
	pathSubmit    = "/resource/v1/app/upd"
	successCode   = 0
	maxRaw        = 2000
	auditOnline   = 111
	auditRejected = 444
	timeLayout    = "2006-01-02 15:04:05"
	boundary      = "----EasyPublisherOppoBoundary"
	formFileField = "file"
	uploadTypeAPK = "apk"
)

// ---- 响应模型 ----
//
// 所有字段一律可空。上游用 Gson 在构造函数里链式取值（obj.get("icon_url").asString），
// 有两个后果：key 缺失时 get() 返回 null、.asString 立刻 NPE；值是 JSON null 时抛
// UnsupportedOperationException。而且十几个字段挤在同一个构造函数里，任何一个缺失
// 都导致整个对象构造失败 —— 即使该字段这次业务上根本用不到。

// errnoOnly 是只取业务码的最小模型。
//
// **必须先用它判码、再去解强类型的 data** —— OPPO 失败响应里 data 的形状与成功时
// 不同（可能是数组，也可能直接是字符串），若先按成功结构解析，抛出的会是解析异常，
// 真正的业务错误码和错误描述都被吞掉。这是上游 issue #18/#19 那类报错
// 难以诊断的直接原因。
type errnoOnly struct {
	Errno *int `json:"errno"`
}

// envelope 用于取错误描述。OPPO 把 message 放在 data 里。
type envelope struct {
	Errno *int `json:"errno"`
	Data  *struct {
		Message string `json:"message"`
	} `json:"data"`
}

type tokenResponse struct {
	Errno *int `json:"errno"`
	Data  *struct {
		AccessToken string `json:"access_token"`
	} `json:"data"`
}

// AppInfo 是应用信息。字段名保持与接口一致的 snake_case 映射，便于和 OPPO 文档对照。
type AppInfo struct {
	Summary          string           `json:"summary"`
	DetailDesc       string           `json:"detail_desc"`
	VersionCode      *jsonx.FlexInt64 `json:"version_code"`
	VersionName      string           `json:"version_name"`
	AuditStatus      *jsonx.FlexInt64 `json:"audit_status"`
	PrivacyURL       string           `json:"privacy_source_url"`
	SecondCategory   string           `json:"ver_second_category_id"`
	ThirdCategory    string           `json:"ver_third_category_id"`
	IconURL          string           `json:"icon_url"`
	PicURL           string           `json:"pic_url"`
	TestDesc         string           `json:"test_desc"`
	BusinessUsername string           `json:"business_username"`
	BusinessEmail    string           `json:"business_email"`
	BusinessMobile   string           `json:"business_mobile"`
	CopyrightURL     string           `json:"copyright_url"`
	ElectronicCert   string           `json:"electronic_cert_url"`
}

// ToMarketInfo 转成渠道无关的状态。
//
// audit_status 缺失时映射为「未知」而不是「审核中」—— 接口没返回状态和商店确实在
// 审核是两件事，混在一起会让排查走错方向。上游在这里会 NPE。
func (a AppInfo) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw := ""
	if a.AuditStatus != nil {
		raw = strconv.FormatInt(int64(*a.AuditStatus), 10)
		switch int64(*a.AuditStatus) {
		case auditOnline:
			state = channel.ReviewOnline
		case auditRejected:
			state = channel.ReviewRejected
		default:
			state = channel.ReviewUnderReview
		}
	}
	var version *channel.Version
	if a.VersionCode != nil && strings.TrimSpace(a.VersionName) != "" {
		version = &channel.Version{Code: int64(*a.VersionCode), Name: a.VersionName}
	}
	return channel.NewMarketInfo(ID, state, version, raw)
}

type uploadURLResponse struct {
	Errno *int `json:"errno"`
	Data  *struct {
		UploadURL string `json:"upload_url"`
		Sign      string `json:"sign"`
	} `json:"data"`
}

type apkResponse struct {
	Errno *int `json:"errno"`
	Data  *struct {
		URL string `json:"url"`
		MD5 string `json:"md5"`
	} `json:"data"`
}

// UploadTarget 是上传地址与其配套的一次性签名。
type UploadTarget struct {
	URL  string
	Sign string
}

// ApkResult 是上传完成后服务端返回的 APK 定位信息，提交版本时要原样回传。
type ApkResult struct {
	URL string
	MD5 string
}

// ReleaseParams 是发布参数。单独定义以免 api 层反向依赖 channel 包。
type ReleaseParams struct {
	UpdateDesc string
	OnlineTime int64
}

func (p ReleaseParams) scheduled() bool { return p.OnlineTime > 0 }

// ---- API ----

// API 是 OPPO 开放平台的接口封装。无状态：凭据由构造入参传入。
type API struct {
	clientID     string
	clientSecret string
	client       *http.Client
	// baseURL 可注入，测试里指向 httptest.Server
	baseURL string
}

// NewAPI 构造接口封装。baseURL 为空时用生产域名。
func NewAPI(clientID, clientSecret string, client *http.Client, baseURL string) *API {
	if baseURL == "" {
		baseURL = Domain
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		clientID:     clientID,
		clientSecret: clientSecret,
		client:       client,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
	}
}

// GetToken 获取 access_token。
//
// ## 为什么凭据仍在 query 上
//
// 这是 OPPO 接口的设计：独立实现 flu-cli/app-ship 同样是 GET + query，
// 未能查证是否支持 POST form。残余风险是 client_secret 会进代理日志与网关审计，
// 因此建议缩短该 secret 的轮换周期。
//
// 但必须用 url.Values 编码而不是字符串插值 —— 上游是
// "$DOMAIN/developer/v1/token?client_id=$clientId&client_secret=$clientSecret"，
// secret 含 & 会被拦腰截断（服务端收到半个 secret，鉴权失败且原因完全不可见），
// 含 # 之后内容变 fragment 不发送，含空格则直接解析失败。
func (a *API) GetToken(ctx context.Context) (string, error) {
	u, err := url.Parse(a.baseURL + pathToken)
	if err != nil {
		return "", eperr.ConfigurationError("OPPO 接口地址不合法：%v", err)
	}
	q := u.Query()
	q.Set("client_id", a.clientID)
	q.Set("client_secret", a.clientSecret)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", eperr.ConfigurationError("构造 token 请求失败：%v", err)
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return "", err
	}
	// 这一条响应体里含裸 access_token，出错时不能把 raw 交给错误对象
	if err := a.checkSuccess(body, "获取 token", false); err != nil {
		return "", err
	}
	var resp tokenResponse
	if err := unmarshal(body, &resp); err != nil {
		return "", err
	}
	token := ""
	if resp.Data != nil {
		token = strings.TrimSpace(resp.Data.AccessToken)
	}
	if token == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "OPPO 返回的响应里没有 access_token，接口可能已变更",
		}
	}
	return token, nil
}

// GetAppInfo 读回商店现有资料。提交版本要求全量字段，因此必须先读回。
func (a *API) GetAppInfo(ctx context.Context, token, applicationID string) (AppInfo, error) {
	req, err := a.signedRequest(ctx, http.MethodGet, a.baseURL+pathAppInfo,
		map[string]string{"pkg_name": applicationID}, token, true, nil)
	if err != nil {
		return AppInfo{}, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return AppInfo{}, err
	}
	if err := a.checkSuccess(body, "获取应用信息", true); err != nil {
		return AppInfo{}, err
	}
	var info AppInfo
	if err := unmarshalData(body, &info); err != nil {
		return AppInfo{}, err
	}
	return info, nil
}

// GetUploadURL 获取本次上传专用的地址与一次性 sign。
func (a *API) GetUploadURL(ctx context.Context, token string) (UploadTarget, error) {
	req, err := a.signedRequest(ctx, http.MethodGet, a.baseURL+pathUploadURL,
		nil, token, true, nil)
	if err != nil {
		return UploadTarget{}, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return UploadTarget{}, err
	}
	if err := a.checkSuccess(body, "获取上传地址", true); err != nil {
		return UploadTarget{}, err
	}
	var resp uploadURLResponse
	if err := unmarshal(body, &resp); err != nil {
		return UploadTarget{}, err
	}
	if resp.Data == nil || strings.TrimSpace(resp.Data.UploadURL) == "" {
		return UploadTarget{}, protocolError("OPPO 未返回 upload_url", body)
	}
	if strings.TrimSpace(resp.Data.Sign) == "" {
		return UploadTarget{}, protocolError("OPPO 未返回上传 sign", body)
	}
	return UploadTarget{URL: resp.Data.UploadURL, Sign: resp.Data.Sign}, nil
}

// UploadAPK 上传安装包。
//
// type 与 sign 同时出现在 query（参与签名）和 multipart body 里 —— 这是上游的行为，
// OPPO 两边都校验，去掉任何一侧都会被拒，因此保持原样。
func (a *API) UploadAPK(
	ctx context.Context,
	target UploadTarget,
	token, artifactPath string,
	onProgress func(float64),
) (ApkResult, error) {
	params := map[string]string{"type": uploadTypeAPK, "sign": target.Sign}

	// 上传地址来自接口响应，可能被篡改为 http —— 明文发出去的是整个安装包与 access_token
	signedURL, err := a.signedURLString(target.URL, params, token, false)
	if err != nil {
		return ApkResult{}, err
	}
	if _, err := httpx.RequireHTTPS(signedURL, ID); err != nil {
		return ApkResult{}, err
	}

	bodyReader, contentLength, err := multipartUploadBody(artifactPath, target.Sign, onProgress)
	if err != nil {
		return ApkResult{}, err
	}
	if closer, ok := bodyReader.(io.Closer); ok {
		defer closer.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signedURL, bodyReader)
	if err != nil {
		return ApkResult{}, eperr.ConfigurationError("构造上传请求失败：%v", err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return ApkResult{}, err
	}
	if err := a.checkSuccess(body, "上传 APK", true); err != nil {
		return ApkResult{}, err
	}
	var resp apkResponse
	if err := unmarshal(body, &resp); err != nil {
		return ApkResult{}, err
	}
	if resp.Data == nil || strings.TrimSpace(resp.Data.URL) == "" {
		return ApkResult{}, protocolError("OPPO 未返回上传后的 APK url", body)
	}
	if strings.TrimSpace(resp.Data.MD5) == "" {
		return ApkResult{}, protocolError("OPPO 未返回上传后的 APK md5", body)
	}
	return ApkResult{URL: resp.Data.URL, MD5: resp.Data.MD5}, nil
}

// Submit 提交新版本。
//
// **注意这里会把从 app/info 读回来的字段原样回传**：图标、截图、一句话介绍、
// 软件介绍、隐私政策地址、二三级分类 id、软著地址、商务联系方式。
// 这不是冗余 —— OPPO 的 app/upd 是全量更新语义，任何一个字段不传就会被清空或被拒，
// 所以必须先读回再整包送回。
func (a *API) Submit(
	ctx context.Context,
	token string,
	info artifact.Info,
	appInfo AppInfo,
	release ReleaseParams,
	apkResult ApkResult,
) error {
	params, err := buildSubmitParams(info, appInfo, release, apkResult)
	if err != nil {
		return err
	}

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := a.signedRequest(ctx, http.MethodPost, a.baseURL+pathSubmit,
		params, token, false, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	return a.checkSuccess(body, "提交版本", true)
}

// buildSubmitParams 构造提交版本的全量参数。
func buildSubmitParams(
	info artifact.Info,
	appInfo AppInfo,
	release ReleaseParams,
	apkResult ApkResult,
) (map[string]string, error) {
	required, err := requireAppFields(appInfo)
	if err != nil {
		return nil, err
	}

	// apk_url 是一个 JSON 数组字符串。字段固定为三个且值来自服务端返回的 url/md5，
	// 没有需要转义的字符；cpu_code 的 0 表示非多包应用（64 位包为 64，32 位为 32）
	apkURLJSON := fmt.Sprintf(`[{"url":%s,"md5":%s,"cpu_code":0}]`,
		quote(apkResult.URL), quote(apkResult.MD5))

	onlineType := "1" // 1 审核后立即发布，2 定时发布
	if release.scheduled() {
		onlineType = "2"
	}

	params := map[string]string{
		"pkg_name":           info.ApplicationID,
		"version_code":       strconv.FormatInt(info.VersionCode, 10),
		"apk_url":            apkURLJSON,
		"update_desc":        release.UpdateDesc,
		"online_type":        onlineType,
		"second_category_id": required.secondCategory,
		"third_category_id":  required.thirdCategory,
		"summary":            required.summary,
		"detail_desc":        required.detailDesc,
		"privacy_source_url": required.privacyURL,
		"icon_url":           required.iconURL,
		"pic_url":            required.picURL,
		// 以下字段商店可以留空，缺失时送空串而不是让整次提交失败
		"test_desc":         appInfo.TestDesc,
		"business_username": appInfo.BusinessUsername,
		"business_email":    appInfo.BusinessEmail,
		"business_mobile":   appInfo.BusinessMobile,
		// 纸质软著缺失时回退到电子版软著 —— OPPO 要求 copyright_url 非空，
		// 而多数开发者只上传了电子版
		"copyright_url":       firstNonBlank(appInfo.CopyrightURL, appInfo.ElectronicCert),
		"electronic_cert_url": appInfo.ElectronicCert,
	}

	if release.scheduled() {
		// online_type=2 时必填，格式 yyyy-MM-dd HH:mm:ss，不能早于当前时间
		params["sche_online_time"] = time.UnixMilli(release.OnlineTime).Format(timeLayout)
	}
	return params, nil
}

type requiredFields struct {
	summary, detailDesc, privacyURL                string
	secondCategory, thirdCategory, iconURL, picURL string
}

// requireAppFields 校验提交版本必须回传的商店资料。
//
// 与「结构性缺失」（access_token、upload_url、sign）区分开：那些意味着接口变更，
// 而这些意味着商店里的应用资料没填全 —— 给可执行的中文提示，
// 告诉用户去 OPPO 开放平台补什么。
func requireAppFields(info AppInfo) (requiredFields, error) {
	checks := []struct {
		label string
		value string
	}{
		{"一句话介绍（summary）", info.Summary},
		{"软件介绍（detail_desc）", info.DetailDesc},
		{"隐私政策网址（privacy_source_url）", info.PrivacyURL},
		{"二级分类（ver_second_category_id）", info.SecondCategory},
		{"三级分类（ver_third_category_id）", info.ThirdCategory},
		{"应用图标（icon_url）", info.IconURL},
		{"应用截图（pic_url）", info.PicURL},
	}
	for _, c := range checks {
		if strings.TrimSpace(c.value) == "" {
			return requiredFields{}, eperr.PreconditionError(ID,
				"OPPO 商店缺少必填资料：%s。提交新版本需要回传这些字段，"+
					"请先在 OPPO 开放平台补全应用信息", c.label)
		}
	}
	return requiredFields{
		summary:        info.Summary,
		detailDesc:     info.DetailDesc,
		privacyURL:     info.PrivacyURL,
		secondCategory: info.SecondCategory,
		thirdCategory:  info.ThirdCategory,
		iconURL:        info.IconURL,
		picURL:         info.PicURL,
	}, nil
}

// signedRequest 构造带签名的请求。
//
// appendParamsToQuery：GET 请求需要把业务参数也放到 query 上；
// POST 请求的业务参数在 body 里，query 上只带鉴权三元组。
// 两种情况下**参与签名的参数集合都是「业务参数 + access_token + timestamp」**，
// 这是签名能否通过的关键。
func (a *API) signedRequest(
	ctx context.Context,
	method, rawURL string,
	params map[string]string,
	token string,
	appendParamsToQuery bool,
	body io.Reader,
) (*http.Request, error) {
	finalURL, err := a.signedURLString(rawURL, params, token, appendParamsToQuery)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, finalURL, body)
	if err != nil {
		return nil, eperr.ConfigurationError("构造 OPPO 请求失败：%v", err)
	}
	return req, nil
}

// signedURLString 拼出带 access_token / timestamp / api_sign 的地址。
//
// ## 为什么这三个参数在 query 上
//
// OPPO 接口的强制要求：签名参数一律从 query 读取，POST 请求也不例外
// （body 只放业务字段）。无法改成 header，因此保留原样。
// 风险可接受：access_token 是短期凭据，且上传地址经过 requireHTTPS 校验。
//
// 副作用是这个 URL 绝不能整体进日志 —— 里面带着 token 与签名。
func (a *API) signedURLString(
	rawURL string,
	params map[string]string,
	token string,
	appendParamsToQuery bool,
) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", eperr.ConfigurationError("OPPO 接口地址不合法：%v", err)
	}
	// timestamp 是**秒**级，与 vivo 的毫秒不同 —— 这两个渠道的签名规则不可互换
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	signParams := StringMap(params)
	signParams["access_token"] = &token
	signParams["timestamp"] = &timestamp
	// api_sign 自己不能参与自己的计算，因此放进表里但值为 nil，
	// Canonicalize 会跳过 nil 值 —— 这正是签名器要支持 nil 的原因
	signParams["api_sign"] = nil

	q := u.Query()
	if appendParamsToQuery {
		for k, v := range params {
			q.Set(k, v)
		}
	}
	q.Set("access_token", token)
	q.Set("timestamp", timestamp)
	q.Set("api_sign", Sign(a.clientSecret, signParams))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// checkSuccess 校验业务码。
//
// 上游是 get("errno").asInt —— 字段缺失时 NPE，把本该抛出的业务错误顶掉，
// 排查时看到的是 NullPointerException 而不是 OPPO 返回的真实原因。
// 这里 errno 缺失按协议不符处理：不能静默当成成功。
//
// includeRaw=false 用于 token 接口 —— 那个响应体里含裸 access_token，
// 不能通过错误的 Raw 字段泄漏到日志。
func (a *API) checkSuccess(body, action string, includeRaw bool) error {
	var code errnoOnly
	if err := json.Unmarshal([]byte(body), &code); err != nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 失败：OPPO 响应不是合法 JSON，接口可能已变更：" + err.Error(),
			Raw:     rawIf(includeRaw, body),
			Err:     err,
		}
	}
	if code.Errno == nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 失败：OPPO 响应里没有 errno 字段，接口可能已变更",
			Raw:     rawIf(includeRaw, body),
		}
	}
	if *code.Errno == successCode {
		return nil
	}

	var env envelope
	reason := ""
	if err := json.Unmarshal([]byte(body), &env); err == nil && env.Data != nil {
		reason = strings.TrimSpace(env.Data.Message)
	}
	if reason == "" {
		reason = "OPPO 未返回错误描述"
	}
	return &eperr.Error{
		Kind:    eperr.KindChannelRejected,
		Channel: ID,
		Code:    strconv.Itoa(*code.Errno),
		Msg:     fmt.Sprintf("%s 失败：%s", action, reason),
		Raw:     rawIf(includeRaw, body),
	}
}

func rawIf(include bool, body string) string {
	if !include {
		return ""
	}
	return truncate(body)
}

func unmarshal(body string, target any) error {
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "解析 OPPO 响应失败，接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return nil
}

// unmarshalData 解析 data 字段为强类型。只在 checkSuccess 之后调用。
func unmarshalData(body string, target any) error {
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &wrapper); err != nil {
		return unmarshal(body, target)
	}
	if len(wrapper.Data) == 0 || string(wrapper.Data) == "null" {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "OPPO 响应缺少 data 字段，接口可能已变更",
			Raw:     truncate(body),
		}
	}
	if err := json.Unmarshal(wrapper.Data, target); err != nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "解析 OPPO 的 data 失败，接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return nil
}

func protocolError(msg, body string) error {
	return &eperr.Error{
		Kind:    eperr.KindProtocolMismatch,
		Channel: ID,
		Msg:     msg,
		Raw:     truncate(body),
	}
}

// multipartUploadBody 构造上传请求体：file + type + sign 三个字段。
//
// 强制 form-data（不是 mixed），上游注释明确写了 OPPO 不接受 mixed。
// 头尾手工拼装并预先算出 ContentLength，不把 APK 整体读进内存。
func multipartUploadBody(path, sign string, onProgress func(float64)) (io.Reader, int64, error) {
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

	var head bytes.Buffer
	head.WriteString("--" + boundary + "\r\n")
	head.WriteString(`Content-Disposition: form-data; name="` + formFileField + `"; filename="` +
		escapeQuotes(baseName(path)) + "\"\r\n")
	head.WriteString("Content-Type: application/octet-stream\r\n\r\n")

	var tail bytes.Buffer
	tail.WriteString("\r\n")
	for _, field := range [][2]string{{"type", uploadTypeAPK}, {"sign", sign}} {
		tail.WriteString("--" + boundary + "\r\n")
		tail.WriteString(`Content-Disposition: form-data; name="` + field[0] + "\"\r\n\r\n")
		tail.WriteString(field[1] + "\r\n")
	}
	tail.WriteString("--" + boundary + "--\r\n")

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

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

func quote(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(data)
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}
