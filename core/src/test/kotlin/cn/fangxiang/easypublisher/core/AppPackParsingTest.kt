package cn.fangxiang.easypublisher.core

import kotlinx.coroutines.test.runTest
import java.io.File
import java.nio.file.Files
import java.util.zip.ZipEntry
import java.util.zip.ZipOutputStream
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertTrue

/**
 * HarmonyOS App Pack（`.app`）的元信息解析。
 *
 * `.app` 本质是 zip，元信息在根目录的 `pack.info`（JSON），
 * 结构为 `summary.app.{bundleName, version.{code, name}}`。
 * 这些用例用真实的 zip 文件构造 fixture，因此是端到端的解析验证，
 * 不是对着假对象断言。
 */
class AppPackParsingTest {

    private val tempDir: File = Files.createTempDirectory("ep-appack-test").toFile()

    @AfterTest
    fun cleanup() {
        tempDir.deleteRecursively()
    }

    /** 构造一个带指定 pack.info 内容的 .app 包 */
    private fun appPack(name: String, packInfo: String?): File {
        val file = File(tempDir, name)
        ZipOutputStream(file.outputStream()).use { zip ->
            zip.putNextEntry(ZipEntry("module.json"))
            zip.write("{}".toByteArray())
            zip.closeEntry()
            if (packInfo != null) {
                zip.putNextEntry(ZipEntry("pack.info"))
                zip.write(packInfo.toByteArray(Charsets.UTF_8))
                zip.closeEntry()
            }
        }
        return file
    }

    private fun packInfo(
        bundleName: String? = "com.example.harmony",
        code: Long? = 1000012,
        name: String? = "1.0.12",
    ): String {
        val version = buildString {
            append("{")
            val parts = mutableListOf<String>()
            code?.let { parts.add("\"code\":$it") }
            name?.let { parts.add("\"name\":\"$it\"") }
            append(parts.joinToString(","))
            append("}")
        }
        val app = if (bundleName == null) "{\"version\":$version}" else "{\"bundleName\":\"$bundleName\",\"version\":$version}"
        return """{"summary":{"app":$app,"modules":[{"name":"entry"}]}}"""
    }

    @Test
    fun `正常 App Pack 解析出包名与版本`() = runTest {
        val file = appPack("demo.app", packInfo())

        val info = readArtifactInfo(file)

        assertEquals(ArtifactKind.HarmonyAppPack, info.kind)
        assertEquals("com.example.harmony", info.applicationId)
        assertEquals(1000012L, info.versionCode)
        assertEquals("1.0.12", info.versionName)
        assertEquals(file.length(), info.sizeBytes)
    }

    @Test
    fun `缺少 version name 时退回用 code 表示`() = runTest {
        val info = readArtifactInfo(appPack("demo.app", packInfo(name = null)))
        assertEquals("1000012", info.versionName)
    }

    @Test
    fun `缺少 pack info 时提示可能传成了 hap`() = runTest {
        val file = appPack("demo.app", packInfo = null)

        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertEquals(ErrorKind.LocalFile, error.kind)
        assertTrue(
            error.message!!.contains("pack.info"),
            "应指明缺少哪个文件：${error.message}",
        )
        assertTrue(
            error.message!!.contains(".hap"),
            "单个 .hap 没有 pack.info，这是最常见的误用，提示里应当点到：${error.message}",
        )
    }

    @Test
    fun `缺少 bundleName 时明确报错`() = runTest {
        // HSP 的 pack.info 不含 summary/app，属于真实会出现的形状
        val file = appPack("demo.app", """{"summary":{"modules":[{"name":"entry"}]}}""")

        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertTrue(
            error.message!!.contains("bundleName"),
            "应指明缺的是哪个字段：${error.message}",
        )
    }

    @Test
    fun `缺少版本号时明确报错而不是跳过校验`() = runTest {
        // PublishPolicy 的版本比对依赖 versionCode。拿不到就报错，
        // 不能静默降级成 0 或跳过校验却让人以为已经检查过了
        val file = appPack("demo.app", packInfo(code = null))

        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertTrue(
            error.message!!.contains("version.code"),
            "应指明缺的是哪个字段：${error.message}",
        )
    }

    @Test
    fun `pack info 不是合法 JSON 时报解析失败`() = runTest {
        val file = appPack("demo.app", "this is not json")
        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertEquals(ErrorKind.LocalFile, error.kind)
    }

    @Test
    fun `hap 文件不被当作 App Pack 接受`() = runTest {
        // .hap 是旧的单模块包，不是 HarmonyOS 5+ 的商店发布包
        val file = File(tempDir, "entry-default.hap").apply { writeText("x".repeat(64)) }

        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertEquals(ErrorKind.LocalFile, error.kind)
        assertTrue(
            error.message!!.contains("hap"),
            "应当回显收到的格式：${error.message}",
        )
    }

    @Test
    fun `按扩展名识别制品类型`() {
        assertEquals(ArtifactKind.Apk, ArtifactKind.ofExtension("apk"))
        assertEquals(ArtifactKind.HarmonyAppPack, ArtifactKind.ofExtension("app"))
        assertEquals(null, ArtifactKind.ofExtension("hap"))
        assertEquals(null, ArtifactKind.ofExtension("aab"))
        assertEquals(ArtifactKind.HarmonyAppPack, ArtifactKind.of(File("x/y/demo.APP")))
    }

    @Test
    fun `空文件被拒绝`() = runTest {
        val file = File(tempDir, "empty.app").apply { writeText("") }
        val error = assertFailsWith<PublishError> { readArtifactInfo(file) }
        assertEquals(ErrorKind.LocalFile, error.kind)
    }
}
