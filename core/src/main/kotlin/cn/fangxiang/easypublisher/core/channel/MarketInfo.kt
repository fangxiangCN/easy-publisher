package cn.fangxiang.easypublisher.core.channel

/** 应用在商店的审核状态 */
enum class ReviewState {
    /** 已上架 */
    Online,

    /** 审核中 */
    UnderReview,

    /** 审核被拒 */
    Rejected,

    /** 草稿，尚未提审 */
    Draft,

    /** 已下架 */
    Offline,

    Unknown,
    ;

    val label: String
        get() = when (this) {
            Online -> "已上架"
            UnderReview -> "审核中"
            Rejected -> "审核被拒"
            Draft -> "草稿"
            Offline -> "已下架"
            Unknown -> "状态未知"
        }
}

/**
 * 应用在某渠道的状态。
 *
 * [lastVersion] 可能为 null —— 例如应用在商店里只有一个尚未上传 APK 的草稿版本时，
 * 华为不返回 versionCode。原项目把该字段声明为非空，遇到这种情况直接抛
 * `JsonDataException: Required value 'versionCode' missing`（对应上游 issue #7）。
 */
data class MarketInfo(
    val channelId: String,
    val reviewState: ReviewState,
    val lastVersion: Version? = null,
    /** 渠道是否允许此刻提交新版本 */
    val canSubmit: Boolean = reviewState != ReviewState.UnderReview,
    /** 渠道返回的原始状态描述，便于排查 */
    val rawState: String? = null,
) {
    data class Version(val code: Long, val name: String) {
        override fun toString(): String = "$name($code)"
    }
}
