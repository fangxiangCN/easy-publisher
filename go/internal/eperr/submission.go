package eperr

import (
	"context"
	"errors"
)

// MarkSubmitted 把任意 error 标记为「发生在送审点或之后」。
//
// 已经是 *Error 的补上阶段标记；其他类型先归一化再标记。
// ctx 取消原样返回 —— 它不是失败，而是控制流。
func MarkSubmitted(channel, displayName, action string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.AtSubmissionPoint(displayName, action)
	}
	return FromError(channel, err).AtSubmissionPoint(displayName, action)
}

// AtSubmissionPoint 执行 block，并把其中产生的任何失败标记为送审点之后。
//
// ## 为什么需要它
//
// 送审是整条发布链路里唯一不可撤销的动作。而它的失败有一种特别危险的形态：
// 请求已经到达服务端并被受理，但响应在回程丢失。从客户端看，这与「请求根本没发出去」
// 完全无法区分 —— 两者都是一个超时错误。
//
// 如果按普通网络错误处理，调用方（脚本、CI、或者按 SKILL.md 行事的 agent）会重试，
// 于是同一个版本被送审两次。
//
// 这里不试图判断服务端到底有没有受理（无法判断），而是把不确定性如实传给调用方：
// Retryable() 返回 false，Msg 里说明「先到后台确认」。
//
// ## 用法
//
// 只包裹真正越过送审点的那一次调用，不要包住整个上传流程 ——
// 上传文件、绑定草稿这些步骤失败是可以安全重试的。
//
//	ctx, cancel := ...
//	err := publish.AtSubmissionPoint(ctx, "huawei", "华为", "提交审核", func(ctx context.Context) error {
//	    return api.Submit(ctx, ...)
//	})
func AtSubmissionPoint(
	ctx context.Context,
	channel, displayName, action string,
	block func(ctx context.Context) error,
) error {
	err := block(ctx)
	if err == nil {
		return nil
	}
	return MarkSubmitted(channel, displayName, action, err)
}
