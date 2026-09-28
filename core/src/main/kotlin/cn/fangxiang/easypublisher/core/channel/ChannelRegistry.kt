package cn.fangxiang.easypublisher.core.channel

import cn.fangxiang.easypublisher.core.PublishError

/**
 * 渠道注册表。
 *
 * 因为渠道实现已改为无状态（凭据与回调走方法入参），此处可以安全地共享实例 ——
 * 与原项目共享**持有可变凭据字段**的单例是两回事。
 */
object ChannelRegistry {

    private val channels: List<Channel> by lazy { builtIn() }

    fun all(): List<Channel> = channels

    fun ids(): List<String> = channels.map { it.id }

    fun find(id: String): Channel? =
        channels.firstOrNull { it.id.equals(id, ignoreCase = true) }

    fun require(id: String): Channel = find(id) ?: throw PublishError.configuration(
        "未知渠道：$id（可用渠道：${ids().joinToString(", ")}）"
    )
}
