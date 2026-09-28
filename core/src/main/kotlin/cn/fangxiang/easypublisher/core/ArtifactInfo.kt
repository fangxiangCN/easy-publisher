package cn.fangxiang.easypublisher.core

import cn.fangxiang.easypublisher.core.net.Json
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import net.dongliu.apk.parser.ApkFile
import java.io.File
import java.util.zip.ZipFile

/** 制品类型。决定怎么解析元信息，也决定能发给哪些渠道 */
enum class ArtifactKind(val extensions: List<String>, val label: String) {
    /** Android 安装包 */
    Apk(listOf("apk"), "APK"),

    /**
     * HarmonyOS 5+ 的 App Pack。
     *
     * 与 APK 的差异不只是文件格式：它必须走华为的分片上传接口
     * （upload/multipart/init → parts → compose），再用 v3 的 app-package-info
     * 关联到草稿。普通的 upload-url + 单次 PUT 不适用于 .app。
     */
    HarmonyAppPack(listOf("app"), "App Pack"),
    ;

    val extensionPattern: String get() = extensions.joinToString("/.") { ".$it" }

    companion object {
        fun of(file: File): ArtifactKind? = ofExtension(file.extension.lowercase())

        fun ofExtension(ext: String): ArtifactKind? =
            entries.firstOrNull { ext in it.extensions }
    }
}

data class ArtifactInfo(
    val path: String,
    val applicationId: String,
    val versionCode: Long,
    val versionName: String,
    val sizeBytes: Long,
    val kind: ArtifactKind = ArtifactKind.Apk,
) {
    val fileName: String get() = File(path).name

    override fun toString(): String =
        "ArtifactInfo(${kind.label} $applicationId, versionCode=$versionCode, versionName=$versionName)"
}

/**
 * 按扩展名分派解析制品元信息。
 */
suspend fun readArtifactInfo(file: File): ArtifactInfo = withContext(Dispatchers.IO) {
    if (!file.exists()) {
        throw PublishError.localFile("制品文件不存在：${file.absolutePath}")
    }
    if (!file.isFile) {
        throw PublishError.localFile("不是文件：${file.absolutePath}")
    }
    if (file.length() == 0L) {
        throw PublishError.localFile("制品文件为空：${file.absolutePath}")
    }
    when (ArtifactKind.of(file)) {
        ArtifactKind.Apk -> readApk(file)
        ArtifactKind.HarmonyAppPack -> readAppPack(file)
        null -> throw PublishError.localFile(
            "不支持的制品格式 .${file.extension}（${file.name}）。支持：" +
                ArtifactKind.entries.joinToString("、") { "${it.label}(${it.extensionPattern})" },
        )
    }
}

private fun readApk(file: File): ArtifactInfo {
    return try {
        val meta = ApkFile(file).use { it.apkMeta }
        ArtifactInfo(
            path = file.absolutePath,
            applicationId = meta.packageName,
            versionCode = meta.versionCode,
            versionName = meta.versionName,
            sizeBytes = file.length(),
            kind = ArtifactKind.Apk,
        )
    } catch (e: Exception) {
        throw PublishError.localFile("解析 APK 失败：${file.absolutePath}", e)
    }
}

/**
 * 解析 HarmonyOS App Pack。
 *
 * `.app` 本质是 zip，元信息在根目录的 `pack.info`（JSON），结构为
 * `summary.app.{bundleName, version.{code, name}}`。
 *
 * 这比 APK 简单：APK 的版本信息在二进制 AndroidManifest.xml 里、需要专门的解析器，
 * 而 pack.info 是纯 JSON，标准库的 zip 加 Moshi 就够了。
 */
private fun readAppPack(file: File): ArtifactInfo {
    val json = try {
        ZipFile(file).use { zip ->
            val entry = zip.getEntry(PACK_INFO_ENTRY)
                ?: throw PublishError.localFile(
                    "App Pack 里找不到 $PACK_INFO_ENTRY：${file.name}。" +
                        "请确认这是 DevEco Studio 打出的 .app 发布包，而不是单个 .hap",
                )
            zip.getInputStream(entry).reader(Charsets.UTF_8).use { it.readText() }
        }
    } catch (e: PublishError) {
        throw e
    } catch (e: Exception) {
        throw PublishError.localFile("读取 App Pack 失败：${file.absolutePath}", e)
    }

    val pack = try {
        Json.parse<PackInfo>(HARMONY_LOG_TAG, json)
    } catch (e: PublishError) {
        throw PublishError.localFile("App Pack 的 $PACK_INFO_ENTRY 无法解析：${file.name}", e)
    }

    val app = pack.summary?.app
    val bundleName = app?.bundleName?.takeIf { it.isNotBlank() }
        ?: throw PublishError.localFile(
            "App Pack 的 $PACK_INFO_ENTRY 缺少 summary.app.bundleName，无法确定包名：${file.name}",
        )
    // 版本号缺失时不做静默降级：PublishPolicy 的版本比对依赖它，
    // 拿不到就明确报错，而不是跳过校验却让人以为已经检查过了
    val code = app.version?.code
        ?: throw PublishError.localFile(
            "App Pack 的 $PACK_INFO_ENTRY 缺少 summary.app.version.code，无法做版本号校验：${file.name}",
        )
    val name = app.version.name?.takeIf { it.isNotBlank() } ?: code.toString()

    return ArtifactInfo(
        path = file.absolutePath,
        applicationId = bundleName,
        versionCode = code,
        versionName = name,
        sizeBytes = file.length(),
        kind = ArtifactKind.HarmonyAppPack,
    )
}

private const val PACK_INFO_ENTRY = "pack.info"

/** 解析 pack.info 失败时错误信息里标注的来源 */
internal const val HARMONY_LOG_TAG = "harmony"

/**
 * `.app` 包的 pack.info。
 *
 * 全部字段可空：HSP 的 pack.info 不含 summary/app，缺字段不该导致解析崩溃，
 * 而应走到上面带中文说明的校验分支。
 */
private data class PackInfo(val summary: PackSummary? = null)

private data class PackSummary(val app: PackApp? = null)

private data class PackApp(
    val bundleName: String? = null,
    val version: PackVersion? = null,
)

private data class PackVersion(
    val code: Long? = null,
    val name: String? = null,
)

/**
 * 校验包名格式。
 *
 * 配置文件名直接由包名拼成，未过滤的 `/` 与 `..` 会写到目录外。
 * 鸿蒙的 bundleName 与 Android 包名规则一致（点分、至少两段）。
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
