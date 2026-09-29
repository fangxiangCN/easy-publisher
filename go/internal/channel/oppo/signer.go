// Package oppo 实现 OPPO 应用市场渠道。
package oppo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Sign 计算 OPPO 的请求签名。
//
// 规则：参数按 key 的字典序排序 → 拼成 k1=v1&k2=v2 → HMAC-SHA256(clientSecret)
// → 小写十六进制。
//
// **值为 nil 的参数整体跳过**（连键名也不参与）。这一点必须保留：黄金向量里
// 有一个 api_sign 为 nil 的用例，跳过与否会直接改变待签串。
// 调用方在计算 api_sign 之前会把它自己放进参数表（值为 nil），
// 因此这个跳过规则不是防御性代码，而是签名能通过的前提。
//
// 与 Kotlin 版逐字节一致，不允许做任何「看起来更合理」的改写 ——
// 参与签名的字符串由服务端同样拼一遍再比对，键的排序规则、分隔符、空值处理、
// 十六进制大小写任意一处不同都会直接鉴权失败，而服务端只会回一个笼统的
// 签名错误码，极难定位。
func Sign(secret string, params map[string]*string) string {
	return hmacSHA256Hex(Canonicalize(params), secret)
}

// Canonicalize 构造待签名串。单独导出是为了黄金向量测试能直接断言拼串结果 ——
// 跨语言重写时最容易出错的不是 HMAC 本身，而是这个拼串规则。
func Canonicalize(params map[string]*string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		v := params[k]
		if v == nil {
			continue // 值为 nil 的项整体跳过，连键名也不参与
		}
		parts = append(parts, k+"="+*v)
	}
	return strings.Join(parts, "&")
}

// StringMap 把普通字符串表转成签名器需要的指针表，便于调用方构造参数。
func StringMap(in map[string]string) map[string]*string {
	out := make(map[string]*string, len(in))
	for k, v := range in {
		value := v
		out[k] = &value
	}
	return out
}

func hmacSHA256Hex(data, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
