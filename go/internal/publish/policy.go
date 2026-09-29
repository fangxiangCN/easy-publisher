package publish

import (
	"fmt"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// 错误模型的别名，让编排层的签名读起来更短。
type (
	Error        = eperr.Error
	ErrorKind    = eperr.ErrorKind
	FailurePhase = eperr.FailurePhase
)

// VersionRule 是版本号策略。
//
// 上游有个 PR 想加「忽略版本检查」开关，但那个实现有两个问题：
// 它跳过了整个 versionCode <= lastVersionCode 判断，连**低于**线上版本也放行；
// 而且开关被持久化进配置文件，容易忘记关掉，后续所有渠道都不再校验。
//
// 这里收窄为三档，且是单次调用的参数而非持久状态。
type VersionRule int

const (
	// VersionStrict 待提交版本必须严格大于线上版本
	VersionStrict VersionRule = iota

	// VersionAllowSame 允许版本号与线上相同。
	//
	// 真实场景：某渠道审核被拒，改了素材要用同一个 versionCode 重新提交。
	VersionAllowSame

	// VersionSkip 完全跳过版本号校验。仅用于排查问题，不建议常规使用。
	VersionSkip
)

func (r VersionRule) String() string {
	switch r {
	case VersionAllowSame:
		return "AllowSame"
	case VersionSkip:
		return "Skip"
	default:
		return "Strict"
	}
}

// Reject 判断某渠道此刻是否可以提交。
//
// 返回 nil 表示可以提交；否则返回拒绝原因。
//
// 这些规则原本散落在 Compose 的状态类里（上游的 ApkPageState.checkChannelEnableSubmit），
// headless 入口会直接绕过。此处提取为独立的纯函数，CLI / MCP 共用同一套判断。
func Reject(info artifact.Info, market *channel.MarketInfo, rule VersionRule) *Error {
	if market != nil {
		if !market.CanSubmit {
			suffix := ""
			if market.RawState != "" {
				suffix = fmt.Sprintf("（原始状态：%s）", market.RawState)
			}
			return eperr.PreconditionError(market.ChannelID,
				"渠道当前状态为「%s」，不接受新版本%s", market.ReviewState.Label(), suffix)
		}
		if market.ReviewState == channel.ReviewUnderReview {
			return eperr.PreconditionError(market.ChannelID, "渠道正在审核中，不能提交新版本")
		}
	}

	if rule == VersionSkip {
		return nil
	}
	// 线上版本未知时不阻断。应用在商店只有未上传 APK 的草稿版本时渠道不返回版本号，
	// 这正是上游 issue #7 的场景
	if market == nil || market.LastVersion == nil {
		return nil
	}

	online := market.LastVersion
	incoming := info.VersionCode
	switch {
	case incoming > online.Code:
		return nil

	case incoming == online.Code && rule == VersionAllowSame:
		return nil

	case incoming == online.Code:
		return eperr.PreconditionError(market.ChannelID,
			"待提交版本号 %d 与线上版本 %s 相同。"+
				"若确实要用同一版本号重新提交（例如审核被拒后仅更新素材），"+
				"请显式指定 --allow-same-version",
			incoming, online)

	default:
		// 版本号变低几乎总是选错了 APK 文件。
		// 这也是与上游那个 PR 的关键差别：它跳过整个判断，连低于线上版本也放行
		return eperr.PreconditionError(market.ChannelID,
			"待提交版本号 %d 低于线上版本 %s，这通常意味着选错了制品文件",
			incoming, online)
	}
}
