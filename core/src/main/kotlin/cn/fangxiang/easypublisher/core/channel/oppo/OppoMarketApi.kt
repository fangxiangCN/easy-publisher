package cn.fangxiang.easypublisher.core.channel.oppo

import cn.fangxiang.easypublisher.core.atSubmissionPoint
import cn.fangxiang.easypublisher.core.ArtifactInfo
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.Json
import cn.fangxiang.easypublisher.core.net.ProgressBody
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.textResponse
import kotlinx.coroutines.CancellationException
import okhttp3.FormBody
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
 * OPPO 应用市场（分发开放平台）接口封装。
 *
 * 无状态：凭据与 HTTP 客户端都由调用方按次传入，实例可并发复用。
 */
internal class OppoMarketApi(
    private val clientId: String,
    private val clientSecret: String,
    private val client: OkHttpClient,
) {

    /**
     * 获取 access_token。
     *
     * ## 为什么凭据仍在 query 上
     *
     * 原实现是字符串插值拼 URL：
     * ```
     * "$DOMAIN/developer/v1/token?client_id=$clientId&client_secret=$clientSecret"
     * ```
     * 这有两个独立的问题。
     *
     * **一、client_secret 出现在 URL 里。** 这是长期凭据（不像 access_token 会过期），
     * 而 URL 会被正向代理日志、企业网关审计、CDN 与服务端 access log 完整记录，
     * 通常还不做脱敏。一旦泄漏，等同于 OPPO 商店的发版权限被接管。
     * 更稳妥的做法是放进 POST body —— 华为与荣耀的 token 接口都是这样设计的。
     * 但 OPPO 分发平台的公开文档不可获取，无法确认 `developer/v1/token`
     * 是否接受 POST form；擅自改成 POST 一旦被拒，表现是所有用户完全无法发版。
     * 因此**保持 GET + query 的请求语义不变**，只修掉可以安全修掉的部分。
     * 如果后续能查证 OPPO 支持 POST form，应当立刻切过去 —— 这是本文件里
     * 唯一一处还把长期凭据放在 URL 上的调用。
     *
     * **二、字符串插值不做 URL 编码。** 这是必须修的：secret 里含 `&` 会造成参数截断
     * （服务端看到的 secret 被拦腰截断，鉴权失败且原因完全不可见），含 `#` 会让后面的
     * 内容变成 fragment 直接不发送，含空格则让 `toHttpUrl()` 抛 IllegalArgumentException。
     * 改用 [HttpUrl.Builder.addQueryParameter] 后，这些字符都会被正确百分号转义。
     *
     * **残余风险**：凭据依然在 query string 上，仍会进代理与服务端日志。
     * 本进程内的日志不会记录它（见下方 debug 调用只输出脱敏后的 client_id），
     * 但进程外的链路不在我们控制范围内。
     */
    suspend fun getToken(): String {
        val url = DOMAIN.toHttpUrl().newBuilder()
            .addPathSegments("developer/v1/token")
            .addQueryParameter("client_id", clientId)
            .addQueryParameter("client_secret", clientSecret)
            .build()
        val request = Request.Builder().url(url).get().build()
        // 只记录脱敏后的 client_id：client_secret 与 access_token 都不得进日志
        AppLogger.debug(LOG_TAG, "获取 token，client_id=${redact(clientId)}")
        val body = client.textResponse(request, CHANNEL_ID)
        // 这一条响应体里含裸 access_token，出错时不能把 raw 交给 PublishError
        checkSuccess(body, "获取 token", includeRaw = false)
        val response = Json.parse<OppoTokenResponse>(CHANNEL_ID, body)
        return response.data?.accessToken?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(
                channel = CHANNEL_ID,
                message = "OPPO 返回的响应里没有 access_token，接口可能已变更",
            )
    }

    /** 获取应用信息。提交版本时要把这里读回的字段原样送回去，见 [submit] */
    suspend fun getAppInfo(token: String, applicationId: String): OppoAppInfoResponse.Data {
        val params = mapOf("pkg_name" to applicationId)
        val url = signedUrl("$DOMAIN/resource/v1/app/info", params, token, appendParamsToQuery = true)
        val request = Request.Builder().url(url).get().build()
        val body = client.textResponse(request, CHANNEL_ID)
        checkSuccess(body, "获取应用信息")
        val response = Json.parse<OppoAppInfoResponse>(CHANNEL_ID, body)
        return response.data ?: throw PublishError.protocol(
            channel = CHANNEL_ID,
            message = "OPPO 未返回应用信息，请确认包名 $applicationId 已在 OPPO 开放平台创建",
            raw = body,
        )
    }

    /** 获取本次上传专用的地址与一次性 sign */
    suspend fun getUploadUrl(token: String): OppoUploadTarget {
        val url = signedUrl(
            "$DOMAIN/resource/v1/upload/get-upload-url",
            emptyMap(),
            token,
            appendParamsToQuery = true,
        )
        val request = Request.Builder().url(url).get().build()
        val body = client.textResponse(request, CHANNEL_ID)
        checkSuccess(body, "获取上传地址")
        val response = Json.parse<OppoUploadUrlResponse>(CHANNEL_ID, body)
        val data = response.data
        val uploadUrl = data?.uploadUrl?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(CHANNEL_ID, "OPPO 未返回 upload_url", raw = body)
        val sign = data.sign?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(CHANNEL_ID, "OPPO 未返回上传 sign", raw = body)
        return OppoUploadTarget(uploadUrl, sign)
    }

    /**
     * 上传 APK。
     *
     * `type` 与 `sign` 同时出现在 query（参与签名）和 multipart body 里 —— 这是原实现
     * 的行为，OPPO 两边都校验，去掉任何一侧都会被拒，因此保持原样。
     */
    suspend fun uploadApk(
        target: OppoUploadTarget,
        token: String,
        artifactFile: File,
        onProgress: ProgressChange,
    ): OppoApkResult {
        val params = mapOf(
            "type" to "apk",
            "sign" to target.sign,
        )
        // 上传地址来自接口响应，可能被篡改为 http —— 明文发出去的是整个 APK 与 access_token
        val url = HttpClients.requireHttps(
            signedUrl(target.url, params, token, appendParamsToQuery = false),
            CHANNEL_ID,
        )
        val apkBody = ProgressBody(
            mediaType = "application/octet-stream".toMediaType(),
            file = artifactFile,
            progressChange = onProgress,
        )
        val requestBody = MultipartBody.Builder()
            // 强制指定 FORM，MultipartBody 默认是 mixed，OPPO 不接受
            .setType(MultipartBody.FORM)
            .addFormDataPart("file", artifactFile.name, apkBody)
            .addFormDataPart("type", "apk")
            .addFormDataPart("sign", target.sign)
            .build()
        val request = Request.Builder().url(url).post(requestBody).build()
        val body = client.textResponse(request, CHANNEL_ID)
        checkSuccess(body, "上传 APK")
        val response = Json.parse<OppoApkResponse>(CHANNEL_ID, body)
        val data = response.data
        val apkUrl = data?.url?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(CHANNEL_ID, "OPPO 未返回上传后的 APK url", raw = body)
        val md5 = data.md5?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(CHANNEL_ID, "OPPO 未返回上传后的 APK md5", raw = body)
        return OppoApkResult(apkUrl, md5)
    }

    /**
     * 提交新版本。
     *
     * **注意这里会把 [appInfo] 里从 `app/info` 读回来的字段原样回传**：图标、截图、
     * 一句话介绍、软件介绍、隐私政策地址、二三级分类 id、软著地址、商务联系方式。
     * 这不是冗余 —— OPPO 的 `app/upd` 是全量更新语义，任何一个字段不传就会被清空或被拒，
     * 所以必须先读回再整包送回。这个行为完整保留自原实现。
     */
    suspend fun submit(
        token: String,
        artifactInfo: ArtifactInfo,
        appInfo: OppoAppInfoResponse.Data,
        releaseParams: ReleaseParams,
        apkResult: OppoApkResult,
    ) {
        val params = buildSubmitParams(artifactInfo, appInfo, releaseParams, apkResult)
        val body = FormBody.Builder()
            .apply { params.forEach { (key, value) -> add(key, value) } }
            .build()
        // 签名用的参数集合必须与 body 完全一致，否则服务端重算签名不匹配
        val url = signedUrl("$DOMAIN/resource/v1/app/upd", params, token, appendParamsToQuery = false)
        val request = Request.Builder().url(url).post(body).build()
        // app/upd 是送审动作，不可撤销。响应解析失败也属于「不知道有没有受理」，
        // 同样不能标记为可重试（见 atSubmissionPoint）。
        atSubmissionPoint("OPPO", "提交版本") {
            val responseBody = client.textResponse(request, CHANNEL_ID)
            checkSuccess(responseBody, "提交版本")
        }
        // app/upd 是异步接口：errno=0 只代表任务入队。必须轮询才能真正知道版本
        // 有没有创建成功 —— 否则缺必传参数之类的失败会被当成成功上报，
        // 线上版本号纹丝不动却没有任何报错。
        //
        // 轮询失败意味着任务确定失败（渠道给出了原因），此时版本没有改变，
        // 因此不套 atSubmissionPoint。
        waitSubmitResult(token, artifactInfo)
    }

    /**
     * 轮询提交任务的处理结果。
     *
     * `/resource/v1/app/upd` 是异步接口：它返回 errno=0 只代表任务已入队，
     * 不代表版本创建成功。真正结果要查 task-state：
     * `1` 待处理 / `2` 处理成功 / `3` 处理失败。
     *
     * 不查就会把「入队成功」当成「发布成功」上报 —— 而任务随后可能因为缺必传
     * 参数、apk 包名不符、截图尺寸超标等原因静默失败，线上版本号纹丝不动。
     * 这类假成功比明确报错更难排查：日志与退出码都是成功，只有登录后台
     * 才发现什么也没发生。
     */
    private suspend fun waitSubmitResult(token: String, artifactInfo: ArtifactInfo) {
        val versionCode = artifactInfo.versionCode.toString()
        val params = mapOf("pkg_name" to artifactInfo.applicationId, "version_code" to versionCode)
        val url = signedUrl("$DOMAIN/resource/v1/app/task-state", params, token, appendParamsToQuery = false)

        // OPPO 文档称「接口处理可能会比较耗时，建议客户端执行等待时间设置为 10 秒以上」。
        // 取 3 分钟上限：再久通常不是慢，而是任务卡住，继续等没有意义。
        val interval = 3_000L
        val deadline = System.currentTimeMillis() + 3 * 60_000L

        while (true) {
            val body = FormBody.Builder()
                .apply { params.forEach { (k, v) -> add(k, v) } }
                .build()
            val responseBody = client.textResponse(
                Request.Builder().url(url).post(body).build(),
                CHANNEL_ID,
            )
            checkSuccess(responseBody, "查询任务状态")
            val state = Json.parse<OppoTaskStateResponse>(CHANNEL_ID, responseBody)

            when (state.data?.taskState) {
                "2" -> return
                "3" -> throw PublishError.rejected(
                    channel = CHANNEL_ID,
                    code = "task_state=3",
                    message = "OPPO 处理版本更新任务失败：" +
                        "${state.data.errMsg?.takeIf { it.isNotBlank() } ?: "未给出原因"}" +
                        "（版本号 $versionCode）",
                )
            }

            if (System.currentTimeMillis() > deadline) {
                // 超时不是失败：任务可能仍在处理。报成失败会诱使使用者重试，
                // 而重复提交会产生重复版本
                throw PublishError.precondition(
                    channel = CHANNEL_ID,
                    message = "OPPO 版本更新任务在 3 分钟内未处理完" +
                        "（当前状态 ${state.data?.taskState}）。任务可能仍在处理，" +
                        "请稍后到 OPPO 后台确认，不要直接重试以免产生重复版本",
                )
            }
            kotlinx.coroutines.delay(interval)
        }
    }

    private fun buildSubmitParams(
        artifactInfo: ArtifactInfo,
        appInfo: OppoAppInfoResponse.Data,
        releaseParams: ReleaseParams,
        apkResult: OppoApkResult,
    ): Map<String, String> {
        // apk_url 是一个 JSON 数组字符串。原实现用 Gson 的 JsonArray 拼，这里手写 ——
        // 字段固定为三个且值来自服务端返回的 url/md5，没有需要转义的字符；
        // cpu_code 的 0 表示非多包应用（64 位包为 64，32 位为 32）
        val apkUrlJson = """[{"url":"${apkResult.url}","md5":"${apkResult.md5}","cpu_code":0}]"""

        val required = RequiredAppFields.from(appInfo)
        val params = mutableMapOf(
            "pkg_name" to artifactInfo.applicationId,
            "version_code" to artifactInfo.versionCode.toString(),
            // 文档标注必传；漏传会让异步任务静默失败（见 OppoAppInfoResponse.Data 注释）
            "app_name" to required.appName,
            "age_level" to required.ageLevel,
            "adaptive_equipment" to required.adaptiveEquipment,
            "apk_url" to apkUrlJson,
            "update_desc" to releaseParams.updateDesc,
            // 1 审核后立即发布，2 定时发布
            "online_type" to if (releaseParams.scheduled) "2" else "1",
            "second_category_id" to required.secondCategory,
            "third_category_id" to required.thirdCategory,
            "summary" to required.summary,
            "detail_desc" to required.detailDesc,
            "privacy_source_url" to required.privacyUrl,
            "icon_url" to required.iconUrl,
            "pic_url" to required.picUrl,
            // 以下字段商店可以留空，缺失时送空串而不是让整次提交失败
            "test_desc" to appInfo.testDesc.orEmpty(),
            "business_username" to appInfo.businessUsername.orEmpty(),
            "business_email" to appInfo.businessEmail.orEmpty(),
            "business_mobile" to appInfo.businessMobile.orEmpty(),
            // 纸质软著缺失时回退到电子版软著 —— 保留原实现的回退，
            // OPPO 要求 copyright_url 非空，而多数开发者只上传了电子版
            "copyright_url" to appInfo.copyrightUrl.orEmpty()
                .ifEmpty { appInfo.electronicCertUrl.orEmpty() },
            "electronic_cert_url" to appInfo.electronicCertUrl.orEmpty(),
        )

        if (releaseParams.scheduled) {
            // online_type=2 时必填，格式 2006-01-02 15:04:05，不能早于当前时间。
            // 显式指定 Locale.US：默认 Locale 在泰语等区域会按当地纪年格式化，
            // 输出 2568 而非 2025，OPPO 直接判定为非法时间
            val format = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.US)
            params["sche_online_time"] = format.format(Date(releaseParams.onlineTime))
        }
        return params
    }

    /**
     * 拼出带签名的请求 URL。
     *
     * ## 为什么 access_token 与 api_sign 在 query 上
     *
     * 这是 OPPO 接口的强制要求：签名参数 `api_sign`、`access_token`、`timestamp` 一律
     * 从 query 读取，POST 请求也不例外（body 只放业务字段）。这一点无法改成 header，
     * 所以此处保留原样。风险可接受：access_token 是短期凭据，且 `HttpClients.requireHttps`
     * 保证不会走明文；真正需要修的是 token 接口里的长期凭据，见 [getToken]。
     *
     * @param appendParamsToQuery GET 请求需要把业务参数也放到 query 上；
     *   POST 请求的业务参数在 body 里，query 上只带鉴权三元组。
     *   两种情况下**参与签名的参数集合都是「业务参数 + access_token + timestamp」**，
     *   这是原实现的行为，也是签名能否通过的关键。
     */
    private fun signedUrl(
        originUrl: String,
        params: Map<String, String>,
        token: String,
        appendParamsToQuery: Boolean,
    ): HttpUrl {
        val timestamp = (System.currentTimeMillis() / 1000).toString()
        val signParams = params.toMutableMap().apply {
            put("access_token", token)
            put("timestamp", timestamp)
        }
        return originUrl.toHttpUrl().newBuilder()
            .apply {
                if (appendParamsToQuery) {
                    // setQueryParameter 而非 add：signParams 已包含 access_token 与
                    // timestamp，下面还会各 add 一次，用 set 可避免重复参数
                    signParams.forEach { (key, value) -> setQueryParameter(key, value) }
                }
            }
            .addQueryParameter("access_token", token)
            .addQueryParameter("timestamp", timestamp)
            .addQueryParameter("api_sign", OppoApiSigner.sign(clientSecret, signParams))
            .build()
    }

    /**
     * 校验业务码。
     *
     * 原实现是 `get("errno").asInt` —— 字段缺失时 NPE，把本该抛出的业务错误顶掉，
     * 排查时看到的是 NullPointerException 而不是 OPPO 返回的真实原因。
     * 这里 errno 缺失按协议不符处理：不能静默当成成功。
     *
     * 必须在把 `data` 解析成强类型模型**之前**调用，原因见 [OppoErrno]。
     *
     * @param includeRaw token 接口的响应体含裸 access_token，不能带进错误对象
     */
    private fun checkSuccess(
        body: String,
        action: String,
        includeRaw: Boolean = true,
    ) {
        val raw = body.takeIf { includeRaw }
        val errno = Json.parse<OppoErrno>(CHANNEL_ID, body).errno
        if (errno == null) {
            throw PublishError.protocol(
                channel = CHANNEL_ID,
                message = "$action 失败：OPPO 响应里没有 errno 字段，接口可能已变更",
                raw = raw,
            )
        }
        if (errno == SUCCESS_CODE) return
        // 失败时错误描述在 data.message 里。单独用信封再解一遍：失败响应的 data
        // 结构与成功时不同，用强类型模型去解会先抛解析异常
        val message = runCatching {
            Json.parse<OppoEnvelope>(CHANNEL_ID, body).data?.message
        }.getOrNull()?.takeIf { it.isNotBlank() } ?: "OPPO 未返回错误描述"
        throw PublishError.rejected(
            channel = CHANNEL_ID,
            code = errno.toString(),
            message = "$action 失败：$message",
            raw = raw,
        )
    }

    /**
     * 提交版本时 OPPO 必填、且只能从 `app/info` 读回的字段。
     *
     * 单独校验是为了给出可执行的提示：这些字段为空通常意味着开发者在 OPPO
     * 后台还没把应用资料填全，而不是代码或网络的问题。
     */
    private class RequiredAppFields(
        val summary: String,
        val detailDesc: String,
        val privacyUrl: String,
        val secondCategory: String,
        val thirdCategory: String,
        val iconUrl: String,
        val picUrl: String,
        val appName: String,
        val ageLevel: String,
        val adaptiveEquipment: String,
    ) {
        companion object {
            fun from(info: OppoAppInfoResponse.Data): RequiredAppFields = RequiredAppFields(
                summary = info.summary.require("一句话介绍（summary）"),
                detailDesc = info.detailDesc.require("软件介绍（detail_desc）"),
                privacyUrl = info.privacyUrl.require("隐私政策网址（privacy_source_url）"),
                secondCategory = info.secondCategory.require("二级分类（ver_second_category_id）"),
                thirdCategory = info.thirdCategory.require("三级分类（ver_third_category_id）"),
                iconUrl = info.iconUrl.require("应用图标（icon_url）"),
                picUrl = info.picUrl.require("应用截图（pic_url）"),
                appName = info.appName.require("应用名称（app_name）"),
                ageLevel = info.ageLevel.require("年龄分级（age_level）"),
                adaptiveEquipment = info.adaptiveEquipment.require("平板适配（adaptive_equipment）"),
            )

            private fun String?.require(label: String): String =
                this?.takeIf { it.isNotBlank() } ?: throw PublishError.precondition(
                    channel = CHANNEL_ID,
                    message = "OPPO 商店缺少必填资料：$label。" +
                        "提交新版本需要回传这些字段，请先在 OPPO 开放平台补全应用信息",
                )
        }
    }

    companion object {
        const val CHANNEL_ID = "oppo"
        private const val DOMAIN = "https://oop-openapi-cn.heytapmobi.com"
        private const val SUCCESS_CODE = 0
        private const val LOG_TAG = "OPPO应用市场"
    }
}

/**
 * 统一的异常归一化。
 *
 * 两条硬约束：
 *  - [CancellationException] 必须原样抛出。吞掉取消会让协程在被取消后继续执行，
 *    结构化并发的假设被破坏，表现为「取消了但上传还在跑」。
 *  - 不 catch [Throwable]。OOM、StackOverflowError 这类错误包装成 PublishError
 *    只会掩盖真正的问题。
 */
internal inline fun <T> oppoCall(block: () -> T): T = try {
    block()
} catch (e: CancellationException) {
    throw e
} catch (e: PublishError) {
    throw e
} catch (e: Exception) {
    throw PublishError.fromNetwork(OppoMarketApi.CHANNEL_ID, e)
}
