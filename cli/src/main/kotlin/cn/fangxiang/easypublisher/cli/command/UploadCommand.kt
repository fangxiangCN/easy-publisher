package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.ExitCodes
import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.cli.renderTable
import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.service.ChannelStage
import cn.fangxiang.easypublisher.core.service.JobState
import cn.fangxiang.easypublisher.core.service.PublishPolicy
import cn.fangxiang.easypublisher.core.service.UploadJob
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.core.ProgramResult
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.parameters.types.choice
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option
import com.github.ajalt.clikt.parameters.options.required
import com.github.ajalt.clikt.parameters.options.split
import com.github.ajalt.clikt.parameters.types.file
import com.github.ajalt.clikt.parameters.types.long
import kotlinx.coroutines.delay
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

class UploadCommand : SuspendingCliktCommand(name = "upload") {

    override fun help(context: Context) = """
        上传 APK 并提交新版本。

        注意：提交后无法通过 API 撤回 —— 各应用商店都不提供撤销版本更新的接口。
        建议先用 `status` 确认各渠道状态。

        默认等待全部渠道完成并实时显示进度；加 --detach 可立即返回任务 id，
        之后用 `job status <id>` 查询。
    """.trimIndent()

    private val app by option("--app", help = "包名").required()

    private val apk by option("--apk", help = "APK 文件，或存放多渠道包的目录")
        .file(mustExist = true, mustBeReadable = true)
        .required()

    private val desc by option("--desc", help = "更新说明").required()

    private val channels by option("--channel", help = "只发指定渠道，逗号分隔").split(",")

    private val onlineTime by option(
        "--online-time",
        help = "定时上线时间，格式 yyyy-MM-dd HH:mm:ss，不填则审核通过后立即发布",
    )

    private val allowSameVersion by option(
        "--allow-same-version",
        help = "允许版本号与线上相同（审核被拒后仅更新素材时使用）。版本号低于线上仍会被拒绝",
    ).flag()

    private val skipVersionCheck by option(
        "--skip-version-check",
        help = "跳过全部版本号校验，仅用于排查问题",
    ).flag()

    private val stopAfter by option(
        "--stop-after",
        help = "流程走到哪一步就停下：artifact=仅上传安装包（不创建版本）、" +
            "draft=停在草稿态（可先到渠道后台核对再送审）、submit=一路走到送审（默认）。" +
            "并非所有渠道都支持中途停下，小米的 dev/push 是原子的，只能 submit",
    ).choice("artifact", "draft", "submit")

    private val timeout by option("--timeout", help = "单次请求超时秒数，默认 120").long()

    private val detach by option("--detach", help = "立即返回任务 id，不等待完成").flag()

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val applicationId = ApplicationId.validate(app)
        val scheduledAt = onlineTime?.let { parseOnlineTime(it) } ?: 0L

        val rule = when {
            skipVersionCheck -> PublishPolicy.VersionRule.Skip
            allowSameVersion -> PublishPolicy.VersionRule.AllowSame
            else -> PublishPolicy.VersionRule.Strict
        }

        val jobId = service.submit(
            applicationId = applicationId,
            apkPath = apk,
            releaseParams = ReleaseParams(updateDesc = desc, onlineTime = scheduledAt),
            channelIds = channels,
            versionRule = rule,
            timeouts = timeout?.let { HttpTimeouts.ofSeconds(it) } ?: HttpTimeouts.DEFAULT,
            stopAfter = when (stopAfter) {
                "artifact" -> ReleaseStage.UploadArtifact
                "draft" -> ReleaseStage.CreateDraft
                else -> ReleaseStage.SubmitReview
            },
        )

        if (detach) {
            val job = service.job(jobId)
            if (json) {
                echo(Output.json(mapOf("ok" to true, "jobId" to jobId, "detached" to true)))
            } else {
                echo("任务已启动：$jobId")
                echo("渠道：${job?.channels?.joinToString(", ") { it.channelId }}")
                echo("用 `easy-publisher job status $jobId` 查询进度")
            }
            return@runCatchingPublish
        }

        val finished = awaitJob(jobId)
        report(finished)
    }

    /**
     * 轮询直到任务结束。
     *
     * 进度在非 JSON 模式下刷新同一行；JSON 模式静默等待，只输出最终结果，
     * 避免中间态污染 stdout 的结构化输出。
     */
    private suspend fun awaitJob(jobId: String): UploadJob {
        var lastLine = ""
        while (true) {
            val job = service.job(jobId) ?: throw PublishError.configuration("任务不存在：$jobId")
            if (!json) {
                val line = job.channels.joinToString("  ") { "${it.channelId}:${it.stage.label}" }
                if (line != lastLine) {
                    echo("\r$line", trailingNewline = false, err = true)
                    lastLine = line
                }
            }
            if (job.done) {
                if (!json && lastLine.isNotEmpty()) echo("", err = true)
                return job
            }
            delay(POLL_INTERVAL_MS)
        }
    }

    private fun report(job: UploadJob) {
        if (json) {
            echo(
                Output.json(
                    mapOf(
                        "ok" to (job.state == JobState.Succeeded),
                        "jobId" to job.id,
                        "state" to job.state.name,
                        "applicationId" to job.applicationId,
                        "versionCode" to job.versionCode,
                        "versionName" to job.versionName,
                        "channels" to job.channels.map { progress ->
                            val stage = progress.stage
                            mapOf(
                                "id" to progress.channelId,
                                "state" to stage::class.simpleName,
                                "message" to stage.label,
                            ) + if (stage is ChannelStage.Failed) {
                                mapOf(
                                    "kind" to stage.kind.name,
                                    "kindLabel" to stage.kind.label,
                                    "code" to stage.code,
                                    "retryable" to stage.retryable,
                                    // 越过送审点后 retryable 恒为 false，phase 说明原因：
                                    // 不是错误不可恢复，而是无法确定服务端是否已受理
                                    "phase" to stage.phase.name,
                                    "detail" to stage.message,
                                )
                            } else {
                                emptyMap()
                            }
                        },
                    )
                )
            )
        } else {
            echo("")
            echo("${job.applicationId}  ${job.versionName}(${job.versionCode})")
            echo(
                renderTable(
                    listOf("渠道", "结果"),
                    job.channels.map { listOf(it.channelId, it.stage.label) },
                )
            )
            val failures = job.failed()
            if (failures.isNotEmpty()) {
                echo("")
                failures.forEach { progress ->
                    val stage = progress.stage as ChannelStage.Failed
                    echo("[${progress.channelId}] ${stage.message}", err = true)
                    if (stage.retryable) echo("  该错误通常可重试", err = true)
                }
            }
        }

        // 部分成功也要用非零退出码，否则 CI 会当成全部成功
        when (job.state) {
            JobState.Succeeded -> Unit

            JobState.Cancelled -> throw ProgramResult(ExitCodes.USAGE)

            else -> {
                val firstFailure = job.failed().firstOrNull()?.stage as? ChannelStage.Failed
                throw ProgramResult(
                    firstFailure?.let { ExitCodes.of(it.kind) } ?: ExitCodes.CHANNEL_REJECTED
                )
            }
        }
    }

    private fun parseOnlineTime(text: String): Long {
        val format = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.US)
        format.isLenient = false
        val parsed = try {
            format.parse(text)
        } catch (e: Exception) {
            throw PublishError.configuration(
                "定时上线时间格式不正确：$text，应为 yyyy-MM-dd HH:mm:ss"
            )
        }
        if (parsed.before(Date())) {
            throw PublishError.configuration("定时上线时间不能早于当前时间：$text")
        }
        return parsed.time
    }

    private companion object {
        const val POLL_INTERVAL_MS = 500L
    }
}
