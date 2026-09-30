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

// AGCReleaseStates 是官方文档里 releaseState 的全部取值。
//
// 来源：AGC《查询应用信息 - Android - Publishing API》的 AppInfo 表，
// 该表注明「releaseType 为 1 时，状态含义如下」（我们发布的都是 releaseType=1
// 的商用版本）。
//
// 导出是因为**鸿蒙走的是同一套枚举** —— AGC 的 v3 接口文档明确写着「state 字段
// 的取值请参考 AppInfo 中的 releaseState 参数」。两个渠道共用一张表，就不存在
// 「改了 Android 忘了改鸿蒙」这种偏差。
//
// 描述一栏照抄文档，不含我们自己的归纳 —— 它会被原样展示给用户，
// 好处是用户在 AGC 后台看到的词与我们报出来的完全一致，不必做二次翻译。
var AGCReleaseStates = map[int]struct {
	State channel.ReviewState
	Label string
}{
	0:  {channel.ReviewOnline, "已上架"},
	1:  {channel.ReviewRejected, "上架审核不通过"},
	2:  {channel.ReviewOffline, "已下架（含强制下架）"},
	3:  {channel.ReviewPending, "待上架，预约上架"},
	4:  {channel.ReviewUnderReview, "审核中"},
	5:  {channel.ReviewUnderReview, "升级审核中"},
	6:  {channel.ReviewOffline, "申请下架"},
	7:  {channel.ReviewDraft, "草稿"},
	8:  {channel.ReviewRejected, "升级审核不通过"},
	9:  {channel.ReviewUnknown, "下架审核不通过"},
	10: {channel.ReviewOffline, "应用被开发者下架"},
	11: {channel.ReviewOffline, "撤销上架"},
	12: {channel.ReviewUnderReview, "预审中"},
	13: {channel.ReviewRejected, "预审不通过"},
}

// ToMarketInfo 映射到渠道无关的状态。
//
// 早期实现只认 0/4/5/8，其余一律「状态未知」，用户无从判断能不能提交。
// 现在按官方文档补全全部 13 个取值（见 huaweiReleaseStates），未收录的值
// 仍是 Unknown —— 那说明华为新增了状态码，此时原始值会留在 RawState 里，
// 拿去后台核对即可。
//
// 几处取值归类的依据：
//   - 1（上架）、8（升级）、13（预审）都是「你提交的版本被拒」，需要修改后
//     重提，统一归 Rejected。9（下架审核不通过）不在其中 —— 它指下架申请被拒，
//     与「新版本被拒」是两件事，我们的桶里没有对应位置，归 Unknown 并保留官方
//     描述，由人判断。
//   - 2/6/10/11 都是「用户此刻下载不到」，统一归 Offline，精确差异由
//     RawStateLabel 表达（「撤销上架」与「强制下架」对用户的含义并不相同）。
//   - 3 是审核已通过、只差发布这一步，既不是 Online（用户还下不到）也不是
//     Offline（没有被打回），单列为 Pending。
//   - 12（预审中）仍在审核流程内，对「能否提交新版本」的影响与 4/5 相同。
//
// ToMarketInfo 映射到渠道无关的状态，含审核意见。
func (a AppInfo) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw, label := "", ""
	if a.ReleaseState != nil {
		raw = fmt.Sprint(*a.ReleaseState)
		if entry, ok := AGCReleaseStates[*a.ReleaseState]; ok {
			state, label = entry.State, entry.Label
		}
	}

	var version *channel.Version
	if a.VersionCode != nil && strings.TrimSpace(a.VersionNumber) != "" {
		version = &channel.Version{Code: *a.VersionCode, Name: a.VersionNumber}
	}
	info := channel.NewMarketInfo(ID, state, version, raw)
	info.RawStateLabel = label
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
