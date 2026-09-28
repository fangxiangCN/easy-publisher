package cn.fangxiang.easypublisher.core.channel.honor

import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.UploadRequest

/**
 * 荣耀应用市场渠道。
 *
 * 无状态实现：每次调用按入参里的超时配置新建/复用客户端，凭据不落字段，
 * 因此同一实例可以并发服务多个应用。
 */
class HonorChannel : Channel {

    override val id: String = HONOR_CHANNEL_ID

    override val displayName: String = "荣耀"

    override val fileNameTag: String = "HONOR"

    override val params: List<ChannelParam> = listOf(
        ChannelParam(name = CLIENT_ID, description = "客户端ID"),
        ChannelParam(name = CLIENT_SECRET, description = "秘钥"),
    )

    override suspend fun upload(request: UploadRequest) {
        val client = HonorConnectClient(request.timeouts)
        client.uploadApk(
            file = request.apkFile,
            apkInfo = request.apkInfo,
            clientId = request.credentials[CLIENT_ID],
            clientSecret = request.credentials[CLIENT_SECRET],
            releaseParams = request.releaseParams,
            progressChange = request.onProgress,
        )
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo {
        val client = HonorConnectClient(query.timeouts)
        val state = client.getReviewState(
            clientId = query.credentials[CLIENT_ID],
            clientSecret = query.credentials[CLIENT_SECRET],
            applicationId = query.applicationId,
        )
        return state.toMarketInfo()
    }

    private companion object {
        // 参数名与原项目一致，沿用同一套配置文件不需要迁移
        const val CLIENT_ID = "client_id"
        const val CLIENT_SECRET = "client_secret"
    }
}
