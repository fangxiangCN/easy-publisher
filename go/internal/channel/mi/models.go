// Package mi 实现小米应用市场渠道。
package mi

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
)

// ID 是渠道标识。
const ID = "mi"

const (
	// Domain 是自动发布接口域名。
	//
	// 必须是 https：早期版本用的是 http，明文发出的表单里含 RSA 加密后的 SIG 与
	// 整个 APK，虽然 SIG 不可解但仍暴露账号与包信息，且可被中间人替换请求体。
	Domain = "https://api.developer.xiaomi.com/devupload"

	// PathPush 是上传 APK 并提交审核
	PathPush = "/dev/push"
	// PathQuery 是查询应用状态
	PathQuery = "/dev/query"

	// APKMediaType 是上传 apk part 的 Content-Type
	APKMediaType = "application/octet-stream"

	// APKPartName 是 multipart 里 apk 字段的名字
	APKPartName = "apk"
	// RequestDataField / SIGField 是表单字段名
	RequestDataField = "RequestData"
	SIGField         = "SIG"

	// SynchroTypeUpdate 表示更新已有 app。0 新增、2 修改信息，发版固定用 1
	SynchroTypeUpdate = 1

	maxRaw = 2000
	logTag = "小米应用市场"
)

// ---- RSA 参数 ----
//
// 下面三个值与算法串全部由小米开放平台的自动发布接口规定：服务端用与之配对的
// 私钥按同样的分组方式解密，任何一处不一致都会直接鉴权失败，
// 而我们在没有真实凭据的情况下无法验证。因此逐字保持原实现。
const (
	// KeySize 是小米下发的证书密钥长度
	KeySize = 1024
	// GroupSize 是单个密文分组长度 = 密钥长度 / 8
	GroupSize = KeySize / 8
	// EncryptGroupSize 是单个明文分组长度。11 字节是 PKCS#1 v1.5 padding 的固定开销
	EncryptGroupSize = GroupSize - 11
)

// ---- 响应模型 ----
//
// 响应字段一律可空 + 默认值。上游把 updateVersion / packageInfo 声明为非空，
// 而小米在 result == 0 时也可能不返回 packageInfo（例如应用尚未创建过版本），
// 此时反序列化直接失败，用户只看到一句英文。

// PackageInfo 是应用信息。
type PackageInfo struct {
	AppName     string           `json:"appName"`
	VersionName string           `json:"versionName"`
	VersionCode *jsonx.FlexInt64 `json:"versionCode"`
	PackageName string           `json:"packageName"`
}

// AppInfoResp 是 dev/query 的响应。
type AppInfoResp struct {
	// Result 用指针：缺失时必须是「未返回」而不是 0。
	// 声明成 int 会让缺失的 result 静默变成 0（成功），
	// 这正是「字段缺失被当成成功」那类错误
	Result  *int   `json:"result"`
	Message string `json:"message"`
	// UpdateVersion 为 false 时小米认为仍有版本在审核中。
	//
	// 用指针是因为「缺失」与「false」含义不同：缺失时不能猜 ——
	// 猜 true 会让上层误以为可以提交，猜 false 又会拦住正常发布
	UpdateVersion *bool        `json:"updateVersion"`
	PackageInfo   *PackageInfo `json:"packageInfo"`
}

// requirePackageInfo 校验 packageInfo 存在。
func (r AppInfoResp) requirePackageInfo() (PackageInfo, error) {
	if r.PackageInfo == nil {
		return PackageInfo{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg: "小米返回的应用信息中缺少 packageInfo，无法提交新版本。" +
				"请确认该应用已在小米开放平台创建并至少有一个历史版本",
		}
	}
	return *r.PackageInfo, nil
}

// CommonResp 是通用响应。小米所有接口都以 result == 0 表示成功。
type CommonResp struct {
	Result  *int   `json:"result"`
	Message string `json:"message"`
}

// checkMiResult 校验业务码。
//
// 上游用 get("result").asInt 手读，字段缺失时直接 NPE。这里把缺失当作协议异常 ——
// 不能静默当成成功。
func checkMiResult(result *int, message, action, raw string) error {
	if result == nil {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 失败：小米响应中没有 result 字段，接口可能已变更",
			Raw:     truncate(raw),
		}
	}
	if *result == 0 {
		return nil
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "小米未返回错误描述"
	}
	code := fmt.Sprint(*result)
	return &eperr.Error{
		Kind:    eperr.KindChannelRejected,
		Channel: ID,
		Code:    code,
		// 错误码写进 message：用户拿它去查小米文档
		Msg: fmt.Sprintf("%s 失败：%s (code=%s)", action, msg, code),
		Raw: truncate(raw),
	}
}

// ---- 请求体模型 ----
//
// RequestData 是「先序列化成字符串、再对同一份字符串算 MD5」，所以模型只负责
// 生成那个字符串，绝不能让表单字段和参与 hash 的文本出现任何差异。
//
// ## 关于 omitempty 的选用
//
// Moshi 只省略 **null** 值，空字符串仍会输出。Go 的 omitempty 连零值也省，
// 所以字符串字段一律不加 omitempty —— 否则 updateDesc 为空时两边的 JSON
// 会不同，MD5 对不上，表现为笼统的鉴权失败。
//
// onlineTime 例外：它在语义上就是「可选」，上游只在 > 0 时才 put 这个键，
// 因此用指针 + omitempty 精确对应。

// QueryRequest 是 dev/query 的 RequestData。
type QueryRequest struct {
	UserName    string `json:"userName"`
	PackageName string `json:"packageName"`
}

// PushRequest 是 dev/push 的 RequestData。
type PushRequest struct {
	UserName    string      `json:"userName"`
	SynchroType int         `json:"synchroType"`
	AppInfo     PushAppInfo `json:"appInfo"`
}

// PushAppInfo 是 PushRequest 的内层结构。
type PushAppInfo struct {
	AppName     string `json:"appName"`
	PackageName string `json:"packageName"`
	UpdateDesc  string `json:"updateDesc"`
	OnlineTime  *int64 `json:"onlineTime,omitempty"`
}

// SigPayload 是 SIG 的明文结构：私钥 + 各部分数据的 MD5 摘要清单。
//
// 这里的 hash 用 MD5 是小米接口规定的字段格式，**不是安全签名**：
// 它的作用是让服务端核对 RequestData 与 apk 在传输中没有损坏，而整个结构随后会被
// RSA 公钥加密才发出，攻击者无法在不持有私钥的情况下构造。
// 因此沿用 MD5 是可接受的，也是唯一能通过小米校验的选择。
type SigPayload struct {
	Password string    `json:"password"`
	Sig      []SigItem `json:"sig"`
}

// SigItem 是一条摘要记录。
type SigItem struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// marshalNoHTMLEscape 序列化 JSON，且不做 HTML 转义。
//
// ## 为什么必须关掉 HTML 转义
//
// Go 的 encoding/json **默认把 & < > 转义成 \u0026 \u003c \u003e**，
// 而 Kotlin 侧的 Moshi 不转义。这个差异会让 RequestData 的字节不同，
// 进而让 MD5 不同，服务端算出的 hash 与我们提交的对不上 ——
// 表现为笼统的鉴权失败，从错误信息完全看不出原因。
//
// json.Marshal 没有关闭转义的选项，只能用 Encoder。
// Encoder.Encode 会追加换行，需要去掉。
func marshalNoHTMLEscape(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// BuildQueryRequestData 构造 dev/query 的 RequestData JSON。
//
// 抽成纯函数是为了黄金向量测试：跨语言重写时，JSON 的字段顺序、是否省略 null、
// 数字是否带引号，任何一处不同都会让 MD5 变化，进而让服务端算出的 hash
// 与我们提交的不一致 —— 表现为笼统的鉴权失败。
func BuildQueryRequestData(account, packageName string) (string, error) {
	out, err := marshalNoHTMLEscape(QueryRequest{UserName: account, PackageName: packageName})
	if err != nil {
		return "", eperr.ConfigurationError("序列化 RequestData 失败：%v", err)
	}
	return out, nil
}

// BuildPushRequestData 构造 dev/push 的 RequestData JSON。
//
// synchroType 固定为 1（更新已有 app）；onlineTime 为 0 时整个字段省略，
// 与原实现的条件 put 行为一致。
func BuildPushRequestData(account string, info PackageInfo, updateDesc string, onlineTime int64) (string, error) {
	payload := PushRequest{
		UserName:    account,
		SynchroType: SynchroTypeUpdate,
		AppInfo: PushAppInfo{
			AppName:     info.AppName,
			PackageName: info.PackageName,
			UpdateDesc:  updateDesc,
		},
	}
	if onlineTime > 0 {
		t := onlineTime
		payload.AppInfo.OnlineTime = &t
	}
	out, err := marshalNoHTMLEscape(payload)
	if err != nil {
		return "", eperr.ConfigurationError("序列化 RequestData 失败：%v", err)
	}
	return out, nil
}

// BuildSig 构造 SIG 的明文 JSON。
func BuildSig(password string, items []SigItem) (string, error) {
	out, err := marshalNoHTMLEscape(SigPayload{Password: password, Sig: items})
	if err != nil {
		return "", eperr.ConfigurationError("序列化 SIG 失败：%v", err)
	}
	return out, nil
}

// MD5Hex 计算字符串的 MD5。
//
// 在此仅作为小米规定的数据指纹字段，不是安全签名。
func MD5Hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ---- RSA ----

// parsePublicKey 从 X.509 证书内容中取出公钥。
//
// Java 的 CertificateFactory("X.509") 同时接受 DER 与 PEM（内部会先尝试按 PEM 解），
// 因此这里也两种都试，保持行为一致。
//
// 证书无效属于凭据问题，归为 Credential 类错误并给中文提示 ——
// 上游在这里 printStackTrace 后抛原异常，用户看到的是一句英文堆栈，
// 无法判断到底是证书填错了还是接口挂了。
//
// 注意：错误信息里绝不能带证书内容本身。
func parsePublicKey(certificate string) (*rsa.PublicKey, error) {
	raw := strings.TrimSpace(certificate)
	if raw == "" {
		return nil, eperr.CredentialError("小米公钥证书为空，请检查 publicKey 参数")
	}

	der := []byte(raw)
	if block, _ := pem.Decode([]byte(raw)); block != nil {
		der = block.Bytes
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		// 也可能是直接给出的 PKIX 公钥而非证书
		if pub, pubErr := x509.ParsePKIXPublicKey(der); pubErr == nil {
			rsaPub, ok := pub.(*rsa.PublicKey)
			if !ok {
				return nil, eperr.CredentialError(
					"小米公钥证书里的密钥不是 RSA 类型，请确认填入的是开放平台下载的 .cer 证书")
			}
			return rsaPub, nil
		}
		return nil, &eperr.Error{
			Kind:    eperr.KindCredential,
			Channel: ID,
			Msg: "小米公钥证书解析失败，请确认填入的是开放平台下载的 .cer 证书文件：" +
				err.Error(),
			Err: err,
		}
	}

	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, eperr.CredentialError(
			"小米公钥证书里的密钥不是 RSA 类型，请确认填入的是开放平台下载的 .cer 证书")
	}
	return pub, nil
}

// Encrypt 用公钥分组加密，返回小写十六进制字符串。
//
// 分组规则必须与 Kotlin 版逐字节一致：每 117 字节明文一组，各加密成 128 字节密文，
// 依次拼接后转小写十六进制。
//
// ## 为什么不能靠断言密文相等来验证
//
// PKCS#1 v1.5 填充含随机数，**同一明文每次密文都不同**。断言密文相等的测试会永远
// 失败，然后让人怀疑是实现错了。因此验证方式改为：
//   - 被加密的**明文**（SIG 的 JSON）字节相等 —— 确定性的，也是最容易出错的地方
//   - 密文长度关系：n 字节明文 → ceil(n/117)*128 字节密文
//   - 用同一密钥对做加解密回环，断言还原出的明文一致
func Encrypt(content, certificate string) (string, error) {
	pub, err := parsePublicKey(certificate)
	if err != nil {
		return "", err
	}

	data := []byte(content)
	var out []byte
	for offset := 0; offset < len(data); offset += EncryptGroupSize {
		end := offset + EncryptGroupSize
		if end > len(data) {
			end = len(data)
		}
		// 分组大小固定 117 字节，是 PKCS#1 v1.5 在 1024 位密钥下的上限
		chunk, err := rsa.EncryptPKCS1v15(rand.Reader, pub, data[offset:end])
		if err != nil {
			return "", &eperr.Error{
				Kind:    eperr.KindCredential,
				Channel: ID,
				Msg:     "小米 SIG 加密失败，请确认公钥证书与开放平台一致：" + err.Error(),
				Err:     err,
			}
		}
		out = append(out, chunk...)
	}
	return hex.EncodeToString(out), nil
}

// EncryptGroupCount 返回给定明文长度会被切成几组。用于测试与诊断。
func EncryptGroupCount(plaintextLen int) int {
	if plaintextLen <= 0 {
		return 0
	}
	return (plaintextLen + EncryptGroupSize - 1) / EncryptGroupSize
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}
