package cn.fangxiang.easypublisher.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import net.dongliu.apk.parser.ApkFile
import java.io.File

data class ApkInfo(
    val path: String,
    val applicationId: String,
    val versionCode: Long,
    val versionName: String,
    val sizeBytes: Long,
) {
    override fun toString(): String =
        "ApkInfo($applicationId, versionCode=$versionCode, versionName=$versionName)"
}

/**
 * 解析 APK 元信息。
 */
suspend fun readApkInfo(file: File): ApkInfo = withContext(Dispatchers.IO) {
    if (!file.exists()) {
        throw PublishError.localFile("APK 文件不存在：${file.absolutePath}")
    }
    if (!file.isFile) {
        throw PublishError.localFile("不是文件：${file.absolutePath}")
    }
    if (file.length() == 0L) {
        throw PublishError.localFile("APK 文件为空：${file.absolutePath}")
    }
    try {
        val meta = ApkFile(file).use { it.apkMeta }
        ApkInfo(
            path = file.absolutePath,
            applicationId = meta.packageName,
            versionCode = meta.versionCode,
            versionName = meta.versionName,
            sizeBytes = file.length(),
        )
    } catch (e: Exception) {
        throw PublishError.localFile("解析 APK 失败：${file.absolutePath}", e)
    }
}

/**
 * 校验包名格式。
 *
 * 配置文件名直接由包名拼成，原实现只校验非空，未过滤 `/` 与 `..`。
 */
object ApplicationId {

    private val PATTERN = Regex("^[A-Za-z][A-Za-z0-9_]*(\\.[A-Za-z0-9_]+)+$")

    fun validate(value: String): String {
        val trimmed = value.trim()
        if (trimmed.isEmpty()) {
            throw PublishError.configuration("包名不能为空")
        }
        if (!PATTERN.matches(trimmed)) {
            throw PublishError.configuration(
                "包名格式不合法：$trimmed（应形如 com.example.app）"
            )
        }
        return trimmed
    }

    fun isValid(value: String): Boolean = PATTERN.matches(value.trim())
}
