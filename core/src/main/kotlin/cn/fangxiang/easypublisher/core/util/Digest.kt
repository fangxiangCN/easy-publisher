package cn.fangxiang.easypublisher.core.util

import java.io.File
import java.io.FileInputStream
import java.security.MessageDigest

/**
 * 摘要与十六进制编码。
 *
 * 替代原先的 commons-codec 1.4（2009 年版本），改用 JDK 内置 [MessageDigest]。
 */
object Digest {

    private const val BUFFER_SIZE = 64 * 1024

    /**
     * 文件 MD5。
     *
     * 注意：MD5 抗碰撞性已被破解，此处仅用于满足各应用商店接口规定的文件指纹字段，
     * 不作为安全签名使用。
     */
    fun md5Hex(file: File): String = hashFile(file, "MD5")

    fun sha256Hex(file: File): String = hashFile(file, "SHA-256")

    /**
     * 字节数组的 SHA-256。
     *
     * 鸿蒙分片上传要求逐片提交摘要，分片在内存里而不是文件里，
     * 所以需要一个接收 ByteArray 的重载。
     */
    fun sha256Hex(bytes: ByteArray): String =
        MessageDigest.getInstance("SHA-256").digest(bytes).toHex()

    fun md5Hex(text: String): String =
        MessageDigest.getInstance("MD5").digest(text.toByteArray()).toHex()

    private fun hashFile(file: File, algorithm: String): String {
        val digest = MessageDigest.getInstance(algorithm)
        FileInputStream(file).use { input ->
            val buffer = ByteArray(BUFFER_SIZE)
            while (true) {
                val read = input.read(buffer)
                if (read <= 0) break
                digest.update(buffer, 0, read)
            }
        }
        return digest.digest().toHex()
    }

    /**
     * 小写十六进制编码。
     *
     * 每字节固定输出两位，高位补零 —— 否则会产生签名歧义。
     */
    fun ByteArray.toHex(): String {
        val out = StringBuilder(size * 2)
        for (byte in this) {
            val v = byte.toInt() and 0xFF
            out.append(HEX[v ushr 4])
            out.append(HEX[v and 0x0F])
        }
        return out.toString()
    }

    private const val HEX = "0123456789abcdef"
}

/** 人类可读的文件大小 */
fun File.readableSize(): String {
    val units = arrayOf("B", "KB", "MB", "GB", "TB")
    var bytes = length().toDouble()
    var index = 0
    while (bytes >= 1000.0 && index < units.lastIndex) {
        bytes /= 1000.0
        index++
    }
    return String.format("%.2f %s", bytes, units[index])
}
