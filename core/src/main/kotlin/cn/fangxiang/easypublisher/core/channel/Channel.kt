package cn.fangxiang.easypublisher.core.channel

import cn.fangxiang.easypublisher.core.ApkInfo
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.net.ProgressChange
import java.io.File

/**
 * 渠道参数声明。渠道自描述参数，MCP 的 inputSchema 与 CLI 的帮助文本都由此生成。
 */
data class ChannelParam(
    val name: String,
    val description: String,
    val required: Boolean = true,
    val type: ParamType = ParamType.Text,
) {
    sealed interface ParamType {
        data object Text : ParamType

        /** 文本文件内容，如小米的公钥证书。[extension] 为期望的扩展名 */
        data class TextFile(val extension: String) : ParamType
    }
}

/** 发布参数 */
data class ReleaseParams(
    val updateDesc: String,
    /** 定时上线的毫秒时间戳；0 表示审核通过后立即发布 */
    val onlineTime: Long = 0,
) {
    val scheduled: Boolean get() = onlineTime > 0
}

/**
 * 渠道凭据。由 core 内部从 [cn.fangxiang.easypublisher.core.config.CredentialStore] 组装，
 * 不经过 CLI / MCP 的对外接口。
 */
class ChannelCredentials(private val values: Map<String, String>) {

    fun optional(key: String): String? = values[key]?.takeIf { it.isNotBlank() }

    operator fun get(key: String): String = optional(key)
        ?: throw cn.fangxiang.easypublisher.core.PublishError.credential("缺少凭据参数 $key")

    /** 绝不输出参数值 */
    override fun toString(): String = "ChannelCredentials(keys=${values.keys})"
}

/**
 * 单次上传所需的全部上下文。
 *
 * 关键设计：**渠道实现不持有任何可变状态。** 原项目的 `ChannelTask` 是
 * `ChannelRegistry` 里的进程级单例，却持有 `clientId` / `clientSecret` /
 * `submitStateListener` 三个无同步的可变字段，由 `init(params)` 与
 * `setSubmitStateListener()` 写入。GUI 里已经可能串台（首页刷新市场状态与上传页
 * 并发 init 同一批实例）；在 MCP server 里并发服务多个应用时则是必然 ——
 * 会用 A 应用的密钥去上传 B 应用的包。
 *
 * 此处把凭据与回调都变成方法入参，渠道实现无状态、可安全并发复用。
 */
data class UploadRequest(
    val apkFile: File,
    val apkInfo: ApkInfo,
    val credentials: ChannelCredentials,
    val releaseParams: ReleaseParams,
    val timeouts: HttpTimeouts = HttpTimeouts.DEFAULT,
    val onProgress: ProgressChange = {},
)

data class MarketQuery(
    val applicationId: String,
    val credentials: ChannelCredentials,
    val timeouts: HttpTimeouts = HttpTimeouts.DEFAULT,
)

/**
 * 一个应用商店渠道。实现必须是无状态的。
 */
interface Channel {

    /** 稳定标识，用于 CLI/MCP 参数与配置文件，如 `huawei` */
    val id: String

    /** 展示名称，如 `华为` */
    val displayName: String

    /**
     * 多渠道包场景下用于匹配文件名的标识，如 `HUAWEI`。
     */
    val fileNameTag: String

    /** 本渠道需要的参数 */
    val params: List<ChannelParam>

    /** 上传并提交新版本。抛出 [cn.fangxiang.easypublisher.core.PublishError] 表示失败 */
    suspend fun upload(request: UploadRequest)

    /** 查询应用在该渠道的状态 */
    suspend fun queryMarket(query: MarketQuery): MarketInfo
}
