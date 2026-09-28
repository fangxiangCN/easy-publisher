package cn.fangxiang.easypublisher.cli

import cn.fangxiang.easypublisher.cli.command.AppCommand
import cn.fangxiang.easypublisher.cli.command.ChannelCommand
import cn.fangxiang.easypublisher.cli.command.JobCommand
import cn.fangxiang.easypublisher.cli.command.StatusCommand
import cn.fangxiang.easypublisher.cli.command.UploadCommand
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.LogLevel
import com.github.ajalt.clikt.command.SuspendingCliktCommand
import com.github.ajalt.clikt.command.main
import com.github.ajalt.clikt.core.Context
import com.github.ajalt.clikt.core.subcommands
import com.github.ajalt.clikt.parameters.options.flag
import com.github.ajalt.clikt.parameters.options.option
import kotlin.time.Duration.Companion.seconds

class EasyPublisher : SuspendingCliktCommand(name = "easy-publisher") {

    override fun help(context: Context) = """
        一键把 APK 提交到多个应用商店。

        支持华为、小米、OPPO、vivo、荣耀。凭据保存在 ~/.easy-publisher/ 下（权限 600），
        也可用环境变量 EP_<渠道>_<参数> 覆盖，CI 场景无需落盘。

        日志写 stderr，结果写 stdout，可直接管道给 jq 处理。
    """.trimIndent()

    private val verbose by option("-v", "--verbose", help = "输出调试日志到 stderr").flag()

    private val quiet by option("-q", "--quiet", help = "只输出错误日志").flag()

    private val noFileLog by option("--no-file-log", help = "不写日志文件").flag()

    override suspend fun run() {
        AppLogger.configure(
            level = when {
                verbose -> LogLevel.Debug
                quiet -> LogLevel.Error
                else -> LogLevel.Info
            },
            fileLogging = !noFileLog,
        )
    }
}

suspend fun main(args: Array<String>) {
    try {
        EasyPublisher()
            .subcommands(
                AppCommand(),
                ChannelCommand(),
                StatusCommand(),
                UploadCommand(),
                JobCommand(),
            )
            .main(args)
    } finally {
        // 日志是异步写入的，进程退出前把队列排空，否则最后几行会丢
        AppLogger.shutdown(3.seconds)
    }
}
