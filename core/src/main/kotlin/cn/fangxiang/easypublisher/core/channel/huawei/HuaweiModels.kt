package cn.fangxiang.easypublisher.core.channel.huawei

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState
import com.squareup.moshi.Json

/**
 * 华为 AppGallery Connect 接口的请求 / 响应模型。
 *
 * 全部响应字段一律「可空 + 带默认值」，这是上游 issue #7 的根因修复：
 * 原实现把 `versionCode` / `versionNumber` 声明为非空无默认值，
 * 而应用只有一个尚未上传 APK 的草稿版本（releaseState=7）时华为压根不返回 versionCode，
 * Moshi 会在反射构造阶段抛 `JsonDataException: Required value 'versionCode' missing at $.appInfo`，
 * 用户连「查询市场状态」都做不了。
 * 解析层不再承担校验职责，缺字段由业务层用中文消息显式判断。
 *
 * 请求模型（[HWTokenParams] / [HWRefreshApk] / [HWVersionDesc]）仍保持非空 ——
 * 它们由本地代码构造，缺值是编程错误而不是渠道行为。
 *
 * 这里没有使用 `@JsonClass(generateAdapter = ...)`：本项目未接 moshi 的注解处理器，
 * 统一走 `Json.moshi` 里的 KotlinJsonAdapterFactory 反射适配器。
 */

/** 通用业务返回码。华为把它放在每个响应的 `ret` 字段里 */
data class HWResult(
    @Json(name = "code")
    val code: Int? = null,
    @Json(name = "msg")
    val msg: String? = null,
)

/** 只关心业务返回码的响应 */
data class HWResp(
    @Json(name = "ret")
    val result: HWResult? = null,
)

data class HWTokenParams(
    @Json(name = "client_id")
    val clientId: String,
    @Json(name = "client_secret")
    val clientSecret: String,
    @Json(name = "grant_type")
    val type: String = "client_credentials",
) {
    /** 绝不让密钥随 toString() 进日志或异常消息 */
    override fun toString(): String = "HWTokenParams(clientId=<hidden>, type=$type)"
}

data class HWTokenResp(
    @Json(name = "access_token")
    val token: String? = null,
    /** 取 token 成功时华为不返回 ret，所以这里天然可空，判空逻辑与原实现一致 */
    @Json(name = "ret")
    val result: HWResult? = null,
) {
    override fun toString(): String = "HWTokenResp(token=<hidden>, result=$result)"
}

data class HWAppIdResp(
    @Json(name = "ret")
    val result: HWResult? = null,
    @Suppress("SpellCheckingInspection")
    @Json(name = "appids")
    val list: List<AppId>? = null,
) {
    data class AppId(
        @Json(name = "key")
        val name: String? = null,
        @Json(name = "value")
        val id: String? = null,
    )
}

data class HWAppInfoResp(
    @Json(name = "ret")
    val result: HWResult? = null,
    @Json(name = "appInfo")
    val appInfo: AppInfo? = null,
) {
    data class AppInfo(
        /**
         * 应用状态。
         *
         * 0：已上架
         * 1：上架审核不通过
         * 2：已下架（含强制下架）
         * 3：待上架，预约上架
         * 4：审核中
         * 5：升级中
         * 6：申请下架
         * 7：草稿
         * 8：升级审核不通过
         * 9：下架审核不通过
         * 10：应用被开发者下架
         * 11：撤销上架
         */
        @Json(name = "releaseState")
        val releaseState: Int? = null,
        @Json(name = "versionCode")
        val versionCode: Long? = null,
        @Json(name = "versionNumber")
        val versionNumber: String? = null,
        // 原实现还声明了 onShelfVersionNumber（在架版本号），但全项目无人读取，
        // 却因为非空声明成为一个额外的解析失败点，直接删掉。
    ) {

        /**
         * 映射到渠道无关的状态。
         *
         * 与原实现的两处差异：
         *  - 补齐草稿态（7）与下架态（2/6/10）。原实现只认 0/4/5/8，
         *    草稿应用一律显示「状态未知」，用户无从判断能不能提交。
         *  - 版本信息缺失时 lastVersion 传 null（[MarketInfo.lastVersion] 本就可空），
         *    而不是拿默认值伪造一个版本号，也不抛异常。
         */
        fun toMarketInfo(channelId: String): MarketInfo {
            val reviewState = when (releaseState) {
                0 -> ReviewState.Online
                4, 5 -> ReviewState.UnderReview
                8 -> ReviewState.Rejected
                7 -> ReviewState.Draft
                2, 6, 10 -> ReviewState.Offline
                else -> ReviewState.Unknown
            }
            val version = if (versionCode != null && !versionNumber.isNullOrBlank()) {
                MarketInfo.Version(versionCode, versionNumber)
            } else {
                null
            }
            return MarketInfo(
                channelId = channelId,
                reviewState = reviewState,
                lastVersion = version,
                // 保留原始值，便于排查华为新增状态码
                rawState = releaseState?.toString(),
            )
        }
    }
}

data class HWUploadUrlResp(
    @Json(name = "ret")
    val result: HWResult? = null,
    @Json(name = "urlInfo")
    val url: UploadUrl? = null,
) {
    data class UploadUrl(
        @Json(name = "url")
        val url: String? = null,
        @Json(name = "objectId")
        val objectId: String? = null,
        /** 华为要求上传请求原样带上这些头（含签名），不可增删 */
        @Json(name = "headers")
        val headers: Map<String, String>? = null,
    )
}

data class HWRefreshApk(
    // 5 = APK 文件类型，华为接口定义的固定值
    @Json(name = "fileType")
    val fileType: Int = 5,
    @Json(name = "files")
    val files: List<FileInfo>,
) {
    data class FileInfo(
        @Json(name = "fileName")
        val fileName: String,
        @Json(name = "fileDestUrl")
        val fileDestUrl: String,
    )
}

data class HWBindFileResp(
    @Json(name = "ret")
    val result: HWResult? = null,
    @Json(name = "pkgVersion")
    val pkgVersion: List<String>? = null,
) {
    /** 绑定成功后用于查询编译状态的包 id；缺失时由业务层给出中文提示 */
    fun requirePkgId(channelId: String): String =
        pkgVersion?.firstOrNull()?.takeIf { it.isNotBlank() }
            ?: throw PublishError.protocol(
                channel = channelId,
                message = "绑定 APK 成功但未返回 pkgVersion，无法查询编译状态",
            )
}

data class HWApkState(
    @Json(name = "ret")
    val result: HWResult? = null,
    @Json(name = "pkgStateList")
    val pkgStateList: List<PackageState>? = null,
) {
    data class PackageState(
        @Json(name = "pkgId")
        val pkgId: String? = null,
        @Json(name = "successStatus")
        val successStatus: Int? = null,
    ) {
        fun isSuccess(): Boolean = successStatus == 0
    }
}

data class HWVersionDesc(
    @Json(name = "newFeatures")
    val desc: String,
    @Json(name = "lang")
    val language: String = "zh-CN",
)
