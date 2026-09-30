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
	pathTaskState = "/resource/v1/app/task-state"
	successCode   = 0
	maxRaw        = 2000
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
	Summary     string           `json:"summary"`
	DetailDesc  string           `json:"detail_desc"`
	VersionCode *jsonx.FlexInt64 `json:"version_code"`
	VersionName string           `json:"version_name"`

	// AuditStatus 是审核状态。**官方文档把它标为 string，实测也确实是字符串**
	// （如 "111"、"444"），因此这里用 FlexString 而不是 FlexInt64。
	//
	// 用整数会踩到一个真实的冲突：官方对照表里既有 0（未发布）也有 00（资质审核中），
	// 两者解析成整数都是 0，而它们的含义完全不同。字符串比较没有这个问题。
	AuditStatus *jsonx.FlexString `json:"audit_status"`
	// AuditStatusName 是审核状态的中文描述，实测随 audit_status 一起返回
	// （如 "上线"、"审核不通过"）。有它就不必自己维护一份标签映射 ——
	// 直接用渠道自己的措辞，用户在后台看到的词与这里完全一致。
	AuditStatusName string `json:"audit_status_name"`
	// OldAuditStatus 是上一次的审核状态，同为字符串。当前状态已回落到正常值、
	// 而旧状态是被拒时，它是「刚被拒过」的线索。
	OldAuditStatus *jsonx.FlexString `json:"old_audit_status"`
	// UpdateInfoCheck 是「更新资料」的审核状态：1-审核中，0-不在审核中。
	// 与 AuditStatus 是两条独立的审核线（改版本 vs 改素材）。
	UpdateInfoCheck *jsonx.FlexInt64 `json:"update_info_check"`

	PrivacyURL     string `json:"privacy_source_url"`
	SecondCategory string `json:"ver_second_category_id"`
	ThirdCategory  string `json:"ver_third_category_id"`
	IconURL        string `json:"icon_url"`
	PicURL         string `json:"pic_url"`
	// 以下三个是发布版本接口的「必传」字段（见 OPPO 文档 id=10999 的更新说明）。
	// 漏传时 app/upd 会返回 errno=0 并把任务排入队列，但异步任务随后静默失败 ——
	// 表现为「提交成功」而线上毫无变化，只有查 task-state 才能发现。
	AppName           string `json:"app_name"`
	AgeLevel          string `json:"age_level"`
	AdaptiveEquipment string `json:"adaptive_equipment"`
	TestDesc          string `json:"test_desc"`
	BusinessUsername  string `json:"business_username"`
	BusinessEmail     string `json:"business_email"`
	BusinessMobile    string `json:"business_mobile"`
	CopyrightURL      string `json:"copyright_url"`
	ElectronicCert    string `json:"electronic_cert_url"`

	// 以下四个是审核意见（官方文档 id=11004「查询普通包详情」有定义）。
	//
	// **不要拿 RefuseReason 直接展示给用户**：实测它把审核员的测试环境也拼了进去，
	// 例如「测试机型：OPPO Find X9；,Android版本：16.0.5；,软件版本：PLJ110；」
	// —— 这不是给应用方看的问题描述。真正需要处理的内容在后面几条。
	//
	// RefuseReasonWithSugg 是**按条目配对**的（reason + advice），与 OPPO 后台
	// 界面上展示的一致。优先用它：直接逐条呈现「问题 + 建议」，
	// 不必自己按逗号切分那个拼接串（而拼接串里本身就含逗号，切分不可靠）。
	RefuseReason         string           `json:"refuse_reason"`
	RefuseAdvice         string           `json:"refuse_advice"`
	RefuseFile           string           `json:"refuse_file"`
	RefuseReasonWithSugg []OppoRefuseItem `json:"refuse_reason_with_sugg"`
}

// OppoRefuseItem 是审核意见的一条，reason 与 advice 配对。
//
// 环境信息条目（测试机型、Android 版本、软件版本）的 advice 是空串，
// 据此可以只挑出真正需要处理的条目；工具不做内容判断，把筛选后的原文交给使用者。
type OppoRefuseItem struct {
	Reason string `json:"reason"`
	Advice string `json:"advice"`
}

// oppoAuditStates 是官方《审核状态对照表》（文档 id=11176）里的全部取值。
//
// 键是字符串而非整数：官方表里 0（未发布）与 00（资质审核中）是两个不同取值，
// 用整数会把它们并成同一个键。文档明确标注 audit_status 的类型是 string。
//
// 描述一栏照抄文档原文，仅在渠道没返回 audit_status_name 时兜底。
var oppoAuditStates = map[string]struct {
	State channel.ReviewState
	Label string
}{
	"0":   {channel.ReviewDraft, "未发布"},
	"1":   {channel.ReviewUnderReview, "审核中"},
	"2":   {channel.ReviewPending, "审核通过"},
	"3":   {channel.ReviewRejected, "测试不通过"},
	"4":   {channel.ReviewUnderReview, "运营审核中"},
	"5":   {channel.ReviewRejected, "运营打回"},
	"6":   {channel.ReviewPending, "运营通过"},
	"7":   {channel.ReviewPending, "定时发布"},
	"00":  {channel.ReviewUnderReview, "资质审核中"},
	"11":  {channel.ReviewUnderReview, "资质审核通过"},
	"-11": {channel.ReviewRejected, "资质审核不通过"},
	"-22": {channel.ReviewPending, "报备提交成功"},
	"22":  {channel.ReviewOffline, "已冻结"},
	"111": {channel.ReviewOnline, "上线"},
	"222": {channel.ReviewOffline, "下线"},
	"444": {channel.ReviewRejected, "审核不通过"},
}

// ToMarketInfo 转成渠道无关的状态。
//
// # 为什么未知状态码归为 Unknown 而不是「审核中」
//
// 原先的实现把 default 分支当成 UnderReview。这看起来保守，实际是错的：
// `canSubmit` 由「是否审核中」推导，而 `PublishPolicy` 会因此**拒绝提交**。
// 于是当 OPPO 返回一个我们不认识的状态码时，用户看到的是
// 「渠道正在审核中，不能提交新版本」—— 但真实情况可能是被拒了、应该重新提交。
//
// 现在按官方对照表（id=11176）补全全部 17 个取值，见 oppoAuditStates。
// 仍未收录的值是 Unknown，原始值留在 RawState 里供人核对 ——
// 那说明 OPPO 新增了状态，而不是我们该去猜。
//
// audit_status 缺失同样归为 Unknown —— 接口没返回状态和商店确实在审核是两件事，
// 混在一起会让排查走错方向。上游在这里会 NPE。
func (a AppInfo) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw, label := "", ""
	if a.AuditStatus != nil {
		raw = string(*a.AuditStatus)
		if entry, ok := oppoAuditStates[raw]; ok {
			state, label = entry.State, entry.Label
		}
	}
	// 渠道自己给的中文描述优先于我们的映射表：它是权威措辞，
	// 且 OPPO 新增状态时这里会自动跟上，不必等我们补表
	if name := strings.TrimSpace(a.AuditStatusName); name != "" {
		label = name
	}
	var version *channel.Version
	if a.VersionCode != nil && strings.TrimSpace(a.VersionName) != "" {
		version = &channel.Version{Code: int64(*a.VersionCode), Name: a.VersionName}
	}

	info := channel.NewMarketInfo(ID, state, version, raw)
	info.RawStateLabel = label
	info.Review = a.reviewFeedback()
	return info
}

// reviewFeedback 组装审核意见。
//
// 三个数据源的取舍：
//
//   - RefuseReasonWithSugg 优先：按条目配对（reason + advice），与后台界面一致，
//     且能通过 advice 是否为空区分「测试环境信息」与「真实问题」。
//   - RefuseAdvice 作为整体建议补进 Notes —— 实测它比逐条 advice 更完整
//     （逐条里只有最后一条带 advice，而 RefuseAdvice 是一段完整的修改指引）。
//   - RefuseFile 是审核员给的附件（实测是 zip，含截图），放 Attachments。
//
// **RefuseReason 不直接用**：实测它把测试环境信息拼在最前面，原样展示会误导使用者
// 以为「测试机型」是需要处理的问题。仅在 RefuseReasonWithSugg 缺失时回退到它，
// 保证有内容可取。
func (a AppInfo) reviewFeedback() *channel.ReviewFeedback {
	fb := &channel.ReviewFeedback{}

	// 逐条意见：跳过纯环境信息（advice 为空且 reason 以「测试机型/Android版本/软件版本」开头）
	var opinions []string
	for _, item := range a.RefuseReasonWithSugg {
		reason := strings.TrimSpace(item.Reason)
		if reason == "" {
			continue
		}
		advice := strings.TrimSpace(item.Advice)
		if advice == "" && isEnvironmentNote(reason) {
			continue
		}
		if advice != "" {
			opinions = append(opinions, reason+" —— 建议："+advice)
		} else {
			opinions = append(opinions, reason)
		}
	}

	if len(opinions) > 0 {
		fb.Opinion = strings.Join(opinions, "\n")
	} else if fallback := strings.TrimSpace(a.RefuseReason); fallback != "" && !isAllEnvironmentNotes(fallback) {
		// 回退：with_sugg 缺失时用原始串，但仅在它确实含有非环境信息时。
		//
		// 实测**应用正常上线时 refuse_reason 也有值**，内容全是审核员的测试环境
		// （「测试机型：OPPO Find X9；,Android版本：16.0.5；,软件版本：PLJ110；」）。
		// 原样带出会让「已上架」的应用也挂一条「审核意见」，纯属噪音且误导。
		fb.Opinion = fallback
	}

	// RefuseAdvice 不单独成为一条 Note。
	//
	// 实测它与最后一条 with_sugg 的 advice 是**同一段文本**（逐条里的 advice 就是
	// 从这里派生的），两条并排展示完全重复。只有当 with_sugg 整体缺失、
	// Opinion 走了 RefuseReason 回退时，它才是唯一可用的建议来源。
	if len(opinions) == 0 {
		if advice := strings.TrimSpace(a.RefuseAdvice); advice != "" {
			fb.Notes = append(fb.Notes, channel.ReviewNote{
				Kind:    "修改建议",
				Opinion: advice,
			})
		}
	}

	if file := strings.TrimSpace(a.RefuseFile); file != "" {
		fb.Attachments = append(fb.Attachments, file)
	}
	return fb
}

// isAllEnvironmentNotes 判断逗号拼接的原始串是否只含环境信息。
//
// refuse_reason 用逗号连接各条目，而条目自身以「；」结尾 —— 按逗号切分后逐条判断。
func isAllEnvironmentNotes(raw string) bool {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if !isEnvironmentNote(p) {
			return false
		}
	}
	return true
}

// isEnvironmentNote 判断一条意见是否为审核员的测试环境信息。
//
// 实测这些条目固定以「测试机型」「Android版本」「软件版本」开头，
// 且 advice 为空 —— 它们不构成需要处理的问题，混在意见里会干扰阅读。
func isEnvironmentNote(reason string) bool {
	for _, prefix := range []string{"测试机型", "Android版本", "Android 版本", "软件版本"} {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
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

// --- 异步任务状态 ---

// taskStates 是 /resource/v1/app/task-state 的取值。
const (
	taskPending = "1" // 待处理
	taskSucceed = "2" // 处理成功
	taskFailed  = "3" // 处理失败
)

type taskStateResp struct {
	PkgName     string `json:"pkg_name"`
	VersionCode string `json:"version_code"`
	TaskState   string `json:"task_state"`
	ErrMsg      string `json:"err_msg"`
}

// WaitSubmitResult 轮询提交任务的处理结果。
//
// # 为什么必须轮询
//
// `/resource/v1/app/upd` 是**异步**接口：它返回 errno=0 只代表任务已入队，
// 不代表版本创建成功。真正结果要查 task-state：
//
//	1 待处理 / 2 处理成功 / 3 处理失败
//
// 不查就会把「入队成功」当成「发布成功」上报 —— 而任务随后可能因为缺必传
// 参数、apk 包名不符、截图尺寸超标等原因静默失败，线上版本号纹丝不动。
// 这类假成功比明确报错更难排查：日志与退出码都是成功，只有登录后台才发现
// 什么也没发生。
//
// 返回 nil 表示任务确实处理成功；失败时返回带渠道原因的错误。
func (a *API) WaitSubmitResult(
	ctx context.Context,
	token, applicationID, versionCode string,
) error {
	const (
		interval = 3 * time.Second
		// OPPO 文档称「接口处理可能会比较耗时，建议客户端执行等待时间设置为 10 秒以上」。
		// 取 3 分钟上限：再久通常不是慢，而是任务卡住，继续等没有意义。
		timeout = 3 * time.Minute
	)

	deadline := time.Now().Add(timeout)
	var last taskStateResp

	for {
		state, err := a.getTaskState(ctx, token, applicationID, versionCode)
		if err != nil {
			return err
		}
		last = state

		switch state.TaskState {
		case taskSucceed:
			return nil
		case taskFailed:
			msg := strings.TrimSpace(state.ErrMsg)
			if msg == "" {
				msg = "OPPO 未给出失败原因"
			}
			return eperr.RejectedError(ID, "task_state="+taskFailed,
				"OPPO 处理版本更新任务失败：%s（版本号 %s）", msg, versionCode)
		}

		if time.Now().After(deadline) {
			// 超时不是失败：任务可能仍在处理，只是比预期慢。
			// 报成「失败」会诱使使用者重试，而重复提交会产生重复版本。
			return eperr.PreconditionError(ID,
				"OPPO 版本更新任务在 %s 内未处理完（当前状态 %q）。"+
					"任务可能仍在处理，请稍后到 OPPO 后台确认，不要直接重试以免产生重复版本",
				timeout, last.TaskState)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (a *API) getTaskState(
	ctx context.Context,
	token, applicationID, versionCode string,
) (taskStateResp, error) {
	params := map[string]string{
		"pkg_name":     applicationID,
		"version_code": versionCode,
	}
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := a.signedRequest(ctx, http.MethodPost, a.baseURL+pathTaskState,
		params, token, false, strings.NewReader(form.Encode()))
	if err != nil {
		return taskStateResp{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return taskStateResp{}, err
	}
	if err := a.checkSuccess(body, "查询任务状态", true); err != nil {
		return taskStateResp{}, err
	}
	var resp taskStateResp
	if err := unmarshalData(body, &resp); err != nil {
		return taskStateResp{}, err
	}
	return resp, nil
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
		"pkg_name":     info.ApplicationID,
		"version_code": strconv.FormatInt(info.VersionCode, 10),
		// 文档标注必传；缺失会让异步任务静默失败（见 AppInfo 里同名字段的注释）
		"app_name":           required.appName,
		"age_level":          required.ageLevel,
		"adaptive_equipment": required.adaptiveEquipment,
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
	appName, ageLevel, adaptiveEquipment           string
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
		{"应用名称（app_name）", info.AppName},
		{"年龄分级（age_level）", info.AgeLevel},
		{"平板适配（adaptive_equipment）", info.AdaptiveEquipment},
	}
	for _, c := range checks {
		if strings.TrimSpace(c.value) == "" {
			return requiredFields{}, eperr.PreconditionError(ID,
				"OPPO 商店缺少必填资料：%s。提交新版本需要回传这些字段，"+
					"请先在 OPPO 开放平台补全应用信息", c.label)
		}
	}
	return requiredFields{
		summary:           info.Summary,
		detailDesc:        info.DetailDesc,
		privacyURL:        info.PrivacyURL,
		secondCategory:    info.SecondCategory,
		thirdCategory:     info.ThirdCategory,
		iconURL:           info.IconURL,
		picURL:            info.PicURL,
		appName:           info.AppName,
		ageLevel:          info.AgeLevel,
		adaptiveEquipment: info.AdaptiveEquipment,
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
