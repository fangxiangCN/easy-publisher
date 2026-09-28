package cn.fangxiang.easypublisher.core.channel.harmony

import cn.fangxiang.easypublisher.core.ArtifactInfo
import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.huawei.HWTokenParams
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.RetrofitFactory
import cn.fangxiang.easypublisher.core.util.Digest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import java.io.File
import java.io.RandomAccessFile
import java.util.Locale
import kotlin.math.ceil
import kotlin.math.min

/**
 * 鸿蒙 App Pack 的上传编排。
 *
 * ## 流程
 *
 * ```
 * token → multipart/init → 逐片算 sha256 → multipart/parts 换签名地址
 *       → 逐片 PUT 并收 ETag → multipart/compose → v3/app-package-info 关联草稿
 * ```
 *
 * 与普通 APK 上传的差别在于中间那一串：`.app` 包体较大且华为对鸿蒙包
 * 走的是 Upload Management API，必须先声明每片摘要换取地址，
 * 上传后还要回传 ETag 才能合并。
 *
 * ## 到草稿为止
 *
 * 最后一步只是把包关联到草稿版本，**不包含送审**。鸿蒙的送审接口未经验证，
 * 见 [HarmonyChannel] 的能力声明。
 */
internal class HarmonyPublishClient(private val timeouts: cn.fangxiang.easypublisher.core.net.HttpTimeouts) {

    private val httpClient = HttpClients.of(timeouts)

    private val api = RetrofitFactory.create<HarmonyPublishApi>(
        HarmonyPublishApi.BASE_URL,
        httpClient,
    )

    /**
     * 上传 App Pack 并关联到草稿。
     *
     * @param linkToDraft false 表示只把包传到华为文件服务，不关联草稿
     * @return 关联草稿时返回华为的 packageId；仅上传时返回 objectId
     */
    suspend fun uploadAppPack(
        artifactFile: File,
        artifactInfo: ArtifactInfo,
        clientId: String,
        clientSecret: String,
        appId: String,
        progressChange: ProgressChange,
        linkToDraft: Boolean = true,
    ): String {
        val token = "Bearer ${getToken(clientId, clientSecret)}"
        AppLogger.info(
            LOG_TAG,
            "开始上传 App Pack：${artifactInfo.applicationId} ${artifactInfo.versionName}" +
                "(${artifactInfo.versionCode})，${artifactFile.length() / 1024 / 1024} MB",
        )

        val init = call("初始化分片上传") {
            api.initMultipart(
                authorization = token,
                clientId = clientId,
                appId = appId,
                fileName = artifactFile.name,
                contentType = INIT_CONTENT_TYPE,
                fileType = INIT_FILE_TYPE,
                releaseType = INIT_RELEASE_TYPE,
            )
        }
        init.ret.throwOnFail("初始化分片上传")

        val objectId = init.objectId?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(HARMONY_CHANNEL_ID, "华为未返回 objectId，无法继续分片上传")
        val uploadId = init.nspUploadId?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(HARMONY_CHANNEL_ID, "华为未返回 nspUploadId，无法继续分片上传")
        // 分片大小必须用华为给的，自己选一个会导致 parts 返回的地址与实际分片不匹配
        val partSize = init.nspPartMinSize?.takeIf { it > 0 } ?: FALLBACK_PART_SIZE
        if (init.nspPartMinSize == null) {
            AppLogger.info(LOG_TAG, "华为未返回分片大小，使用兜底值 ${partSize / 1024 / 1024} MB")
        }

        val fileSize = artifactFile.length()
        val partCount = ceil(fileSize.toDouble() / partSize).toInt().coerceAtLeast(1)
        AppLogger.info(LOG_TAG, "分片大小 ${partSize / 1024} KB，共 $partCount 片")

        // 第一遍：算每片摘要。必须在换取上传地址之前完成
        val descriptors = computePartDigests(artifactFile, fileSize, partSize, partCount)

        val partsResp = call("获取分片上传地址") {
            api.getPartUploadInfo(token, clientId, objectId, uploadId, descriptors)
        }
        partsResp.ret.throwOnFail("获取分片上传地址")
        val uploadInfoMap = partsResp.uploadInfoMap
            ?: throw PublishError.protocol(HARMONY_CHANNEL_ID, "华为未返回 uploadInfoMap")

        // 第二遍：逐片上传并收集 ETag
        val completed = uploadParts(artifactFile, fileSize, partSize, partCount, uploadInfoMap, progressChange)

        val composeResp = call("合并分片") {
            api.compose(token, clientId, objectId, uploadId, completed)
        }
        composeResp.ret.throwOnFail("合并分片")
        AppLogger.info(LOG_TAG, "分片已合并，objectId=$objectId")

        if (!linkToDraft) {
            // 停在「仅上传」：文件已在华为的对象存储里，但没有关联到任何草稿版本，
            // AGC 后台看不到它。用途是验证凭据、appId 与包体是否被接受。
            AppLogger.info(LOG_TAG, "已按请求停在「仅上传」，未关联草稿，objectId=$objectId")
            return objectId
        }

        // 合并成功只代表文件进了华为的文件服务，还必须关联到草稿才会出现在后台
        val linkResp = call("关联 App Pack 到草稿") {
            api.updateAppPackageInfo(token, clientId, appId, AppPackageInfoReq(artifactFile.name, objectId))
        }
        linkResp.ret.throwOnFail("关联 App Pack 到草稿")
        val packageId = linkResp.resolvePackageId()
            ?: throw PublishError.protocol(
                HARMONY_CHANNEL_ID,
                "关联草稿成功但华为未返回 packageId，请到 AGC 后台确认草稿状态",
            )

        AppLogger.info(LOG_TAG, "App Pack 已关联到草稿，packageId=$packageId")
        return packageId
    }

    private suspend fun getToken(clientId: String, clientSecret: String): String {
        val resp = call("获取 token") { api.getToken(HWTokenParams(clientId, clientSecret)) }
        // 取 token 成功时华为不返回 ret，只判 access_token 是否存在
        return resp.token?.takeIf { it.isNotBlank() }
            ?: throw PublishError.credential(
                "华为未返回 access_token，请检查 client_id 与 client_secret 是否正确",
                channel = HARMONY_CHANNEL_ID,
            ).also {
                AppLogger.debug(LOG_TAG, "获取 token 失败，client_id=${redact(clientId)}")
            }
    }

    /**
     * 第一遍扫描：逐片计算 sha256 与长度。
     *
     * 内存占用上限是一个分片（华为规定的最小分片大小，通常 5MB），
     * 不会把整个包读进内存。
     */
    private fun computePartDigests(
        file: File,
        fileSize: Long,
        partSize: Long,
        partCount: Int,
    ): Map<String, PartDescriptor> = RandomAccessFile(file, "r").use { raf ->
        val descriptors = LinkedHashMap<String, PartDescriptor>(partCount)
        for (partNumber in 1..partCount) {
            val bytes = readPart(raf, partNumber, partSize, fileSize)
            descriptors["$PART_KEY_PREFIX$partNumber"] = PartDescriptor(
                sha256 = Digest.sha256Hex(bytes),
                length = bytes.size.toLong(),
            )
        }
        descriptors
    }

    /**
     * 第二遍扫描：逐片上传，收集华为返回的 ETag。
     */
    private suspend fun uploadParts(
        file: File,
        fileSize: Long,
        partSize: Long,
        partCount: Int,
        uploadInfoMap: Map<String, PartUploadInfo>,
        progressChange: ProgressChange,
    ): Map<String, CompletedPart> = RandomAccessFile(file, "r").use { raf ->
        val completed = LinkedHashMap<String, CompletedPart>(partCount)
        for (partNumber in 1..partCount) {
            val key = "$PART_KEY_PREFIX$partNumber"
            val info = uploadInfoMap[key]
                ?: throw PublishError.protocol(
                    HARMONY_CHANNEL_ID,
                    "华为没有为分片 $key 返回上传地址（共 $partCount 片，返回 ${uploadInfoMap.size} 条）",
                )
            val bytes = readPart(raf, partNumber, partSize, fileSize)
            val etag = uploadOnePart(key, info, bytes)
            completed[key] = CompletedPart(
                partObjectId = info.partObjectId.orEmpty(),
                etag = etag,
            )
            progressChange(partNumber.toFloat() / partCount)
            if (partNumber % PROGRESS_LOG_EVERY == 0 || partNumber == partCount) {
                AppLogger.info(LOG_TAG, "分片上传进度 $partNumber/$partCount")
            }
        }
        completed
    }

    private fun readPart(raf: RandomAccessFile, partNumber: Int, partSize: Long, fileSize: Long): ByteArray {
        val offset = (partNumber - 1) * partSize
        val length = min(partSize, fileSize - offset).toInt()
        if (length <= 0) return ByteArray(0)
        val buffer = ByteArray(length)
        raf.seek(offset)
        raf.readFully(buffer)
        return buffer
    }

    /**
     * 上传单个分片，返回响应里的 ETag。
     *
     * 三个要点：
     *  - 签名地址是短时效的，**必须原样转发华为给的请求头**，增删都可能导致签名校验失败
     *  - 强制 https，避免分片内容与签名头明文外泄
     *  - ETag 原样保留（含引号）回传给 compose，不做任何加工
     *
     * 这里用同步 execute 而非协程桥接：单个分片大小有上限（华为规定的分片大小），
     * 阻塞时间是有界的，不像整包上传那样可能持续数十分钟。
     */
    private suspend fun uploadOnePart(partKey: String, info: PartUploadInfo, bytes: ByteArray): String =
        withContext(Dispatchers.IO) {
            val url = info.url?.toHttpUrlOrNull()
                ?: throw PublishError.protocol(
                    HARMONY_CHANNEL_ID,
                    "分片 $partKey 的上传地址缺失或无法解析",
                )
            HttpClients.requireHttps(url, HARMONY_CHANNEL_ID)

            val method = (info.method?.uppercase(Locale.US) ?: "PUT").also {
                if (it != "PUT" && it != "POST") {
                    throw PublishError.protocol(
                        HARMONY_CHANNEL_ID,
                        "分片 $partKey 要求的上传方法 $it 不受支持",
                    )
                }
            }
            val request = Request.Builder()
                .url(url)
                .method(method, bytes.toRequestBody(null))
                .apply {
                    normalizePartHeaders(info.headers).forEach { (name, value) -> header(name, value) }
                }
                .build()

            try {
                httpClient.newCall(request).execute().use { response ->
                    if (!response.isSuccessful) {
                        throw PublishError(
                            kind = ErrorKind.ChannelRejected,
                            channel = HARMONY_CHANNEL_ID,
                            code = response.code.toString(),
                            message = "分片 $partKey 上传失败：HTTP ${response.code} ${response.message}".trim(),
                        )
                    }
                    response.header("ETag") ?: response.header("etag")
                        ?: throw PublishError.protocol(
                            HARMONY_CHANNEL_ID,
                            "分片 $partKey 的上传响应缺少 ETag，无法合并分片",
                        )
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: PublishError) {
                throw e
            } catch (e: Exception) {
                throw PublishError.fromNetwork(HARMONY_CHANNEL_ID, e)
            }
        }

    /**
     * 统一包装 Retrofit 调用的异常。
     *
     * CancellationException 必须原样重抛，否则用户取消会被当成失败上报。
     * 不打印任何返回值 —— token 就在里面。
     */
    private suspend fun <T> call(action: String, block: suspend () -> T): T {
        AppLogger.info(LOG_TAG, "$action 开始")
        return try {
            val result = block()
            AppLogger.info(LOG_TAG, "$action 成功")
            result
        } catch (e: CancellationException) {
            throw e
        } catch (e: PublishError) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw e
        } catch (e: retrofit2.HttpException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            val raw = runCatching { e.response()?.errorBody()?.string() }.getOrNull()
            throw PublishError.rejected(
                channel = HARMONY_CHANNEL_ID,
                code = e.code().toString(),
                message = "$action 失败：HTTP ${e.code()} ${e.message()}".trim(),
                raw = raw?.take(MAX_RAW_LENGTH),
            )
        } catch (e: com.squareup.moshi.JsonDataException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError.protocol(
                channel = HARMONY_CHANNEL_ID,
                message = "$action 失败：鸿蒙接口响应格式不符合预期（${e.message}）",
                cause = e,
            )
        } catch (e: java.io.IOException) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError.fromNetwork(HARMONY_CHANNEL_ID, e)
        } catch (e: Exception) {
            AppLogger.error(LOG_TAG, "$action 失败", e)
            throw PublishError(
                kind = ErrorKind.Unknown,
                channel = HARMONY_CHANNEL_ID,
                message = "$action 失败：${e.message ?: e::class.simpleName}",
                cause = e,
            )
        }
    }

    private fun HarmonyRet?.throwOnFail(action: String) {
        if (this == null) {
            throw PublishError.protocol(
                channel = HARMONY_CHANNEL_ID,
                message = "$action 失败：鸿蒙接口响应缺少 ret 字段，无法判定成败",
            )
        }
        val code = code
        if (code != null && code != 0) {
            throw PublishError.rejected(
                channel = HARMONY_CHANNEL_ID,
                code = code.toString(),
                message = "$action 失败：${msg ?: "华为未返回错误描述"}",
            )
        }
    }

    private companion object {
        const val LOG_TAG = "鸿蒙应用市场Api"
        const val MAX_RAW_LENGTH = 2000
        const val PROGRESS_LOG_EVERY = 10
    }
}
