package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.cli.renderTable
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.service.ChannelStage
import cn.fangxiang.easypublisher.core.service.JobState
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.command.SuspendingNoOpCliktCommand
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.core.subcommands
import com.github.ajalt.clikt.parameters.arguments.argument
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option

class JobCommand : SuspendingNoOpCliktCommand(name = "job") {

    override fun help(context: Context) = """
        查询上传任务。

        注意：任务状态保存在进程内存里，CLI 进程退出后即失效。
        `--detach` 主要用于长驻进程（如 MCP server）；在 CLI 里通常直接等待更实用。
    """.trimIndent()

    init {
        subcommands(JobStatus(), JobList())
    }
}

private class JobStatus : SuspendingCliktCommand(name = "status") {

    override fun help(context: Context) = "查询指定任务的进度"

    private val jobId by argument("jobId", help = "任务 id")

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val job = service.job(jobId)
            ?: throw PublishError.configuration(
                "任务不存在：$jobId（任务状态不跨进程保留，CLI 每次执行都是新进程）"
            )

        if (json) {
            echo(
                Output.json(
                    mapOf(
                        "ok" to (job.state == JobState.Succeeded),
                        "jobId" to job.id,
                        "state" to job.state.name,
                        "done" to job.done,
                        "applicationId" to job.applicationId,
                        "versionCode" to job.versionCode,
                        "versionName" to job.versionName,
                        "channels" to job.channels.map { progress ->
                            mapOf(
                                "id" to progress.channelId,
                                "state" to progress.stage::class.simpleName,
                                "message" to progress.stage.label,
                                "retryable" to
                                    ((progress.stage as? ChannelStage.Failed)?.retryable ?: false),
                            )
                        },
                    )
                )
            )
        } else {
            echo("$jobId  ${job.state}  ${job.applicationId}  ${job.versionName}(${job.versionCode})")
            echo(
                renderTable(
                    listOf("渠道", "状态"),
                    job.channels.map { listOf(it.channelId, it.stage.label) },
                )
            )
        }
    }
}

private class JobList : SuspendingCliktCommand(name = "list") {

    override fun help(context: Context) = "列出本进程内的任务"

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val ids = service.jobIds()
        if (json) {
            echo(Output.json(mapOf("ok" to true, "jobIds" to ids)))
        } else if (ids.isEmpty()) {
            echo("当前进程没有任务")
        } else {
            ids.forEach { echo(it) }
        }
    }
}
