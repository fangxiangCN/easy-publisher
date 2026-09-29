package channel

import (
	"sort"
	"strings"
	"sync"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// Withdrawal 描述送审之后能否撤回，以及通过什么途径。
type Withdrawal int

const (
	// WithdrawalNotApplicable 该动作本身不产生需要撤回的东西（如仅上传文件）
	WithdrawalNotApplicable Withdrawal = iota
	// WithdrawalAPISupported API 支持撤回
	WithdrawalAPISupported
	// WithdrawalConsoleOnly API 不支持，但可登录渠道后台手动操作
	WithdrawalConsoleOnly
	// WithdrawalNotVerified 未经验证 —— 不代表不能，只代表没人确认过
	WithdrawalNotVerified
	// WithdrawalUnsupported 不可撤回
	WithdrawalUnsupported
)

func (w Withdrawal) String() string {
	switch w {
	case WithdrawalNotApplicable:
		return "NotApplicable"
	case WithdrawalAPISupported:
		return "ApiSupported"
	case WithdrawalConsoleOnly:
		return "ConsoleOnly"
	case WithdrawalUnsupported:
		return "Unsupported"
	default:
		return "NotVerified"
	}
}

func (w Withdrawal) Label() string {
	switch w {
	case WithdrawalNotApplicable:
		return "无需撤回"
	case WithdrawalAPISupported:
		return "API 可撤回"
	case WithdrawalConsoleOnly:
		return "仅后台可撤回"
	case WithdrawalUnsupported:
		return "不可撤回"
	default:
		return "未验证"
	}
}

// Evidence 是能力声明的证据来源。
//
// 这一项是刻意加的：本项目的渠道逻辑大部分**没有用真实凭据验证过**，
// 把「读了文档」、「看了代码」、「真机跑过」区分开，比笼统地声称「支持」
// 诚实得多，也让使用者知道该对哪些结论保持怀疑。
type Evidence int

const (
	// EvidenceCodeObservation 仅来自代码推断（含从上游项目继承的实现）
	EvidenceCodeObservation Evidence = iota
	// EvidenceOfficialDocumentation 仅来自渠道官方文档
	EvidenceOfficialDocumentation
	// EvidenceOfficialAndCode 文档与代码相互印证
	EvidenceOfficialAndCode
	// EvidenceVerifiedInProduction 已用真实凭据实际跑通
	EvidenceVerifiedInProduction
)

func (e Evidence) String() string {
	switch e {
	case EvidenceOfficialDocumentation:
		return "OfficialDocumentation"
	case EvidenceOfficialAndCode:
		return "OfficialAndCode"
	case EvidenceVerifiedInProduction:
		return "VerifiedInProduction"
	default:
		return "CodeObservation"
	}
}

func (e Evidence) Label() string {
	switch e {
	case EvidenceOfficialDocumentation:
		return "官方文档"
	case EvidenceOfficialAndCode:
		return "文档+代码"
	case EvidenceVerifiedInProduction:
		return "已实测"
	default:
		return "代码推断"
	}
}

// RiskLevel 是渠道动作的风险等级。
type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
	RiskCritical
)

func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "Low"
	case RiskMedium:
		return "Medium"
	case RiskCritical:
		return "Critical"
	default:
		return "High"
	}
}

func (r RiskLevel) Label() string {
	switch r {
	case RiskLow:
		return "低"
	case RiskMedium:
		return "中"
	case RiskCritical:
		return "极高"
	default:
		return "高"
	}
}

// Capabilities 是一个渠道的发布能力与风险画像。
//
// 调用方（脚本、CI、agent）应当先读这个再决定怎么做，而不是发完之后才发现
// 这个渠道压根不能停、不能撤。
type Capabilities struct {
	// SupportedStages 是本渠道支持停在哪些阶段，按流程顺序排列
	SupportedStages []ReleaseStage

	RiskLevel RiskLevel

	// Withdrawal 是送审后的撤回途径
	Withdrawal Withdrawal

	// RequiresExplicitConfirmation 报告送审动作是否要求显式确认
	RequiresExplicitConfirmation bool

	// AutomaticRetryAfterSubmission 报告送审步骤是否允许自动重试。
	//
	// 一律应为 false：服务端可能已受理而响应丢失，重试会重复送审。
	// 见 eperr.FailurePhase。
	AutomaticRetryAfterSubmission bool

	Evidence Evidence

	// VerifiedScope 是已验证的范围，仅当 Evidence 为 VerifiedInProduction 时有意义。
	//
	// 单独成一个字段而不是塞进 Note，是因为「已实测」这句话很容易被读成
	// 「整条链路都实测过」。实际上目前验证到的只是鉴权与上传/建草稿，
	// **送审路径一个渠道都没验证过** —— 而送审恰恰是不可撤销的那一步。
	VerifiedScope string

	// Note 是给使用者看的说明，尤其是与直觉不符的地方
	Note string
}

// Supports 报告是否支持停在指定阶段。
func (c Capabilities) Supports(stage ReleaseStage) bool {
	for _, s := range c.SupportedStages {
		if s == stage {
			return true
		}
	}
	return false
}

// CanStopBefore 报告是否能停在送审之前。
func (c Capabilities) CanStopBefore() bool {
	for _, s := range c.SupportedStages {
		if s != StageSubmitReview {
			return true
		}
	}
	return false
}

// MaxStage 是最靠后的可停位置。
//
// 调用方未指定停留点时用这个作为默认值 —— 对每个渠道都是它能安全做到的最大值，
// 而不是一律假设可以送审。
func (c Capabilities) MaxStage() ReleaseStage {
	if len(c.SupportedStages) == 0 {
		return StageSubmitReview
	}
	return c.SupportedStages[len(c.SupportedStages)-1]
}

// RequireSupportedStage 校验请求的停留点本渠道是否支持。
//
// 必须显式报错，不能默默一路走到送审 —— 调用方以为停在草稿态、实际已经提交，
// 是这个功能最坏的失败模式。
func RequireSupportedStage(ch Channel, requested ReleaseStage) error {
	c := ch.Capabilities()
	if c.Supports(requested) {
		return nil
	}
	stages := make([]string, 0, len(c.SupportedStages))
	for _, s := range c.SupportedStages {
		stages = append(stages, s.Label())
	}
	return eperr.ConfigurationError(
		"%s 不支持「%s」。该渠道可停在：%s。%s",
		ch.DisplayName(), requested.Label(), strings.Join(stages, "、"), c.Note,
	)
}

// ---- 注册表 ----

var (
	registryMu sync.RWMutex
	registry   []Channel
)

// Register 注册一个渠道实现。
//
// 因为渠道实现是无状态的（凭据与回调走方法入参），这里共享实例是安全的 ——
// 与上游共享**持有可变凭据字段**的单例是两回事。
func Register(ch Channel) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, existing := range registry {
		if strings.EqualFold(existing.ID(), ch.ID()) {
			registry[i] = ch // 允许覆盖，便于测试注入替身
			return
		}
	}
	registry = append(registry, ch)
}

// All 返回已注册的渠道，按注册顺序。
func All() []Channel {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Channel, len(registry))
	copy(out, registry)
	return out
}

// IDs 返回全部渠道 id。
func IDs() []string {
	all := All()
	out := make([]string, 0, len(all))
	for _, ch := range all {
		out = append(out, ch.ID())
	}
	return out
}

// Find 按 id 查找渠道，不区分大小写。
func Find(id string) (Channel, bool) {
	for _, ch := range All() {
		if strings.EqualFold(ch.ID(), id) {
			return ch, true
		}
	}
	return nil, false
}

// Require 按 id 查找渠道，找不到时报带可用清单的错误。
func Require(id string) (Channel, error) {
	if ch, ok := Find(id); ok {
		return ch, nil
	}
	return nil, eperr.ConfigurationError(
		"未知渠道：%s（可用渠道：%s）", id, strings.Join(IDs(), ", "))
}

// Reset 清空注册表，仅供测试使用。
func Reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = nil
}

// SortedStages 按流程顺序返回给定阶段集合，用于校验声明是否有序。
func SortedStages(stages []ReleaseStage) []ReleaseStage {
	out := append([]ReleaseStage(nil), stages...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DescribeStages 把阶段列表渲染成「上传/草稿/送审」这样的短串。
func DescribeStages(stages []ReleaseStage) string {
	parts := make([]string, 0, len(stages))
	for _, s := range stages {
		parts = append(parts, s.ShortLabel())
	}
	return strings.Join(parts, "/")
}
