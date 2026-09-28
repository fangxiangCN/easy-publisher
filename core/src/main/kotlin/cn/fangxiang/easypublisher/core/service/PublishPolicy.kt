package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ArtifactInfo
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState

/**
 * 发布前置条件。
 *
 * 这些规则原本散落在 Compose 的状态类里（`ApkPageState.checkChannelEnableSubmit`），
 * headless 入口会直接绕过。此处提取为独立的纯函数，CLI / MCP 与将来可能接回的 GUI
 * 共用同一套判断。
 */
object PublishPolicy {

    /**
     * 版本号策略。
     *
     * 上游有个 PR 想加「忽略版本检查」开关，但那个实现有两个问题：
     * 它跳过了整个 `versionCode <= lastVersionCode` 判断，连**低于**线上版本也放行；
     * 而且开关被持久化进配置文件，容易忘记关掉，后续所有渠道都不再校验。
     *
     * 这里收窄为三档，且是单次调用的参数而非持久状态。
     */
    enum class VersionRule {
        /** 待提交版本必须严格大于线上版本 */
        Strict,

        /**
         * 允许版本号与线上相同。
         *
         * 真实场景：某渠道审核被拒，改了素材要用同一个 versionCode 重新提交。
         */
        AllowSame,

        /** 完全跳过版本号校验。仅用于排查问题，不建议常规使用 */
        Skip,
    }

    /**
     * 判断某渠道此刻是否可以提交。
     *
     * @return null 表示可以提交；否则返回拒绝原因
     */
    fun reject(
        artifactInfo: ArtifactInfo,
        marketInfo: MarketInfo?,
        rule: VersionRule = VersionRule.Strict,
    ): PublishError? {
        val channel = marketInfo?.channelId

        if (marketInfo != null && !marketInfo.canSubmit) {
            return PublishError.precondition(
                "渠道当前状态为「${marketInfo.reviewState.label}」，不接受新版本" +
                    (marketInfo.rawState?.let { "（原始状态：$it）" } ?: ""),
                channel = channel,
            )
        }

        if (marketInfo?.reviewState == ReviewState.UnderReview) {
            return PublishError.precondition("渠道正在审核中，不能提交新版本", channel = channel)
        }

        if (rule == VersionRule.Skip) return null

        val online = marketInfo?.lastVersion ?: return null
        val incoming = artifactInfo.versionCode

        return when {
            incoming > online.code -> null

            incoming == online.code && rule == VersionRule.AllowSame -> null

            incoming == online.code -> PublishError.precondition(
                "待提交版本号 $incoming 与线上版本 ${online} 相同。" +
                    "若确实要用同一版本号重新提交（例如审核被拒后仅更新素材），" +
                    "请显式指定 --allow-same-version",
                channel = channel,
            )

            else -> PublishError.precondition(
                "待提交版本号 $incoming 低于线上版本 ${online}，这通常意味着选错了 APK 文件",
                channel = channel,
            )
        }
    }
}
