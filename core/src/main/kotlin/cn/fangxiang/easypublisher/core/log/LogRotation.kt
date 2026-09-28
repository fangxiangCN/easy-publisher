package cn.fangxiang.easypublisher.core.log

import java.io.File
import java.util.concurrent.TimeUnit

/**
 * 日志保留策略。
 *
 * 原实现按天分文件但从不清理，`~/.XiaoZhuan/log/` 会无限增长。
 */
internal object LogRotation {

    private const val RETAIN_DAYS = 14L

    private const val MAX_FILE_BYTES = 16L * 1024 * 1024

    fun cleanup(dir: File) {
        val files = dir.listFiles { f: File -> f.isFile && f.name.endsWith(".log") } ?: return
        val deadline = System.currentTimeMillis() - TimeUnit.DAYS.toMillis(RETAIN_DAYS)
        for (file in files) {
            if (file.lastModified() < deadline) {
                file.delete()
            } else if (file.length() > MAX_FILE_BYTES) {
                roll(file)
            }
        }
    }

    /** 单文件超限时归档为 .1，只保留一份历史 */
    private fun roll(file: File) {
        val archived = File(file.parentFile, file.name + ".1")
        if (archived.exists()) archived.delete()
        file.renameTo(archived)
    }
}
