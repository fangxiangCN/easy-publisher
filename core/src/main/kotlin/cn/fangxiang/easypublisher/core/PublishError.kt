package cn.fangxiang.easypublisher.core

import java.io.IOException
import java.net.SocketTimeoutException
import java.net.UnknownHostException

/**
 * 失败原因分类。agent 与脚本依据此字段决定重试、换渠道还是交给人处理。
 */
enum class ErrorKind {
    /** 本地配置问题：缺参数、包名非法 */
    Configuration,

    /** 凭据缺失或被渠道拒绝 */
    Credential,

    /** 本地文件问题：APK 不存在、解析失败 */
    LocalFile,

    /** 网络层失败，通常可重试 */
    Network,

    /** 渠道返回了业务错误码 */
    ChannelRejected,

    /** 渠道响应无法解析，通常意味着接口变更 */
    ProtocolMismatch,

    /** 违反发布前置条件，如版本号不大于线上版本 */
    Precondition,

    Unknown,
    ;

    /** 中文标签，用于表格等空间受限的场合 */
    val label: String
        get() = when (this) {
            Configuration -> "配置错误"
            Credential -> "凭据问题"
            LocalFile -> "本地文件问题"
            Network -> "网络失败"
            ChannelRejected -> "渠道拒绝"
            ProtocolMismatch -> "接口不匹配"
            Precondition -> "不满足前置条件"
            Unknown -> "未知错误"
        }
}

/**
 * 失败发生在发布流程的哪个阶段。
 *
 * 重试是否安全不只取决于错误类型，还取决于远端是否已经产生了不可撤销的副作用。
 */
enum class FailurePhase {
    /**
     * 尚未越过送审点（取 token、查状态、上传文件、绑定草稿等）。
     *
     * 重试是安全的：远端最多留下一个草稿，不会产生重复的正式版本。
     */
    PreSubmission,

    /**
     * 已越过送审点，或无法确定是否越过。
     *
     * 此时任何失败都不能盲目重试 —— 服务端可能已经受理请求，只是响应在回程丢失
     * （超时、连接重置都会表现成这样）。重试会重复送审或产生重复版本，
     * 而各应用商店都不提供撤销版本更新的 API。
     */
    AtOrAfterSubmission,
}

/**
 * 结构化的发布失败信息。
 *
 * 原项目的 `ApiException` 把渠道返回的中文 message 拼成字符串，且
 * `check(response.isSuccessful)` 不带 lazyMessage —— 抛出的是默认的 "Check failed."，
 * 状态码与响应体全部丢失，导致所有渠道出错时都无法定位。
 */
class PublishError(
    val kind: ErrorKind,
    /** 渠道标识，非渠道相关的错误为 null */
    val channel: String? = null,
    /** 渠道返回的业务错误码 */
    val code: String? = null,
    message: String,
    /** 渠道原始响应，便于排查接口变更 */
    val raw: String? = null,
    cause: Throwable? = null,
    /** 失败发生的阶段，决定是否可以重试 */
    val phase: FailurePhase = FailurePhase.PreSubmission,
) : Exception(message, cause) {

    /**
     * 是否值得自动重试。
     *
     * 越过送审点之后一律为 false —— 见 [FailurePhase.AtOrAfterSubmission]。
     */
    val retryable: Boolean
        get() = when {
            phase == FailurePhase.AtOrAfterSubmission -> false
            kind == ErrorKind.Network -> true
            kind == ErrorKind.Unknown -> true
            else -> false
        }

    /**
     * 标记这个错误发生在送审点或之后，并在 message 后追加确认提示。
     *
     * 保留 [kind] / [code] / [channel] / [raw] / cause 链，只改变重试语义。
     *
     * @param displayName 渠道展示名，用于提示语
     * @param action 越过送审点的那一步，如「提交审核」
     */
    fun atSubmissionPoint(displayName: String, action: String): PublishError {
        if (phase == FailurePhase.AtOrAfterSubmission) return this
        val hint = "$action 可能已被服务端受理（响应在回程丢失也会报此错误）。" +
            "请先登录$displayName 开发者后台确认该版本是否已提交成功，" +
            "确认未提交后再重试 —— 重复送审无法撤销。"
        val original = message ?: ""
        return PublishError(
            kind = kind,
            channel = channel,
            code = code,
            message = if (original.isEmpty()) hint else "$original。$hint",
            raw = raw,
            cause = cause,
            phase = FailurePhase.AtOrAfterSubmission,
        )
    }

    fun describe(): String = buildString {
        channel?.let { append('[').append(it).append("] ") }
        append(message)
        code?.let { append(" (code=").append(it).append(')') }
    }

    companion object {

        fun configuration(message: String, channel: String? = null): PublishError =
            PublishError(ErrorKind.Configuration, channel = channel, message = message)

        fun credential(message: String, channel: String? = null): PublishError =
            PublishError(ErrorKind.Credential, channel = channel, message = message)

        fun localFile(message: String, cause: Throwable? = null): PublishError =
            PublishError(ErrorKind.LocalFile, message = message, cause = cause)

        fun precondition(message: String, channel: String? = null): PublishError =
            PublishError(ErrorKind.Precondition, channel = channel, message = message)

        fun rejected(
            channel: String,
            code: String?,
            message: String,
            raw: String? = null,
        ): PublishError = PublishError(
            ErrorKind.ChannelRejected,
            channel = channel,
            code = code,
            message = message,
            raw = raw,
        )

        fun protocol(channel: String, message: String, raw: String? = null, cause: Throwable? = null): PublishError =
            PublishError(
                ErrorKind.ProtocolMismatch,
                channel = channel,
                message = message,
                raw = raw,
                cause = cause,
            )

        /** 把网络层异常归一化 */
        fun fromNetwork(channel: String?, cause: Throwable): PublishError = when (cause) {
            is SocketTimeoutException -> PublishError(
                ErrorKind.Network,
                channel = channel,
                message = "请求超时，可尝试增大超时时间（--timeout）",
                cause = cause,
            )

            is UnknownHostException -> PublishError(
                ErrorKind.Network,
                channel = channel,
                message = "无法解析主机，请检查网络连接",
                cause = cause,
            )

            is IOException -> PublishError(
                ErrorKind.Network,
                channel = channel,
                message = cause.message ?: "网络请求失败",
                cause = cause,
            )

            else -> PublishError(
                ErrorKind.Unknown,
                channel = channel,
                message = cause.message ?: cause::class.simpleName ?: "未知错误",
                cause = cause,
            )
        }
    }
}
