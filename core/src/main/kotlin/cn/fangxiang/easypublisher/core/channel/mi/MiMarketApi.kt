package cn.fangxiang.easypublisher.core.channel.mi

import cn.fangxiang.easypublisher.core.atSubmissionPoint
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.net.Json
import cn.fangxiang.easypublisher.core.net.ProgressBody
import cn.fangxiang.easypublisher.core.net.ProgressChange
import cn.fangxiang.easypublisher.core.net.textResponse
import cn.fangxiang.easypublisher.core.util.Digest
import okhttp3.FormBody
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.Request
import java.io.File

/**
 * 小米应用市场自动发布接口。
 *
 * 官方文档：小米开放平台 - 自动化发布接口。
 *
 * 请求语义与原实现逐字对齐：URL、表单字段名（`RequestData` / `SIG` / `apk`）、
 * multipart 中 apk 的空文件名、字段顺序、RequestData 的 JSON 结构与 SIG 的加密方式
 * 全部保持不变 —— 这些都参与服务端校验。
 *
 * 本类无状态：凭据与超时都通过入参传入，可安全并发复用（原项目的 `MiMarketClient`
 * 把账号与密钥存在字段里，MCP 并发场景下会串台）。
 */
internal class MiMarketApi {

    /**
     * 查询应用信息。
     *
     * @param certificate 小米下发的 X.509 公钥证书文本，用于加密 SIG
     * @param password 小米开放平台的「私钥」（接口里字段名是 password）
     */
    suspend fun getAppInfo(
        account: String,
        certificate: String,
        password: String,
        packageName: String,
        timeouts: HttpTimeouts,
    ): MiAppInfoResp {
        // RequestData 必须「序列化一次」：表单里发的和参与 MD5 的必须是同一份字符串，
        // 否则服务端算出的 hash 与我们给的不一致，直接鉴权失败。
        val requestData = Json.adapter<MiQueryRequest>()
            .toJson(MiQueryRequest(userName = account, packageName = packageName))
        val sig = buildSig(
            password = password,
            items = listOf(
                // MD5 在此仅作为小米规定的数据指纹字段，见 MiSigPayload 注释
                MiSigPayload.Item(name = "RequestData", hash = Digest.md5Hex(requestData)),
            ),
        )
        val body = FormBody.Builder()
            .add("RequestData", requestData)
            .add("SIG", MiApiSigner.encrypt(sig, certificate))
            .build()
        val request = Request.Builder().url(QUERY).post(body).build()
        val response = client(timeouts).textResponse(request, MI_CHANNEL_ID)
        val result = Json.parse<MiAppInfoResp>(MI_CHANNEL_ID, response)
        checkMiResult(result.result, result.message, "获取应用信息", response)
        return result
    }

    /**
     * 上传 APK 并提交审核。
     */
    suspend fun uploadApk(
        account: String,
        certificate: String,
        password: String,
        artifactFile: File,
        packageInfo: MiPackageInfo,
        updateDesc: String,
        onlineTime: Long,
        timeouts: HttpTimeouts,
        progressChange: ProgressChange,
    ) {
        val requestData = Json.adapter<MiPushRequest>().toJson(
            MiPushRequest(
                userName = account,
                // 1 = 更新已有 app，发版场景固定如此
                synchroType = 1,
                appInfo = MiPushRequest.AppInfo(
                    appName = packageInfo.appName.orEmpty(),
                    packageName = packageInfo.packageName.orEmpty(),
                    updateDesc = updateDesc,
                    // 非定时发布时不带该字段，与原实现一致
                    onlineTime = onlineTime.takeIf { it > 0 },
                ),
            )
        )
        val sig = buildSig(
            password = password,
            items = listOf(
                MiSigPayload.Item(name = "RequestData", hash = Digest.md5Hex(requestData)),
                // apk 的 MD5 同样是小米规定的完整性字段，不是安全签名
                MiSigPayload.Item(name = "apk", hash = Digest.md5Hex(artifactFile)),
            ),
        )
        val apkBody = ProgressBody(
            mediaType = APK_MEDIA_TYPE.toMediaType(),
            file = artifactFile,
            progressChange = progressChange,
        )
        // apk 的 part 文件名是空串：原实现如此，小米服务端按 part 名 "apk" 取文件，
        // 填入真实文件名未经验证，保持原样。
        val body = MultipartBody.Builder()
            .addFormDataPart("apk", "", apkBody)
            .addFormDataPart("RequestData", requestData)
            .addFormDataPart("SIG", MiApiSigner.encrypt(sig, certificate))
            .build()
        val request = Request.Builder().url(PUSH).post(body).build()
        // 小米的 dev/push 把「上传 APK」和「提交审核」合并成一次请求，
        // 因此整个调用都落在不可撤销区间内，无法像其他渠道那样只保护最后一步。
        //
        // 这里有个已知的取舍：若失败发生在上传途中，服务端几乎肯定没有受理，
        // 此时重试其实是安全的。但客户端无法区分「上传中断」与「已受理但响应丢失」，
        // 而两者代价不对称 —— 重复送审不可撤销，多做一次人工确认只是麻烦。
        // 因此一律按不可重试处理，并在提示里说明需要先到后台确认。
        atSubmissionPoint("小米", "上传并提交审核") {
            val response = client(timeouts).textResponse(request, MI_CHANNEL_ID)
            val result = Json.parse<MiCommonResp>(MI_CHANNEL_ID, response)
            checkMiResult(result.result, result.message, "上传并提交审核", response)
        }
    }

    private fun buildSig(password: String, items: List<MiSigPayload.Item>): String =
        Json.adapter<MiSigPayload>().toJson(MiSigPayload(password = password, sig = items))

    private fun client(timeouts: HttpTimeouts) =
        cn.fangxiang.easypublisher.core.net.HttpClients.of(timeouts)

    private companion object {

        /**
         * 自动发布接口域名。
         *
         * 必须是 https：早期版本用的是 http，明文发出的表单里含 RSA 加密后的 SIG 与
         * 整个 APK，虽然 SIG 不可解但仍暴露账号与包信息，且可被中间人替换请求体。
         */
        const val DOMAIN: String = "https://api.developer.xiaomi.com/devupload"

        /** 推送普通 apk */
        const val PUSH: String = "$DOMAIN/dev/push"

        /** 查询 app 状态 */
        const val QUERY: String = "$DOMAIN/dev/query"

        const val APK_MEDIA_TYPE = "application/octet-stream"
    }
}
