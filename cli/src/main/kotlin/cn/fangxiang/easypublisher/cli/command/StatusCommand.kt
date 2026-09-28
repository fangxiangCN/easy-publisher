package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.cli.renderTable
import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.core.ProgramResult
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option
import com.github.ajalt.clikt.parameters.options.required
import com.github.ajalt.clikt.parameters.options.split
import com.github.ajalt.clikt.parameters.types.long

class StatusCommand : SuspendingCliktCommand(name = "status") {

    override fun help(context: Context) = """
        查询应用在各渠道的审核状态与线上版本号。

        发版前应当先跑这个命令：如果某渠道正在审核中，提交新版本会被拒绝。
    """.trimIndent()

    private val app by option("--app", help = "包名").required()

    private val channels by option("--channel", help = "只查指定渠道，逗号分隔").split(",")

    private val timeout by option("--timeout", help = "单次请求超时秒数，默认 120").long()

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val applicationId = ApplicationId.validate(app)
        val timeouts = timeout?.let { HttpTimeouts.ofSeconds(it) } ?: HttpTimeouts.DEFAULT

        val results = service.marketStates(applicationId, channels, timeouts)
        if (results.isEmpty()) {
            throw PublishError.configuration(
                "应用 $applicationId 没有启用任何渠道，用 " +
                    "`easy-publisher channel toggle --app $applicationId --channel <渠道> --enable` 启用"
            )
        }

        if (json) {
            echo(
                Output.json(
                    mapOf(
                        "ok" to true,
                        "applicationId" to applicationId,
                        "channels" to results.map { (id, result) ->
                            result.fold(
                                onSuccess = { info ->
                                    mapOf(
                                        "id" to id,
                                        "ok" to true,
                                        "reviewState" to info.reviewState.name,
                                        "reviewStateLabel" to info.reviewState.label,
                                        "canSubmit" to info.canSubmit,
                                        "lastVersionCode" to info.lastVersion?.code,
                                        "lastVersionName" to info.lastVersion?.name,
                                        "rawState" to info.rawState,
                                    )
                                },
                                onFailure = { error ->
                                    val publishError = error as? PublishError
                                    mapOf(
                                        "id" to id,
                                        "ok" to false,
                                        "kind" to (publishError?.kind?.name ?: "Unknown"),
                                        "code" to publishError?.code,
                                        "message" to (error.message ?: "查询失败"),
                                        "retryable" to (publishError?.retryable ?: false),
                                    )
                                },
                            )
                        },
                    )
                )
            )
        } else {
            echo(
                renderTable(
                    listOf("渠道", "审核状态", "线上版本", "可提交"),
                    results.map { (id, result) ->
                        result.fold(
                            onSuccess = { info ->
                                listOf(
                                    id,
                                    info.reviewState.label,
                                    info.lastVersion?.toString() ?: "无",
                                    if (info.canSubmit) "是" else "否",
                                )
                            },
                            onFailure = { error ->
                                listOf(id, "查询失败", "-", "-").also {
                                    echo("[$id] ${error.message}", err = true)
                                }
                            },
                        )
                    },
                )
            )
        }

        // 有渠道查询失败时用非零退出码，CI 才能感知
        if (results.values.any { it.isFailure }) {
            val first = results.values.first { it.isFailure }.exceptionOrNull()
            throw ProgramResult(
                (first as? PublishError)?.let { cn.fangxiang.easypublisher.cli.ExitCodes.of(it.kind) }
                    ?: 1
            )
        }
    }
}
