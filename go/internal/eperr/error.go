// Package eperr 定义发布流程的结构化错误模型。
//
// 单独成包是为了打破依赖环：渠道实现需要用它报错，而编排层（publish）
// 又需要引用渠道接口。错误类型不依赖任何内部包，因此放在最底层。
package eperr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// ErrorKind 是失败原因分类。调用方（脚本、CI、agent）依据它决定重试、换渠道还是交给人处理。
type ErrorKind int

const (
	// KindConfiguration 本地配置问题：缺参数、包名非法
	KindConfiguration ErrorKind = iota
	// KindCredential 凭据缺失或被渠道拒绝
	KindCredential
	// KindLocalFile 本地文件问题：制品不存在、解析失败
	KindLocalFile
	// KindNetwork 网络层失败
	KindNetwork
	// KindChannelRejected 渠道返回了业务错误码
	KindChannelRejected
	// KindProtocolMismatch 渠道响应无法解析，通常意味着接口变更
	KindProtocolMismatch
	// KindPrecondition 违反发布前置条件，如版本号不大于线上版本
	KindPrecondition
	// KindUnknown 未归类
	KindUnknown
)

// String 返回稳定的机器可读名，用于 JSON 输出。改名会破坏调用方脚本。
func (k ErrorKind) String() string {
	switch k {
	case KindConfiguration:
		return "Configuration"
	case KindCredential:
		return "Credential"
	case KindLocalFile:
		return "LocalFile"
	case KindNetwork:
		return "Network"
	case KindChannelRejected:
		return "ChannelRejected"
	case KindProtocolMismatch:
		return "ProtocolMismatch"
	case KindPrecondition:
		return "Precondition"
	default:
		return "Unknown"
	}
}

// Label 返回中文标签，用于表格等空间受限的场合。
func (k ErrorKind) Label() string {
	switch k {
	case KindConfiguration:
		return "配置错误"
	case KindCredential:
		return "凭据问题"
	case KindLocalFile:
		return "本地文件问题"
	case KindNetwork:
		return "网络失败"
	case KindChannelRejected:
		return "渠道拒绝"
	case KindProtocolMismatch:
		return "接口不匹配"
	case KindPrecondition:
		return "不满足前置条件"
	default:
		return "未知错误"
	}
}

// FailurePhase 标记失败发生在发布流程的哪个阶段。
//
// 重试是否安全不只取决于错误类型，还取决于远端是否已经产生了不可撤销的副作用。
type FailurePhase int

const (
	// PhasePreSubmission 尚未越过送审点（取 token、查状态、上传文件、绑定草稿等）。
	// 重试是安全的：远端最多留下一个草稿，不会产生重复的正式版本。
	PhasePreSubmission FailurePhase = iota

	// PhaseAtOrAfterSubmission 已越过送审点，或无法确定是否越过。
	//
	// 此时任何失败都不能盲目重试 —— 服务端可能已经受理请求，只是响应在回程丢失
	// （超时、连接重置都会表现成这样，从客户端无法区分）。重试会重复送审或产生
	// 重复版本，而各应用商店都不提供撤销版本更新的 API。
	PhaseAtOrAfterSubmission
)

func (p FailurePhase) String() string {
	if p == PhaseAtOrAfterSubmission {
		return "AtOrAfterSubmission"
	}
	return "PreSubmission"
}

// Error 是结构化的发布失败信息。
//
// 上游项目的 ApiException 把渠道返回的中文 message 拼成字符串，且
// check(response.isSuccessful) 不带 lazyMessage —— 抛出的是默认的 "Check failed."，
// 状态码与响应体全部丢失，导致所有渠道出错时都无法定位。这里保留结构。
type Error struct {
	Kind    ErrorKind
	Channel string // 渠道标识，非渠道相关的错误为空
	Code    string // 渠道返回的业务错误码
	Msg     string
	Raw     string       // 渠道原始响应，便于排查接口变更
	Phase   FailurePhase // 失败发生的阶段，决定是否可以重试

	Err error // 底层原因
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error { return e.Err }

// Retryable 报告是否值得自动重试。
//
// 越过送审点之后一律 false，见 PhaseAtOrAfterSubmission。
func (e *Error) Retryable() bool {
	if e.Phase == PhaseAtOrAfterSubmission {
		return false
	}
	return e.Kind == KindNetwork || e.Kind == KindUnknown
}

// Describe 是给人看的一行描述。
func (e *Error) Describe() string {
	var b strings.Builder
	if e.Channel != "" {
		b.WriteString("[")
		b.WriteString(e.Channel)
		b.WriteString("] ")
	}
	b.WriteString(e.Msg)
	if e.Code != "" {
		b.WriteString(" (code=")
		b.WriteString(e.Code)
		b.WriteString(")")
	}
	return b.String()
}

// AtSubmissionPoint 标记这个错误发生在送审点或之后，并追加确认提示。
//
// 保留 Kind / Code / Channel / Raw / Err，只改变重试语义。
func (e *Error) AtSubmissionPoint(displayName, action string) *Error {
	if e.Phase == PhaseAtOrAfterSubmission {
		return e
	}
	hint := fmt.Sprintf(
		"%s 可能已被服务端受理（响应在回程丢失也会报此错误）。"+
			"请先登录%s 开发者后台确认该版本是否已提交成功，确认未提交后再重试 —— 重复送审无法撤销。",
		action, displayName,
	)
	msg := e.Msg
	if msg == "" {
		msg = hint
	} else {
		msg = msg + "。" + hint
	}
	clone := *e
	clone.Msg = msg
	clone.Phase = PhaseAtOrAfterSubmission
	return &clone
}

// 构造器。

func ConfigurationError(format string, args ...any) *Error {
	return &Error{Kind: KindConfiguration, Msg: fmt.Sprintf(format, args...)}
}

func CredentialError(format string, args ...any) *Error {
	return &Error{Kind: KindCredential, Msg: fmt.Sprintf(format, args...)}
}

func LocalFileError(format string, args ...any) *Error {
	return &Error{Kind: KindLocalFile, Msg: fmt.Sprintf(format, args...)}
}

func PreconditionError(channel, format string, args ...any) *Error {
	return &Error{Kind: KindPrecondition, Channel: channel, Msg: fmt.Sprintf(format, args...)}
}

func RejectedError(channel, code, format string, args ...any) *Error {
	return &Error{
		Kind:    KindChannelRejected,
		Channel: channel,
		Code:    code,
		Msg:     fmt.Sprintf(format, args...),
	}
}

func ProtocolError(channel, format string, args ...any) *Error {
	return &Error{Kind: KindProtocolMismatch, Channel: channel, Msg: fmt.Sprintf(format, args...)}
}

// NetworkError 把网络层异常归一化。
//
// 超时单独给提示，因为它是最常见且最容易被误判为「可以重试」的一类 ——
// 而能否重试取决于失败发生在哪一步，不取决于它是超时。
func NetworkError(channel string, err error) *Error {
	msg := err.Error()
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		msg = "请求超时，可尝试增大超时时间（--timeout）"
	case errors.Is(err, net.ErrClosed):
		msg = "连接已关闭"
	}
	return &Error{Kind: KindNetwork, Channel: channel, Msg: msg, Err: err}
}

// FromError 把任意 error 归一化为 *Error。已经是 *Error 的原样返回。
func FromError(channel string, err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, net.ErrClosed) || isNetworkish(err) {
		return NetworkError(channel, err)
	}
	return &Error{Kind: KindUnknown, Channel: channel, Msg: err.Error(), Err: err}
}

// isNetworkish 判断是否是 IO 类错误。
//
// Go 没有 Java 的 IOException 这个统一类型，net/http 的传输错误可能是
// *url.Error 包着 *net.OpError 包着各种具体类型，因此按 *net.Error 与
// 常见哨兵值判断，其余归为 Unknown。
func isNetworkish(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection refused")
}
