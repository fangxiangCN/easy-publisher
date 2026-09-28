package cn.fangxiang.easypublisher.mcp

import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.LogLevel
import cn.fangxiang.easypublisher.core.service.PublishService
import io.modelcontextprotocol.kotlin.sdk.server.Server
import io.modelcontextprotocol.kotlin.sdk.server.ServerOptions
import io.modelcontextprotocol.kotlin.sdk.server.StdioServerTransport
import io.modelcontextprotocol.kotlin.sdk.types.Implementation
import io.modelcontextprotocol.kotlin.sdk.types.ServerCapabilities
import kotlinx.coroutines.Job
import kotlinx.coroutines.runBlocking
import kotlinx.io.asSink
import kotlinx.io.asSource
import kotlinx.io.buffered
import java.io.FileOutputStream
import java.io.FileDescriptor
import java.io.PrintStream
import kotlin.time.Duration.Companion.seconds

/**
 * easy-publisher 的 MCP server（stdio transport）。
 *
 * ## stdout 必须被保护
 *
 * stdio transport 用 stdout 独占传输 JSON-RPC 报文，**任何一行多余输出都会让宿主
 * 解析失败**，而且症状是「server 莫名连不上」，极难定位到某个 `println`。
 *
 * 这里不只是"约定日志走 stderr"，而是做了实际隔离：启动时先把真正的 stdout 交给
 * 传输层，随后把进程级的 `System.out` 重定向到 stderr。这样即便 core、依赖库或将来
 * 有人新加的代码里有 `println`，也只会落到 stderr，不会破坏协议。
 */
fun main() = runBlocking {
    // 1. 先抓住真正的 stdout（fd 1），这是传输层唯一的出口
    val protocolOut = FileOutputStream(FileDescriptor.out)

    // 2. 把 System.out 换成 stderr。此后任何 println 都不会污染协议。
    //    必须在创建 transport 之后、任何业务代码执行之前完成。
    System.setOut(PrintStream(FileOutputStream(FileDescriptor.err), true))

    // 3. 日志：写 stderr（AppLogger 本身已保证），并关掉文件日志之外的噪声
    AppLogger.configure(level = LogLevel.Info, fileLogging = true)
    AppLogger.info(TAG, "easy-publisher MCP server 启动")

    val service = PublishService()

    val server = Server(
        serverInfo = Implementation(name = "easy-publisher", version = VERSION),
        options = ServerOptions(
            capabilities = ServerCapabilities(
                tools = ServerCapabilities.Tools(listChanged = false),
            ),
        ),
    )
    server.registerTools(service)

    val transport = StdioServerTransport(
        System.`in`.asSource().buffered(),
        protocolOut.asSink().buffered(),
    )

    val closed = Job()
    val session = server.createSession(transport)
    session.onClose {
        AppLogger.info(TAG, "MCP 连接已关闭")
        closed.complete()
    }
    closed.join()

    AppLogger.flush(3.seconds)
    AppLogger.shutdown(3.seconds)
}

private const val TAG = "mcp"

private const val VERSION = "1.0.0"
