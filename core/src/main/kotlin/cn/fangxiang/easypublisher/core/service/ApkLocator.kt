package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import java.io.File

/**
 * 为渠道定位对应的 APK 文件。
 *
 * 多渠道包场景下每个渠道一个包，靠文件名里的渠道标识匹配（如 `app-HUAWEI-1.2.0.apk`）。
 */
object ApkLocator {

    /**
     * @param apkPath 单个 APK 文件，或存放多渠道包的目录
     * @param multiChannel 是否按渠道标识匹配文件
     */
    fun locate(apkPath: File, channel: Channel, multiChannel: Boolean): File {
        if (!apkPath.exists()) {
            throw PublishError.localFile("路径不存在：${apkPath.absolutePath}")
        }
        if (apkPath.isFile) {
            if (multiChannel && !matches(apkPath, channel)) {
                throw PublishError.localFile(
                    "文件名 ${apkPath.name} 不含渠道标识 ${channel.fileNameTag}，" +
                        "与 ${channel.displayName} 渠道不匹配"
                )
            }
            return apkPath
        }
        // 目录：按渠道标识挑选
        val candidates = apkPath.walkTopDown()
            .maxDepth(MAX_DEPTH)
            .filter { it.isFile && it.name.endsWith(".apk", ignoreCase = true) }
            .toList()

        if (candidates.isEmpty()) {
            throw PublishError.localFile("目录下没有找到 APK 文件：${apkPath.absolutePath}")
        }
        val matched = candidates.filter { matches(it, channel) }
        return when {
            matched.size == 1 -> matched.single()

            matched.isEmpty() -> throw PublishError.localFile(
                "目录下没有找到文件名包含 ${channel.fileNameTag} 的 APK" +
                    "（${channel.displayName} 渠道）：${apkPath.absolutePath}"
            )

            else -> {
                // 多个候选时取最新修改的，并说明选择依据，避免静默挑错包
                matched.maxByOrNull { it.lastModified() }!!
            }
        }
    }

    private fun matches(file: File, channel: Channel): Boolean =
        file.name.contains(channel.fileNameTag, ignoreCase = true)

    private const val MAX_DEPTH = 3
}
