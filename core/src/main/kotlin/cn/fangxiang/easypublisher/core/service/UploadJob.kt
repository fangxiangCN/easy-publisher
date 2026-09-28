package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.PublishError

/** 单个渠道的提交阶段 */
sealed interface ChannelStage {

    data object Waiting : ChannelStage

    /** 正在执行某个步骤，如「获取 token」「绑定文件」 */
    data class Working(val action: String) : ChannelStage

    /** 上传中，[fraction] 取值 [0f, 1f] */
    data class Uploading(val fraction: Float) : ChannelStage

    data object Succeeded : ChannelStage

    data class Failed(
        val kind: ErrorKind,
        val code: String?,
        val message: String,
        val retryable: Boolean,
    ) : ChannelStage

    data object Cancelled : ChannelStage

    val label: String
        get() = when (this) {
            is Waiting -> "等待中"
            is Working -> action
            is Uploading -> "上传中 ${(fraction * 100).toInt()}%"
            is Succeeded -> "已提交"
            is Failed -> "失败：$message"
            is Cancelled -> "已取消"
        }

    val terminal: Boolean
        get() = this is Succeeded || this is Failed || this is Cancelled

    companion object {
        fun of(error: PublishError): Failed = Failed(
            kind = error.kind,
            code = error.code,
            message = error.describe(),
            retryable = error.retryable,
        )
    }
}

data class ChannelProgress(
    val channelId: String,
    val displayName: String,
    val stage: ChannelStage,
)

enum class JobState { Running, Succeeded, PartiallyFailed, Failed, Cancelled }

/**
 * 一次发布任务的快照。
 *
 * 上传一个大包动辄数分钟到数十分钟，MCP 的工具调用是一次请求一次响应，
 * 不能让 agent 阻塞等待。因此 `upload` 立即返回 [id]，
 * 调用方轮询 [PublishService.job] 获取此快照。
 */
data class UploadJob(
    val id: String,
    val applicationId: String,
    val apkPath: String,
    val versionCode: Long,
    val versionName: String,
    val channels: List<ChannelProgress>,
    val startedAt: Long,
    val finishedAt: Long? = null,
) {
    val state: JobState
        get() {
            val stages = channels.map { it.stage }
            return when {
                stages.any { !it.terminal } -> JobState.Running
                stages.all { it is ChannelStage.Succeeded } -> JobState.Succeeded
                stages.all { it is ChannelStage.Cancelled } -> JobState.Cancelled
                stages.any { it is ChannelStage.Succeeded } -> JobState.PartiallyFailed
                else -> JobState.Failed
            }
        }

    val done: Boolean get() = state != JobState.Running

    fun succeeded(): List<String> =
        channels.filter { it.stage is ChannelStage.Succeeded }.map { it.channelId }

    fun failed(): List<ChannelProgress> =
        channels.filter { it.stage is ChannelStage.Failed }
}
