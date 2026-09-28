package cn.fangxiang.easypublisher.core.channel.vivo

import cn.fangxiang.easypublisher.core.channel.requireSupportedStage
import cn.fangxiang.easypublisher.core.channel.ChannelCapability
import cn.fangxiang.easypublisher.core.channel.Evidence
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.Withdrawal
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCredentials
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact
import cn.fangxiang.easypublisher.core.net.HttpClients
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import kotlinx.coroutines.CancellationException

/**
 * vivo 应用市场渠道。
 *
 * 无状态：凭据、超时、进度回调全部来自方法入参，实例可被 ChannelRegistry 共享，
 * 并发上传多个应用不会串台。
 */
class VivoChannel : Channel {

    override val id: String = ID

    override val displayName: String = "vivo"

    override val fileNameTag: String = "VIVO"

    override val capability: ChannelCapability = ChannelCapability(
        supportedStages = listOf(ReleaseStage.UploadArtifact, ReleaseStage.SubmitReview),
        riskLevel = ChannelCapability.RiskLevel.Critical,
        withdrawal = Withdrawal.NotVerified,
        evidence = Evidence.CodeObservation,
        note = "app.sync.update.app 一次完成版本更新与送审，没有草稿态。" +
            "onlineType=1 审核通过后立即上架，2 为定时上架。" +
            "签名参数全部走 query（router/rest 网关的强制要求）。",
    )

    override val params: List<ChannelParam> = listOf(
        ChannelParam(
            name = ACCESS_KEY,
            description = "vivo 开放平台的 access_key，在「开发者服务 - API 调用凭据」中获取",
        ),
        ChannelParam(
            name = ACCESS_SECRET,
            description = "vivo 开放平台的 access_secret，与 access_key 配套签发",
        ),
    )

    override suspend fun upload(request: UploadRequest): ReleaseStage {
        requireSupportedStage(request.stopAfter)
        val api = api(request.credentials, request.timeouts)
        val packageName = request.artifactInfo.applicationId

        // 保持原实现的请求顺序：先查详情再上传。查询会提前暴露包名不属于本账号、
        // 凭据失效这类问题，避免白传一个上百兆的包才失败。
        val appInfo = step("获取应用信息") { api.getAppInfo(packageName) }
        AppLogger.info(
            LOG_TAG,
            "vivo 线上状态：${appInfo.toMarketInfo().reviewState.label}，" +
                "线上版本：${appInfo.toMarketInfo().lastVersion ?: "无"}",
        )

        val uploadResult = step("上传 APK") {
            api.uploadApk(request.artifactFile, packageName, request.onProgress)
        }
        if (request.stopAfter == ReleaseStage.UploadArtifact) {
            // vivo 同样没有草稿态：停在这里只证明文件与签名被接受
            AppLogger.info(
                LOG_TAG,
                "已按请求停在「仅上传安装包」：文件已被 vivo 接受，未提交版本",
            )
            return ReleaseStage.UploadArtifact
        }
        step("提交更新") { api.submit(uploadResult, request.releaseParams) }
        return ReleaseStage.SubmitReview
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo {
        val api = api(query.credentials, query.timeouts)
        return step("获取应用信息") { api.getAppInfo(query.applicationId) }.toMarketInfo()
    }

    private fun api(credentials: ChannelCredentials, timeouts: HttpTimeouts): VivoMarketApi {
        val accessKey = credentials[ACCESS_KEY]
        val accessSecret = credentials[ACCESS_SECRET]
        // 凭据只以脱敏形式出现在日志里；access_secret 与 sign 任何时候都不打印原值
        AppLogger.debug(LOG_TAG, "使用 access_key=${redact(accessKey)}")
        return VivoMarketApi(
            accessKey = accessKey,
            accessSecret = accessSecret,
            // 超时完全交给调用方传入的配置，不在渠道内硬编码
            client = HttpClients.of(timeouts),
        )
    }

    /**
     * 统一记录步骤起止并归一化异常。
     *
     * 取消必须原样抛出：CancellationException 一旦被当成普通失败包装成 PublishError，
     * 协程的取消语义就断了，上层 `withTimeout` / 用户中断都会失效。
     * 也因此这里不 catch Throwable —— OOM、StackOverflow 之类不该被伪装成发布失败。
     */
    private inline fun <T> step(action: String, block: () -> T): T {
        AppLogger.info(LOG_TAG, "$action 开始")
        try {
            val result = block()
            AppLogger.info(LOG_TAG, "$action 完成")
            return result
        } catch (e: CancellationException) {
            throw e
        } catch (e: PublishError) {
            AppLogger.error(LOG_TAG, "$action 失败：${e.describe()}")
            throw e
        } catch (e: Exception) {
            val error = PublishError.fromNetwork(ID, e)
            AppLogger.error(LOG_TAG, "$action 失败：${error.describe()}")
            throw error
        }
    }

    internal companion object {
        const val ID = "vivo"

        private const val ACCESS_KEY = "access_key"
        private const val ACCESS_SECRET = "access_secret"

        private const val LOG_TAG = "vivo应用市场"
    }
}
