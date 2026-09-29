// Package vivo 实现 vivo 应用市场渠道。
package vivo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 公共参数。这七项由 vivo 网关规定，多一个少一个都会鉴权失败。
const (
	paramAccessKey    = "access_key"
	paramTimestamp    = "timestamp"
	paramMethod       = "method"
	paramVersion      = "v"
	paramSignMethod   = "sign_method"
	paramFormat       = "format"
	paramTargetAppKey = "target_app_key"
	paramSign         = "sign"

	versionValue      = "1.0"
	signMethodValue   = "HMAC-SHA256"
	formatValue       = "json"
	targetAppKeyValue = "developer"
)

// SignResult 是签名结果明细。
//
// Canonical 单独暴露是为了黄金向量测试：跨语言重写时最容易出错的不是 HMAC 本身，
// 而是拼串规则（排序方式、空值处理、分隔符）。把它变成可直接断言的字段，
// 才能逐字节比对。
type SignResult struct {
	// Canonical 是待签名串
	Canonical string
	// Signature 是小写十六进制的 HMAC-SHA256
	Signature string
	// Params 是补齐公共参数并追加 sign 之后的完整参数集，可直接作为 query
	Params map[string]string
}

// SignDetailed 在业务参数上补齐公共参数并计算签名。
//
// timestamp 由调用方给定，这样签名可复现 —— 生产路径走 Sign，
// 它用当前时间。测试若自己拼那七个公共参数，必然与生产代码漂移，
// 而漂移后的测试仍然会通过，等于没有保护。
func SignDetailed(accessKey, accessSecret, method string, originParams map[string]string, timestampMillis int64) SignResult {
	params := make(map[string]string, len(originParams)+8)
	for k, v := range originParams {
		params[k] = v
	}
	// 公共参数。timestamp 是毫秒（vivo 要求），不是秒
	params[paramAccessKey] = accessKey
	params[paramTimestamp] = strconv.FormatInt(timestampMillis, 10)
	params[paramMethod] = method
	params[paramVersion] = versionValue
	params[paramSignMethod] = signMethodValue
	params[paramFormat] = formatValue
	params[paramTargetAppKey] = targetAppKeyValue

	// sign 自身不参与待签串，所以必须在拼串之后才放进 map
	canonical := Canonicalize(params)
	signature := hmacSHA256Hex(canonical, accessSecret)
	params[paramSign] = signature

	return SignResult{Canonical: canonical, Signature: signature, Params: params}
}

// Sign 用当前时间签名，返回完整参数集。
//
// 返回值包含 access_key 与 sign，**不可整体写入日志** ——
// 签好的 URL 里带着凭据与签名。
func Sign(accessKey, accessSecret, method string, originParams map[string]string) map[string]string {
	return SignDetailed(accessKey, accessSecret, method, originParams,
		time.Now().UnixMilli()).Params
}

// Canonicalize 构造待签名串：按 key 的自然序（等价于 Java 的 Collections.sort，
// 都是 UTF-16/字节字典序）拼成 k1=v1&k2=v2。
//
// 与 Kotlin 版逐字节一致，包括这些细节：
//   - 排序用字符串字典序，不是插入顺序
//   - 参数值不做 URL 编码，签名针对的是原始值
//   - 值为空串仍然参与（`key=`），与「键不存在」不同
//
// 不允许做任何「看起来更合理」的改写：参与签名的字符串由服务端同样拼一遍再比对，
// 任意一处不同都会直接鉴权失败，而服务端只会回一个笼统的签名错误码，极难定位。
func Canonicalize(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

func hmacSHA256Hex(data, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
