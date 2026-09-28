package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.ExitCodes
import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.service.PublishService
import com.github.ajalt.clikt.core.CliktCommand
import com.github.ajalt.clikt.core.ProgramResult
import com.github.ajalt.clikt.command.SuspendingCliktCommand

/**
 * 共享的 PublishService。
 *
 * CLI 是单次执行的进程，这里用惰性单例即可；渠道实现无状态，可安全共享。
 */
internal val service: PublishService by lazy { PublishService() }

/**
 * 把 [PublishError] 翻译成「可读消息 + 稳定退出码」。
 *
 * 之所以统一在这里做：错误信息要同时满足人和脚本两种消费方式 ——
 * 人看 stderr 上的中文描述，脚本看退出码或 `--json` 的结构化输出。
 */
internal suspend fun SuspendingCliktCommand.runCatchingPublish(
    json: Boolean,
    block: suspend () -> Unit,
) {
    try {
        block()
    } catch (e: PublishError) {
        if (json) {
            // 结构化错误也走 stdout，脚本才能一并解析
            echo(
                Output.errorJson(
                    kind = e.kind.name,
                    message = e.message ?: "未知错误",
                    channel = e.channel,
                    code = e.code,
                    retryable = e.retryable,
                )
            )
        } else {
            echo(e.describe(), err = true)
            if (e.retryable) echo("（该错误通常可重试）", err = true)
            e.raw?.takeIf { it.isNotBlank() }?.let {
                echo("渠道原始响应：$it", err = true)
            }
        }
        throw ProgramResult(ExitCodes.of(e.kind))
    }
}

/** 非 suspend 版本，供普通 [CliktCommand] 使用 */
internal fun CliktCommand.failWith(error: PublishError, json: Boolean): Nothing {
    if (json) {
        echo(
            Output.errorJson(
                kind = error.kind.name,
                message = error.message ?: "未知错误",
                channel = error.channel,
                code = error.code,
                retryable = error.retryable,
            )
        )
    } else {
        echo(error.describe(), err = true)
    }
    throw ProgramResult(ExitCodes.of(error.kind))
}
