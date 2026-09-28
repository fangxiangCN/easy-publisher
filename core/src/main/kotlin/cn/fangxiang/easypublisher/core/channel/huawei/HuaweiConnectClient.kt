package cn.fangxiang.easypublisher.core.channel.huawei

import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.atSubmissionPoint
import cn.fangxiang.easypublisher.core.ApkInfo
import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.net.ProgressBody
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.executeChecked
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import okhttp3.Headers
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

/**
 * 华为应用市场的发布流程实现。
 *
 * 无状态：凭据、超时、进度回调全部随调用传入，实例可并发复用。
 * 原实现把 clientId / clientSecret 存在渠道单例的可变字段里，并发上传会串台。
 */
internal class HuaweiConnectClient(private val channelId: String) {

    /**
     * 完整提交一个新版本。
     *
     * 步骤顺序与原实现严格一致：
     * token → appId → 上传地址 → 上传文件 → 绑定文件 → 等待编译 → 更新描述 → 提交审核。
     * 华为的后续接口依赖前一步的产物，顺序不能调整。
     */
    suspend fun uploadApk(
        file: File,
        apkInfo: ApkInfo,
        clientId: String,
        clientSecret: String,
        releaseParams: ReleaseParams,
        timeouts: HttpTimeouts,
        progressChange: ProgressChange,
        stopAfter: ReleaseStage = ReleaseStage.SubmitReview,
    ): ReleaseStage {
        val client = HttpClients.of(timeouts)
        val api = huaweiConnectApi(client)
        val token = "Bearer ${getToken(api, clientId, clientSecret)}"
        val appId = getAppId(api, clientId, token, apkInfo.applicationId)
        val uploadUrl = getUploadUrl(api, clientId, token, appId, file)
        uploadFile(client, file, uploadUrl, progressChange)
        if (stopAfter == ReleaseStage.UploadArtifact) {
            // 文件已在华为的对象存储里，但尚未绑定到应用，不产生任何版本
            AppLogger.info(LOG_TAG, "已按请求停在「仅上传安装包」：未绑定文件、未创建版本")
            return ReleaseStage.UploadArtifact
        }
        val pkgId = bindApk(api, clientId, token, appId, file, uploadUrl)
        waitApkReady(api, clientId, token, appId, pkgId)
        modifyUpdateDesc(api, clientId, token, appId, releaseParams.updateDesc)
        if (stopAfter == ReleaseStage.CreateDraft) {
            // 此时草稿已完整：文件已绑定、编译检查已通过、更新描述已写入。
            // 停在这里的价值是让人先到 AppGallery Connect 后台核对，再决定是否送审 ——
            // 送审不可撤销，而草稿可以随时改。
            AppLogger.info(
                LOG_TAG,
                "草稿已就绪（文件已绑定并通过编译检查），未送审。" +
                    "可到 AppGallery Connect 后台核对后再执行送审",
            )
            return ReleaseStage.CreateDraft
        }
        submit(api, clientId, token, appId, releaseParams.onlineTime)
        return ReleaseStage.SubmitReview
    }

    /**
     * 查询应用在华为市场的状态。
     */
    suspend fun getMarketInfo(
        clientId: String,
        clientSecret: String,
        applicationId: String,
        timeouts: HttpTimeouts,
    ): MarketInfo {
        val api = huaweiConnectApi(HttpClients.of(timeouts))
        val token = "Bearer ${getToken(api, clientId, clientSecret)}"
        val appId = getAppId(api, clientId, token, applicationId)
        val resp = step("获取App信息") { api.getAppInfo(clientId, token, appId) }
        resp.result.throwOnFail(channelId, "获取App信息")
        val appInfo = resp.appInfo ?: throw PublishError.protocol(
            channel = channelId,
            message = "华为未返回 appInfo，无法判断应用状态",
        )
        // 只打印映射后的结果，不含任何凭据
        val info = appInfo.toMarketInfo(channelId)
        AppLogger.info(LOG_TAG, "应用市场状态:$info")
        return info
    }

    /**
     * 获取 token。
     *
     * 与原实现的差别：不打印返回值。原项目的 `AppLogger.action()` 统一 `debug("结果:$result")`，
     * 而这一步的返回值就是裸 access_token，等于把凭据写进日志文件。
     */
    private suspend fun getToken(
        api: HuaweiConnectApi,
        clientId: String,
        clientSecret: String,
    ): String {
        val result = step("获取token") { api.getToken(HWTokenParams(clientId, clientSecret)) }
        // 华为成功时不返回 ret，与原实现一致：只有 ret 存在时才校验
        result.result.throwOnFailIfPresent(channelId, "获取token")
        val token = result.token?.takeIf { it.isNotBlank() }
            ?: throw PublishError.credential(
                "华为未返回 access_token，请检查 client_id 与 client_secret 是否正确",
                channelId,
            )
        AppLogger.debug(LOG_TAG, "获取token 成功: ${redact(token)}")
        return token
    }

    private suspend fun getAppId(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        applicationId: String,
    ): String {
        val result = step("获取AppId") { api.getAppId(clientId, token, applicationId) }
        result.result.throwOnFail(channelId, "获取AppId")
        val appId = result.list?.firstOrNull()?.id?.takeIf { it.isNotBlank() }
            ?: throw PublishError.precondition(
                "华为账号下未找到包名 $applicationId 对应的应用，请确认应用已在 AppGallery Connect 创建",
                channelId,
            )
        return appId
    }

    private suspend fun getUploadUrl(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        appId: String,
        file: File,
    ): HWUploadUrlResp.UploadUrl {
        val result = step("获取Apk上传地址") {
            api.getUploadUrl(clientId, token, appId, file.name, file.length())
        }
        result.result.throwOnFail(channelId, "获取Apk上传地址")
        return result.url ?: throw PublishError.protocol(
            channel = channelId,
            message = "华为未返回上传地址（urlInfo）",
        )
    }

    /**
     * 上传 APK 到华为返回的 OBS 地址。
     *
     * 三处修正：
     *  - 用 [executeChecked]，响应一定被关闭。原实现 `client.newCall(request).execute()`
     *    没有 `.use`，每次上传泄漏一个连接与响应体。
     *  - 上传前校验地址是 https。地址来自接口响应，OkHttp 本身接受 http://，
     *    若响应被篡改，APK 与签名头会明文发出。
     *  - 失败消息带上状态码与响应体（由 executeChecked 负责），原实现只有 "http error 4xx"。
     *
     * 请求语义保持不变：PUT、`application/octet-stream`、原样透传华为下发的 headers。
     */
    private suspend fun uploadFile(
        client: OkHttpClient,
        file: File,
        uploadUrl: HWUploadUrlResp.UploadUrl,
        progressChange: ProgressChange,
    ) {
        val rawUrl = uploadUrl.url?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(
                channel = channelId,
                message = "华为返回的上传地址为空",
            )
        val parsed = rawUrl.toHttpUrlOrNull() ?: throw PublishError.protocol(
            channel = channelId,
            message = "华为返回的上传地址无法解析",
        )
        val url = HttpClients.requireHttps(parsed, channelId)

        val headers = Headers.Builder()
        uploadUrl.headers?.forEach { (k, v) -> headers.add(k, v) }
        val body = ProgressBody("application/octet-stream".toMediaType(), file, progressChange)
        val request = Request.Builder()
            .url(url)
            .headers(headers.build())
            .put(body)
            .build()
        step("上传Apk文件") { client.executeChecked(request, channelId) }
    }

    private suspend fun bindApk(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        appId: String,
        file: File,
        uploadUrl: HWUploadUrlResp.UploadUrl,
    ): String {
        val objectId = uploadUrl.objectId ?: throw PublishError.protocol(
            channel = channelId,
            message = "华为未返回 objectId，无法绑定已上传的 APK",
        )
        val params = HWRefreshApk(files = listOf(HWRefreshApk.FileInfo(file.name, objectId)))
        val result = step("绑定Apk文件") { api.bindApkFile(clientId, token, appId, params) }
        result.result.throwOnFail(channelId, "绑定Apk文件")
        return result.requirePkgId(channelId)
    }

    /**
     * 轮询等待华为完成 APK 编译。
     *
     * 相对原实现的两处修正：
     *  - 先查再等。原实现 `while(true) { delay(10.seconds); ... }` 把等待放在检查之前，
     *    并且超时判断也在 delay 之后，于是首次检查必然先空等 10 秒。
     *  - `pkgStateList.firstOrNull()`。原实现用 `first()`，华为返回空数组时
     *    抛 NoSuchElementException，用户看到的是毫无信息量的堆栈。
     */
    private suspend fun waitApkReady(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        appId: String,
        pkgId: String,
    ) {
        val startTime = System.currentTimeMillis()
        val timeoutMillis = COMPILE_TIMEOUT.inWholeMilliseconds
        while (true) {
            val result = step("检测Apk编译状态") {
                api.getApkCompileState(clientId, token, appId, pkgId)
            }
            result.result.throwOnFail(channelId, "检测Apk编译状态")
            val state = result.pkgStateList?.firstOrNull()
            if (state == null) {
                AppLogger.debug(LOG_TAG, "华为暂未返回编译状态（pkgStateList 为空），继续等待")
            } else if (state.isSuccess()) {
                return
            }
            if (System.currentTimeMillis() - startTime >= timeoutMillis) {
                throw PublishError(
                    kind = ErrorKind.Network,
                    channel = channelId,
                    message = "等待华为完成 APK 编译超时（超过 ${COMPILE_TIMEOUT.inWholeMinutes} 分钟），" +
                        "可稍后在 AppGallery Connect 后台确认包状态",
                )
            }
            delay(POLL_INTERVAL)
        }
    }

    private suspend fun modifyUpdateDesc(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        appId: String,
        updateDesc: String,
    ) {
        val result = step("修改新版本更新描述") {
            api.updateVersionDesc(clientId, token, appId, HWVersionDesc(updateDesc))
        }
        result.result.throwOnFail(channelId, "修改新版本更新描述")
    }

    private suspend fun submit(
        api: HuaweiConnectApi,
        clientId: String,
        token: String,
        appId: String,
        onlineTime: Long,
    ) {
        // 格式与原实现逐字一致（yyyy-MM-dd'T'HH:mm:ssZZ，形如 2015-01-01T01:01:01+0800）。
        // 仅补上 Locale.US：pattern 里没有本地化文本字段，但避免在阿拉伯语等 locale 下
        // 输出非 ASCII 数字导致华为解析失败。
        val time = if (onlineTime > 0) {
            SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ssZZ", Locale.US).format(Date(onlineTime))
        } else {
            null
        }
        // 送审是整条链路里唯一不可撤销的动作，之后的任何失败都不能盲目重试：
        // 服务端可能已受理，只是响应在回程丢失。见 atSubmissionPoint 的说明。
        atSubmissionPoint("华为", "提交审核") {
            val result = step("提交审核") { api.submit(clientId, token, appId, time) }
            result.result.throwOnFail(channelId, "提交审核")
        }
    }

    /**
     * 包裹单个步骤：记日志 + 把 Retrofit / OkHttp / Moshi 的异常归一成 [PublishError]。
     *
     * 关键点：先原样重抛 [CancellationException]，否则用户取消上传会被当成失败上报，
     * 并且协程的取消传播会被吞掉（原实现是 `catch (e: Throwable)`，两个问题都有）。
     * 不打印任何返回值 —— 返回值里可能就是 token。
     */
    private suspend fun <T> step(action: String, block: suspend () -> T): T {
        AppLogger.info(LOG_TAG, "$action 开始")
        try {
            val result = block()
            AppLogger.info(LOG_TAG, "$action 成功")
            return result
        } catch (e: CancellationException) {
            throw e
        } catch (e: PublishError) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw e
        } catch (e: retrofit2.HttpException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            val raw = runCatching { e.response()?.errorBody()?.string() }.getOrNull()
            throw PublishError.rejected(
                channel = channelId,
                code = e.code().toString(),
                message = "$action 失败：HTTP ${e.code()} ${e.message()}".trim(),
                raw = raw?.take(MAX_RAW_LENGTH),
            )
        } catch (e: com.squareup.moshi.JsonDataException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError.protocol(
                channel = channelId,
                message = "$action 失败：华为响应格式不符合预期，接口可能已变更（${e.message}）",
                cause = e,
            )
        } catch (e: java.io.IOException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError.fromNetwork(channelId, e)
        } catch (e: Exception) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError(
                kind = ErrorKind.Unknown,
                channel = channelId,
                message = "$action 失败：${e.message ?: e::class.simpleName}",
                cause = e,
            )
        }
    }

    private companion object {
        const val LOG_TAG = "华为应用市场Api"
        const val MAX_RAW_LENGTH = 2000

        /** 与原实现一致：最多等 3 分钟编译完成，每 10 秒查一次 */
        val COMPILE_TIMEOUT = 3.minutes
        val POLL_INTERVAL = 10.seconds
    }
}

/**
 * 业务码校验。华为约定 code=0 为成功。
 *
 * ret 缺失视为协议异常而不是成功 —— 除了取 token 那一步（华为确实不返回 ret），
 * 那里走 [throwOnFailIfPresent]。
 */
private fun HWResult?.throwOnFail(channelId: String, action: String) {
    if (this == null) {
        throw PublishError.protocol(
            channel = channelId,
            message = "$action 失败：华为响应缺少 ret 字段，接口可能已变更",
        )
    }
    throwOnFailIfPresent(channelId, action)
}

private fun HWResult?.throwOnFailIfPresent(channelId: String, action: String) {
    val result = this ?: return
    val code = result.code
    if (code != null && code != 0) {
        throw PublishError.rejected(
            channel = channelId,
            code = code.toString(),
            message = "$action 失败：${result.msg ?: "华为未返回错误描述"}",
        )
    }
}
