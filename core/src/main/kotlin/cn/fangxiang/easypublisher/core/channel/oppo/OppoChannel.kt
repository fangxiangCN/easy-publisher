package cn.fangxiang.easypublisher.core.channel.oppo

import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCredentials
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.ReviewState
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.HttpTimeouts

/**
 * OPPO 应用市场渠道。
 *
 * 无状态：凭据与进度回调都来自方法入参，实例可被 ChannelRegistry 安全共享。
 * 原实现把 clientId / clientSecret 存在渠道单例的可变字段里，并发上传两个应用时
 * 会用 A 的密钥去发 B 的包。
 *
 * 发版流程（顺序不可调整，后一步依赖前一步的产物）：
 * token → 读回应用资料 → 取上传地址 → 上传 APK → 提交版本
 */
class OppoChannel : Channel {

    override val id: String = OppoMarketApi.CHANNEL_ID

    override val displayName: String = "OPPO"

    override val fileNameTag: String = "OPPO"

    override val params: List<ChannelParam> = listOf(
        ChannelParam(
            name = CLIENT_ID,
            description = "OPPO 开放平台的 client_id，在「账号管理 - API 密钥」中获取",
        ),
        ChannelParam(
            name = CLIENT_SECRET,
            description = "OPPO 开放平台的 client_secret，与 client_id 成对获取",
        ),
    )

    override suspend fun upload(request: UploadRequest) {
        val api = request.credentials.api(request.timeouts)
        oppoCall {
            val token = api.getToken()
            // 提交版本要求全量字段，必须先把商店里现有的资料读回来，见 OppoMarketApi.submit
            val appInfo = api.getAppInfo(token, request.apkInfo.applicationId)
            val target = api.getUploadUrl(token)
            val apkResult = api.uploadApk(target, token, request.apkFile, request.onProgress)
            api.submit(token, request.apkInfo, appInfo, request.releaseParams, apkResult)
            AppLogger.info(
                LOG_TAG,
                "已提交新版本：${request.apkInfo.applicationId} ${request.apkInfo.versionName}" +
                    "(${request.apkInfo.versionCode})",
            )
        }
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo {
        val api = query.credentials.api(query.timeouts)
        return oppoCall {
            val token = api.getToken()
            api.getAppInfo(token, query.applicationId).toMarketInfo()
        }
    }

    /** 每次调用新建：凭据来自入参，不在渠道实例上留任何状态 */
    private fun ChannelCredentials.api(timeouts: HttpTimeouts): OppoMarketApi = OppoMarketApi(
        clientId = this[CLIENT_ID],
        clientSecret = this[CLIENT_SECRET],
        client = HttpClients.of(timeouts),
    )

    /**
     * 把 OPPO 的审核状态码映射成统一模型。
     *
     * 只有 111 与 444 有明确含义，其余状态码 OPPO 未公开完整清单，沿用原实现
     * 一律归为「审核中」—— 这是保守的选择：宁可拦住一次提交，也不要在审核中重复提交。
     * 区别在于 audit_status 缺失时归为 Unknown 而非 UnderReview：接口没返回状态
     * 和商店确实在审核是两件事，混在一起会让排查走错方向。
     *
     * lastVersion 允许为 null —— 商店里只有尚未上传 APK 的草稿版本时不返回版本号，
     * 此时强行要求非空会直接抛异常（新项目的 MarketInfo 已把该字段声明为可空）。
     */
    private fun OppoAppInfoResponse.Data.toMarketInfo(): MarketInfo {
        val state = when (auditStatus) {
            OppoAuditStatus.ONLINE -> ReviewState.Online
            OppoAuditStatus.REJECTED -> ReviewState.Rejected
            null -> ReviewState.Unknown
            else -> ReviewState.UnderReview
        }
        val version = versionCode?.let { code ->
            MarketInfo.Version(code, versionName.orEmpty())
        }
        return MarketInfo(
            channelId = OppoMarketApi.CHANNEL_ID,
            reviewState = state,
            lastVersion = version,
            rawState = auditStatus?.toString(),
        )
    }

    private companion object {
        const val CLIENT_ID = "client_id"
        const val CLIENT_SECRET = "client_secret"
        const val LOG_TAG = "OPPO应用市场"
    }
}
