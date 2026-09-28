package cn.fangxiang.easypublisher.core.channel.oppo

import cn.fangxiang.easypublisher.core.util.Digest.toHex
import java.nio.charset.StandardCharsets
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

/**
 * OPPO 开放平台的请求签名。
 *
 * 算法与原实现逐字节等价，**不允许做任何"看起来更好"的改写** —— 参与签名的字符串
 * 由服务端同样拼一遍再比对，键的排序规则、分隔符、空值的处理、十六进制的大小写
 * 任意一处不同都会直接鉴权失败，且服务端只会回一个笼统的签名错误码，极难定位。
 *
 * 相对原实现唯一的改动是编码工具：原先依赖 commons-codec，这里换成项目内的
 * [toHex]。两者都是「小写、每字节固定两位、高位补零」，输出完全一致 ——
 * 高位补零这一点很关键，`Integer.toHexString(0x0a)` 只返回 "a"，
 * 漏掉补零会让签名在约 1/16 的字节上产生歧义，表现为「偶发」鉴权失败。
 */
internal object OppoApiSigner {

    /**
     * 参数按键名的自然顺序（[String] 的字典序，与原实现的 `Collections.sort` 一致）
     * 升序排列后拼成 `k1=v1&k2=v2`，再用 client_secret 做 HMAC-SHA256。
     *
     * 值为 null 的参数整体跳过（连键名也不参与），同样沿用原实现的行为。
     */
    fun sign(secret: String, params: Map<String, String?>): String =
        hmacSha256(canonicalize(params), secret)

    /**
     * 待签名串。
     *
     * 单独抽出来是为了黄金向量测试：跨语言重写时最容易出错的不是 HMAC 本身，
     * 而是这个拼串规则（排序方式、null 值处理、分隔符）。把它变成可直接断言的
     * 纯函数，才能逐字节比对。
     */
    internal fun canonicalize(params: Map<String, String?>): String =
        params.keys.sorted()
            .mapNotNull { key ->
                val value = params[key] ?: return@mapNotNull null
                "$key=$value"
            }
            .joinToString("&")

    private fun hmacSha256(data: String, key: String): String {
        val signingKey = SecretKeySpec(key.toByteArray(StandardCharsets.UTF_8), ALGORITHM)
        val mac = Mac.getInstance(ALGORITHM)
        mac.init(signingKey)
        return mac.doFinal(data.toByteArray(StandardCharsets.UTF_8)).toHex()
    }

    private const val ALGORITHM = "HmacSHA256"
}
