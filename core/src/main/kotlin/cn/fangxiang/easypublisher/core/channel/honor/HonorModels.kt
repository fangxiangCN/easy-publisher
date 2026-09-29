package cn.fangxiang.easypublisher.core.channel.honor

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState
import com.squareup.moshi.Json

/**
 * 荣耀接口的请求/响应模型。
 *
 * 所有响应字段一律「可空 + 默认值」：渠道随时可能增删字段，用非空字段声明会让
 * Moshi 在字段缺失时抛 JsonDataException，错误信息里只有字段名，排查不到是哪一步失败。
 * 缺失的必要字段改为在业务层显式校验，抛带中文说明的 [PublishError]。
 *
 * 本项目未引入 Moshi codegen（kapt/ksp），解析走 KotlinJsonAdapterFactory 反射，
 * 因此这里不加 `@JsonClass(generateAdapter = true)`。
 */
data class HonorResult<T>(
    @Json(name = "code") val code: Int? = null,
    @Json(name = "msg") val msg: String? = null,
    @Json(name = "data") val data: T? = null,
) {
    /** 荣耀以 code == 0 表示成功 */
    val success: Boolean get() = code == 0

    fun throwOnFail(action: String) {
        if (!success) {
            throw PublishError.rejected(
                channel = HONOR_CHANNEL_ID,
                code = code?.toString(),
                message = "$action 失败：${msg ?: "渠道未返回错误描述"}",
            )
        }
    }

    /** data 为空时把动作名带进错误信息，避免只剩一句 "Required value missing" */
    fun requireData(action: String): T {
        throwOnFail(action)
        return data ?: throw PublishError.protocol(
            channel = HONOR_CHANNEL_ID,
            message = "$action 成功但未返回数据，渠道接口可能已变更",
        )
    }
}

data class HonorTokenResp(
    @Json(name = "access_token") val token: String? = null,
)

data class HonorAppId(
    @Json(name = "packageName") val packageName: String? = null,
    @Json(name = "appId") val appId: String? = null,
)

data class HonorAppInfo(
    @Json(name = "languageInfo") val languageInfo: List<LanguageInfo>? = null,
    @Json(name = "releaseInfo") val releaseInfo: PubReleaseInfo? = null,
    @Json(name = "basicInfo") val basicInfo: BasicInfo? = null,
) {
    /**
     * 应用基础信息。
     *
     * `ratingId` 是**年龄分级**（3+ / 8+ / 12+ 等）。它阻塞送审：未设置时
     * submit-audit 返回 `app rating id is empty (code=20046)`，而提示里既不说
     * 去哪设置、也不说是哪个字段 —— 实测只能靠比对本字段是否为 null 定位。
     *
     * 其余字段是 update-app-info 的必填项（该接口是全量更新语义，
     * 改一个字段也要把整份资料回传）。
     */
    data class BasicInfo(
        @Json(name = "appCategoryId") val appCategoryId: Int? = null,
        @Json(name = "appClassification") val appClassification: String? = null,
        @Json(name = "supplyName") val supplyName: String? = null,
        @Json(name = "supplyNameEn") val supplyNameEn: String? = null,
        @Json(name = "devName") val devName: String? = null,
        @Json(name = "devNameEn") val devNameEn: String? = null,
        @Json(name = "defaultLanguage") val defaultLanguage: String? = null,
        @Json(name = "releaseCountry") val releaseCountry: String? = null,
        @Json(name = "gameType") val gameType: Int? = null,
        @Json(name = "paymentInfo") val paymentInfo: Int? = null,
        @Json(name = "privacyPolicyUrl") val privacyPolicyUrl: String? = null,
        /** 为 null 表示尚未设置年龄分级，会导致送审被拒 */
        @Json(name = "ratingId") val ratingId: Int? = null,
    )

    data class LanguageInfo(
        @Json(name = "languageId") val languageId: String? = null,
        @Json(name = "appName") val appName: String? = null,
        @Json(name = "intro") val intro: String? = null,
        @Json(name = "briefIntro") val briefIntro: String? = null,
    )

    /** 线上版本信息 */
    data class PubReleaseInfo(
        @Json(name = "versionCode") val versionCode: Long? = null,
        @Json(name = "versionName") val versionName: String? = null,
    )
}

/** 提交审核前用于申请上传地址的文件描述 */
data class HonorUploadFile(
    @Json(name = "fileName") val fileName: String,
    /** 荣耀用 100 表示 APK 安装包，保持与原实现一致 */
    @Json(name = "fileType") val fileType: Int,
    @Json(name = "fileSize") val fileSize: Long,
    @Json(name = "fileSha256") val fileSha256: String,
)

data class HonorUploadUrl(
    @Json(name = "uploadUrl") val url: String? = null,
    @Json(name = "objectId") val objectId: Long? = null,
)

/** 上传接口直接返回裸 JSON（不是 HonorResult 包装体），同样以 code == 0 表示成功 */
data class HonorUploadAck(
    @Json(name = "code") val code: Int? = null,
    @Json(name = "msg") val msg: String? = null,
)

data class HonorBindApkFile(
    @Json(name = "bindingFileList") val items: List<Item>,
) {
    data class Item(
        @Json(name = "objectId") val objectId: Long,
    )
}

data class HonorVersionDesc(
    @Json(name = "languageInfoList") val list: List<LanguageInfo>,
) {
    data class LanguageInfo(
        @Json(name = "appName") val appName: String,
        @Json(name = "intro") val intro: String,
        @Json(name = "briefIntro") val briefIntro: String?,
        @Json(name = "newFeature") val desc: String,
        @Json(name = "languageId") val languageId: String = "zh-CN",
    )
}

data class HonorSubmitParam(
    /** 1-全网发布 2-指定时间发布 */
    @Json(name = "releaseType") val releaseType: Int,
    /**
     * 仅「指定时间发布」必填。
     * UTC 时间格式：yyyy-MM-dd'T'HH:mm:ssZZ，例如 2024-01-01T01:01:01+0800
     */
    @Json(name = "releaseTime") val releaseTime: String?,
)

data class HonorReviewState(
    /**
     * 0-审核中 1-审核通过 2-审核不通过 3-其他非审核状态 4-编辑中，未提交审核
     */
    @Json(name = "auditResult") val auditResult: Int? = null,
    @Json(name = "versionCode") val versionCode: Long? = null,
    @Json(name = "versionName") val versionName: String? = null,
) {

    /**
     * 版本信息缺失时传 null 而不是伪造 0 —— 商店里只有草稿版本时荣耀不返回版本号，
     * 上层「线上版本」展示与版本号比较都需要能区分「没有」和「是 0」。
     */
    fun toMarketInfo(): MarketInfo {
        val state = when (auditResult) {
            0 -> ReviewState.UnderReview
            1 -> ReviewState.Online
            2 -> ReviewState.Rejected
            // 3 是「其他非审核状态」，荣耀文档未细分，无法判断是下架还是别的，归为未知
            3 -> ReviewState.Unknown
            4 -> ReviewState.Draft
            else -> ReviewState.Unknown
        }
        val version = if (versionCode != null) {
            MarketInfo.Version(versionCode, versionName.orEmpty())
        } else {
            null
        }
        return MarketInfo(
            channelId = HONOR_CHANNEL_ID,
            reviewState = state,
            lastVersion = version,
            // 原始状态值留给排查：映射到 Unknown 时至少能看到渠道给了什么
            rawState = "auditResult=${auditResult ?: "null"}",
        )
    }
}

internal const val HONOR_CHANNEL_ID = "honor"
