package cn.fangxiang.easypublisher.core.config

import cn.fangxiang.easypublisher.core.AppPaths
import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.net.Json
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File
import java.nio.file.AtomicMoveNotSupportedException
import java.nio.file.Files
import java.nio.file.StandardCopyOption

/**
 * 应用配置的持久化。
 *
 * 相对原实现修正了三处：
 *
 * 1. **原子写。** 原 `writeApkConfig` 直接 `file.writeText(json)`，非原子。
 *    并发写同一文件会产生截断的 JSON，而读取失败只 `printStackTrace` 并静默返回 null，
 *    用户的全部渠道凭据会"凭空消失"且无迹可查。此处写临时文件后 `ATOMIC_MOVE` 替换。
 * 2. **权限 600。** 原实现继承 umask（通常 644），同机其他用户可读凭据。
 * 3. **真正删除。** 原 `removeConfig` 只是 `renameTo(file + ".bak")` 并不删除内容，
 *    而列表扫描只认 `.json` —— 用户以为删掉了凭据，明文其实长期留在磁盘上。
 *
 * 另：原 `saveApkConfig` 的流程是先 `removeConfig`（把原文件改名）再 `saveConfig`，
 * 中间失败会导致配置彻底丢失且无回滚。此处 [save] 是单次原子替换，不存在中间态。
 */
class AppConfigStore(
    private val dir: File = AppPaths.appsDir,
) {

    private val adapter = Json.adapter<AppConfig>().indent("  ")

    private val lock = Mutex()

    suspend fun list(): List<AppConfig> = withContext(Dispatchers.IO) {
        val files = dir.listFiles { f: File -> f.isFile && f.name.endsWith(SUFFIX) }
            ?: return@withContext emptyList()
        files.sortedBy { it.name }.mapNotNull { read(it) }
    }

    suspend fun get(applicationId: String): AppConfig? = withContext(Dispatchers.IO) {
        read(fileOf(applicationId))
    }

    suspend fun require(applicationId: String): AppConfig =
        get(applicationId) ?: throw PublishError.configuration(
            "未找到应用配置：$applicationId，请先执行 `easy-publisher app add --id $applicationId`"
        )

    suspend fun save(config: AppConfig): Unit = lock.withLock {
        withContext(Dispatchers.IO) {
            ApplicationId.validate(config.applicationId)
            AppPaths.ensureSecureDir(dir)
            val target = fileOf(config.applicationId)
            val json = adapter.toJson(config)
            writeAtomically(target, json)
            AppLogger.info(TAG, "已保存配置：${config.applicationId}")
        }
    }

    /**
     * 删除配置。先用随机数据覆写再删除，降低明文凭据在磁盘上残留的概率。
     *
     * 注意：在带日志结构或写时复制的文件系统（APFS、Btrfs、SSD 磨损均衡）上，
     * 覆写并不能保证物理擦除。真正敏感的凭据应当在渠道后台轮换。
     */
    suspend fun remove(applicationId: String): Boolean = lock.withLock {
        withContext(Dispatchers.IO) {
            val file = fileOf(applicationId)
            if (!file.exists()) return@withContext false
            runCatching {
                val size = file.length().toInt().coerceAtMost(1 shl 20)
                if (size > 0) {
                    val noise = ByteArray(size)
                    java.security.SecureRandom().nextBytes(noise)
                    file.writeBytes(noise)
                }
            }
            val deleted = file.delete()
            AppLogger.info(TAG, "已删除配置：$applicationId")
            deleted
        }
    }

    private fun read(file: File): AppConfig? {
        if (!file.exists()) return null
        return try {
            adapter.fromJson(file.readText())
        } catch (e: Exception) {
            // 原实现只 printStackTrace，配置损坏时无迹可查
            AppLogger.error(TAG, "配置文件解析失败：${file.absolutePath}", e)
            null
        }
    }

    private fun writeAtomically(target: File, content: String) {
        val temp = File.createTempFile(".${target.name}", ".tmp", target.parentFile)
        try {
            AppPaths.restrictToOwner(temp)
            temp.writeText(content)
            try {
                Files.move(
                    temp.toPath(),
                    target.toPath(),
                    StandardCopyOption.REPLACE_EXISTING,
                    StandardCopyOption.ATOMIC_MOVE,
                )
            } catch (_: AtomicMoveNotSupportedException) {
                // 某些文件系统不支持原子移动，退化为普通替换
                Files.move(temp.toPath(), target.toPath(), StandardCopyOption.REPLACE_EXISTING)
            }
            AppPaths.restrictToOwner(target)
        } finally {
            if (temp.exists()) temp.delete()
        }
    }

    private fun fileOf(applicationId: String): File =
        File(dir, ApplicationId.validate(applicationId) + SUFFIX)

    private companion object {
        const val SUFFIX = ".json"
        const val TAG = "config"
    }
}
