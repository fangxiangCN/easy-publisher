// Package harmony 实现鸿蒙（HarmonyOS）AppGallery 渠道。
package harmony

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
	"strconv"
)

// ID 是渠道标识。
const ID = "harmony"

const (
	// BaseURL 与华为渠道同域：鸿蒙应用与 Android 应用共用同一套 AGC 鉴权
	BaseURL = "https://connect-api.cloud.huawei.com/"

	// InitContentType / InitFileType / InitReleaseType 是 init 接口的固定参数。
	// 取值与参照实现一致，不要改动
	InitContentType = "application/octet-stream"
	InitFileType    = 1
	InitReleaseType = 1

	// FallbackPartSize 是华为未返回 nspPartMinSize 时的兜底分片大小
	FallbackPartSize = 5 * 1024 * 1024

	// PartKeyPrefix 是分片请求体里的键名前缀。
	//
	// 华为文档用 Swagger 占位符命名，看着像没改完的代码，但必须逐字匹配 ——
	// 改成 "part1" 之类会被服务端拒绝。
	PartKeyPrefix = "additionalProp"

	// RemarkMin / RemarkMax 是华为 remark 字段的长度约束。
	// 该字段本身可选，但填写时必须落在这个区间
	RemarkMin = 10
	RemarkMax = 300

	// CodePackageNotCompiled 表示「软件包尚未编译完成就提交」。
	//
	// 这是华为**明确拒绝**了本次送审 —— 结果确定，服务端没有受理，
	// 因此等待后重试不会造成重复送审。
	// 这与网络超时的性质相反：超时是「不知道有没有受理」，那种一律不自动重试。
	CodePackageNotCompiled = "204144660"

	maxRaw = 2000
	logTag = "鸿蒙应用市场"
)

// 各接口路径。注意 v3 与 v2 的差别：鸿蒙走 v3，Android 走 v2。
const (
	pathToken            = "api/oauth2/v1/token"
	pathMultipartInit    = "api/publish/v2/upload/multipart/init"
	pathMultipartParts   = "api/publish/v2/upload/multipart/parts"
	pathMultipartCompose = "api/publish/v2/upload/multipart/compose"
	pathAppInfoV3        = "api/publish/v3/app-info"
	pathAppPackageInfo   = "api/publish/v3/app-package-info"
	pathAppSubmit        = "api/publish/v3/app-submit"
)

// ret 是华为系接口通用的业务结果包装。
type ret struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
}

// checkSuccess 校验业务码。ret 缺失视为协议异常而不是成功。
func (r *ret) checkSuccess(action string) error {
	if r == nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 失败：鸿蒙接口响应缺少 ret 字段，无法判定成败",
		}
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
		// 错误码写进 message：用户拿它去查华为文档
		Msg: fmt.Sprintf("%s 失败：%s (code=%s)", action, msg, code),
	}
}

// MultipartInitResp 是 multipart/init 的响应。
//
// NspPartMinSize 是华为规定的分片大小，必须按它切片 —— 自己选一个「合理」的值
// 会导致 parts 接口返回的地址与实际分片不匹配。
type MultipartInitResp struct {
	Ret            *ret   `json:"ret"`
	ObjectID       string `json:"objectId"`
	NspUploadID    string `json:"nspUploadId"`
	NspPartMinSize *int64 `json:"nspPartMinSize"`
}

// PartDescriptor 是分片的摘要与长度。
type PartDescriptor struct {
	Sha256 string `json:"sha256"`
	Length int64  `json:"length"`
}

// PartUploadInfo 是每个分片的上传信息。
//
// Headers 声明为 any：华为返回的可能是 JSON 对象，也可能是序列化后的字符串。
// 用 any 接住再归一化，避免类型不匹配把真实错误顶掉。
//
// URL 是短时效签名地址，**必须原样使用它给的请求头**，少一个或多一个都可能导致
// 签名校验失败。
type PartUploadInfo struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Headers      any    `json:"headers"`
	PartObjectID string `json:"partObjectId"`
}

// MultipartPartsResp 是 multipart/parts 的响应。
type MultipartPartsResp struct {
	Ret           *ret                      `json:"ret"`
	UploadInfoMap map[string]PartUploadInfo `json:"uploadInfoMap"`
}

// CompletedPart 是合并分片时需要回传的每片信息。
//
// ETag 必须原样保留（含引号），不做任何加工。
type CompletedPart struct {
	PartObjectID string `json:"partObjectId"`
	ETag         string `json:"etag"`
}

// ComposeResp 是 multipart/compose 的响应。
type ComposeResp struct {
	Ret *ret `json:"ret"`
}

// AppPackageInfoReq 是 v3 关联草稿的请求体。
type AppPackageInfoReq struct {
	FileName string `json:"fileName"`
	ObjectID string `json:"objectId"`
}

// AppPackageInfoResp 是 v3 关联草稿的响应。
//
// packageId 可能直接在顶层，也可能嵌在 data 里 —— 两种形状都要接住。
type AppPackageInfoResp struct {
	Ret       *ret   `json:"ret"`
	PackageID string `json:"packageId"`
	Data      *struct {
		PackageID string `json:"packageId"`
	} `json:"data"`
}

// resolvePackageID 取出 packageId，两种位置都接受。
func (r AppPackageInfoResp) resolvePackageID() string {
	if id := strings.TrimSpace(r.PackageID); id != "" {
		return id
	}
	if r.Data != nil {
		return strings.TrimSpace(r.Data.PackageID)
	}
	return ""
}

// SubmitReq 是 v3 送审的请求体。
//
// ## 为什么是 body 而不是 query
//
// 华为的 v2 app-submit（Android 用）把 releaseTime 放在 query 上，
// 但 v3 系列（如 app-package-info）统一是「appId 走 query + 负载走 JSON body」。
// 另外社区实测报错 `registeredIdType and registeredIdNumber can not be null`
// 说明服务端在解析 body 字段。
//
// ## 不含 releaseType / releasePhase
//
// 不传即全网发布。分阶段发布需要额外的一组字段（比例、时间窗、国家分级），
// 尚未验证，因此不猜测性地塞进去。
type SubmitReq struct {
	// Remark 是提审备注。可空；填写时华为要求长度 10-300 字
	Remark *string `json:"remark,omitempty"`
	// ReleaseTime 是定时上架时间；为空时不带该字段，等价于审核通过后立即上架
	ReleaseTime *string `json:"releaseTime,omitempty"`
	// RegisteredIDType / RegisteredIDNumber 是主体登记信息。
	//
	// 社区实测缺失会被服务端拒绝（registeredIdType and registeredIdNumber can
	// not be null），但并非所有应用都需要，因此做成可选配置
	RegisteredIDType   *int   `json:"registeredIdType,omitempty"`
	RegisteredIDNumber string `json:"registeredIdNumber,omitempty"`
}

// SubmitResp 是 v3 送审的响应。
type SubmitResp struct {
	Ret *ret `json:"ret"`
}

// normalizePartHeaders 把分片上传的请求头归一化成字符串键值对。
//
// 华为可能返回 JSON 对象，也可能返回序列化后的字符串；两种都要能处理，
// 否则会因为一个字段形状变化就让整次上传失败。
func normalizePartHeaders(value any) (map[string]string, error) {
	switch v := value.(type) {
	case nil:
		return map[string]string{}, nil

	case map[string]any:
		out := make(map[string]string, len(v))
		for k, item := range v {
			if item == nil {
				continue
			}
			out[k] = fmt.Sprint(item)
		}
		return out, nil

	case string:
		if strings.TrimSpace(v) == "" {
			return map[string]string{}, nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return nil, &eperr.Error{
				Kind:    eperr.KindProtocolMismatch,
				Channel: ID,
				Msg:     "华为返回的分片上传请求头不是合法 JSON，无法按原样转发",
				Raw:     truncate(v),
				Err:     err,
			}
		}
		out := make(map[string]string, len(parsed))
		for k, item := range parsed {
			if item == nil {
				continue
			}
			out[k] = fmt.Sprint(item)
		}
		return out, nil

	default:
		return map[string]string{}, nil
	}
}

// partPlan 描述一个分片在文件中的位置。
//
// 抽成纯数据结构是为了能脱离网络单测：给定文件大小与分片大小，
// 断言片数、每片长度、偏移量。分片切错会让整个上传失败，而失败信息
// 通常只说「分片不匹配」，从这里下手最快。
type partPlan struct {
	// Index 是片序号，从 1 开始
	Index int
	// Offset 是该片在文件中的起始字节
	Offset int64
	// Length 是该片的字节数
	Length int64
}

// key 返回该片在请求体里的键名，如 additionalProp3。
func (p partPlan) key() string { return fmt.Sprintf("%s%d", PartKeyPrefix, p.Index) }

// planParts 把文件切成若干片。
//
// 分片大小必须用华为返回的 nspPartMinSize，不要自己选。
func planParts(fileSize, partSize int64) ([]partPlan, error) {
	if fileSize <= 0 {
		return nil, eperr.LocalFileError("待上传文件为空，无法分片")
	}
	if partSize <= 0 {
		partSize = FallbackPartSize
	}

	count := (fileSize + partSize - 1) / partSize
	plans := make([]partPlan, 0, count)
	for i := int64(1); i <= count; i++ {
		offset := (i - 1) * partSize
		length := partSize
		if remain := fileSize - offset; remain < length {
			length = remain
		}
		plans = append(plans, partPlan{Index: int(i), Offset: offset, Length: length})
	}
	return plans, nil
}

// normalizeMethod 归一化分片上传方法，只接受 PUT 与 POST。
func normalizeMethod(method string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		return "PUT", nil
	}
	if m != "PUT" && m != "POST" {
		return "", &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "华为要求的分片上传方法 " + m + " 不受支持",
		}
	}
	return m, nil
}

// buildRemark 决定 updateDesc 能否作为提审备注提交。
//
// remark 是提审备注，不是商店里的「新版本介绍」。华为要求填写时长度 10-300 字，
// 且该字段本身可选 —— 长度不合规时宁可不传，也不要让整次送审被拒。
//
// 鸿蒙的新版本介绍需要走 v3 的语言信息接口，该接口形状未经验证，暂未实现，
// 因此商店里的版本介绍仍需人工到 AGC 后台维护。
func buildRemark(updateDesc string) (*string, string) {
	trimmed := strings.TrimSpace(updateDesc)
	if trimmed == "" {
		return nil, ""
	}
	if len([]rune(trimmed)) < RemarkMin || len([]rune(trimmed)) > RemarkMax {
		return nil, fmt.Sprintf(
			"更新说明长度 %d 不在华为 remark 要求的 %d-%d 字范围内，本次不作为提审备注提交",
			len([]rune(trimmed)), RemarkMin, RemarkMax)
	}
	return &trimmed, ""
}

// AppInfoV3 是 v3 app-info 的应用基本信息。
//
// 字段取值与 v2 的 AppInfo 同源，releaseState 那套枚举通用。
type AppInfoV3 struct {
	// ReleaseState 取值同 Android 版：0 已上架 / 1 上架审核不通过 / 2 已下架
	// 3 待上架 / 4 审核中 / 5 升级审核中 / 6 申请下架 / 7 草稿 / 8 升级审核不通过
	// 9 下架审核不通过 / 10 应用被开发者下架 / 11 撤销上架 / 12 预审中 / 13 预审不通过
	ReleaseState  *jsonx.FlexInt64 `json:"releaseState"`
	VersionCode   *jsonx.FlexInt64 `json:"versionCode"`
	VersionNumber string           `json:"versionNumber"`
	// OnShelfVersionCode 是在架版本的版本号，与「最新版本」可能不同：
	// 有草稿或审核中的新版本时，versionCode 是新版本而 onShelf 才是在架的那个
	OnShelfVersionCode   *jsonx.FlexInt64 `json:"onShelfVersionCode"`
	OnShelfVersionNumber string           `json:"onShelfVersionNumber"`
	// ReleaseTime 是版本发布时间
	ReleaseTime string `json:"releaseTime"`
}

// AuditInfoV3 是 v3 的审核意见。
//
// v3 的独立数据模型页只列出 auditOpinion 一个字段（Android 版 v2 内联了 7 个：
// 整体 + 版权 + 版号 + 备案各一对）。v3 是否也返回那 6 个字段，文档无法判定 ——
// 因此这里只声明文档明确的这一个，不猜。
type AuditInfoV3 struct {
	// AuditOpinion 是应用整体审核意见
	AuditOpinion *jsonx.FlexString `json:"auditOpinion"`
}

// AppInfoRespV3 是 v3 app-info 的响应。
//
// auditInfo 与 appInfo 平级，这是官方文档的层级。
type AppInfoRespV3 struct {
	Ret       *ret         `json:"ret"`
	AppInfo   *AppInfoV3   `json:"appInfo"`
	AuditInfo *AuditInfoV3 `json:"auditInfo"`
}

// ToMarketInfo 映射为渠道无关的状态。
//
// 只有 Android 版的四类「审核不通过」（1 上架、8 升级、13 预审）才归为 Rejected。
// 9「下架审核不通过」不在此列 —— 它指的是下架申请被拒，与「新版本被拒」是两件事，
// 归为 Unknown 而不是让人误以为版本被拒。
func (a AppInfoV3) ToMarketInfo(audit *AuditInfoV3) channel.MarketInfo {
	state := channel.ReviewUnknown
	raw := ""
	if a.ReleaseState != nil {
		raw = strconv.FormatInt(int64(*a.ReleaseState), 10)
		switch int64(*a.ReleaseState) {
		case 0:
			state = channel.ReviewOnline
		case 1, 8, 13:
			state = channel.ReviewRejected
		case 4, 5:
			state = channel.ReviewUnderReview
		case 7:
			state = channel.ReviewDraft
		case 2, 6, 10:
			state = channel.ReviewOffline
		}
	}

	// 优先用「在架版本」：有草稿或审核中的新版本时，versionCode 是新版本，
	// 而上层做版本号比对需要的是线上那个版本
	code := a.OnShelfVersionCode
	name := a.OnShelfVersionNumber
	if code == nil {
		code = a.VersionCode
		name = a.VersionNumber
	}
	var version *channel.Version
	if code != nil {
		version = &channel.Version{Code: int64(*code), Name: name}
	}

	info := channel.NewMarketInfo(ID, state, version, raw)
	if audit != nil && audit.AuditOpinion != nil {
		if opinion := strings.TrimSpace(string(*audit.AuditOpinion)); opinion != "" {
			info.Review = &channel.ReviewFeedback{Opinion: opinion}
		}
	}
	return info
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}
