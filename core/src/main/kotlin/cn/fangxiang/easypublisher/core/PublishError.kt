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
) : Exception(message, cause) {

    /** 是否值得自动重试 */
    val retryable: Boolean
        get() = when (kind) {
            ErrorKind.Network -> true
            ErrorKind.Unknown -> true
            else -> false
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
