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
type ReviewState int

const (
	ReviewOnline ReviewState = iota
	ReviewUnderReview
	ReviewRejected
	ReviewDraft
	ReviewOffline
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
