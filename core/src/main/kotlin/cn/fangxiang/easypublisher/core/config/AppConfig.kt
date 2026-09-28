package cn.fangxiang.easypublisher.core.config

import cn.fangxiang.easypublisher.core.log.isSensitiveKey
import cn.fangxiang.easypublisher.core.log.redact
import com.squareup.moshi.Json
import com.squareup.moshi.JsonClass

/**
 * 一个待发布应用的配置。
 *
 * 新增字段必须带默认值，否则旧配置文件会解析失败。
 */
@JsonClass(generateAdapter = false)
data class AppConfig(
    @Json(name = "name")
    val name: String,
    @Json(name = "applicationId")
    val applicationId: String,
    @Json(name = "createTime")
    val createTime: Long = System.currentTimeMillis(),
    @Json(name = "channels")
    val channels: List<ChannelConfig> = emptyList(),
    /** 是否为每个渠道使用独立的渠道包 */
    @Json(name = "multiChannelApk")
    val multiChannelApk: Boolean = false,
) {

    fun channel(name: String): ChannelConfig? =
        channels.firstOrNull { it.name.equals(name, ignoreCase = true) }

    fun isChannelEnabled(name: String): Boolean = channel(name)?.enabled == true

    fun enabledChannels(): List<ChannelConfig> = channels.filter { it.enabled }

    fun withChannel(channel: ChannelConfig): AppConfig {
        val others = channels.filterNot { it.name.equals(channel.name, ignoreCase = true) }
        return copy(channels = others + channel)
    }

    /** 不输出参数值，避免凭据进入日志 */
    override fun toString(): String =
        "AppConfig($applicationId, name=$name, channels=${channels.map { it.name }})"

    @JsonClass(generateAdapter = false)
    data class ChannelConfig(
        @Json(name = "name")
        val name: String,
        @Json(name = "enabled")
        val enabled: Boolean = true,
        @Json(name = "params")
        val params: List<Param> = emptyList(),
    ) {
        fun param(name: String): Param? =
            params.firstOrNull { it.name.equals(name, ignoreCase = true) }

        fun value(name: String): String? = param(name)?.value?.takeIf { it.isNotBlank() }

        fun withParam(name: String, value: String): ChannelConfig {
            val others = params.filterNot { it.name.equals(name, ignoreCase = true) }
            return copy(params = others + Param(name, value))
        }

        override fun toString(): String =
            "ChannelConfig($name, enabled=$enabled, params=${params.map { it.name }})"
    }

    @JsonClass(generateAdapter = false)
    data class Param(
        @Json(name = "name")
        val name: String,
        @Json(name = "value")
        val value: String,
    ) {
        /**
         * 脱敏输出。
         *
         * 原项目的 data class 默认 toString() 会展开 value，配合
         * `debug("保存配置:${apkConfig}")` 把全部 clientSecret 与私钥打进日志。
         */
        override fun toString(): String =
            if (isSensitiveKey(name)) "Param($name=${redact(value)})" else "Param($name=$value)"
    }
}
