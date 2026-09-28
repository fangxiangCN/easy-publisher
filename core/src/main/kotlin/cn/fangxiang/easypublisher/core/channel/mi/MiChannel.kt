package cn.fangxiang.easypublisher.core.channel.mi

import cn.fangxiang.easypublisher.core.channel.requireSupportedStage
import cn.fangxiang.easypublisher.core.channel.ChannelCapability
import cn.fangxiang.easypublisher.core.channel.Evidence
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.Withdrawal
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCredentials
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact

/**
 * 小米应用市场渠道。
 *
 * 无状态：凭据来自每次调用的入参，实例可被 [cn.fangxiang.easypublisher.core.channel.ChannelRegistry]
 * 安全共享。
 */
class MiChannel : Channel {

    override val id: String = MI_CHANNEL_ID

    override val displayName: String = "小米"

    /** 沿用原项目的多渠道包文件名标识 */
    override val fileNameTag: String = "MI"

    override val capability: ChannelCapability = ChannelCapability(
        // dev/push 是原子的，没有任何可停下的中间位置
        supportedStages = listOf(ReleaseStage.SubmitReview),
        riskLevel = ChannelCapability.RiskLevel.Critical,
        withdrawal = Withdrawal.NotVerified,
        evidence = Evidence.CodeObservation,
        note = "dev/push 把「上传安装包」与「提交审核」合并为一次请求，中间无处可停 —— " +
            "一旦调用即送审，无法先建草稿确认。",
    )

    override val params: List<ChannelParam> = listOf(
        ChannelParam(
            name = KEY_ACCOUNT,
            description = "小米开放平台账号（邮箱）",
        ),
        ChannelParam(
            name = KEY_PUBLIC_KEY,
            description = "公钥证书，小米开放平台下载的 .cer 文件",
            // 证书是多行 PEM 文本，走文件类型参数而不是让用户往命令行里粘贴
            type = ChannelParam.ParamType.TextFile("cer"),
        ),
        ChannelParam(
            name = KEY_PRIVATE_KEY,
            description = "私钥（开放平台「API 密码」）",
        ),
    )

    private val api = MiMarketApi()

    override suspend fun upload(request: UploadRequest): ReleaseStage {
        // 小米的 dev/push 把上传与送审合并成一次原子请求，没有可停下的中间位置。
        // 这里必须显式报错：若默默继续走到送审，调用方会以为停在了草稿态。
        requireSupportedStage(request.stopAfter)
        val account = request.credentials[KEY_ACCOUNT]
        val certificate = request.credentials[KEY_PUBLIC_KEY]
        val password = request.credentials[KEY_PRIVATE_KEY]
        val packageName = request.apkInfo.applicationId

        // 日志里只出现账号与包名；证书、私钥、SIG 一律不落盘
        AppLogger.info(LOG_TAG, "开始提交新版本：$packageName，账号=${redact(account)}")

        // 上传需要 appName / packageName，小米只在查询接口返回，所以先查一次
        val appInfo = api.getAppInfo(
            account = account,
            certificate = certificate,
            password = password,
            packageName = packageName,
            timeouts = request.timeouts,
        )
        val packageInfo = appInfo.requirePackageInfo()

        api.uploadApk(
            account = account,
            certificate = certificate,
            password = password,
            apkFile = request.apkFile,
            packageInfo = packageInfo,
            updateDesc = request.releaseParams.updateDesc,
            onlineTime = request.releaseParams.onlineTime,
            timeouts = request.timeouts,
            progressChange = request.onProgress,
        )
        AppLogger.info(LOG_TAG, "提交成功：$packageName")
        return ReleaseStage.SubmitReview
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo {
        val credentials: ChannelCredentials = query.credentials
        val appInfo = api.getAppInfo(
            account = credentials[KEY_ACCOUNT],
            certificate = credentials[KEY_PUBLIC_KEY],
            password = credentials[KEY_PRIVATE_KEY],
            packageName = query.applicationId,
            timeouts = query.timeouts,
        )
        return appInfo.toMarketInfo()
    }

    private companion object {
        const val LOG_TAG = "小米应用市场"
        const val KEY_ACCOUNT = "account"
        const val KEY_PUBLIC_KEY = "publicKey"
        const val KEY_PRIVATE_KEY = "privateKey"
    }
}
