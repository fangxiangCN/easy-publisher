// Package huawei 实现华为 AppGallery 渠道。
package huawei

import (
	"fmt"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
)

// ID 是渠道标识。
const ID = "huawei"

const (
	// BaseURL 是华为 AppGallery Connect 的接口域名
	BaseURL = "https://connect-api.cloud.huawei.com/"

	// APKFileType 是华为接口定义的 APK 文件类型固定值
	APKFileType = 5
	// APKMediaType 是上传时的 Content-Type
	APKMediaType = "application/octet-stream"
	// DefaultLang 是更新版本描述时的语言
	DefaultLang = "zh-CN"

	maxRaw = 2000
	logTag = "华为应用市场"
)

// 各接口路径
const (
	pathToken         = "api/oauth2/v1/token"
	pathAppIDList     = "api/publish/v2/appid-list"
	pathAppInfo       = "api/publish/v2/app-info"
	pathUploadURL     = "api/publish/v2/upload-url/for-obs"
	pathAppFileInfo   = "api/publish/v2/app-file-info"
	pathCompileStatus = "api/publish/v2/package/compile/status"
	pathAppLanguage   = "api/publish/v2/app-language-info"
	pathAppSubmit     = "api/publish/v2/app-submit"
)

// ret 是华为的通用业务返回码，放在每个响应的 ret 字段里。
type ret struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
}

// checkSuccess 校验业务码，要求 ret 必须存在。
//
// ret 缺失视为协议异常而不是成功：那说明响应不是华为的正常信封。
func (r *ret) checkSuccess(action string) error {
	if r == nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 失败：华为响应缺少 ret 字段，接口可能已变更",
		}
	}
	return r.checkSuccessIfPresent(action)
}

// checkSuccessIfPresent 只在 ret 存在时校验。
//
// 取 token 那一步华为**成功时不返回 ret**，所以那里必须用这个版本 ——
// 与上游的 throwOnFailIfPresent 行为一致。
func (r *ret) checkSuccessIfPresent(action string) error {
	if r == nil {
		return nil
	}
	if r.Code == nil || *r.Code == 0 {
		return nil
	}
	msg := strings.TrimSpace(r.Msg)
	if msg == "" {
		msg = "华为未返回错误描述"
	}
	code := fmt.Sprint(*r.Code)
	return &eperr.Error{
		Kind:    eperr.KindChannelRejected,
		Channel: ID,
		Code:    code,
		// 错误码写进 message：用户拿它去查华为文档，
		// 只给一句自然语言描述等于把唯一的线索藏起来
		Msg: fmt.Sprintf("%s 失败：%s (code=%s)", action, msg, code),
	}
}

// tokenReq 是取 token 的请求体。
type tokenReq struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	GrantType    string `json:"grant_type"`
}

// String 绝不输出密钥。
func (t tokenReq) String() string {
	return fmt.Sprintf("tokenReq(clientId=%s, grantType=%s)", "***", t.GrantType)
}

// tokenResp 是取 token 的响应。
type tokenResp struct {
	AccessToken string `json:"access_token"`
	// Ret 天然可空：取 token 成功时华为不返回它
	Ret *ret `json:"ret"`
}

// String 绝不输出 token。
func (t tokenResp) String() string { return "tokenResp(token=***)" }

// appIDResp 是 get-app-id 的响应。
type appIDResp struct {
	Ret    *ret         `json:"ret"`
	AppIDs []appIDEntry `json:"appids"`
}

type appIDEntry struct {
	Name string `json:"key"`
	ID   string `json:"value"`
}

// AppInfo 是应用信息。
//
// 全部字段可空，这是上游 issue #7 的根因修复：原实现把 versionCode / versionNumber
// 声明为非空无默认值，而应用只有一个尚未上传 APK 的草稿版本（releaseState=7）时
// 华为压根不返回 versionCode，反序列化直接抛异常，用户连「查询市场状态」都做不了。
// 解析层不再承担校验职责，缺字段由业务层显式判断。
type AppInfo struct {
	// ReleaseState 取值：
	//
	//	0 已上架 / 1 上架审核不通过 / 2 已下架（含强制下架）/ 3 待上架
	//	4 审核中 / 5 升级中 / 6 申请下架 / 7 草稿
	//	8 升级审核不通过 / 9 下架审核不通过 / 10 应用被开发者下架 / 11 撤销上架
	ReleaseState  *int   `json:"releaseState"`
	VersionCode   *int64 `json:"versionCode"`
	VersionNumber string `json:"versionNumber"`
	// 原实现还声明了 onShelfVersionNumber（在架版本号），但全项目无人读取，
	// 却因为非空声明成为一个额外的解析失败点，直接删掉。

	// audit 承载响应里与 appInfo 平级的 auditInfo。
	//
	// 非导出且不参与反序列化：它不在这层 JSON 里，由 GetAppInfo 在解析完整响应后填入。
	// 这样放是为了让 ToMarketInfo 能一次性给出完整状态，调用方不必再解析一次响应体。
	audit *AuditInfo
}

// AuditInfo 是华为的审核意见信息。
//
// 官方文档（v2 查询应用信息）把 AuditInfo 列为响应里与 appInfo 平级的一级字段：
//
//	auditInfo | O | AuditInfo | 审核意见信息
//
// 官方响应示例里未拒审时是 `"auditInfo": { "auditOpinion": "" }` ——
// 字段始终返回，没有内容时是空串而不是缺失。
//
// # 为什么用 FlexString / FlexInt64 而不是原生类型
//
// 文档写 auditOpinion 是 String(1024)、copyRightAuditResult 是 Integer(4)，
// 但这家渠道的类型并不可靠 —— 同一个接口的 versionCode 在不同应用上
// 都可能以字符串返回。已由 jsonx 包统一兜住，这里沿用同一策略。
type AuditInfo struct {
	// AuditOpinion 是应用整体审核意见。必选字段，未拒审时为空串
	AuditOpinion *jsonx.FlexString `json:"auditOpinion"`

	// 以下三项只有在中国大陆地区发布的应用才会返回。
	// Result 取值：0 通过 / 1 不通过（注意不是布尔）
	CopyRightAuditResult      *jsonx.FlexInt64  `json:"copyRightAuditResult"`
	CopyRightAuditOpinion     *jsonx.FlexString `json:"copyRightAuditOpinion"`
	CopyRightCodeAuditResult  *jsonx.FlexInt64  `json:"copyRightCodeAuditResult"`
	CopyRightCodeAuditOpinion *jsonx.FlexString `json:"copyRightCodeAuditOpinion"`
	RecordAuditResult         *jsonx.FlexInt64  `json:"recordAuditResult"`
	RecordAuditOpinion        *jsonx.FlexString `json:"recordAuditOpinion"`
}

// toReviewFeedback 把华为的多条审核意见整理成统一的反馈结构。
//
// 华为与其它渠道的差别：它按维度分别给出结果（整体 / 版权 / 版号 / 备案），
// 后三项是工信部合规要求，被拒时往往只有其中某一项不通过。
// 因此这里既填 Opinion（整体意见），也填 Notes（各维度明细）。
func (a *AuditInfo) toReviewFeedback() *channel.ReviewFeedback {
	if a == nil {
		return nil
	}
	feedback := &channel.ReviewFeedback{}
	if a.AuditOpinion != nil {
		feedback.Opinion = strings.TrimSpace(string(*a.AuditOpinion))
	}
	for _, item := range []struct {
		kind    string
		result  *jsonx.FlexInt64
		opinion *jsonx.FlexString
	}{
		{"版权", a.CopyRightAuditResult, a.CopyRightAuditOpinion},
		{"版号", a.CopyRightCodeAuditResult, a.CopyRightCodeAuditOpinion},
		{"备案", a.RecordAuditResult, a.RecordAuditOpinion},
	} {
		note := channel.ReviewNote{Kind: item.kind}
		if item.result != nil {
			// 0 通过 / 1 不通过。其它取值不做猜测
			switch int64(*item.result) {
			case 0:
				passed := true
				note.Passed = &passed
			case 1:
				passed := false
				note.Passed = &passed
			}
		}
		if item.opinion != nil {
			note.Opinion = strings.TrimSpace(string(*item.opinion))
		}
		// 既没有结果也没有意见的维度不列出，避免刷出一堆空条目
		if note.Passed != nil || note.Opinion != "" {
			feedback.Notes = append(feedback.Notes, note)
		}
	}
	if feedback.Empty() {
		return nil
	}
	return feedback
}

// ToMarketInfo 映射到渠道无关的状态。
//
// 与原实现的两处差异：
//   - 补齐草稿态（7）与下架态（2/6/10）。原实现只认 0/4/5/8，
//     草稿应用一律显示「状态未知」，用户无从判断能不能提交。
//   - 版本信息缺失时 LastVersion 传 nil 而不是伪造一个版本号，也不抛异常。
//
// ToMarketInfo 映射到渠道无关的状态，含审核意见。
func (a AppInfo) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw := ""
	if a.ReleaseState != nil {
		raw = fmt.Sprint(*a.ReleaseState)
		switch *a.ReleaseState {
		case 0:
			state = channel.ReviewOnline
		case 4, 5:
			state = channel.ReviewUnderReview
		case 8:
			state = channel.ReviewRejected
		case 7:
			state = channel.ReviewDraft
		case 2, 6, 10:
			state = channel.ReviewOffline
		}
	}

	var version *channel.Version
	if a.VersionCode != nil && strings.TrimSpace(a.VersionNumber) != "" {
		version = &channel.Version{Code: *a.VersionCode, Name: a.VersionNumber}
	}
	info := channel.NewMarketInfo(ID, state, version, raw)
	info.Review = a.audit.toReviewFeedback()
	return info
}

// UploadURL 是上传地址与需要原样透传的请求头。
type UploadURL struct {
	URL      string            `json:"url"`
	ObjectID string            `json:"objectId"`
	Headers  map[string]string `json:"headers"`
}

// uploadURLResp 是 get-upload-url 的响应。
type uploadURLResp struct {
	Ret  *ret       `json:"ret"`
	Info *UploadURL `json:"urlInfo"`
}

// refreshApk 是绑定文件的请求体。
type refreshApk struct {
	FileType int         `json:"fileType"`
	Files    []fileEntry `json:"files"`
}

type fileEntry struct {
	FileName    string `json:"fileName"`
	FileDestURL string `json:"fileDestUrl"`
}

// bindFileResp 是绑定文件的响应。
type bindFileResp struct {
	Ret        *ret     `json:"ret"`
	PkgVersion []string `json:"pkgVersion"`
}

// apkState 是编译状态查询的响应。
type apkState struct {
	Ret          *ret            `json:"ret"`
	PkgStateList []pkgStateEntry `json:"pkgStateList"`
}

type pkgStateEntry struct {
	PkgID         string `json:"pkgId"`
	SuccessStatus *int   `json:"successStatus"`
}

// isSuccess 报告编译是否成功。0 表示成功。
func (p pkgStateEntry) isSuccess() bool {
	return p.SuccessStatus != nil && *p.SuccessStatus == 0
}

// versionDesc 是更新版本描述的请求体。
type versionDesc struct {
	NewFeatures string `json:"newFeatures"`
	Lang        string `json:"lang"`
}

// resp 是只关心业务码的响应。
type resp struct {
	Ret *ret `json:"ret"`
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}
