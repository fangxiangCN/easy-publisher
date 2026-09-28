package cn.fangxiang.easypublisher.core.net

import cn.fangxiang.easypublisher.core.PublishError
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import okhttp3.Call
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import java.io.IOException
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/**
 * 执行请求并返回响应体文本。
 *
 * 两处相对原实现的修正：
 *  - 响应一律通过 `use {}` 关闭。原项目华为渠道的上传调用漏了 `.use`，
 *    每次上传泄漏一个连接与响应体，且异常路径同样不关闭。
 *  - 失败时带上状态码与响应体片段。原实现是裸 `check(response.isSuccessful)`，
 *    抛出的消息是默认的 "Check failed."。
 */
suspend fun OkHttpClient.textResponse(
    request: Request,
    channel: String? = null,
): String = withContext(Dispatchers.IO) {
    await(request, channel).use { response ->
        val body = response.body?.string().orEmpty()
        if (!response.isSuccessful) {
            throw PublishError(
                kind = cn.fangxiang.easypublisher.core.ErrorKind.ChannelRejected,
                channel = channel,
                code = response.code.toString(),
                message = "HTTP ${response.code} ${response.message}".trim(),
                raw = body.take(MAX_RAW_LENGTH),
            )
        }
        body
    }
}

/**
 * 执行请求，只关心是否成功。响应体在失败时用于诊断。
 */
suspend fun OkHttpClient.executeChecked(
    request: Request,
    channel: String? = null,
): Unit = withContext(Dispatchers.IO) {
    await(request, channel).use { response ->
        if (!response.isSuccessful) {
            val body = runCatching { response.body?.string().orEmpty() }.getOrDefault("")
            throw PublishError(
                kind = cn.fangxiang.easypublisher.core.ErrorKind.ChannelRejected,
                channel = channel,
                code = response.code.toString(),
                message = "HTTP ${response.code} ${response.message}".trim(),
                raw = body.take(MAX_RAW_LENGTH),
            )
        }
    }
}

/**
 * 把 OkHttp 调用桥接到协程，并在协程取消时取消底层 [Call]。
 *
 * 原实现用同步 `execute()`，协程取消无法中断正在进行的 socket 写。
 */
private suspend fun OkHttpClient.await(request: Request, channel: String?): Response =
    suspendCancellableCoroutine { continuation ->
        val call = newCall(request)
        continuation.invokeOnCancellation { runCatching { call.cancel() } }
        call.enqueue(object : okhttp3.Callback {
            override fun onResponse(call: Call, response: Response) {
                continuation.resume(response)
            }

            override fun onFailure(call: Call, e: IOException) {
                if (call.isCanceled()) {
                    continuation.resumeWithException(
                        kotlinx.coroutines.CancellationException("请求已取消")
                    )
                } else {
                    continuation.resumeWithException(PublishError.fromNetwork(channel, e))
                }
            }
        })
    }

private const val MAX_RAW_LENGTH = 2000
