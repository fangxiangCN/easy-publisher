package cn.fangxiang.easypublisher.core.channel.oppo

import com.squareup.moshi.Json as JsonField

/**
 * OPPO 接口响应模型。
 *
 * **所有字段一律可空。** 原实现用 Gson 在构造函数里链式取值（`obj.get("icon_url").asString`），
 * 有两个后果：
 *  1. key 缺失时 `get()` 返回 null，`.asString` 立刻 NPE；值是 JSON null 时抛
 *     UnsupportedOperationException。两者都让人误以为是网络问题。
 *  2. 十几个字段挤在同一个构造函数里，任何一个缺失都导致整个对象构造失败 ——
 *     即使该字段（如 copyright_url）这次业务上根本用不到。
 *
 * 原文件里 `test_desc` / `business_username` / `copyright_url` 等已经写成了
 * `get("x")?.asString ?: ""` 的安全形式，而 `icon_url` / `pic_url` / 分类 id 仍是裸取值，
 * 风格不统一 —— 说明这是历史上被线上问题逐个打的补丁，而不是有意的设计。
 * 这里统一成可空模型，是否必需由业务层显式校验并给出可执行的中文提示。
 */

/**
 * 通用响应信封。
 *
 * OPPO 用 `errno` 表示业务码（0 为成功），错误描述放在 `data.message` 里。
 * 这里单独解析一层信封，是为了在把 `data` 当作强类型对象解析之前先判业务码 ——
 * 否则失败响应里 `data` 的结构与成功时不同，会先抛解析异常，把真正的业务错误顶掉。
 */
internal data class OppoEnvelope(
    val errno: Int? = null,
    val data: Message? = null,
) {
    data class Message(val message: String? = null)
}

/**
 * 只取业务码的最小模型。
 *
 * 必须先用它判码、再去解强类型的 `data` —— OPPO 失败响应里 `data` 的形状与成功时不同
 * （可能是数组，也可能直接是字符串），若先按成功结构解析，抛出的会是解析异常，
 * 真正的业务错误码和错误描述都被吞掉。
 */
internal data class OppoErrno(val errno: Int? = null)

internal data class OppoTokenResponse(
    val errno: Int? = null,
    val data: Data? = null,
) {
    data class Data(
        @JsonField(name = "access_token") val accessToken: String? = null,
    )
}

/**
 * 应用信息。
 *
 * 字段名保持与接口一致的 snake_case 映射，便于和 OPPO 文档对照。
 */
internal data class OppoAppInfoResponse(
    val errno: Int? = null,
    val data: Data? = null,
) {
    data class Data(
        /** 一句话介绍 */
        val summary: String? = null,
        /** 软件介绍 */
        @JsonField(name = "detail_desc") val detailDesc: String? = null,
        @JsonField(name = "version_code") val versionCode: Long? = null,
        @JsonField(name = "version_name") val versionName: String? = null,
        /** 审核状态，取值见 [OppoAuditStatus] */
        @JsonField(name = "audit_status") val auditStatus: Int? = null,
        /** 隐私政策网址 */
        @JsonField(name = "privacy_source_url") val privacyUrl: String? = null,
        /** 二级分类 id */
        @JsonField(name = "ver_second_category_id") val secondCategory: String? = null,
        /** 三级分类 id */
        @JsonField(name = "ver_third_category_id") val thirdCategory: String? = null,
        @JsonField(name = "icon_url") val iconUrl: String? = null,
        @JsonField(name = "pic_url") val picUrl: String? = null,
        /** 测试附加说明 */
        @JsonField(name = "test_desc") val testDesc: String? = null,
        /** 商务联系方式 */
        @JsonField(name = "business_username") val businessUsername: String? = null,
        @JsonField(name = "business_email") val businessEmail: String? = null,
        @JsonField(name = "business_mobile") val businessMobile: String? = null,
        /** 纸质版软著 */
        @JsonField(name = "copyright_url") val copyrightUrl: String? = null,
        /** 电子版软著 */
        @JsonField(name = "electronic_cert_url") val electronicCertUrl: String? = null,
    )
}

internal data class OppoUploadUrlResponse(
    val errno: Int? = null,
    val data: Data? = null,
) {
    data class Data(
        @JsonField(name = "upload_url") val uploadUrl: String? = null,
        val sign: String? = null,
    )
}

internal data class OppoApkResponse(
    val errno: Int? = null,
    val data: Data? = null,
) {
    data class Data(
        val url: String? = null,
        val md5: String? = null,
    )
}

/** 上传地址与其配套的一次性签名 */
internal data class OppoUploadTarget(val url: String, val sign: String)

/** 上传完成后服务端返回的 APK 定位信息，提交版本时要原样回传 */
internal data class OppoApkResult(val url: String, val md5: String)

/**
 * OPPO 的审核状态码。
 *
 * 只有 111（已上架）与 444（审核被拒）是原实现明确识别的，其余一律归为审核中。
 * 这里保留同样的映射，但把「未返回状态」单独区分出来 —— 原实现中
 * `audit_status` 缺失会 NPE，而 NPE 和"审核中"在排查时是两件完全不同的事。
 */
internal object OppoAuditStatus {
    const val ONLINE = 111
    const val REJECTED = 444
}
