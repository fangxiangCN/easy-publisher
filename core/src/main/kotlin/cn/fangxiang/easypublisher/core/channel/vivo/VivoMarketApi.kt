package cn.fangxiang.easypublisher.core.channel.vivo

import cn.fangxiang.easypublisher.core.atSubmissionPoint
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.net.Json
import cn.fangxiang.easypublisher.core.net.ProgressBody
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.textResponse
import cn.fangxiang.easypublisher.core.util.Digest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * vivo 开放平台接口。无状态：凭据由构造入参传入，实例随单次请求创建。
 *
 * 超时不在此处决定 —— [client] 由调用方按 `HttpClients.of(request.timeouts)` 构造，
 * 这样 issue #17（60 秒不足以上传大包）可以由用户直接用超时参数解决，
 * 而不需要改代码里的常量。
 */
internal class VivoMarketApi(
    private val accessKey: String,
    private val accessSecret: String,
    private val client: OkHttpClient,
) {

    suspend fun getAppInfo(packageName: String): VivoAppInfo {
        val url = signedUrl("app.query.details", mapOf("packageName" to packageName))
        val request = Request.Builder().url(url).get().build()
        val body = client.textResponse(request, VivoChannel.ID)
        val response = Json.parse<VivoAppInfoResponse>(VivoChannel.ID, body)
        response.ensureSuccess("查询应用详情", body)
        return response.data ?: throw PublishError.protocol(
            channel = VivoChannel.ID,
            message = "vivo 查询应用详情成功但未返回 data，请确认包名 $packageName 是否属于该开发者账号",
            raw = body.take(MAX_RAW),
        )
    }

    suspend fun uploadApk(
        artifactFile: File,
        packageName: String,
        progressChange: ProgressChange,
    ): VivoUploadResult {
        val fileMd5 = md5Of(artifactFile)
        // fileMd5 参与签名，因此必须在构造 URL 之前算好
        val params = mapOf(
            "packageName" to packageName,
            "fileMd5" to fileMd5,
        )
        val apkBody = ProgressBody(
            mediaType = "application/octet-stream".toMediaType(),
            file = artifactFile,
            progressChange = progressChange,
        )
        val requestBody = MultipartBody.Builder()
            .addFormDataPart("file", artifactFile.name, apkBody)
            .build()
        val url = signedUrl("app.upload.apk.app", params)
        val request = Request.Builder().url(url).post(requestBody).build()
        val body = client.textResponse(request, VivoChannel.ID)
        val response = Json.parse<VivoApkUploadResponse>(VivoChannel.ID, body)
        response.ensureSuccess("上传 APK", body)
        return response.data.requireUploadResult(body)
    }

    /**
     * 提交更新。
     *
     * 保持原实现的 GET —— 全部业务参数都在 query 上（见 [signedUrl]），
     * 换成 POST 表单会改变待签串与服务端的取参方式。
     */
    suspend fun submit(uploadResult: VivoUploadResult, releaseParams: ReleaseParams) {
        val onlineTime = releaseParams.onlineTime
        // 立即上架:1，定时上架:2
        val onlineType = if (onlineTime > 0) "2" else "1"
        val params = mutableMapOf(
            "packageName" to uploadResult.packageName,
            "versionCode" to uploadResult.versionCode.toString(),
            "apk" to uploadResult.serialnumber,
            "fileMd5" to uploadResult.fileMd5,
            "onlineType" to onlineType,
            "updateDesc" to releaseParams.updateDesc,
        )
        if (onlineTime > 0) {
            // onlineType = 2 时上架时间必填，格式固定 yyyy-MM-dd HH:mm:ss。
            // 显式指定 Locale.ROOT：默认 Locale 在部分区域（如 th-TH）会输出佛历年份，
            // 那样签名虽然仍然自洽，但服务端解析出的日期是错的。
            val format = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.ROOT)
            params["scheOnlineTime"] = format.format(Date(onlineTime))
        }
        val url = signedUrl("app.sync.update.app", params)
        val request = Request.Builder().url(url).get().build()
        // 请求一旦发出就进入不可撤销区间。注意连响应解析也纳入保护：
        // 解析失败同样意味着「不知道服务端有没有受理」，不能当成可重试的普通错误。
        atSubmissionPoint("vivo", "提交更新") {
            val body = client.textResponse(request, VivoChannel.ID)
            val response = Json.parse<VivoSubmitResponse>(VivoChannel.ID, body)
            response.ensureSuccess("提交更新", body)
        }
    }

    private suspend fun md5Of(artifactFile: File): String = withContext(Dispatchers.IO) {
        try {
            Digest.md5Hex(artifactFile)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            throw PublishError.localFile("计算 APK 的 MD5 失败：${artifactFile.absolutePath}", e)
        }
    }

    /**
     * 构造带签名的请求地址。
     *
     * vivo 用的是 `router/rest` 风格的网关：**method、access_key、timestamp、sign
     * 等签名参数以及全部业务参数都必须放在 query string 上**，连上传 APK 这种
     * multipart 请求也一样（body 里只有文件）。这不是移植时的偷懒，而是该网关的
     * 强制要求，改成放进 body 或 header 会直接鉴权失败。
     *
     * 副作用是签名与凭据会出现在 URL 里，所以这个 [HttpUrl] 绝不能整体进日志。
     */
    private fun signedUrl(method: String, params: Map<String, String>): HttpUrl {
        val signParams = VivoApiSigner.sign(accessKey, accessSecret, method, params)
        return DOMAIN.toHttpUrl().newBuilder()
            .apply { signParams.forEach { (key, value) -> addQueryParameter(key, value) } }
            .build()
    }

    /**
     * 判定业务成败。
     *
     * 原实现是 `get("subCode").asString.toIntOrNull()` —— 错误响应（限流、鉴权失败）
     * 往往只有 code 没有 subCode，于是这一行先抛 NPE，**把本该抛出的业务异常顶掉了**，
     * 用户看到的是 NullPointerException 而不是真实错误码。这是 vivo 渠道报错难以
     * 诊断的直接原因之一。
     *
     * 现在两个码都空安全：只要任一存在且非 0 就按渠道拒绝处理，并把原始响应
     * 放进 [PublishError.raw] 供排查。
     *
     * 但「两个码都没有」不能当成成功 —— 那说明响应不是 vivo 的正常信封（接口变更、
     * 网关返回了错误页、或者响应被截断）。把它判成成功会让 `submit` 这种没有后续
     * data 校验的调用谎报发版成功，这比原实现抛 NPE 更危险。
     */
    private fun VivoEnvelope.ensureSuccess(action: String, raw: String) {
        val code = codeText
        val subCode = subCodeText
        if (code == null && subCode == null) {
            throw PublishError.protocol(
                channel = VivoChannel.ID,
                message = "$action 的响应里既无 code 也无 subCode，无法判定成败，" +
                    "已按失败处理（接口可能已变更，或返回的不是 vivo 的正常响应）",
                raw = raw.take(MAX_RAW),
            )
        }
        val failed = (code != null && code != SUCCESS_CODE) ||
            (subCode != null && subCode != SUCCESS_CODE)
        if (!failed) return
        val reason = msg?.takeIf { it.isNotBlank() } ?: "渠道未返回错误描述"
        throw PublishError.rejected(
            channel = VivoChannel.ID,
            // subCode 更具体，优先作为错误码上报
            code = subCode?.takeIf { it != SUCCESS_CODE } ?: code,
            message = "$action 失败：$reason（错误码含义见 " +
                "https://dev.vivo.com.cn/documentCenter/doc/330 ）",
            raw = raw.take(MAX_RAW),
        )
    }

    private companion object {
        /** 沙箱环境，联调时把 [DOMAIN] 指向这里 */
        const val SANDBOX_DOMAIN = "https://sandbox-developer-api.vivo.com.cn/router/rest"
        const val RELEASE_DOMAIN = "https://developer-api.vivo.com.cn/router/rest"

        const val DOMAIN = RELEASE_DOMAIN

        const val SUCCESS_CODE = "0"
    }
}
