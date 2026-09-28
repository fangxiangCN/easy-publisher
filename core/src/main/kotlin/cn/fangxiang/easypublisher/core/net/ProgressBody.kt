package cn.fangxiang.easypublisher.core.net

import okhttp3.MediaType
import okhttp3.RequestBody
import okio.BufferedSink
import okio.source
import java.io.File
import java.io.IOException
import java.io.InterruptedIOException

/** 进度回调，取值范围 [0f, 1f] */
typealias ProgressChange = (progress: Float) -> Unit

/**
 * 带进度回调的文件请求体。
 *
 * 相比原实现的三处改动：
 *
 * 1. 缓冲区从 2KB 提到 64KB。原先每 2048 字节就 `flush()` 并回调一次，
 *    100MB 的包会产生约 5 万次系统调用与 5 万次回调。
 * 2. 回调按「百分比变化 ≥1% 或间隔 ≥200ms」节流。
 * 3. 循环内检查线程中断标志。原实现是不可中断的阻塞写，
 *    协程取消只在挂起点生效，用户取消上传后大文件仍会在后台继续传完。
 */
class ProgressBody(
    private val mediaType: MediaType,
    private val file: File,
    private val progressChange: ProgressChange,
) : RequestBody() {

    override fun contentType(): MediaType = mediaType

    override fun contentLength(): Long = file.length()

    @Throws(IOException::class)
    override fun writeTo(sink: BufferedSink) {
        val length = contentLength()
        if (length <= 0L) {
            throw IOException("APK 文件为空，无法上传：${file.absolutePath}")
        }
        var written = 0L
        var lastPercent = -1
        var lastReportAt = 0L

        file.source().use { source ->
            while (true) {
                if (Thread.currentThread().isInterrupted) {
                    throw InterruptedIOException("上传已取消")
                }
                val read = source.read(sink.buffer, SEGMENT_SIZE)
                if (read == -1L) break
                written += read
                sink.flush()

                val fraction = (written.toFloat() / length).coerceIn(0f, 1f)
                val percent = (fraction * 100).toInt()
                val now = System.currentTimeMillis()
                val changed = percent != lastPercent
                val elapsed = now - lastReportAt >= REPORT_INTERVAL_MS
                if (changed && (elapsed || percent == 100)) {
                    lastPercent = percent
                    lastReportAt = now
                    progressChange(fraction)
                }
            }
        }
        // 确保最终状态被送达
        if (lastPercent != 100) progressChange(1f)
    }

    private companion object {
        const val SEGMENT_SIZE = 64L * 1024
        const val REPORT_INTERVAL_MS = 200L
    }
}
