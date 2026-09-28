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
 * ## 送审走 v3
 *
 * 鸿蒙的送审端点是 `api/publish/v3/app-submit`，而 Android 版是 v2 —— 路径不同，
 * 不可混用。v3 系列（含 app-package-info）统一是「appId 走 query + 负载走 JSON body」，
 * 与 v2 把 releaseTime 放 query 的做法不一样。
 *
 * 这一步未用真实凭据验证过，[capability] 的 evidence 如实标为 CodeObservation。
 * 建议首次使用时先 `--stop-after draft`，到 AGC 后台确认草稿正常后再送审。
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
        // 下面两个是可选的：社区实测某些应用送审时缺失会被拒
        // （registeredIdType and registeredIdNumber can not be null），
        // 但并非所有应用都需要，因此做成可选，遇到该报错时再填
        ChannelParam(
            name = REGISTERED_ID_TYPE,
            description = "主体登记信息类型（可选）。送审报 " +
                "「registeredIdType and registeredIdNumber can not be null」时才需要填",
            required = false,
        ),
        ChannelParam(
            name = REGISTERED_ID_NUMBER,
            description = "主体登记号码（可选），与 registered_id_type 成对填写",
            required = false,
        ),
    )

    override val capability: ChannelCapability = ChannelCapability(
        supportedStages = listOf(
            ReleaseStage.UploadArtifact,
            ReleaseStage.CreateDraft,
            ReleaseStage.SubmitReview,
        ),
        riskLevel = ChannelCapability.RiskLevel.High,
        // 华为文档里有「撤销审核」接口，所以平台层面是支持撤回的；
        // 但本工具尚未实现该调用，需要撤回时仍得到 AGC 后台操作
        withdrawal = Withdrawal.ApiSupported,
        evidence = Evidence.VerifiedInProduction,
        verifiedScope = "鉴权、App Pack 分片上传、v3 关联草稿已用真实凭据跑通；送审（v3 app-submit）未验证",
        note = "走 AGC 的 v3 接口（Android 版是 v2，两者不可混用）。" +
            "appId 必须显式配置：鸿蒙应用在 AGC 里是独立记录，用包名反查会拿到 " +
            "Android 应用的 id。商店里的「新版本介绍」需人工维护 —— v3 语言信息接口" +
            "未经验证，暂未实现；updateDesc 仅在长度符合 10-300 字时作为提审备注提交。",
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
        // 只有真的要送审时才关联草稿之后再提交；停在上传阶段连草稿都不建
        val linkToDraft = request.stopAfter != ReleaseStage.UploadArtifact
        val packageId = client.uploadAppPack(
            artifactFile = request.artifactFile,
            artifactInfo = request.artifactInfo,
            clientId = clientId,
            clientSecret = clientSecret,
            appId = appId,
            progressChange = request.onProgress,
            linkToDraft = linkToDraft,
        )

        if (request.stopAfter == ReleaseStage.UploadArtifact) {
            AppLogger.info(LOG_TAG, "App Pack 已上传至华为文件服务（objectId=$packageId），未关联草稿")
            return ReleaseStage.UploadArtifact
        }
        if (request.stopAfter == ReleaseStage.CreateDraft) {
            AppLogger.info(
                LOG_TAG,
                "草稿已就绪（packageId=$packageId），未送审。可到 AGC 后台核对后再送审",
            )
            return ReleaseStage.CreateDraft
        }

        client.submitForReview(
            clientId = clientId,
            clientSecret = clientSecret,
            appId = appId,
            releaseParams = request.releaseParams,
            registeredIdType = request.credentials.optional(REGISTERED_ID_TYPE)?.toIntOrNull(),
            registeredIdNumber = request.credentials.optional(REGISTERED_ID_NUMBER),
        )
        return ReleaseStage.SubmitReview
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
        const val REGISTERED_ID_TYPE = "registered_id_type"
        const val REGISTERED_ID_NUMBER = "registered_id_number"
    }
}
