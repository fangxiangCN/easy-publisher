// Package output 提供 CLI 的结构化输出与退出码。
//
// 两条约定，脚本与 MCP 都依赖它们：
//
//  1. **stdout 只放结果，日志一律走 stderr。** 这样 `--json` 的输出可以直接
//     管道给 jq，也是 MCP 的 stdio transport 能复用同一套 core 的前提。
//  2. **退出码按错误类型区分**，不是笼统的 1。CI 据此决定重试、换渠道
//     还是交给人处理。
package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// ExitCodes 是进程退出码。数值与 Kotlin 版逐字一致 —— 改动会破坏调用方脚本。
const (
	// ExitSuccess 成功
	ExitSuccess = 0
	// ExitUsage 参数错误、配置缺失
	ExitUsage = 1
	// ExitCredential 凭据缺失或被渠道拒绝
	ExitCredential = 2
	// ExitChannelRejected 渠道返回业务错误
	ExitChannelRejected = 3
	// ExitNetwork 网络失败或超时，通常可重试
	ExitNetwork = 4
	// ExitPrecondition 不满足发布前置条件，如版本号不大于线上版本
	ExitPrecondition = 5
	// ExitLocalFile 本地文件问题
	ExitLocalFile = 6
	// ExitProtocol 渠道响应无法解析，接口可能已变更
	ExitProtocol = 7
)

// ExitCodeOf 把错误类型映射成退出码。
func ExitCodeOf(kind eperr.ErrorKind) int {
	switch kind {
	case eperr.KindConfiguration:
		return ExitUsage
	case eperr.KindCredential:
		return ExitCredential
	case eperr.KindLocalFile:
		return ExitLocalFile
	case eperr.KindNetwork:
		return ExitNetwork
	case eperr.KindChannelRejected:
		return ExitChannelRejected
	case eperr.KindProtocolMismatch:
		return ExitProtocol
	case eperr.KindPrecondition:
		return ExitPrecondition
	default:
		return ExitUsage
	}
}

// JSON 序列化结果到 stdout。
//
// 用 Encoder 并关掉 HTML 转义：包名、更新说明里出现 & 时不该被写成 &，
// 那会让输出对人类与 jq 都更难读（小米渠道的 MD5 必须转义，但那是协议要求，
// 与这里的人类可读输出是两回事）。
func JSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Line 输出一行纯文本到 stdout。
func Line(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

// Table 输出表格到 stdout，列宽按内容自适应。
//
// 中文字符在等宽终端里占两列，直接用 len 会让表格错位。
func Table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i := range headers {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for i := range headers {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(cell)
			if i != len(headers)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+2))
			}
		}
		b.WriteString("\n")
	}
	writeRow(headers)
	dashes := make([]string, len(headers))
	for i, w := range widths {
		dashes[i] = strings.Repeat("-", w)
	}
	writeRow(dashes)
	for _, row := range rows {
		writeRow(row)
	}
	fmt.Fprint(os.Stdout, b.String())
}

// displayWidth 计算字符串在终端里占的列数。CJK 字符按两列算。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
