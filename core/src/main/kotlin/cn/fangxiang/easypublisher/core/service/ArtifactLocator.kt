package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import java.io.File

/**
 * 为渠道定位对应的制品文件。
 *
 * 多渠道包场景下每个渠道一个包，靠文件名里的渠道标识匹配
 * （如 `app-HUAWEI-1.2.0.apk`、`app-HARMONY-1.2.0.app`）。
 */
object ArtifactLocator {

    /**
     * @param artifactPath 单个制品文件，或存放多渠道包的目录
     * @param multiChannel 是否按渠道标识匹配文件
     */
    fun locate(artifactPath: File, channel: Channel, multiChannel: Boolean): File {
        if (!artifactPath.exists()) {
            throw PublishError.localFile("路径不存在：${artifactPath.absolutePath}")
        }
        val accepted = channel.artifactExtensions
        if (artifactPath.isFile) {
            // 扩展名必须被该渠道接受。把 .apk 传给鸿蒙渠道这类错误应当立刻暴露，
            // 而不是等到上传了几百兆之后被服务端拒绝
            if (!hasAcceptedExtension(artifactPath, accepted)) {
                throw PublishError.localFile(
                    "${channel.displayName} 渠道不接受 .${artifactPath.extension} 文件" +
                        "（${artifactPath.name}），该渠道需要：${accepted.joinToString("、") { ".$it" }}",
                )
            }
            if (multiChannel && !matchesTag(artifactPath, channel)) {
                throw PublishError.localFile(
                    "文件名 ${artifactPath.name} 不含渠道标识 ${channel.fileNameTag}，" +
                        "与 ${channel.displayName} 渠道不匹配",
                )
            }
            return artifactPath
        }

        // 目录：先按扩展名筛，再按渠道标识挑
        val candidates = artifactPath.walkTopDown()
            .maxDepth(MAX_DEPTH)
            .filter { it.isFile && hasAcceptedExtension(it, accepted) }
            .toList()

        if (candidates.isEmpty()) {
            throw PublishError.localFile(
                "目录下没有找到 ${accepted.joinToString("、") { ".$it" }} 文件：" +
                    artifactPath.absolutePath,
            )
        }
        val matched = candidates.filter { matchesTag(it, channel) }
        return when {
            matched.size == 1 -> matched.single()

            matched.isEmpty() -> throw PublishError.localFile(
                "目录下没有找到文件名包含 ${channel.fileNameTag} 的制品" +
                    "（${channel.displayName} 渠道）：${artifactPath.absolutePath}",
            )

            // 多个候选时取最新修改的。不报错是因为多渠道包目录里同一渠道
            // 出现多个历史版本很常见，取最新符合直觉；但会在日志里说明依据
            else -> matched.maxByOrNull { it.lastModified() }!!
        }
    }

    private fun hasAcceptedExtension(file: File, accepted: List<String>): Boolean =
        accepted.any { file.name.endsWith(".$it", ignoreCase = true) }

    private fun matchesTag(file: File, channel: Channel): Boolean =
        file.name.contains(channel.fileNameTag, ignoreCase = true)

    private const val MAX_DEPTH = 3
}
