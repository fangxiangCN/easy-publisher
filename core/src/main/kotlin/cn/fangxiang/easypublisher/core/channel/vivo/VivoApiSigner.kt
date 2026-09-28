package cn.fangxiang.easypublisher.core.channel.vivo

import cn.fangxiang.easypublisher.core.util.Digest.toHex
import java.nio.charset.StandardCharsets
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

/**
 * vivo 开放平台的请求签名。
 *
 * 算法逐字节沿用原实现：公共参数固定七项、按 key 的自然（ASCII）序拼成
 * `k=v&k=v`、HMAC-SHA256 后转小写十六进制。**任何一处差异（多一个参数、
 * 排序规则不同、时间戳单位不同、十六进制大小写不同）都会直接导致鉴权失败**，
 * 因此这里不做「看起来更合理」的整理。
 *
 * 唯一的实现替换：原先依赖 commons-codec 做十六进制编码，此处改用
 * [cn.fangxiang.easypublisher.core.util.Digest.toHex]。两者输出一致 ——
 * 都是小写、每字节固定两位高位补零。
 */
internal object VivoApiSigner {

    /**
     * 在业务参数上补齐公共参数并追加 sign。
     *
     * 返回值包含 access_key 与 sign，**不可整体写入日志**。
     */
    fun sign(
        accessKey: String,
        accessSecret: String,
        method: String,
        originParams: Map<String, String>,
    ): Map<String, String> {
        val params = originParams.toMutableMap()
        // 公共参数。timestamp 是毫秒时间戳（vivo 要求），不是秒
        params["access_key"] = accessKey
        params["timestamp"] = System.currentTimeMillis().toString()
        params["method"] = method
        params["v"] = "1.0"
        params["sign_method"] = "HMAC-SHA256"
        params["format"] = "json"
        params["target_app_key"] = "developer"
        // sign 自身不参与待签串，所以必须在拼串之后才放进 map
        params["sign"] = hmacSha256(canonicalize(params), accessSecret)
        return params
    }

    /**
     * 待签名串：按 key 的自然序（等价于原实现的 `Collections.sort`）拼接。
     *
     * value 为 null 的项跳过 —— 保留原行为，避免出现 `key=null` 这种和服务端
     * 不一致的拼法。参数值不做 URL 编码，签名针对的是原始值。
     */
    private fun canonicalize(paramsMap: Map<String, String>): String =
        paramsMap.keys.sorted()
            .mapNotNull { key -> paramsMap[key]?.let { "$key=$it" } }
            .joinToString("&")

    private fun hmacSha256(data: String, key: String): String {
        val signingKey = SecretKeySpec(key.toByteArray(StandardCharsets.UTF_8), HMAC_ALGORITHM)
        val mac = Mac.getInstance(HMAC_ALGORITHM)
        mac.init(signingKey)
        return mac.doFinal(data.toByteArray(StandardCharsets.UTF_8)).toHex()
    }

    private const val HMAC_ALGORITHM = "HmacSHA256"
}
