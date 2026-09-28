package cn.fangxiang.easypublisher.core.channel.huawei

import cn.fangxiang.easypublisher.core.channel.requireSupportedStage
import cn.fangxiang.easypublisher.core.channel.ChannelCapability
import cn.fangxiang.easypublisher.core.channel.Evidence
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.Withdrawal
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.UploadRequest

/**
 * 华为应用市场渠道。
 *
 * 实现无状态：凭据从 [UploadRequest] / [MarketQuery] 取，进度回调随调用传入，
 * 因此单个实例可以被 MCP server 并发复用而不会用错密钥。
 */
class HuaweiChannel : Channel {

    override val id: String = CHANNEL_ID

    override val displayName: String = "华为"

    override val fileNameTag: String = "HUAWEI"

    override val capability: ChannelCapability = ChannelCapability(
        supportedStages = listOf(
            ReleaseStage.UploadArtifact,
            ReleaseStage.CreateDraft,
            ReleaseStage.SubmitReview,
        ),
        riskLevel = ChannelCapability.RiskLevel.High,
        withdrawal = Withdrawal.NotVerified,
        evidence = Evidence.CodeObservation,
        note = "上传与送审分离：绑定 APK 后华为生成草稿版本，可在 AppGallery Connect " +
            "后台确认后再送审。绑定后有异步编译检查需轮询（最长 3 分钟）。",
    )

    override val params: List<ChannelParam> = listOf(
        ChannelParam("client_id", "客户端ID"),
        ChannelParam("client_secret", "秘钥"),
    )

    private val client = HuaweiConnectClient(CHANNEL_ID)

    override suspend fun upload(request: UploadRequest): ReleaseStage {
        requireSupportedStage(request.stopAfter)
        return client.uploadApk(
            file = request.artifactFile,
            artifactInfo = request.artifactInfo,
            clientId = request.credentials["client_id"],
            clientSecret = request.credentials["client_secret"],
            releaseParams = request.releaseParams,
            timeouts = request.timeouts,
            progressChange = request.onProgress,
            stopAfter = request.stopAfter,
        )
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo =
        client.getMarketInfo(
            clientId = query.credentials["client_id"],
            clientSecret = query.credentials["client_secret"],
            applicationId = query.applicationId,
            timeouts = query.timeouts,
        )

    private companion object {
        const val CHANNEL_ID = "huawei"
    }
}
