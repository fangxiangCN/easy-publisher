package cn.fangxiang.easypublisher.core.net

import okhttp3.HttpUrl
import okhttp3.OkHttpClient
import java.time.Duration
import java.util.concurrent.ConcurrentHashMap

/**
 * 超时配置。
 *
 * 原实现只有一个硬编码的 60 秒常量，且没有 `callTimeout` —— 网络半死状态下，
 * 只要单次读写不超时，大文件上传可以长时间悬挂。issue 反馈 vivo 渠道 60 秒仍然不够。
 */
data class HttpTimeouts(
    val connect: Duration = Duration.ofSeconds(30),
    val read: Duration = Duration.ofSeconds(120),
    val write: Duration = Duration.ofSeconds(120),
    /** 整体调用超时，覆盖连接 + 读写全过程 */
    val call: Duration = Duration.ofMinutes(60),
) {
    companion object {
        val DEFAULT = HttpTimeouts()

        fun ofSeconds(seconds: Long): HttpTimeouts = HttpTimeouts(
            connect = Duration.ofSeconds(minOf(seconds, 60)),
            read = Duration.ofSeconds(seconds),
            write = Duration.ofSeconds(seconds),
            call = Duration.ofSeconds(maxOf(seconds * 30, seconds)),
        )
    }
}

/**
 * OkHttp 客户端工厂。
 *
 * 与原项目的重要区别：**不提供任何关闭 TLS 校验的客户端**。原实现保留了一个
 * `debugClient()`，内含空实现的 X509TrustManager、`hostnameVerifier { _, _ -> true }`
 * 与 `SSLContext.getInstance("SSL")`。虽然入口被编译期常量挡住，但代码仍随产物分发，
 * 一行改动即可让全部渠道凭据在中间人面前明文可读。此处直接不移植。
 *
 * 需要抓包调试时，通过 JVM 参数导入代理根证书：
 * `-Djavax.net.ssl.trustStore=... -Dhttps.proxyHost=... -Dhttps.proxyPort=...`
 */
object HttpClients {

    private val cache = ConcurrentHashMap<HttpTimeouts, OkHttpClient>()

    fun of(timeouts: HttpTimeouts = HttpTimeouts.DEFAULT): OkHttpClient =
        cache.computeIfAbsent(timeouts) { t ->
            OkHttpClient.Builder()
                .connectTimeout(t.connect)
                .readTimeout(t.read)
                .writeTimeout(t.write)
                .callTimeout(t.call)
                .retryOnConnectionFailure(true)
                .build()
        }

    /**
     * 校验服务端下发的上传地址必须是 https。
     *
     * 华为与荣耀的上传地址来自接口响应，原实现直接交给 `Request.Builder().url(...)`，
     * 而 OkHttp 接受 http:// —— 若响应被篡改或服务端返回明文地址，
     * APK 与 Authorization 头会以明文发出。
     */
    fun requireHttps(url: HttpUrl, channel: String): HttpUrl {
        if (!url.isHttps) {
            throw cn.fangxiang.easypublisher.core.PublishError.protocol(
                channel = channel,
                message = "渠道返回的上传地址不是 https，已拒绝上传：${url.redact()}",
            )
        }
        return url
    }

    /** 隐去 query，避免把签名参数写进日志 */
    private fun HttpUrl.redact(): String = "$scheme://$host$encodedPath"
}
