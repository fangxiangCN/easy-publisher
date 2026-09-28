package cn.fangxiang.easypublisher.core.net

import cn.fangxiang.easypublisher.core.PublishError
import com.squareup.moshi.JsonAdapter
import com.squareup.moshi.Moshi
import com.squareup.moshi.kotlin.reflect.KotlinJsonAdapterFactory
import okhttp3.OkHttpClient
import retrofit2.Retrofit
import retrofit2.converter.moshi.MoshiConverterFactory

/**
 * 统一的 JSON 处理。
 *
 * 原项目同时依赖 Moshi（华为/荣耀，Retrofit）与 Gson（小米/OPPO/vivo，手写解析），
 * 其中 Gson 那部分在构造函数里链式取值 `obj.get("x").asString`，字段缺失直接 NPE。
 * 此处统一到 Moshi，响应模型全部改为可空字段 + 业务层显式校验。
 */
object Json {

    val moshi: Moshi = Moshi.Builder()
        .add(KotlinJsonAdapterFactory())
        .build()

    inline fun <reified T> adapter(): JsonAdapter<T> = moshi.adapter(T::class.java)

    /**
     * 解析响应体，失败时抛出带渠道与原始响应的 [PublishError]。
     */
    inline fun <reified T> parse(channel: String, body: String): T {
        val result = try {
            adapter<T>().fromJson(body)
        } catch (e: Exception) {
            throw PublishError.protocol(
                channel = channel,
                message = "解析 ${T::class.simpleName} 失败，渠道接口可能已变更：${e.message}",
                raw = body.take(2000),
                cause = e,
            )
        }
        return result ?: throw PublishError.protocol(
            channel = channel,
            message = "渠道返回空响应体",
            raw = body.take(2000),
        )
    }
}

object RetrofitFactory {

    inline fun <reified T> create(baseUrl: String, client: OkHttpClient): T =
        Retrofit.Builder()
            .baseUrl(baseUrl)
            .callFactory(client)
            .addConverterFactory(MoshiConverterFactory.create(Json.moshi))
            .build()
            .create(T::class.java)
}
