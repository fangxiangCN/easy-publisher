package cn.fangxiang.easypublisher.core

import java.io.File
import java.nio.file.Files
import java.nio.file.attribute.PosixFilePermission
import java.nio.file.attribute.PosixFilePermissions

/**
 * 应用数据目录。
 *
 * 可通过环境变量 `EP_HOME` 覆盖根目录，便于测试与 CI 隔离。
 */
object AppPaths {

    private const val ENV_HOME = "EP_HOME"

    private const val DIR_NAME = ".easy-publisher"

    val rootDir: File
        get() {
            val override = System.getenv(ENV_HOME)?.takeIf { it.isNotBlank() }
            return if (override != null) {
                File(override)
            } else {
                val home = requireNotNull(System.getProperty("user.home")) { "无法确定用户主目录" }
                File(home, DIR_NAME)
            }
        }

    /** 应用配置目录，权限收紧为 700 */
    val appsDir: File get() = File(rootDir, "apps")

    val logDir: File get() = File(rootDir, "log")

    /**
     * 创建目录并尽可能收紧权限为 700。
     *
     * 非 POSIX 文件系统（Windows）不支持该属性，静默跳过 —— 此时目录权限由系统 ACL 决定。
     */
    fun ensureSecureDir(dir: File): File {
        dir.mkdirs()
        runCatching {
            Files.setPosixFilePermissions(
                dir.toPath(),
                PosixFilePermissions.fromString("rwx------"),
            )
        }
        return dir
    }

    /**
     * 将文件权限收紧为 600，使同机其他用户无法读取。
     *
     * 凭据文件必须调用此方法；非 POSIX 文件系统静默跳过。
     */
    fun restrictToOwner(file: File) {
        runCatching {
            Files.setPosixFilePermissions(
                file.toPath(),
                setOf(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE),
            )
        }
    }
}
