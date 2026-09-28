package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.cli.renderTable
import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.channel.ChannelRegistry
import cn.fangxiang.easypublisher.core.config.AppConfig
import cn.fangxiang.easypublisher.core.config.AppConfigStore
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.command.SuspendingNoOpCliktCommand
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.core.subcommands
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option
import com.github.ajalt.clikt.parameters.options.required
import com.github.ajalt.clikt.parameters.options.split

class AppCommand : SuspendingNoOpCliktCommand(name = "app") {

    override fun help(context: Context) = "管理待发布的应用"

    init {
        subcommands(AppList(), AppAdd(), AppRemove())
    }
}

private class AppList : SuspendingCliktCommand(name = "list") {

    override fun help(context: Context) = "列出已配置的应用"

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val apps = service.listApps()
        if (json) {
            echo(
                Output.json(
                    mapOf(
                        "ok" to true,
                        "apps" to apps.map { app ->
                            mapOf(
                                "applicationId" to app.applicationId,
                                "name" to app.name,
                                "multiChannelApk" to app.multiChannelApk,
                                "channels" to app.channels.map {
                                    mapOf(
                                        "id" to it.name,
                                        "enabled" to it.enabled,
                                        // 只报参数名，绝不报参数值
                                        "configured" to it.params.map { p -> p.name },
                                    )
                                },
                            )
                        },
                    )
                )
            )
            return@runCatchingPublish
        }
        if (apps.isEmpty()) {
            echo("还没有配置任何应用。用 `easy-publisher app add --id <包名> --name <名称>` 添加。")
            return@runCatchingPublish
        }
        echo(
            renderTable(
                listOf("包名", "名称", "已启用渠道"),
                apps.map { app ->
                    listOf(
                        app.applicationId,
                        app.name,
                        app.enabledChannels().joinToString(", ") { it.name }.ifEmpty { "(无)" },
                    )
                },
            )
        )
    }
}

private class AppAdd : SuspendingCliktCommand(name = "add") {

    override fun help(context: Context) = "添加一个应用"

    private val id by option("--id", help = "包名，如 com.example.app").required()

    private val name by option("--name", help = "应用名称，默认用包名")

    private val channels by option(
        "--channels",
        help = "启用的渠道，逗号分隔。可用：${ChannelRegistry.ids().joinToString(",")}",
    ).split(",")

    private val multiChannelApk by option(
        "--multi-channel-apk",
        help = "每个渠道使用独立的渠道包（按文件名中的渠道标识匹配）",
    ).flag()

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val applicationId = ApplicationId.validate(id)
        val store = AppConfigStore()
        val existing = store.get(applicationId)

        val targets = channels?.map { ChannelRegistry.require(it.trim()).id }
            ?: ChannelRegistry.ids()

        val config = AppConfig(
            name = name ?: existing?.name ?: applicationId,
            applicationId = applicationId,
            createTime = existing?.createTime ?: System.currentTimeMillis(),
            multiChannelApk = multiChannelApk || existing?.multiChannelApk == true,
            channels = targets.map { channelId ->
                // 保留已填写的参数，避免重复执行 add 把凭据清掉
                existing?.channel(channelId)?.copy(enabled = true)
                    ?: AppConfig.ChannelConfig(name = channelId, enabled = true)
            },
        )
        store.save(config)

        if (json) {
            echo(Output.json(mapOf("ok" to true, "applicationId" to applicationId)))
        } else {
            val verb = if (existing == null) "已添加" else "已更新"
            echo("$verb 应用 $applicationId，启用渠道：${targets.joinToString(", ")}")
            echo("下一步：为每个渠道填写凭据")
            targets.forEach { channelId ->
                val channel = ChannelRegistry.require(channelId)
                channel.params.forEach { param ->
                    echo(
                        "  easy-publisher channel set --app $applicationId " +
                            "--channel $channelId --key ${param.name} --value <${param.description}>"
                    )
                }
            }
        }
    }
}

private class AppRemove : SuspendingCliktCommand(name = "remove") {

    override fun help(context: Context) =
        "删除应用配置。凭据会被覆写后真正删除，不保留备份"

    private val id by option("--id", help = "包名").required()

    private val json by option("--json", help = "输出 JSON").flag()

    override suspend fun run() = runCatchingPublish(json) {
        val removed = AppConfigStore().remove(ApplicationId.validate(id))
        if (json) {
            echo(Output.json(mapOf("ok" to true, "removed" to removed)))
        } else if (removed) {
            echo("已删除 $id 的配置及其凭据")
        } else {
            echo("没有找到 $id 的配置")
        }
    }
}
