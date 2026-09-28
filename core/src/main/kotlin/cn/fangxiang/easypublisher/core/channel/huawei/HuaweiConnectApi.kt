package cn.fangxiang.easypublisher.core.channel.huawei

import cn.fangxiang.easypublisher.core.net.RetrofitFactory
import okhttp3.OkHttpClient
import retrofit2.http.Body
import retrofit2.http.GET
import retrofit2.http.Header
import retrofit2.http.POST
import retrofit2.http.PUT
import retrofit2.http.Query

/** 华为 AppGallery Connect 的接口域名 */
internal const val HUAWEI_BASE_URL = "https://connect-api.cloud.huawei.com/"

internal fun huaweiConnectApi(client: OkHttpClient): HuaweiConnectApi =
    RetrofitFactory.create(HUAWEI_BASE_URL, client)

/**
 * 华为提供的 Api
 * https://developer.huawei.com/consumer/cn/doc/AppGallery-connect-Guides/agcapi-getstarted-0000001111845114
 *
 * 路径、header 名、query 参数名与请求体字段全部与原实现逐字一致 —— 这些是能否真实发版的关键，
 * 不做任何「顺手优化」。
 */
internal interface HuaweiConnectApi {

    /**
     * 获取token
     */
    @POST("api/oauth2/v1/token")
    suspend fun getToken(
        @Body params: HWTokenParams,
    ): HWTokenResp

    /**
     * 通过包名获取AppId
     */
    @GET("api/publish/v2/appid-list")
    suspend fun getAppId(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("packageName") packageName: String,
    ): HWAppIdResp

    /**
     * 获取线上APP信息
     */
    @GET("api/publish/v2/app-info")
    suspend fun getAppInfo(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
    ): HWAppInfoResp

    /**
     * 获取文件上传地址
     */
    @GET("api/publish/v2/upload-url/for-obs")
    suspend fun getUploadUrl(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Query("fileName") fileName: String,
        @Query("contentLength") contentLength: Long,
    ): HWUploadUrlResp

    /**
     * Apk上传以后，绑定文件
     */
    @PUT("api/publish/v2/app-file-info")
    suspend fun bindApkFile(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body params: HWRefreshApk,
    ): HWBindFileResp

    /**
     * 获取Apk编译状态
     */
    @GET("api/publish/v2/package/compile/status")
    suspend fun getApkCompileState(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Query("pkgIds") pkgIds: String,
    ): HWApkState

    /**
     * 更新版本描述
     */
    @PUT("api/publish/v2/app-language-info")
    suspend fun updateVersionDesc(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body versionDesc: HWVersionDesc,
    ): HWResp

    /**
     * 提交审核
     */
    @POST("api/publish/v2/app-submit")
    suspend fun submit(
        @Header("client_id") clientId: String,
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        /**
         * 指定发布的UTC时间，格式为：yyyy-MM-dd'T'HH:mm:ssZZ，例如“2015-01-01T01:01:01+0800”。
         * 为 null 时 Retrofit 不拼这个 query，等价于「审核通过后立即发布」。
         */
        @Query("releaseTime") releaseTime: String?,
    ): HWResp
}
