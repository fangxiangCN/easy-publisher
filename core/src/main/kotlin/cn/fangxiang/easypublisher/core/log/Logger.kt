package cn.fangxiang.easypublisher.core.log

import cn.fangxiang.easypublisher.core.AppPaths
import java.io.File
import java.io.PrintWriter
import java.io.StringWriter
import java.io.Writer
import java.net.UnknownHostException
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import kotlin.time.Duration

enum class LogLevel { Debug, Info, Error }

interface Logger {

    fun debug(tag: String, message: String, throwable: Throwable? = null)

    fun info(tag: String, message: String, throwable: Throwable? = null)

    fun error(tag: String, message: String, throwable: Throwable? = null)

    fun log(level: LogLevel, tag: String, message: String, throwable: Throwable? = null)

    /**
     * 等待队列排空，但**不关闭**日志系统。
     *
     * 与原实现的关键区别：原 `awaitTermination` 内部执行 `shutdown()`，而 `log()`
     * 开头有 `if (isShutdown) return`。结果是任意一次崩溃处理之后，进程若继续运行，
     * 所有后续日志会被静默丢弃。此处拆分为 [flush] 与 [shutdown] 两个操作。
     */
    fun flush(timeout: Duration)

    fun shutdown(timeout: Duration)

    companion object : Logger by AppLogger
}

/**
 * 默认日志实现。
 *
 * 两个关键约束：
 *
 * 1. **绝不写 stdout。** MCP 的 stdio transport 以 stdout 独占传输 JSON-RPC 报文，
 *    任何一行日志都会破坏协议解析。所有级别一律写 stderr。
 * 2. **级别过滤在控制台输出之前生效。** 原实现把 `printable(level)` 只用于文件写入，
 *    控制台输出无条件执行，导致 release 构建下 token 与凭据仍然被打印出来。
 */
object AppLogger : Logger {

    private val console: Writer = System.err.writer()

    private val timeFormat = SimpleDateFormat("yyyy-MM-dd HH:mm:ss.SSS", Locale.getDefault())

    private val dayFormat = SimpleDateFormat("yyyy-MM-dd", Locale.getDefault())

    private val executor = Executors.newSingleThreadExecutor { runnable ->
        Thread(runnable, "ep-logger").apply { isDaemon = true }
    }

    @Volatile
    private var minLevel: LogLevel = LogLevel.Info

    @Volatile
    private var fileLogging: Boolean = true

    /** 当前日志文件对应的日期，用于跨天切换 */
    private var currentDay: String? = null

    private var fileWriter: Writer? = null

    fun configure(level: LogLevel = LogLevel.Info, fileLogging: Boolean = true) {
        this.minLevel = level
        this.fileLogging = fileLogging
    }

    override fun debug(tag: String, message: String, throwable: Throwable?) =
        log(LogLevel.Debug, tag, message, throwable)

    override fun info(tag: String, message: String, throwable: Throwable?) =
        log(LogLevel.Info, tag, message, throwable)

    override fun error(tag: String, message: String, throwable: Throwable?) =
        log(LogLevel.Error, tag, message, throwable)

    override fun log(level: LogLevel, tag: String, message: String, throwable: Throwable?) {
        if (executor.isShutdown) return
        if (level < minLevel) return
        val thread = Thread.currentThread().name
        val timestamp = Date()
        runCatching {
            executor.execute {
                val time = timeFormat.format(timestamp)
                val line = format(level, thread, time, tag, message, throwable)
                runCatching {
                    console.write(line)
                    console.flush()
                }
                if (fileLogging) {
                    runCatching { writeToFile(timestamp, line) }
                }
            }
        }
    }

    override fun flush(timeout: Duration) {
        val latch = java.util.concurrent.CountDownLatch(1)
        runCatching { executor.execute { latch.countDown() } }
            .onFailure { return }
        runCatching { latch.await(timeout.inWholeMilliseconds, TimeUnit.MILLISECONDS) }
        runCatching {
            console.flush()
            fileWriter?.flush()
        }
    }

    override fun shutdown(timeout: Duration) {
        runCatching {
            executor.shutdown()
            executor.awaitTermination(timeout.inWholeMilliseconds, TimeUnit.MILLISECONDS)
        }
        runCatching {
            fileWriter?.flush()
            fileWriter?.close()
        }
    }

    /**
     * 写入当天的日志文件。
     *
     * 文件名按调用时的日期动态解析，因此长时间运行的进程跨天后会自动切到新文件 ——
     * 原实现在初始化时一次性确定文件名，跨天仍然追加到启动那天的文件里。
     */
    private fun writeToFile(timestamp: Date, line: String) {
        val day = dayFormat.format(timestamp)
        if (day != currentDay || fileWriter == null) {
            fileWriter?.let { old -> runCatching { old.flush(); old.close() } }
            val dir = AppPaths.ensureSecureDir(AppPaths.logDir)
            val file = File(dir, "$day.log")
            fileWriter = runCatching {
                file.appendingWriter().also { AppPaths.restrictToOwner(file) }
            }.getOrNull()
            currentDay = day
            runCatching { LogRotation.cleanup(dir) }
        }
        val writer = fileWriter ?: return
        writer.write(line)
        writer.flush()
    }

    private fun File.appendingWriter(): Writer =
        java.io.FileOutputStream(this, true).bufferedWriter()

    private fun format(
        level: LogLevel,
        thread: String,
        time: String,
        tag: String,
        message: String,
        throwable: Throwable?,
    ): String = buildString {
        append(time)
        append(" [").append(thread).append("] ")
        append(level.name).append('/').append(tag).append(": ")
        append(message)
        if (throwable != null) {
            val trace = stackTraceOf(throwable)
            if (trace.isNotEmpty()) {
                append(System.lineSeparator())
                append(trace)
            }
        }
        append(System.lineSeparator())
    }

    private fun stackTraceOf(throwable: Throwable): String {
        var cause: Throwable? = throwable
        while (cause != null) {
            // 网络不可用是常见情形，完整堆栈没有诊断价值
            if (cause is UnknownHostException) return ""
            cause = cause.cause
        }
        val buffer = StringWriter()
        PrintWriter(buffer).use { throwable.printStackTrace(it) }
        return buffer.toString()
    }
}
