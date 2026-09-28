package cn.fangxiang.easypublisher.core.channel.mi

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState
import com.squareup.moshi.Json

internal const val MI_CHANNEL_ID = "mi"

/**
 * 小米接口的请求 / 响应模型。
 *
 * ## 为什么全部改成 Moshi data class
 *
 * 原实现用 `org.json` 手拼请求体、用 Gson 的 `JsonObject.get("result").asInt`
 * 手读响应：字段缺失时是 NPE 或 `IllegalStateException`，错误里看不出是哪一步、
 * 渠道返回了什么。新项目已移除 Gson，这里统一到 Moshi。
 *
 * ## 为什么响应字段一律可空 + 默认值
 *
 * 原 `MiAppInfoResp` 的 `updateVersion` / `packageInfo` 是非空声明。
 * `MiMarketApi` 虽然先用 `checkSuccess` 过滤了失败码，但小米在 `result == 0`
 * 的情况下也可能不返回 `packageInfo`（例如应用尚未创建过版本），此时 Moshi 直接抛
 * `JsonDataException: Required value 'packageInfo' missing`，用户只看到一句英文。
 * 改为可空后，缺字段在业务层用中文消息显式校验。
 */
internal data class MiAppInfoResp(
    @Json(name = "result") val result: Int? = null,
    @Json(name = "message") val message: String? = null,
    /** 是否允许版本更新。为 false 时小米认为仍有版本在审核中 */
    @Json(name = "updateVersion") val updateVersion: Boolean? = null,
    @Json(name = "packageInfo") val packageInfo: MiPackageInfo? = null,
) {

    /**
     * 上传接口需要回传 appName / packageName，缺失时无法继续，
     * 在这里给出带上下文的中文错误而不是让 NPE 冒到顶层。
     */
    fun requirePackageInfo(): MiPackageInfo = packageInfo ?: throw PublishError.protocol(
        channel = MI_CHANNEL_ID,
        message = "小米返回的应用信息中缺少 packageInfo，无法提交新版本。" +
            "请确认该应用已在小米开放平台创建并至少有一个历史版本",
    )

    /**
     * 映射为统一的市场状态。
     *
     * `updateVersion` 缺失时不能猜 —— 猜 true 会让上层误以为可以提交，
     * 猜 false 又会拦住正常发布，所以归为 [ReviewState.Unknown]。
     * 版本号缺失时 `lastVersion` 传 null，不伪造 0。
     */
    fun toMarketInfo(): MarketInfo {
        val state = when (updateVersion) {
            true -> ReviewState.Online
            false -> ReviewState.UnderReview
            null -> ReviewState.Unknown
        }
        val info = packageInfo
        val version = if (info?.versionCode != null) {
            MarketInfo.Version(info.versionCode, info.versionName.orEmpty())
        } else {
            // 小米在「审核中」状态下不返回正在审核的版本号，这属于正常情况
            null
        }
        return MarketInfo(
            channelId = MI_CHANNEL_ID,
            reviewState = state,
            lastVersion = version,
            rawState = "updateVersion=${updateVersion ?: "null"}",
        )
    }
}

internal data class MiPackageInfo(
    @Json(name = "appName") val appName: String? = null,
    @Json(name = "versionName") val versionName: String? = null,
    @Json(name = "versionCode") val versionCode: Long? = null,
    @Json(name = "packageName") val packageName: String? = null,
)

/**
 * 上传接口的响应。小米所有接口都以 `result == 0` 表示成功，
 * 失败时 `message` 是中文描述。
 */
internal data class MiCommonResp(
    @Json(name = "result") val result: Int? = null,
    @Json(name = "message") val message: String? = null,
)

/**
 * 校验小米业务码。
 *
 * `result` 缺失按协议异常处理：原实现 `get("result").asInt` 在这种情况下直接 NPE。
 */
internal fun checkMiResult(result: Int?, message: String?, action: String, raw: String) {
    if (result == null) {
        throw PublishError.protocol(
            channel = MI_CHANNEL_ID,
            message = "$action 失败：小米响应中没有 result 字段，接口可能已变更",
            raw = raw,
        )
    }
    if (result != 0) {
        throw PublishError.rejected(
            channel = MI_CHANNEL_ID,
            code = result.toString(),
            message = "$action 失败：${message ?: "小米未返回错误描述"}",
            raw = raw,
        )
    }
}

// ---------------------------------------------------------------------------
// 请求体模型
//
// RequestData 是「先序列化成字符串、再对同一份字符串算 MD5」，所以模型只负责生成
// 那个字符串，绝不能让表单字段和参与 hash 的文本出现任何差异。
// ---------------------------------------------------------------------------

/** 查询应用信息的 RequestData */
internal data class MiQueryRequest(
    @Json(name = "userName") val userName: String,
    @Json(name = "packageName") val packageName: String,
)

/** 上传 APK 的 RequestData */
internal data class MiPushRequest(
    @Json(name = "userName") val userName: String,
    /** 0：新增 app；1：更新 app；2：修改 app 信息。发版固定用 1 */
    @Json(name = "synchroType") val synchroType: Int,
    @Json(name = "appInfo") val appInfo: AppInfo,
) {
    data class AppInfo(
        @Json(name = "appName") val appName: String,
        @Json(name = "packageName") val packageName: String,
        @Json(name = "updateDesc") val updateDesc: String,
        /**
         * 定时上线的毫秒时间戳。
         *
         * 原实现只在 `onlineTime > 0` 时才 `put` 这个键，不定时发布的请求里**没有**
         * 这个字段。Moshi 默认省略 null 字段，语义与之一致。
         */
        @Json(name = "onlineTime") val onlineTime: Long? = null,
    )
}

/**
 * SIG 的明文结构：私钥 + 各部分数据的 MD5 摘要清单。
 *
 * 这里的 `hash` 用 MD5 是小米接口规定的字段格式，**不是安全签名**：
 * 它的作用是让服务端核对 RequestData 与 apk 在传输中没有损坏，而整个结构随后会被
 * RSA 公钥加密（见 [MiApiSigner]）才发出，攻击者无法在不持有私钥的情况下构造。
 * 因此沿用 MD5 是可接受的，也是唯一能通过小米校验的选择。
 */
internal data class MiSigPayload(
    @Json(name = "password") val password: String,
    @Json(name = "sig") val sig: List<Item>,
) {
    data class Item(
        @Json(name = "name") val name: String,
        @Json(name = "hash") val hash: String,
    )
}
