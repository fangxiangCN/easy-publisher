package output

import (
	"errors"
	"fmt"
	"os"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// ErrorJSON 是失败结果的统一形状，便于脚本判断。
type ErrorJSON struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind"`
	Channel *string `json:"channel"`
	Code    *string `json:"code"`
	Message string `json:"message"`
	// Retryable 越过送审点后恒为 false
	Retryable bool `json:"retryable"`
	// Phase 解释「为什么不可重试」：不是错误不可恢复，
	// 而是无法确定服务端是否已受理
	Phase string `json:"phase"`
}

// ReportError 输出错误并按错误类型返回退出码。
//
// jsonOut 为 true 时错误也走 stdout 的结构化输出 —— 脚本才能和正常结果
// 一并解析。人类可读的信息走 stderr。
func ReportError(err error, jsonOut bool) int {
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		pe = eperr.FromError("", err)
	}

	if jsonOut {
		payload := ErrorJSON{
			OK:        false,
			Kind:      pe.Kind.String(),
			Message:   pe.Msg,
			Retryable: pe.Retryable(),
			Phase:     pe.Phase.String(),
		}
		if pe.Channel != "" {
			c := pe.Channel
			payload.Channel = &c
		}
		if pe.Code != "" {
			c := pe.Code
			payload.Code = &c
		}
		_ = JSON(payload)
		return ExitCodeOf(pe.Kind)
	}

	fmt.Fprintln(os.Stderr, pe.Describe())
	switch {
	case pe.Phase == eperr.PhaseAtOrAfterSubmission:
		// 越过送审点后不能只说「不可重试」，必须说明原因是
		// 「无法确定服务端是否已受理」，否则使用者会以为是网络抖动而反复重试
		fmt.Fprintln(os.Stderr,
			"（已越过送审点：请勿直接重试，先到开发者后台确认该版本是否已提交成功）")
	case pe.Retryable():
		fmt.Fprintln(os.Stderr, "（该错误通常可重试）")
	}
	if pe.Raw != "" {
		fmt.Fprintf(os.Stderr, "渠道原始响应：%s\n", pe.Raw)
	}
	return ExitCodeOf(pe.Kind)
}
