// Package channel 定义渠道抽象与能力矩阵。
//
// 具体的渠道实现在各自的子包里，通过 Register 注册。
package channel

import (
	"context"
	"fmt"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// ParamType 是渠道参数的取值类型。
type ParamType int

const (
	// ParamText 是普通文本
	ParamText ParamType = iota
	// ParamTextFile 是文本文件内容，如小米的公钥证书
	ParamTextFile
)

// ChannelParam 是渠道自描述的参数。
//
// 渠道自己声明需要什么参数，CLI 的帮助文本、MCP 的 inputSchema 与
// `channel list` 的输出都由此生成，不必在三处各写一遍。
type ChannelParam struct {
	Name        string
	Description string
	Required    bool
	Type        ParamType
	// FileExtension 仅 ParamTextFile 有意义，如 "cer"
	FileExtension string
}

// ReleaseStage 是发布流程中可以停下来的位置。
//
// 之所以需要这个概念：各渠道的 API 粒度差别很大。华为可以先建草稿、人工到后台
// 看一眼再送审；小米的 dev/push 把上传和送审合并成一次请求，中间没有任何可以
// 停下的位置。用统一的「upload」掩盖这个差异，会让调用方误以为所有渠道都有
// 反悔的机会。
type ReleaseStage int

const (
	// StageUploadArtifact 仅把安装包传到渠道的文件存储，不创建任何版本。
	// 可用于验证凭据与签名是否可用。
	StageUploadArtifact ReleaseStage = iota
	// StageCreateDraft 创建草稿版本，可在渠道后台查看但尚未送审。
	// 只有部分渠道有这个状态；OPPO/vivo 的文件上传不会生成可检视的草稿。
	StageCreateDraft
	// StageSubmitReview 提交审核。不可撤销。
	StageSubmitReview
)

func (s ReleaseStage) String() string {
	switch s {
	case StageUploadArtifact:
		return "UploadArtifact"
	case StageCreateDraft:
		return "CreateDraft"
	default:
		return "SubmitReview"
	}
}

// Label 是中文标签。
func (s ReleaseStage) Label() string {
	switch s {
	case StageUploadArtifact:
		return "仅上传安装包"
	case StageCreateDraft:
		return "创建草稿（不送审）"
	default:
		return "提交审核"
	}
}

// ShortLabel 用于表格，避免列宽被撑爆。
func (s ReleaseStage) ShortLabel() string {
	switch s {
	case StageUploadArtifact:
		return "上传"
	case StageCreateDraft:
		return "草稿"
	default:
		return "送审"
	}
}

// AllStages 按流程顺序返回全部阶段。
func AllStages() []ReleaseStage {
	return []ReleaseStage{StageUploadArtifact, StageCreateDraft, StageSubmitReview}
}

// ParseStage 解析 CLI/MCP 传入的停留点名称。空串表示「未指定」。
func ParseStage(name string) (ReleaseStage, bool, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return StageSubmitReview, false, nil
	case "artifact":
		return StageUploadArtifact, true, nil
	case "draft":
		return StageCreateDraft, true, nil
	case "submit":
		return StageSubmitReview, true, nil
	default:
		return StageSubmitReview, false, eperr.ConfigurationError(
			"停留点只能是 artifact / draft / submit，收到 %q", name)
	}
}

// ReleaseParams 是发布参数。
type ReleaseParams struct {
	// UpdateDesc 是更新说明
	UpdateDesc string
	// OnlineTime 是定时上线的毫秒时间戳；0 表示审核通过后立即发布
	OnlineTime int64
}

// Scheduled 报告是否是定时发布。
func (p ReleaseParams) Scheduled() bool { return p.OnlineTime > 0 }

// Credentials 是渠道凭据。
//
// 由 core 内部从 CredentialStore 组装，不经过 CLI / MCP 的对外接口 ——
// 否则密钥会进入 shell 历史、CI 日志或 agent 的对话上下文。
type Credentials struct {
	values map[string]string
}

// NewCredentials 从键值对构造凭据。
func NewCredentials(values map[string]string) Credentials {
	return Credentials{values: values}
}

// Get 取必填参数，缺失时报凭据错误。
func (c Credentials) Get(key string) (string, error) {
	if v, ok := c.values[key]; ok && strings.TrimSpace(v) != "" {
		return v, nil
	}
	return "", eperr.CredentialError("缺少凭据参数 %s", key)
}

// Optional 取可选参数。
func (c Credentials) Optional(key string) string {
	return c.values[key]
}

// String 绝不输出参数值。
func (c Credentials) String() string {
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	return fmt.Sprintf("Credentials(keys=%v)", keys)
}

// ProgressFunc 报告上传进度，取值范围 [0,1]。
type ProgressFunc func(fraction float64)

// UploadRequest 是单次上传所需的全部上下文。
//
// 关键设计：**渠道实现不持有任何可变状态。** 上游项目的 ChannelTask 是
// ChannelRegistry 里的进程级单例，却持有 clientId / clientSecret /
// submitStateListener 三个无同步的可变字段，由 init() 与 setSubmitStateListener()
// 写入。GUI 里已经可能串台（首页刷新市场状态与上传页并发 init 同一批实例）；
// 在 MCP server 里并发服务多个应用时则是必然 —— 会用 A 应用的密钥上传 B 应用的包。
//
// 这里把凭据与回调都变成入参，渠道实现无状态、可安全并发复用。
type UploadRequest struct {
	ArtifactFile  string
	ArtifactInfo  artifact.Info
	Credentials   Credentials
	ReleaseParams ReleaseParams
	Timeouts      httpx.Timeouts
	OnProgress    ProgressFunc

	// StopAfter 是流程走到哪一步就停下。
	//
	// 由 Service 解析后填入，渠道实现可以假定它已被校验过
	// （即本渠道确实支持停在这个位置）。
	StopAfter ReleaseStage
}

// MarketQuery 是查询渠道状态的入参。
type MarketQuery struct {
	ApplicationID string
	Credentials   Credentials
	Timeouts      httpx.Timeouts
}

// Channel 是一个应用商店渠道。实现必须是无状态的。
type Channel interface {
	// ID 是稳定标识，用于 CLI/MCP 参数与配置文件，如 "huawei"
	ID() string
	// DisplayName 是展示名称，如 "华为"
	DisplayName() string
	// FileNameTag 是多渠道包场景下用于匹配文件名的标识，如 "HUAWEI"
	FileNameTag() string
	// Params 是本渠道需要的参数
	Params() []ChannelParam
	// Capabilities 是发布能力与风险画像
	Capabilities() Capabilities
	// ArtifactKinds 是本渠道接受的制品类型
	ArtifactKinds() []artifact.Kind

	// Upload 上传，并按 UploadRequest.StopAfter 决定是否送审。
	//
	// 返回实际到达的阶段。调用方必须以此为准，而不是假设请求里的 StopAfter
	// 已达成 —— 渠道可能因为 API 粒度限制而无法停在指定位置。
	Upload(ctx context.Context, req UploadRequest) (ReleaseStage, error)

	// QueryMarket 查询应用在该渠道的状态。
	QueryMarket(ctx context.Context, q MarketQuery) (MarketInfo, error)
}

// ReviewState 是应用在商店的审核状态。
//
// 这是**渠道无关的粗分类**：它的职责是支撑决策（能否提交、是否在审核）与
// 概览展示，不可能穷尽各渠道的全部状态。渠道文档里的精确状态名由
// [MarketInfo.RawStateLabel] 承载。
//
// 这也是为什么新增值时要放在 ReviewUnknown 之前：枚举的数值不对外暴露
// （输出走 String()），但保持 Unknown 是最后一个值，能让任何漏掉新分支的
// switch 落进 default 时仍是「未知」而不是误判成某个具体状态。
type ReviewState int

const (
	// ReviewOnline 已上架，用户可下载
	ReviewOnline ReviewState = iota
	// ReviewUnderReview 审核流程进行中，此时提交新版本会被拒绝
	ReviewUnderReview
	// ReviewRejected 审核未通过，需修改后重新提交
	ReviewRejected
	// ReviewDraft 草稿，尚未提交审核
	ReviewDraft
	// ReviewOffline 不在架上（含下架、撤销上架、冻结等）
	ReviewOffline
	// ReviewPending 审核已通过，等待发布（定时发布、待上架）
	//
	// 与 Online 的区别是「用户此刻还下载不到这个版本」，与 Offline 的区别是
	// 「没有被打回，只差发布这一步」。华为的 releaseState=3（待上架/预约上架）
	// 与 OPPO 的 audit_status=7（定时发布）都属于这一档。
	ReviewPending
	// ReviewUnknown 渠道返回了文档未覆盖的状态值，需人工到后台核对
	ReviewUnknown
)

func (r ReviewState) String() string {
	switch r {
	case ReviewOnline:
		return "Online"
	case ReviewUnderReview:
		return "UnderReview"
	case ReviewRejected:
		return "Rejected"
	case ReviewDraft:
		return "Draft"
	case ReviewOffline:
		return "Offline"
	case ReviewPending:
		return "Pending"
	default:
		return "Unknown"
	}
}

func (r ReviewState) Label() string {
	switch r {
	case ReviewOnline:
		return "已上架"
	case ReviewUnderReview:
		return "审核中"
	case ReviewRejected:
		return "审核被拒"
	case ReviewDraft:
		return "草稿"
	case ReviewOffline:
		return "已下架"
	case ReviewPending:
		return "待上架"
	default:
		return "状态未知"
	}
}

// Version 是一个版本号。
type Version struct {
	Code int64
	Name string
}

func (v Version) String() string { return fmt.Sprintf("%s(%d)", v.Name, v.Code) }

// MarketInfo 是应用在某渠道的状态。
//
// LastVersion 用指针表达可空：应用在商店只有未上传 APK 的草稿版本时，
// 华为不返回 versionCode（上游 issue #7）。此时应当是「没有版本信息」，
// 而不是伪造一个 0 —— 0 会让版本号比对得出「待提交版本更高」的错误结论。
type MarketInfo struct {
	ChannelID   string
	ReviewState ReviewState
	LastVersion *Version
	// CanSubmit 报告渠道此刻是否接受新版本
	CanSubmit bool
	// RawState 是渠道返回的原始状态值，便于排查新增的状态码
	RawState string
	// RawStateLabel 是渠道文档里该状态值的原始描述，如「撤销上架」「运营打回」。
	//
	// ReviewState 只有六档粗分类，无法表达渠道特有的状态细分（华为 13 个取值
	// 里有 5 个都归为「不在架上」）。这里原样透传官方描述，让用户看到的是
	// 渠道后台里的那个词，而不是我们归纳后的近义词。
	RawStateLabel string
	// Review 是渠道给出的审核反馈，渠道未提供时为 nil
	Review *ReviewFeedback
}

// ReviewFeedback 是渠道给出的审核反馈。
//
// # 为什么是自由文本而不是原因码
//
// 四个渠道（华为、荣耀、vivo，以及鸿蒙的 v3 接口）都提供审核意见，但**没有一家
// 给出结构化的原因码**（如 rejectCode）—— 全部是一段自由文本外加附件链接。
// 因此「自动分类拒审原因再自动修复」做不到，只能把原文交给人和 agent 去读。
//
// 附件值得留意：审核员截的图经常比文字说明更能说明问题，荣辱的文档明确写了
// 「为url，可查看或下载」。
//
// 字段可能为空：vivo 的文档把 unPassReason 标为「非必填」，华为的响应示例里
// 未拒审时 auditOpinion 是空串。调用方需要处理「有字段但没内容」。
type ReviewFeedback struct {
	// Opinion 是审核意见原文
	Opinion string
	// Attachments 是审核意见附件的 URL，可能是截图
	Attachments []string
	// Notes 是渠道特有的补充审核意见。
	//
	// 华为会按维度分别给出结果：整体、版权、版号、备案（后三项仅中国大陆应用返回）。
	// 其他渠道通常只有一条，此时 Notes 为空，意见在 Opinion 里。
	Notes []ReviewNote
}

// Empty 报告这条反馈是否没有任何内容。
//
// 渠道经常返回结构但内容为空（华为的 auditOpinion 未拒审时是空串），
// 调用方据此决定要不要展示。
func (r *ReviewFeedback) Empty() bool {
	if r == nil {
		return true
	}
	return strings.TrimSpace(r.Opinion) == "" &&
		len(r.Attachments) == 0 &&
		len(r.Notes) == 0
}

// ReviewNote 是一条补充审核意见。
type ReviewNote struct {
	// Kind 是意见的维度，如「版权」「版号」「备案」
	Kind string
	// Passed 表示该维度是否通过。nil 表示渠道未给出结果，不代表通过
	Passed *bool
	// Opinion 是该维度的意见内容
	Opinion string
}

// NewMarketInfo 构造状态，CanSubmit 默认按「不在审核中」推导。
func NewMarketInfo(channelID string, state ReviewState, last *Version, raw string) MarketInfo {
	return MarketInfo{
		ChannelID:   channelID,
		ReviewState: state,
		LastVersion: last,
		CanSubmit:   state != ReviewUnderReview,
		RawState:    raw,
	}
}
