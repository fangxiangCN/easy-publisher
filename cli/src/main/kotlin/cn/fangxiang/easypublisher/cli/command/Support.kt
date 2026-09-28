package cn.fangxiang.easypublisher.cli.command

import cn.fangxiang.easypublisher.cli.ExitCodes
import cn.fangxiang.easypublisher.cli.Output
import cn.fangxiang.easypublisher.core.FailurePhase
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
        reportPublishError({ msg, isErr -> echo(msg, err = isErr) }, e, json)
        throw ProgramResult(ExitCodes.of(e.kind))
    }
}

/** 非 suspend 版本，供普通 [CliktCommand] 使用 */
internal fun CliktCommand.failWith(error: PublishError, json: Boolean): Nothing {
    reportPublishError({ msg, isErr -> echo(msg, err = isErr) }, error, json)
    throw ProgramResult(ExitCodes.of(error.kind))
}

/**
 * 输出错误。`--json` 走 stdout（脚本解析），人类可读信息走 stderr。
 *
 * 重试提示按 [FailurePhase] 分流：越过送审点之后不能只说「不可重试」，
 * 必须说明原因是「无法确定服务端是否已受理」，否则使用者会以为是网络抖动而反复重试。
 */
private fun reportPublishError(
    echo: (message: String, isErrorStream: Boolean) -> Unit,
    e: PublishError,
    json: Boolean,
) {
    if (json) {
        // 结构化错误走 stdout，脚本才能和正常结果一并解析
        echo(
            Output.errorJson(
                kind = e.kind.name,
                message = e.message ?: "未知错误",
                channel = e.channel,
                code = e.code,
                retryable = e.retryable,
                phase = e.phase.name,
            ),
            false,
        )
        return
    }
    echo(e.describe(), true)
    when {
        e.phase == FailurePhase.AtOrAfterSubmission ->
            echo("（已越过送审点：请勿直接重试，先到开发者后台确认该版本是否已提交成功）", true)

        e.retryable -> echo("（该错误通常可重试）", true)
    }
    e.raw?.takeIf { it.isNotBlank() }?.let {
        echo("渠道原始响应：$it", true)
    }
}
