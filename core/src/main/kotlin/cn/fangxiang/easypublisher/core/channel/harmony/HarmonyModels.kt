package cn.fangxiang.easypublisher.core.channel.harmony


/**
 * 鸿蒙 App Pack 上传相关的请求/响应模型。
 *
 * 全部响应字段可空带默认值：华为系接口在不同状态下返回的字段集合并不一致，
 * 缺字段应当走到带中文说明的业务校验，而不是让 Moshi 抛 JsonDataException。
 */

/** 华为系接口通用的业务结果包装 */
data class HarmonyRet(
    val code: Int? = null,
    val msg: String? = null,
)

/**
 * `upload/multipart/init` 的响应。
 *
 * [nspPartMinSize] 是华为规定的分片大小，必须按它切片 —— 自己选一个「合理」的
 * 分片大小会导致 parts 接口返回的地址与实际分片不匹配。
 */
data class MultipartInitResp(
    val ret: HarmonyRet? = null,
    val objectId: String? = null,
    val nspUploadId: String? = null,
    val nspPartMinSize: Long? = null,
)

/** `upload/multipart/parts` 的请求体条目：每片的摘要与长度 */
data class PartDescriptor(
    val sha256: String,
    val length: Long,
)

/**
 * `upload/multipart/parts` 响应里每个分片的上传信息。
 *
 * [headers] 声明为 [Any] 而不是 Map：华为返回的可能是 JSON 对象，也可能是
 * 序列化后的字符串。用 Any 接住再归一化，避免类型不匹配把真实错误顶掉。
 *
 * 这些 URL 是短时效签名地址，**必须原样使用它给的请求头**，
 * 少一个或多一个都可能导致签名校验失败。
 */
data class PartUploadInfo(
    val url: String? = null,
    val method: String? = null,
    val headers: Any? = null,
    val partObjectId: String? = null,
)

data class MultipartPartsResp(
    val ret: HarmonyRet? = null,
    val uploadInfoMap: Map<String, PartUploadInfo>? = null,
)

/** `upload/multipart/compose` 的请求体条目：每片的对象 id 与上传返回的 ETag */
data class CompletedPart(
    val partObjectId: String,
    val etag: String,
)

data class ComposeResp(val ret: HarmonyRet? = null)

/** `api/publish/v3/app-package-info` 的请求体 */
data class AppPackageInfoReq(
    val fileName: String,
    val objectId: String,
)

/**
 * v3 关联草稿的响应。
 *
 * packageId 可能直接在顶层，也可能嵌在 data 里 —— 两种形状都要接住。
 */
data class AppPackageInfoResp(
    val ret: HarmonyRet? = null,
    val packageId: String? = null,
    val data: PackageIdHolder? = null,
) {
    fun resolvePackageId(): String? =
        packageId?.takeIf { it.isNotBlank() } ?: data?.packageId?.takeIf { it.isNotBlank() }
}

data class PackageIdHolder(val packageId: String? = null)

/**
 * `api/publish/v3/app-submit` 的请求体。
 *
 * ## 为什么是 body 而不是 query
 *
 * 华为的 v2 `app-submit` 把 releaseTime 放在 query 上（见华为渠道的实现），
 * 但 v3 系列接口（如 app-package-info）统一是「appId 走 query + 负载走 JSON body」。
 * 另外社区实测报错 `registeredIdType and registeredIdNumber can not be null`
 * 说明服务端确实在解析 body 字段。因此这里按 v3 的惯例用 body。
 *
 * ## releaseTime 的格式存在两种说法
 *
 * 官方 v2 文档与一个可用的 v3 封装实现都写 `yyyy-MM-dd'T'HH:mm:ssZZ`
 * （形如 2026-10-01T10:00:00+0800），而个别社区帖子说是毫秒时间戳。
 * 这里采用前者：两个较可靠的来源一致，且与本项目华为渠道已在用的格式相同。
 * 若定时发布报「时间格式有误」，应首先怀疑这一点。
 *
 * ## 不含 releaseType / releasePhase
 *
 * 不传即全网发布。分阶段发布需要额外的一组字段（比例、时间窗、国家分级），
 * 尚未验证，因此不猜测性地塞进去。
 */
data class HarmonySubmitReq(
    /** 提审备注。可空；若填写，华为要求长度 10-300 字 */
    val remark: String? = null,
    /** 定时上架时间；为 null 时 Moshi 省略该字段，等价于审核通过后立即上架 */
    val releaseTime: String? = null,
    /**
     * 主体登记信息。社区实测缺失会被服务端拒绝
     * （`registeredIdType and registeredIdNumber can not be null`），
     * 但并非所有应用都需要，因此做成可选配置：用户遇到该报错时再填。
     */
    val registeredIdType: Int? = null,
    val registeredIdNumber: String? = null,
)

data class HarmonySubmitResp(val ret: HarmonyRet? = null)

/**
 * 把 [PartUploadInfo.headers] 归一化成字符串键值对。
 *
 * 华为可能返回对象，也可能返回 JSON 字符串；两种都要能处理，
 * 否则会因为一个字段形状变化就让整次上传失败。
 */
internal fun normalizePartHeaders(value: Any?): Map<String, String> = when (value) {
    null -> emptyMap()
    is Map<*, *> -> value.entries.mapNotNull { (k, v) ->
        if (k == null || v == null) null else k.toString() to v.toString()
    }.toMap()

    is String -> runCatching {
        @Suppress("UNCHECKED_CAST")
        val parsed = cn.fangxiang.easypublisher.core.net.Json.moshi
            .adapter(Map::class.java)
            .fromJson(value) as? Map<String, Any?>
        parsed?.entries?.mapNotNull { (k, v) ->
            if (v == null) null else k to v.toString()
        }?.toMap() ?: emptyMap()
    }.getOrElse {
        throw cn.fangxiang.easypublisher.core.PublishError.protocol(
            channel = HARMONY_CHANNEL_ID,
            message = "华为返回的分片上传请求头不是合法 JSON，无法按原样转发",
            raw = value.take(500),
        )
    }

    else -> emptyMap()
}

/** 渠道标识，配置与错误信息里都用它 */
const val HARMONY_CHANNEL_ID = "harmony"

/** 分片请求体里的键名前缀。华为文档用 Swagger 占位符命名，必须逐字匹配 */
internal const val PART_KEY_PREFIX = "additionalProp"

/** init 接口的固定参数，取值与参照实现一致 */
internal const val INIT_CONTENT_TYPE = "application/octet-stream"
internal const val INIT_FILE_TYPE = 1
internal const val INIT_RELEASE_TYPE = 1

/** 华为未返回分片大小时的兜底值 */
internal const val FALLBACK_PART_SIZE = 5L * 1024 * 1024
