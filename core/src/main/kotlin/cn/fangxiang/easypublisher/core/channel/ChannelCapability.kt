package cn.fangxiang.easypublisher.core.channel

import cn.fangxiang.easypublisher.core.PublishError

/**
 * 发布流程中可以停下来的位置。
 *
 * 之所以需要这个概念：各渠道的 API 粒度差别很大。华为可以先建草稿、人工到后台
 * 看一眼再送审；小米的 `dev/push` 把上传和送审合并成一次请求，中间没有任何
 * 可以停下的位置。用统一的「upload」掩盖这个差异，会让调用方误以为所有渠道
 * 都有反悔的机会。
 */
enum class ReleaseStage {
    /** 仅把安装包传到渠道的文件存储，不创建任何版本。可用于验证凭据与签名是否可用 */
    UploadArtifact,

    /**
     * 创建草稿版本，可在渠道后台查看但尚未送审。
     *
     * 只有部分渠道有这个状态；OPPO/vivo 的文件上传不会生成可检视的草稿。
     */
    CreateDraft,

    /** 提交审核。不可撤销 */
    SubmitReview,
    ;

    val label: String
        get() = when (this) {
            UploadArtifact -> "仅上传安装包"
            CreateDraft -> "创建草稿（不送审）"
            SubmitReview -> "提交审核"
        }
}

/** 送审之后能否撤回，以及通过什么途径 */
enum class Withdrawal {
    /** 该动作本身不产生需要撤回的东西（如仅上传文件） */
    NotApplicable,

    /** API 支持撤回 */
    ApiSupported,

    /** API 不支持，但可登录渠道后台手动操作 */
    ConsoleOnly,

    /** 未经验证 —— 不代表不能，只代表没人确认过 */
    NotVerified,

    Unsupported,
    ;

    val label: String
        get() = when (this) {
            NotApplicable -> "无需撤回"
            ApiSupported -> "API 可撤回"
            ConsoleOnly -> "仅后台可撤回"
            NotVerified -> "未验证"
            Unsupported -> "不可撤回"
        }
}

/**
 * 能力声明的证据来源。
 *
 * 这一项是刻意加的：本项目的渠道逻辑**没有用真实凭据验证过**，
 * 把「读了文档」、「看了代码」、「真机跑过」区分开，
 * 比笼统地声称「支持」诚实得多，也让使用者知道该对哪些结论保持怀疑。
 */
enum class Evidence {
    /** 仅来自渠道官方文档 */
    OfficialDocumentation,

    /** 仅来自代码推断（含从上游项目继承的实现） */
    CodeObservation,

    /** 文档与代码相互印证 */
    OfficialAndCode,

    /** 已用真实凭据实际跑通 */
    VerifiedInProduction,
    ;

    val label: String
        get() = when (this) {
            OfficialDocumentation -> "官方文档"
            CodeObservation -> "代码推断"
            OfficialAndCode -> "文档+代码"
            VerifiedInProduction -> "已实测"
        }
}

/**
 * 一个渠道的发布能力与风险画像。
 *
 * 调用方（脚本、CI、agent）应当先读这个再决定怎么做，而不是发完之后才发现
 * 这个渠道压根不能停、不能撤。
 */
data class ChannelCapability(
    /** 本渠道支持停在哪些阶段，按流程顺序排列，最后一项总是 [ReleaseStage.SubmitReview] */
    val supportedStages: List<ReleaseStage>,

    val riskLevel: RiskLevel,

    /** 送审后的撤回途径 */
    val withdrawal: Withdrawal,

    /** 送审动作是否要求显式确认 */
    val requiresExplicitConfirmation: Boolean = true,

    /**
     * 送审步骤是否允许自动重试。
     *
     * 一律为 false：服务端可能已受理而响应丢失，重试会重复送审。
     * 见 [cn.fangxiang.easypublisher.core.FailurePhase]。
     */
    val automaticRetryAfterSubmission: Boolean = false,

    val evidence: Evidence,

    /**
     * 已验证的范围，仅当 [evidence] 为 [Evidence.VerifiedInProduction] 时有意义。
     *
     * 单独成一个字段而不是塞进 [note]，是因为「实测过」这句话很容易被读成
     * 「整条链路都实测过」。实际上目前验证到的只是鉴权与上传/建草稿，
     * **送审路径一个渠道都没验证过** —— 而送审恰恰是不可撤销的那一步。
     * 把范围显式写出来，才不会让人误判风险。
     */
    val verifiedScope: String? = null,

    /** 给使用者看的说明，尤其是与直觉不符的地方 */
    val note: String,
) {
    enum class RiskLevel {
        Low, Medium, High, Critical,
        ;

        val label: String
            get() = when (this) {
                Low -> "低"
                Medium -> "中"
                High -> "高"
                Critical -> "极高"
            }
    }

    /** 是否能停在送审之前 */
    fun canStopBefore(stages: List<ReleaseStage> = supportedStages): Boolean =
        stages.any { it != ReleaseStage.SubmitReview }

    fun supports(stage: ReleaseStage): Boolean = stage in supportedStages

    /** 最靠前的可停位置，用于默认的安全模式 */
    val safestStage: ReleaseStage get() = supportedStages.first()
}

/**
 * 校验请求的停留点本渠道是否支持。
 *
 * 必须显式报错，不能默默一路走到送审 —— 调用方以为停在草稿态、实际已经提交，
 * 是这个功能最坏的失败模式。
 */
internal fun Channel.requireSupportedStage(requested: ReleaseStage) {
    if (capability.supports(requested)) return
    throw PublishError.configuration(
        "$displayName 不支持「${requested.label}」。" +
            "该渠道可停在：${capability.supportedStages.joinToString("、") { it.label }}。" +
            capability.note,
        channel = id,
    )
}
