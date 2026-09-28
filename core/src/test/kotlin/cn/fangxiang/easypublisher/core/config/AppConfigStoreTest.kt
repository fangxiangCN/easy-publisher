package cn.fangxiang.easypublisher.core.config

import cn.fangxiang.easypublisher.core.PublishError
import kotlinx.coroutines.test.runTest
import java.io.File
import java.nio.file.Files
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.test.fail

class AppConfigStoreTest {

    private val tempDir: File = Files.createTempDirectory("ep-config-test").toFile()

    private val store = AppConfigStore(tempDir)

    @AfterTest
    fun cleanup() {
        tempDir.deleteRecursively()
    }

    private fun sample(id: String = "com.example.app") = AppConfig(
        name = "示例应用",
        applicationId = id,
        channels = listOf(
            AppConfig.ChannelConfig(
                name = "huawei",
                enabled = true,
                params = listOf(
                    AppConfig.Param("client_id", "id-123"),
                    AppConfig.Param("client_secret", "secret-456"),
                ),
            )
        ),
    )

    @Test
    fun `保存后能读回`() = runTest {
        store.save(sample())
        val loaded = store.get("com.example.app")
        assertEquals("示例应用", loaded?.name)
        assertEquals("secret-456", loaded?.channel("huawei")?.value("client_secret"))
    }

    @Test
    fun `配置文件权限收紧为 600`() = runTest {
        store.save(sample())
        val file = File(tempDir, "com.example.app.json")
        assertTrue(file.exists())

        val perms = runCatching { Files.getPosixFilePermissions(file.toPath()) }.getOrNull()
            ?: return@runTest // 非 POSIX 文件系统跳过

        assertEquals(
            "rw-------",
            java.nio.file.attribute.PosixFilePermissions.toString(perms),
            "凭据文件必须只有所有者可读写，否则同机其他用户能读到各商店密钥",
        )
    }

    @Test
    fun `删除后文件真正消失且不留 bak`() = runTest {
        store.save(sample())
        assertTrue(store.remove("com.example.app"))

        assertNull(store.get("com.example.app"))
        val leftovers = tempDir.listFiles()?.map { it.name } ?: emptyList()
        assertTrue(
            leftovers.none { it.contains("com.example.app") },
            "原实现只把文件改名为 .bak，明文凭据仍留在磁盘上。残留：$leftovers",
        )
    }

    @Test
    fun `删除不存在的配置返回 false`() = runTest {
        assertFalse(store.remove("com.example.absent"))
    }

    @Test
    fun `覆盖保存不产生中间态`() = runTest {
        store.save(sample())
        store.save(sample().copy(name = "改名后"))
        assertEquals("改名后", store.get("com.example.app")?.name)
        // 原子写实现会用临时文件，结束后不应残留
        val temps = tempDir.listFiles()?.filter { it.name.endsWith(".tmp") } ?: emptyList()
        assertTrue(temps.isEmpty(), "临时文件未清理：${temps.map { it.name }}")
    }

    @Test
    fun `非法包名被拒绝`() = runTest {
        // 配置文件名直接由包名拼成，未过滤的 ../ 会写到目录外
        try {
            store.save(sample(id = "../../etc/passwd"))
            fail("非法包名应当被拒绝")
        } catch (e: PublishError) {
            assertEquals(cn.fangxiang.easypublisher.core.ErrorKind.Configuration, e.kind)
        }
    }

    @Test
    fun `require 在缺失时给出可执行提示`() = runTest {
        try {
            store.require("com.example.absent")
            fail("应当抛出异常")
        } catch (e: PublishError) {
            assertTrue(e.message!!.contains("app add"), "提示应包含下一步动作：${e.message}")
        }
    }

    @Test
    fun `凭据不出现在 toString 中`() {
        val text = sample().toString() + sample().channels.first().toString() +
            AppConfig.Param("client_secret", "secret-456").toString()
        assertFalse(
            text.contains("secret-456"),
            "toString 泄漏了凭据，原实现正是这样把密钥打进日志的：$text",
        )
    }
}
