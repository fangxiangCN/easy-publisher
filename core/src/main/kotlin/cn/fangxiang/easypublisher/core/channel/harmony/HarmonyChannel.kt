package cn.fangxiang.easypublisher.core.channel.harmony

import cn.fangxiang.easypublisher.core.ArtifactKind
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCapability
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.Evidence
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.channel.Withdrawal
import cn.fangxiang.easypublisher.core.channel.requireSupportedStage
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.log.redact

/**
 * 鸿蒙应用市场（HarmonyOS AppGallery）。
 *
 * ## 只做到草稿，不送审
 *
 * [capability] 的 supportedStages **不包含** [ReleaseStage.SubmitReview]。
 * 原因是鸿蒙的送审接口没有经过任何验证 —— 参照实现同样只做到草稿，
 * 并显式拒绝其他发布类型。在拿到真实凭据验证之前，声称支持送审是不负责任的：
 * 送审不可撤销，猜错接口的代价由用户承担。
 *
 * 因此本渠道的用途是：把 App Pack 传上去、关联到 AGC 后台的草稿版本，
 * 由人工核对无误后在网页端送审。
 *
 * ## 与华为渠道的关系
 *
 * 鉴权完全同源（同端点、同请求体、同 access_token 字段），
 * 但发布流程不同：`.app` 走 Upload Management API 分片上传 + v3 关联草稿，
 * 而 APK 走 upload-url + 单次 PUT + v2 绑定。两者不可互换。
 *
 * 另外 appId 必须显式配置：HarmonyOS NEXT 应用在 AGC 里是独立的应用记录，
 * 用包名反查会拿到 Android 应用的 id。
 */
class HarmonyChannel : Channel {

    override val id: String = HARMONY_CHANNEL_ID

    override val displayName: String = "鸿蒙"

    override val fileNameTag: String = "HARMONY"

    override val artifactExtensions: List<String> = ArtifactKind.HarmonyAppPack.extensions

    override val params: List<ChannelParam> = listOf(
        ChannelParam(
            name = CLIENT_ID,
            description = "AGC 项目的客户端 ID（与华为渠道同一套凭据）",
        ),
        ChannelParam(
            name = CLIENT_SECRET,
            description = "AGC 项目的客户端密钥",
        ),
        ChannelParam(
            name = APP_ID,
            description = "鸿蒙应用在 AGC 里的 appId。注意与同名 Android 应用不是同一个 id，" +
                "需到 AGC 后台「我的应用」里查看鸿蒙应用条目",
        ),
    )

    override val capability: ChannelCapability = ChannelCapability(
        // 刻意不含 SubmitReview：鸿蒙送审未经验证，见类注释
        supportedStages = listOf(ReleaseStage.UploadArtifact, ReleaseStage.CreateDraft),
        riskLevel = ChannelCapability.RiskLevel.Medium,
        // 停在草稿态本身不产生需要撤回的东西 —— 不送审就不会上架
        withdrawal = Withdrawal.NotApplicable,
        evidence = Evidence.CodeObservation,
        note = "只做到草稿：上传 App Pack 并关联到 AGC 草稿版本，不送审。" +
            "鸿蒙的送审接口未经验证，需人工到 AGC 后台提交。" +
            "appId 必须显式配置（鸿蒙应用在 AGC 里是独立记录）。",
    )

    override suspend fun upload(request: UploadRequest): ReleaseStage {
        requireSupportedStage(request.stopAfter)
        if (request.artifactInfo.kind != ArtifactKind.HarmonyAppPack) {
            throw PublishError.localFile(
                "鸿蒙渠道需要 .app 格式的 App Pack，收到的是 " +
                    "${request.artifactInfo.kind.label}（${request.artifactFile.name}）",
            )
        }

        val clientId = request.credentials[CLIENT_ID]
        val clientSecret = request.credentials[CLIENT_SECRET]
        val appId = request.credentials[APP_ID]

        // 鸿蒙拿不到市场状态，版本号比对做不了。明确记一条日志，
        // 不要让人以为已经校验过了
        AppLogger.info(
            LOG_TAG,
            "鸿蒙渠道不支持查询市场状态，本次跳过线上版本号比对；" +
                "待提交版本 ${request.artifactInfo.versionName}(${request.artifactInfo.versionCode})",
        )
        AppLogger.info(LOG_TAG, "开始上传，appId=$appId，账号=${redact(clientId)}")

        val client = HarmonyPublishClient(request.timeouts)
        val linkToDraft = request.stopAfter == ReleaseStage.CreateDraft
        val packageId = client.uploadAppPack(
            artifactFile = request.artifactFile,
            artifactInfo = request.artifactInfo,
            clientId = clientId,
            clientSecret = clientSecret,
            appId = appId,
            progressChange = request.onProgress,
            linkToDraft = linkToDraft,
        )

        return if (linkToDraft) {
            AppLogger.info(
                LOG_TAG,
                "草稿已就绪（packageId=$packageId），未送审。" +
                    "请到 AGC 后台核对后再手动提交审核",
            )
            ReleaseStage.CreateDraft
        } else {
            AppLogger.info(LOG_TAG, "App Pack 已上传至华为文件服务，未关联草稿")
            ReleaseStage.UploadArtifact
        }
    }

    override suspend fun queryMarket(query: MarketQuery): MarketInfo {
        // 不猜测、不复用华为的 app-info：那查的是 Android 应用记录，
        // 把它的状态当成鸿蒙应用的状态报出去会误导发布决策
        throw PublishError(
            kind = cn.fangxiang.easypublisher.core.ErrorKind.ProtocolMismatch,
            channel = id,
            message = "鸿蒙渠道暂不支持查询市场状态：其送审与状态接口未经验证，" +
                "而复用华为 app-info 查到的是同名 Android 应用的记录。" +
                "请直接到 AGC 后台查看鸿蒙应用的状态",
        )
    }

    private companion object {
        const val LOG_TAG = "鸿蒙应用市场"
        const val CLIENT_ID = "client_id"
        const val CLIENT_SECRET = "client_secret"
        const val APP_ID = "app_id"
    }
}
