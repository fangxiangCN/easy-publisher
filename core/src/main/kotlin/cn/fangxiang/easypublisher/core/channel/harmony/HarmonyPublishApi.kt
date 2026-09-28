package cn.fangxiang.easypublisher.core.channel.harmony

import cn.fangxiang.easypublisher.core.channel.huawei.HWTokenParams
import cn.fangxiang.easypublisher.core.channel.huawei.HWTokenResp
import retrofit2.http.Body
import retrofit2.http.Header
import retrofit2.http.POST
import retrofit2.http.PUT
import retrofit2.http.Query

/**
 * 鸿蒙 App Pack 发布接口。
 *
 * ## 为什么复用华为的 token 模型
 *
 * 鸿蒙应用与 Android 应用共用同一套 AppGallery Connect 鉴权：
 * 端点（`api/oauth2/v1/token`）、请求体（client_id / client_secret /
 * grant_type=client_credentials）、响应字段（access_token）完全一致。
 * 因此直接复用 [HWTokenParams] 与 [HWTokenResp]，避免两份定义各自漂移。
 *
 * ## 为什么 appId 必须由用户配置
 *
 * HarmonyOS NEXT 应用在 AGC 里是**独立的应用记录**，与同名的 Android 应用
 * 不是同一个 appId。华为渠道可以用包名调 `appid-list` 反查，
 * 但那样查到的是 Android 应用的 id，用在鸿蒙包上会关联到错误的草稿。
 * 所以这里要求显式配置，不做反查。
 */
internal interface HarmonyPublishApi {

    @POST("api/oauth2/v1/token")
    suspend fun getToken(@Body params: HWTokenParams): HWTokenResp

    /**
     * 初始化分片上传，拿到 objectId、nspUploadId 与华为规定的分片大小。
     */
    @POST("api/publish/v2/upload/multipart/init")
    suspend fun initMultipart(
        @Header("Authorization") authorization: String,
        @Header("client_id") clientId: String,
        @Query("appId") appId: String,
        @Query("fileName") fileName: String,
        @Query("contentType") contentType: String,
        @Query("fileType") fileType: Int,
        @Query("releaseType") releaseType: Int,
    ): MultipartInitResp

    /**
     * 提交各分片的 sha256 与长度，换取每片的短时效签名上传地址。
     */
    @POST("api/publish/v2/upload/multipart/parts")
    suspend fun getPartUploadInfo(
        @Header("Authorization") authorization: String,
        @Header("client_id") clientId: String,
        @Query("objectId") objectId: String,
        @Query("nspUploadId") nspUploadId: String,
        @Body descriptors: Map<String, PartDescriptor>,
    ): MultipartPartsResp

    /**
     * 合并分片。必须回传每片的 partObjectId 与上传响应里的 ETag。
     */
    @POST("api/publish/v2/upload/multipart/compose")
    suspend fun compose(
        @Header("Authorization") authorization: String,
        @Header("client_id") clientId: String,
        @Query("objectId") objectId: String,
        @Query("nspUploadId") nspUploadId: String,
        @Body completedParts: Map<String, CompletedPart>,
    ): ComposeResp

    /**
     * 把已合并的 App Pack 关联到当前草稿版本（v3 接口）。
     *
     * 分片合并成功只代表文件进入了华为的文件服务，
     * 还必须调这个接口才会出现在 AGC 后台的草稿里。
     */
    @PUT("api/publish/v3/app-package-info")
    suspend fun updateAppPackageInfo(
        @Header("Authorization") authorization: String,
        @Header("client_id") clientId: String,
        @Query("appId") appId: String,
        @Body body: AppPackageInfoReq,
    ): AppPackageInfoResp

    /**
     * 提交发布（送审）。
     *
     * 鸿蒙走 v3，Android 走 v2 —— 两者路径不同，不可混用。
     * 这是整条链路里唯一不可撤销的一步。
     */
    @POST("api/publish/v3/app-submit")
    suspend fun submit(
        @Header("Authorization") authorization: String,
        @Header("client_id") clientId: String,
        @Query("appId") appId: String,
        @Body body: HarmonySubmitReq,
    ): HarmonySubmitResp

    companion object {
        const val BASE_URL = "https://connect-api.cloud.huawei.com/"
    }
}
