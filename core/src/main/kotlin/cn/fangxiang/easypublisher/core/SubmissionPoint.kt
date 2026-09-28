package cn.fangxiang.easypublisher.core

import kotlinx.coroutines.CancellationException

/**
 * 把「送审」这一步包起来，使其后的任何失败都带上
 * [FailurePhase.AtOrAfterSubmission] 标记。
 *
 * ## 为什么需要这个
 *
 * 送审是整条发布链路里唯一不可撤销的动作。而它的失败有一种特别危险的形态：
 * 请求已经到达服务端并被受理，但响应在回程丢失。从客户端看，这与「请求根本没发出去」
 * 完全无法区分 —— 两者都是一个 `SocketTimeoutException`。
 *
 * 如果按普通网络错误处理，调用方（脚本、CI、或者按 SKILL.md 行事的 agent）会重试，
 * 于是同一个版本被送审两次。
 *
 * 这里不试图判断服务端到底有没有受理（无法判断），而是把不确定性如实传给调用方：
 * `retryable = false`，message 里说明「先到后台确认」。
 *
 * ## 用法
 *
 * 只包裹真正越过送审点的那一次调用，不要包住整个上传流程 ——
 * 上传文件、绑定草稿这些步骤失败是可以安全重试的。
 *
 * ```
 * val result = atSubmissionPoint("华为", "提交审核") { api.submit(...) }
 * ```
 */
suspend fun <T> atSubmissionPoint(
    displayName: String,
    action: String,
    block: suspend () -> T,
): T {
    try {
        return block()
    } catch (e: CancellationException) {
        // 取消必须原样传播，否则协程框架无法感知，且用户取消会被显示成失败
        throw e
    } catch (e: PublishError) {
        throw e.atSubmissionPoint(displayName, action)
    } catch (e: Exception) {
        // 未归一化的异常（OkHttp/Retrofit/Moshi 的原始类型）在此收敛
        throw PublishError.fromNetwork(null, e).atSubmissionPoint(displayName, action)
    }
}
