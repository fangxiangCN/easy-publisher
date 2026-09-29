// Package jsonx 提供对渠道响应中「形态不稳定」字段的兼容解析。
//
// # 为什么需要这个包
//
// 国内几家应用商店的 API 并没有统一的 JSON 类型约定，同一个语义字段在不同渠道
// （甚至同一渠道的不同接口）会以不同 JSON 类型返回：
//
//	OPPO  app/info   "version_code": "10"          ← 字符串
//	vivo  app.details "versionCode": "10"          ← 字符串
//	荣耀  get-app-id  "appId": 900876322            ← 数字
//	华为  app-info    "versionCode": 1000           ← 数字
//
// Kotlin 版用 Moshi 反序列化，它的宽松转换（String 字段可接数字、Long 字段可接
// 字符串）把这些差异悄悄兜住了，所以问题从未暴露。Go 的 encoding/json 是严格
// 类型匹配，直接报 cannot unmarshal —— 表现为三个渠道的查询与上传全部失败。
//
// 这个包把「宽松转换」变成显式的、可测试的行为，而不是依赖某个库的副作用。
//
// # 为什么不是 optional 语义
//
// 这些类型只解决「类型形态」问题，不改变「字段可缺失」的既有设计 ——
// 可空仍然由指针（*FlexInt64）表达，缺失字段照样得到 nil。
// 两种问题分开处理，出错时的信息才足够定位。
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexInt64 是能同时接受 JSON 数字与字符串数字的 int64。
//
// 接受的形式：
//
//	10        数字
//	"10"      字符串数字
//	" 10 "    带空白的字符串数字（部分渠道会带 padding）
//	null      忽略，保持不变（配合指针使用即为「未提供」）
//
// 拒绝空字符串与非数字字符串。这与 Kotlin 版行为一致：Moshi 的 nextLong()
// 能解析带引号的数字，但对 "" 会抛 NumberFormatException。保持一致意味着
// 两版在同一个异常响应上给出同样的结果，而不是一个静默变 0、一个报错。
//
// 「字段缺失」与「字段类型不同」是两件事：前者由指针得到 nil，后者由本类型消化。
type FlexInt64 int64

// Int64 返回其 int64 值，便于在需要原生类型处使用。
func (f FlexInt64) Int64() int64 { return int64(f) }

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *FlexInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	// 数字形态直接解析
	if trimmed[0] != '"' {
		var n int64
		if err := json.Unmarshal(trimmed, &n); err != nil {
			// 浮点形态（如 10.0）也放行，截断为整数
			var fl float64
			if ferr := json.Unmarshal(trimmed, &fl); ferr == nil {
				*f = FlexInt64(int64(fl))
				return nil
			}
			return fmt.Errorf("期望整数或数字字符串，实际为 %s", compact(trimmed))
		}
		*f = FlexInt64(n)
		return nil
	}

	// 字符串形态：剥引号后按数字解析
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return fmt.Errorf("期望整数或数字字符串，实际为 %s", compact(trimmed))
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("期望整数或数字字符串，实际为空字符串")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// 兼容 "10.0" 这类带小数点的字符串
		if fl, ferr := strconv.ParseFloat(s, 64); ferr == nil {
			*f = FlexInt64(int64(fl))
			return nil
		}
		return fmt.Errorf("期望整数或数字字符串，实际为 %q", s)
	}
	*f = FlexInt64(n)
	return nil
}

// FlexString 是能同时接受 JSON 字符串与数字的 string。
//
// 典型场景是荣耀的 appId：文档写作字符串，实际返回裸数字 900876322。
// 数字会按其字面量转成十进制字符串（不做千分位等格式化）。
type FlexString string

// String 返回其字符串值。
func (f FlexString) String() string { return string(f) }

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *FlexString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("期望字符串或数字，实际为 %s", compact(trimmed))
		}
		*f = FlexString(s)
		return nil
	}

	// 数字：用 json.Number 保留原始字面量，避免 float64 让大整数丢精度
	var num json.Number
	if err := json.Unmarshal(trimmed, &num); err != nil {
		return fmt.Errorf("期望字符串或数字，实际为 %s", compact(trimmed))
	}
	*f = FlexString(num.String())
	return nil
}

// FlexBool 是能同时接受 JSON 布尔与字符串布尔的 bool。
//
// 部分渠道用 "1"/"0" 或 "true"/"false" 表示布尔。
type FlexBool bool

// Bool 返回其布尔值。
func (f FlexBool) Bool() bool { return bool(f) }

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *FlexBool) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("期望布尔或布尔字符串，实际为 %s", compact(trimmed))
		}
		switch strings.TrimSpace(strings.ToLower(s)) {
		case "true", "1", "yes":
			*f = true
		case "false", "0", "no", "":
			*f = false
		default:
			return fmt.Errorf("期望布尔或布尔字符串，实际为 %q", s)
		}
		return nil
	}

	var b bool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		// 数字 1/0 也接受
		var n int
		if nerr := json.Unmarshal(trimmed, &n); nerr == nil {
			*f = FlexBool(n != 0)
			return nil
		}
		return fmt.Errorf("期望布尔或布尔字符串，实际为 %s", compact(trimmed))
	}
	*f = FlexBool(b)
	return nil
}

// compact 把过长或含换行的原始 JSON 片段压成一行短串，用于错误信息。
func compact(data []byte) string {
	s := strings.Join(strings.Fields(string(data)), " ")
	const max = 64
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
