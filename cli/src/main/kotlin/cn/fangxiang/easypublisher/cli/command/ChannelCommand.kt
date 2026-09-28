package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.cli.renderTable
import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.ChannelRegistry
import cn.fangxiang.easypublisher.core.config.AppConfig
import cn.fangxiang.easypublisher.core.config.AppConfigStore
import cn.fangxiang.easypublisher.core.config.EnvCredentialStore
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.command.SuspendingNoOpCliktCommand
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.core.subcommands
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option
import com.github.ajalt.clikt.parameters.options.required
import com.github.ajalt.clikt.parameters.types.file
import java.io.File

class ChannelCommand : SuspendingNoOpCliktCommand(name = "channel") {

    override fun help(context: Context) = "查看渠道与配置凭据"

    init {
        subcommands(ChannelList(), ChannelSet(), ChannelToggle())
    }
}

private class ChannelList : SuspendingCliktCommand(name = "list") {

    override fun help(context: Context) = "列出支持的渠道及其所需参数"

    private val app by option("--app", help = "指定应用后额外显示各参数是否已配置")

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val config = app?.let { AppConfigStore().get(ApplicationId.validate(it)) }

        if (json) {
            echo(
                Output.json(
                    mapOf(
                        "ok" to true,
                        "channels" to ChannelRegistry.all().map { channel ->
                            mapOf(
                                "id" to channel.id,
                                "displayName" to channel.displayName,
                                "fileNameTag" to channel.fileNameTag,
                                "capability" to mapOf(
                                    "supportedStages" to channel.capability.supportedStages.map { it.name },
                                    "riskLevel" to channel.capability.riskLevel.name,
                                    "withdrawal" to channel.capability.withdrawal.name,
                                    "requiresExplicitConfirmation" to
                                        channel.capability.requiresExplicitConfirmation,
                                    "automaticRetryAfterSubmission" to
                                        channel.capability.automaticRetryAfterSubmission,
                                    "evidence" to channel.capability.evidence.name,
                                    "verifiedScope" to channel.capability.verifiedScope,
                                    "note" to channel.capability.note,
                                ),
                                "params" to channel.params.map { param ->
                                    mapOf(
                                        "name" to param.name,
                                        "description" to param.description,
                                        "required" to param.required,
                                        "type" to param.type.describe(),
                                        "envName" to EnvCredentialStore.envName(channel.id, param.name),
                                        "configured" to config?.let {
                                            !it.channel(channel.id)?.value(param.name).isNullOrBlank()
                                        },
                                    )
                                },
                            )
                        },
                    )
                )
            )
            return@runCatchingPublish
        }

        // 先输出能力画像：风险等级与「能不能停在送审之前」决定了该怎么用这个渠道，
        // 应该在动手之前就看到，而不是发完才发现没法反悔
        echo(
            renderTable(
                listOf("渠道", "名称", "风险", "可停在", "撤回", "证据"),
                ChannelRegistry.all().map { ch ->
                    val cap = ch.capability
                    listOf(
                        ch.id,
                        ch.displayName,
                        cap.riskLevel.label,
                        cap.supportedStages.joinToString("/") { it.shortName() },
                        cap.withdrawal.label,
                        cap.evidence.label,
                    )
                },
            )
        )
        echo("")
        ChannelRegistry.all().forEach { ch ->
            echo("${ch.displayName}（${ch.id}）：${ch.capability.note}")
            ch.capability.verifiedScope?.let { echo("    实测范围：$it") }
        }
        echo("")
        echo("所需参数：")
        val rows = ChannelRegistry.all().flatMap { channel ->
            channel.params.map { param ->
                val state = when {
                    config == null -> "-"
                    !config.channel(channel.id)?.value(param.name).isNullOrBlank() -> "已配置"
                    System.getenv(EnvCredentialStore.envName(channel.id, param.name)) != null -> "环境变量"
                    else -> "缺失"
                }
                listOf(channel.id, channel.displayName, param.name, param.description, state)
            }
        }
        echo(renderTable(listOf("渠道", "名称", "参数", "说明", "状态"), rows))
        echo("")
        echo("凭据也可用环境变量提供，优先于配置文件，例如：")
        echo("  export ${EnvCredentialStore.envName("huawei", "client_secret")}=xxx")
    }
}

private class ChannelSet : SuspendingCliktCommand(name = "set") {

    override fun help(context: Context) = """
        设置渠道凭据。

        凭据以 600 权限保存在 ~/.easy-publisher/apps/ 下。注意用 --value 传入的值会进入
        shell 历史，敏感凭据建议用 --value-file 从文件读取，或改用环境变量。
    """.trimIndent()

    private val app by option("--app", help = "包名").required()

    private val channel by option("--channel", help = "渠道 id").required()

    private val key by option("--key", help = "参数名").required()

    private val value by option("--value", help = "参数值")

    private val valueFile by option(
        "--value-file",
        help = "从文件读取参数值（小米的公钥证书等文件型参数必须用这个）",
    ).file(mustExist = true, canBeDir = false, mustBeReadable = true)

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val applicationId = ApplicationId.validate(app)
        val target = ChannelRegistry.require(channel)

        val param = target.params.firstOrNull { it.name.equals(key, ignoreCase = true) }
            ?: throw PublishError.configuration(
                "渠道 ${target.displayName} 没有参数 $key。可用参数：" +
                    target.params.joinToString(", ") { it.name },
                channel = target.id,
            )

        val resolved = when {
            valueFile != null && value != null ->
                throw PublishError.configuration("--value 与 --value-file 只能用一个")

            valueFile != null -> readValueFile(valueFile!!, param)

            value != null -> value!!

            else -> throw PublishError.configuration("必须提供 --value 或 --value-file")
        }

        val store = AppConfigStore()
        val config = store.get(applicationId)
            ?: throw PublishError.configuration(
                "未找到应用 $applicationId，请先执行 `easy-publisher app add --id $applicationId`"
            )

        val existing = config.channel(target.id)
            ?: AppConfig.ChannelConfig(name = target.id, enabled = true)
        store.save(config.withChannel(existing.withParam(param.name, resolved)))

        if (json) {
            echo(Output.json(mapOf("ok" to true, "channel" to target.id, "key" to param.name)))
        } else {
            // 绝不回显参数值
            echo("已保存 ${target.displayName} 的 ${param.name}")
        }
    }

    private fun readValueFile(file: File, param: ChannelParam): String {
        val type = param.type
        if (type is ChannelParam.ParamType.TextFile) {
            val expected = type.extension
            if (!file.name.endsWith(".$expected", ignoreCase = true)) {
                echo(
                    "提示：${param.name} 通常是 .$expected 文件，当前传入的是 ${file.name}",
                    err = true,
                )
            }
        }
        return file.readText().trim()
    }
}

private class ChannelToggle : SuspendingCliktCommand(name = "toggle") {

    override fun help(context: Context) = "启用或停用某个渠道"

    private val app by option("--app", help = "包名").required()

    private val channel by option("--channel", help = "渠道 id").required()

    private val enable by option("--enable", help = "启用").flag()

    private val disable by option("--disable", help = "停用").flag()

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        if (enable == disable) {
            throw PublishError.configuration("必须指定 --enable 或 --disable 其中之一")
        }
        val applicationId = ApplicationId.validate(app)
        val target = ChannelRegistry.require(channel)
        val store = AppConfigStore()
        val config = store.require(applicationId)
        val existing = config.channel(target.id)
            ?: AppConfig.ChannelConfig(name = target.id)
        store.save(config.withChannel(existing.copy(enabled = enable)))

        if (json) {
            echo(Output.json(mapOf("ok" to true, "channel" to target.id, "enabled" to enable)))
        } else {
            echo("${target.displayName} 已${if (enable) "启用" else "停用"}")
        }
    }
}

/** 表格里用的短名，避免列宽被撑爆 */
private fun ReleaseStage.shortName(): String = when (this) {
    ReleaseStage.UploadArtifact -> "上传"
    ReleaseStage.CreateDraft -> "草稿"
    ReleaseStage.SubmitReview -> "送审"
}

private fun ChannelParam.ParamType.describe(): String = when (this) {
    is ChannelParam.ParamType.Text -> "text"
    is ChannelParam.ParamType.TextFile -> "file(.$extension)"
}
