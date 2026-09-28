package cn.fangxiang.easypublisher.core.channel.vivo

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState
import com.squareup.moshi.Json as JsonName

/**
 * vivo 接口响应的公共外层。
 *
 * [code] / [subCode] 声明为 [Any] 而不是 Int/String：vivo 在不同错误场景下这两个
 * 字段的 JSON 类型并不稳定（限流走数字、部分鉴权失败走字符串），而 Moshi 的类型
 * 适配器比原先 Gson 的 `asInt` 更严格 —— 类型对不上就抛解析异常，于是又会把真实
 * 错误码顶掉。用 Any 接住再自己归一化，目的是保证**无论渠道返回什么形状，
 * 都先把错误码原样交到用户手里**。
 *
 * 外层没有用泛型（`VivoResponse<T>`）：[cn.fangxiang.easypublisher.core.net.Json.parse]
 * 走的是 `T::class.java`，泛型参数会被擦除，Moshi 拿不到 data 的实际类型。
 * 因此每个接口各自声明一个具体的响应类。
 */
internal interface VivoEnvelope {
    val code: Any?
    val subCode: Any?
    val msg: String?
}

/**
 * 所有字段可空带默认值：错误响应常常只带其中一两个字段，缺字段不该导致构造失败。
 */
internal data class VivoAppInfoResponse(
    override val code: Any? = null,
    override val subCode: Any? = null,
    override val msg: String? = null,
    val data: VivoAppInfo? = null,
) : VivoEnvelope

internal data class VivoApkUploadResponse(
    override val code: Any? = null,
    override val subCode: Any? = null,
    override val msg: String? = null,
    val data: VivoApkResult? = null,
) : VivoEnvelope

/** 提交更新只关心成败，data 的结构不参与业务判断 */
internal data class VivoSubmitResponse(
    override val code: Any? = null,
    override val subCode: Any? = null,
    override val msg: String? = null,
) : VivoEnvelope

/**
 * APK 上传结果。
 *
 * 原实现在构造函数里链式取 `obj.get("x").asString`，任一 key 缺失即 NPE，
 * 且一个字段缺失整个对象就构造不出来。此处全部可空，校验推迟到业务层
 * （[requireUploadResult]），以便给出带字段名的中文提示。
 */
internal data class VivoApkResult(
    val packageName: String? = null,
    /** 流水号，提交更新时作为 apk 参数回传 */
    val serialnumber: String? = null,
    val versionCode: Long? = null,
    val versionName: String? = null,
    val fileMd5: String? = null,
)

/** 上传结果的校验视图，字段已确认非空 */
internal data class VivoUploadResult(
    val packageName: String,
    val serialnumber: String,
    val versionCode: Long,
    val fileMd5: String,
)

/**
 * 校验上传结果。缺字段说明接口协议变了，属于 ProtocolMismatch 而非业务拒绝。
 *
 * 只校验后续「提交更新」真正会用到的四个字段；versionName 不参与提交参数，
 * 缺失不影响流程，因此不作为硬性要求。
 */
internal fun VivoApkResult?.requireUploadResult(raw: String): VivoUploadResult {
    val result = this
    val missing = buildList {
        if (result?.packageName.isNullOrBlank()) add("packageName")
        if (result?.serialnumber.isNullOrBlank()) add("serialnumber")
        if (result?.versionCode == null) add("versionCode")
        if (result?.fileMd5.isNullOrBlank()) add("fileMd5")
    }
    if (result == null || missing.isNotEmpty()) {
        throw PublishError.protocol(
            channel = VivoChannel.ID,
            message = "vivo 上传接口响应缺少必要字段：" +
                missing.joinToString("、").ifEmpty { "data" },
            raw = raw.take(MAX_RAW),
        )
    }
    return VivoUploadResult(
        packageName = result.packageName!!,
        serialnumber = result.serialnumber!!,
        versionCode = result.versionCode!!,
        fileMd5 = result.fileMd5!!,
    )
}

/**
 * 应用详情。审核状态字段在 vivo 文档里叫 status，取值含义见 [toMarketInfo]。
 */
internal data class VivoAppInfo(
    @JsonName(name = "status") val reviewStatus: Int? = null,
    val versionCode: Long? = null,
    val versionName: String? = null,
) {
    /**
     * 版本信息缺失时返回 null 而不是塞占位值。
     *
     * 商店里只有尚未上传 APK 的草稿时，vivo 不返回版本号；原实现把 lastVersion
     * 声明为非空，这种情况下直接崩在解析阶段。
     */
    fun toMarketInfo(): MarketInfo {
        val state = when (reviewStatus) {
            1 -> ReviewState.Draft
            2 -> ReviewState.UnderReview
            3 -> ReviewState.Online
            4 -> ReviewState.Rejected
            else -> ReviewState.Unknown
        }
        val version = if (versionCode != null && !versionName.isNullOrBlank()) {
            MarketInfo.Version(versionCode, versionName)
        } else {
            null
        }
        return MarketInfo(
            channelId = VivoChannel.ID,
            reviewState = state,
            lastVersion = version,
            rawState = reviewStatus?.toString(),
        )
    }
}

/** 归一化后的错误码文本，null 表示渠道没返回该字段 */
internal val VivoEnvelope.codeText: String? get() = code.asScalarText()

internal val VivoEnvelope.subCodeText: String? get() = subCode.asScalarText()

/**
 * 把 Moshi 解出的标量（Double / String / Boolean）归一化成文本。
 *
 * Moshi 的 Any 适配器把所有 JSON 数字读成 Double，直接 toString 会得到
 * "20000.0" 这种错误码，因此整数值要还原成整型文本。
 */
private fun Any?.asScalarText(): String? = when (this) {
    null -> null
    is Double -> if (this % 1.0 == 0.0) toLong().toString() else toString()
    is Number -> toString()
    is String -> takeIf { it.isNotBlank() }
    else -> toString()
}

internal const val MAX_RAW = 2000
