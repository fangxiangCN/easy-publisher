package cn.fangxiang.easypublisher.core.log

/**
 * 凭据脱敏。
 *
 * 原项目有两条链路会把明文凭据写进日志：
 *  - `AppLogger.action()` 的 `debug(tag, "$action 结果:$result")`，而三个渠道的
 *    `getToken()` 返回值就是裸 access_token；
 *  - `AppLogger.debug(LOG_TAG, "保存配置:${apkConfig}")`，data class 的 toString()
 *    会展开全部 clientSecret 与私钥。
 *
 * 本项目的约定：凭据值一律通过 [redact] 输出，且 `action` 辅助函数不再打印返回值。
 */
fun redact(value: String?): String {
    if (value.isNullOrEmpty()) return "<empty>"
    if (value.length <= 8) return "***"
    return value.take(4) + "***" + value.takeLast(2) + "(len=${value.length})"
}

/** 参数名是否看起来承载敏感值 */
fun isSensitiveKey(name: String): Boolean {
    val lower = name.lowercase()
    return SENSITIVE_HINTS.any { lower.contains(it) }
}

private val SENSITIVE_HINTS = listOf(
    "secret", "token", "password", "passwd", "private", "key", "sign", "credential",
)
