package cn.fangxiang.easypublisher.core.channel.honor

import cn.fangxiang.easypublisher.core.atSubmissionPoint
import cn.fangxiang.easypublisher.core.ApkInfo
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.net.Json
import cn.fangxiang.easypublisher.core.net.ProgressBody
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.RetrofitFactory
import cn.fangxiang.easypublisher.core.net.textResponse
import cn.fangxiang.easypublisher.core.util.Digest
import com.squareup.moshi.JsonDataException
import kotlinx.coroutines.CancellationException
import okhttp3.Headers
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.File
import java.io.IOException
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * 荣耀应用市场的发布流程。
 *
 * 这个类无状态：凭据、超时、进度回调全部走方法入参，因此可以被无状态的
 * [HonorChannel] 安全地并发复用（原实现把 clientId/clientSecret 存成可变字段，
 * 并发发布两个应用时会互相串台）。
 */
internal class HonorConnectClient(private val timeouts: HttpTimeouts) {

    private val httpClient: OkHttpClient = HttpClients.of(timeouts)

    private val api: HonorConnectApi =
        RetrofitFactory.create(HonorConnectApi.BASE_URL, httpClient)

    /**
     * 上传并提交新版本。
     *
     * 步骤顺序与原实现一致：取 token → 取 appId → 取语言信息 → 申请上传地址 →
     * 上传 → 绑定文件 → 改更新说明 → 提交审核。荣耀要求「先绑定文件再改语言信息」，
     * 顺序变动会导致提交的版本缺内容，故保持原样。
     */
    suspend fun uploadApk(
        file: File,
        apkInfo: ApkInfo,
        clientId: String,
        clientSecret: String,
        releaseParams: ReleaseParams,
        progressChange: ProgressChange,
    ) {
        AppLogger.info(LOG_TAG, "开始提交新版本：${apkInfo.applicationId} ${apkInfo.versionName}")
        val token = bearerToken(clientId, clientSecret)
        val appId = getAppId(token, apkInfo.applicationId)
        val languageInfo = getLanguageInfo(token, appId)
        val uploadUrl = getUploadUrl(token, appId, file)
        uploadFile(file, token, uploadUrl, progressChange)
        bindUploadedApk(token, appId, uploadUrl)
        modifyUpdateDesc(token, appId, releaseParams.updateDesc, languageInfo)
        submit(token, appId, releaseParams.onlineTime)
        AppLogger.info(LOG_TAG, "新版本已提交审核：${apkInfo.applicationId}")
    }

    /** 查询审核状态 */
    suspend fun getReviewState(
        clientId: String,
        clientSecret: String,
        applicationId: String,
    ): HonorReviewState {
        val token = bearerToken(clientId, clientSecret)
        val appId = getAppId(token, applicationId)
        val result = call("获取审核状态") { api.getReviewState(token, appId) }
        return result.requireData("获取审核状态")
    }

    /**
     * 取 token 并拼成 Authorization 头的值。
     *
     * 只记录脱敏后的片段：日志文件长期留存，明文 access_token 等同于凭据泄露。
     */
    private suspend fun bearerToken(clientId: String, clientSecret: String): String {
        val resp = call("获取token") { api.getToken(clientId, clientSecret) }
        val raw = resp.token?.takeIf { it.isNotBlank() } ?: throw PublishError.credential(
            message = "荣耀未返回 access_token，请检查 client_id 与 client_secret 是否正确",
            channel = HONOR_CHANNEL_ID,
        )
        AppLogger.debug(LOG_TAG, "获取token成功：${redact(raw)}")
        return "Bearer $raw"
    }

    private suspend fun getAppId(token: String, applicationId: String): String {
        val result = call("获取AppId") { api.getAppId(token, applicationId) }
        val appIds = result.requireData("获取AppId")
        val appId = appIds.firstOrNull()?.appId?.takeIf { it.isNotBlank() }
        return appId ?: throw PublishError.precondition(
            message = "荣耀应用市场中找不到包名 $applicationId 对应的应用，请确认应用已创建且账号有权限",
            channel = HONOR_CHANNEL_ID,
        )
    }

    /**
     * 取第一条语言信息，用于改更新说明时回填 appName/intro。
     *
     * 原实现是 `.languageInfo.first()`：字段缺失抛 JsonDataException，空数组抛
     * NoSuchElementException，两种情况用户都只看到一句英文异常。
     */
    private suspend fun getLanguageInfo(token: String, appId: String): HonorAppInfo.LanguageInfo {
        val result = call("获取App信息") { api.getAppInfo(token, appId) }
        val appInfo = result.requireData("获取App信息")
        return appInfo.languageInfo?.firstOrNull() ?: throw PublishError.precondition(
            message = "荣耀未返回应用的语言信息（appId=$appId），请先在荣耀开发者后台补全应用介绍后再发版",
            channel = HONOR_CHANNEL_ID,
        )
    }

    /** 申请上传地址。fileType=100 是荣耀约定的 APK 安装包类型，保持原值 */
    private suspend fun getUploadUrl(token: String, appId: String, file: File): HonorUploadUrl {
        val uploadFile = HonorUploadFile(
            fileName = file.name,
            fileType = APK_FILE_TYPE,
            fileSize = file.length(),
            fileSha256 = Digest.sha256Hex(file),
        )
        val result = call("获取Apk上传地址") { api.getUploadUrl(token, appId, listOf(uploadFile)) }
        val urls = result.requireData("获取Apk上传地址")
        return urls.firstOrNull() ?: throw PublishError.protocol(
            channel = HONOR_CHANNEL_ID,
            message = "荣耀未返回 APK 上传地址，渠道接口可能已变更",
        )
    }

    /**
     * 上传 APK。
     *
     * 上传地址由接口下发，必须先校验是 https —— 否则一旦响应被篡改成 http，
     * APK 与 Authorization 头会明文发出。
     */
    private suspend fun uploadFile(
        file: File,
        token: String,
        uploadUrl: HonorUploadUrl,
        progressChange: ProgressChange,
    ) {
        val rawUrl = uploadUrl.url?.takeIf { it.isNotBlank() } ?: throw PublishError.protocol(
            channel = HONOR_CHANNEL_ID,
            message = "荣耀返回的上传地址为空",
        )
        val parsed = rawUrl.toHttpUrlOrNull() ?: throw PublishError.protocol(
            channel = HONOR_CHANNEL_ID,
            message = "荣耀返回的上传地址无法解析",
        )
        val url = HttpClients.requireHttps(parsed, HONOR_CHANNEL_ID)

        val headers = Headers.Builder()
            .add("Authorization", token)
            .build()
        val apkBody = ProgressBody(APK_MEDIA_TYPE.toMediaType(), file, progressChange)
        val body = MultipartBody.Builder()
            // 强制指定 FORM，MultipartBody 默认是 mixed，荣耀只接受 form-data
            .setType(MultipartBody.FORM)
            .addFormDataPart("file", file.name, apkBody)
            .build()
        val request = Request.Builder()
            .url(url)
            .headers(headers)
            .post(body)
            .build()

        // textResponse 内部 use{} 关闭响应，并在非 2xx 时带上状态码与响应体片段
        val responseBody = httpClient.textResponse(request, HONOR_CHANNEL_ID)
        // 上传接口返回裸 JSON，不是 HonorResult 包装体，同样以 code == 0 表示成功
        val ack = Json.parse<HonorUploadAck>(HONOR_CHANNEL_ID, responseBody)
        if (ack.code != 0) {
            throw PublishError.rejected(
                channel = HONOR_CHANNEL_ID,
                code = ack.code?.toString(),
                message = "上传APK文件失败：${ack.msg ?: "渠道未返回错误描述"}",
                raw = responseBody.take(MAX_RAW_LENGTH),
            )
        }
    }

    private suspend fun bindUploadedApk(token: String, appId: String, uploadUrl: HonorUploadUrl) {
        val objectId = uploadUrl.objectId ?: throw PublishError.protocol(
            channel = HONOR_CHANNEL_ID,
            message = "荣耀未返回上传文件的 objectId，无法绑定 APK",
        )
        val fileInfo = HonorBindApkFile(listOf(HonorBindApkFile.Item(objectId)))
        call("绑定已上传的apk文件") { api.bindApkFile(token, appId, fileInfo) }
            .throwOnFail("绑定已上传的apk文件")
    }

    /**
     * 改更新说明。
     *
     * appName / intro 必填，所以用线上语言信息原样回填；荣耀缺字段时用空串而不是
     * 中断流程 —— 后台已有应用必然有名称，真为空时由渠道自己报错更准确。
     */
    private suspend fun modifyUpdateDesc(
        token: String,
        appId: String,
        updateDesc: String,
        languageInfo: HonorAppInfo.LanguageInfo,
    ) {
        val newInfo = HonorVersionDesc.LanguageInfo(
            appName = languageInfo.appName.orEmpty(),
            intro = languageInfo.intro.orEmpty(),
            desc = updateDesc,
            briefIntro = languageInfo.briefIntro,
        )
        val desc = HonorVersionDesc(listOf(newInfo))
        call("修改新版本更新描述") { api.updateVersionDesc(token, appId, desc) }
            .throwOnFail("修改新版本更新描述")
    }

    private suspend fun submit(token: String, appId: String, onlineTime: Long) {
        val params = if (onlineTime > 0) {
            // 荣耀要求 yyyy-MM-dd'T'HH:mm:ssZZ。显式指定 Locale.US，
            // 否则在阿拉伯语等 locale 下会输出非 ASCII 数字导致渠道解析失败
            val format = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ssZZ", Locale.US)
            HonorSubmitParam(releaseType = 2, releaseTime = format.format(Date(onlineTime)))
        } else {
            HonorSubmitParam(releaseType = 1, releaseTime = null)
        }
        // 送审不可撤销，之后的失败一律不标记为可重试（见 atSubmissionPoint）
        atSubmissionPoint("荣耀", "提交审核") {
            call("提交审核") { api.submit(token, appId, params) }.throwOnFail("提交审核")
        }
    }

    /**
     * 统一包装 Retrofit 调用的异常。
     *
     * CancellationException 必须先原样抛出：它是协程取消的控制流，一旦被归一化成
     * PublishError，上层就会把用户主动取消当成发版失败，并继续执行后面的步骤。
     * 也不 catch Throwable —— OOM 之类的错误不该被当成渠道错误咽下。
     */
    private suspend fun <T> call(action: String, block: suspend () -> T): T {
        return try {
            block()
        } catch (e: CancellationException) {
            throw e
        } catch (e: PublishError) {
            throw e
        } catch (e: retrofit2.HttpException) {
            val raw = runCatching { e.response()?.errorBody()?.string() }.getOrNull()
            throw PublishError.rejected(
                channel = HONOR_CHANNEL_ID,
                code = e.code().toString(),
                message = "$action 失败：HTTP ${e.code()} ${e.message()}".trim(),
                raw = raw?.take(MAX_RAW_LENGTH),
            )
        } catch (e: JsonDataException) {
            throw PublishError.protocol(
                channel = HONOR_CHANNEL_ID,
                message = "$action 的响应无法解析，渠道接口可能已变更：${e.message}",
                cause = e,
            )
        } catch (e: IOException) {
            throw PublishError.fromNetwork(HONOR_CHANNEL_ID, e)
        }
    }

    private companion object {
        const val LOG_TAG = "荣耀应用市场Api"
        const val APK_FILE_TYPE = 100
        const val APK_MEDIA_TYPE = "application/vnd.android.package-archive"
        const val MAX_RAW_LENGTH = 2000
    }
}
