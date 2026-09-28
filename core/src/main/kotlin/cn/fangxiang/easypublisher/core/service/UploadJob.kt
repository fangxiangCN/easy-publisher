package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.FailurePhase
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ReleaseStage

/** 单个渠道的提交阶段 */
sealed interface ChannelStage {

    data object Waiting : ChannelStage

    /** 正在执行某个步骤，如「获取 token」「绑定文件」 */
    data class Working(val action: String) : ChannelStage

    /** 上传中，[fraction] 取值 [0f, 1f] */
    data class Uploading(val fraction: Float) : ChannelStage

    /**
     * 流程正常结束。
     *
     * [reached] 记录实际到达的阶段 —— 停在草稿态时不能显示「已提交」，
     * 那会让使用者以为版本已经送审。
     */
    data class Succeeded(
        val reached: ReleaseStage = ReleaseStage.SubmitReview,
    ) : ChannelStage

    data class Failed(
        val kind: ErrorKind,
        val code: String?,
        val message: String,
        val retryable: Boolean,
        /** 失败发生在送审点之前还是之后，决定重试是否安全 */
        val phase: FailurePhase = FailurePhase.PreSubmission,
    ) : ChannelStage {

        /**
         * 表格用的简短结论。
         *
         * [message] 在越过送审点时会带上「先到后台确认」的长提示，塞进表格会把列宽
         * 撑爆，所以表格只显示结论，完整原因另走 stderr 或 JSON 输出。
         */
        val summary: String
            get() = when {
                phase == FailurePhase.AtOrAfterSubmission -> "失败（${kind.label}，已送审需人工确认）"
                code != null -> "失败（${kind.label} code=$code）"
                else -> "失败（${kind.label}）"
            }
    }

    data object Cancelled : ChannelStage

    val label: String
        get() = when (this) {
            is Waiting -> "等待中"
            is Working -> action
            is Uploading -> "上传中 ${(fraction * 100).toInt()}%"
            is Succeeded -> when (reached) {
                ReleaseStage.UploadArtifact -> "已上传安装包（未创建版本）"
                ReleaseStage.CreateDraft -> "草稿已就绪（未送审）"
                ReleaseStage.SubmitReview -> "已提交审核"
            }
            is Failed -> summary
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
            phase = error.phase,
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
    /** 本次请求希望停在哪一步。实际到达的阶段见各渠道的 [ChannelStage.Succeeded] */
    val requestedStage: ReleaseStage = ReleaseStage.SubmitReview,
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
