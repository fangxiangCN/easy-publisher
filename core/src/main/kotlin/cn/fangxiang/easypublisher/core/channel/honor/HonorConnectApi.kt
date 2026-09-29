package cn.fangxiang.easypublisher.core.channel.honor

import retrofit2.http.Body
import retrofit2.http.Field
import retrofit2.http.FormUrlEncoded
import retrofit2.http.GET
import retrofit2.http.Header
import retrofit2.http.POST
import retrofit2.http.Query

/**
 * 荣耀开放平台接口。
 *
 * 官方文档：https://developer.honor.com/cn/doc/guides/101359
 *
 * 请求语义与原实现逐字对齐（路径、query 参数名、header、body 结构、token 的
 * form-urlencoded 表单），这是能否真实发版的前提，不确定的地方一律保持原样。
 */
interface HonorConnectApi {

    /**
     * 获取 token。
     *
     * 注意两点刻意保留的细节：
     *  1. 它是 POST + application/x-www-form-urlencoded（@FormUrlEncoded + @Field），
     *     不是 JSON body —— 荣耀 IAM 只接受表单。
     *  2. 这里用的是绝对地址，域名（iam.developer.honor.com）与其余接口的
     *     baseUrl（appmarket-openapi-drcn.cloud.honor.com）不同，Retrofit 会用它覆盖 baseUrl。
     */
    @POST("https://iam.developer.honor.com/auth/token")
    @FormUrlEncoded
    suspend fun getToken(
        @Field("client_id")
        clientId: String,
        @Field("client_secret")
        clientSecret: String,
        @Field("grant_type")
        type: String = "client_credentials",
    ): HonorTokenResp

    /** 通过包名获取 AppId */
    @GET("openapi/v1/publish/get-app-id")
    suspend fun getAppId(
        @Header("Authorization") token: String,
        @Query("pkgName") packageName: String,
    ): HonorResult<List<HonorAppId>>

    /** 获取 App 信息（语言信息、线上版本） */
    @GET("openapi/v1/publish/get-app-detail")
    suspend fun getAppInfo(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
    ): HonorResult<HonorAppInfo>

    /** 获取 App 当前版本的审核状态 */
    @GET("openapi/v1/publish/get-app-current-release")
    suspend fun getReviewState(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
    ): HonorResult<HonorReviewState>

    /** 获取文件上传地址，body 是文件描述数组 */
    @POST("openapi/v1/publish/get-file-upload-url")
    suspend fun getUploadUrl(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body body: List<HonorUploadFile>,
    ): HonorResult<List<HonorUploadUrl>>

    /** APK 上传完成后，用这个接口把文件绑定到版本上 */
    @POST("openapi/v1/publish/update-file-info")
    suspend fun bindApkFile(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body body: HonorBindApkFile,
    ): HonorResult<Any?>

    /** 更新版本描述 */
    @POST("openapi/v1/publish/update-language-info")
    suspend fun updateVersionDesc(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body versionDesc: HonorVersionDesc,
    ): HonorResult<Any?>

    /**
     * 更新应用基础信息（全量更新语义）。
     *
     * 用于设置年龄分级。该接口要求整份资料回传，漏字段会逐个报「xxx is empty」，
     * 因此调用方必须先从 get-app-detail 读回，改字段后再送回。
     */
    @POST("openapi/v1/publish/update-app-info")
    suspend fun updateAppInfo(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body info: HonorAppInfo.BasicInfo,
    ): HonorResult<Any?>

    /** 提交审核 */
    @POST("openapi/v1/publish/submit-audit")
    suspend fun submit(
        @Header("Authorization") token: String,
        @Query("appId") appId: String,
        @Body param: HonorSubmitParam,
    ): HonorResult<Any?>

    companion object {
        const val BASE_URL = "https://appmarket-openapi-drcn.cloud.honor.com/"
    }
}
