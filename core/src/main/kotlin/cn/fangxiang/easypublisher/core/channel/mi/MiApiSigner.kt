package cn.fangxiang.easypublisher.core.channel.mi

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.util.Digest.toHex
import org.bouncycastle.jce.provider.BouncyCastleProvider
import java.io.ByteArrayOutputStream
import java.security.PublicKey
import java.security.Security
import java.security.cert.CertificateFactory
import javax.crypto.Cipher
import kotlin.coroutines.cancellation.CancellationException

/**
 * 小米开放平台的 SIG 字段加密。
 *
 * ## 为什么这些参数一个都不能动
 *
 * 下面的算法串、分组长度、padding、provider 名称全部由小米开放平台的自动发布接口
 * 规定：服务端用与之配对的私钥按同样的分组方式解密，任何一处不一致都会直接鉴权失败，
 * 而我们在没有真实凭据的情况下无法验证。因此这里逐字保持原实现：
 *
 *  - `RSA/NONE/PKCS1Padding` + provider `"BC"`：实测当前 JDK 的 SunJCE 也接受这个
 *    算法串，但原实现显式指定了 `"BC"`，不同 provider 在填充随机数来源等细节上
 *    没必要去赌，这里保持显式指定 BouncyCastle。
 *  - 1024 位密钥 + PKCS#1 v1.5 padding 均低于当今的推荐强度（前者密钥长度不足，
 *    后者存在 Bleichenbacher 类攻击的历史包袱）。但密钥由小米下发、padding 由服务端
 *    决定，**客户端无从选择**。此处记录事实，不做「改进」—— 改了就发不了版。
 *  - 分组按 117 字节明文 / 128 字节密文切分，PKCS#1 v1.5 的 11 字节开销正是这个差值。
 *
 * ## 依赖替换
 *
 *  - BouncyCastle：`bcprov-jdk15on:1.62` → `bcprov-jdk18on:1.86`。仅是 JDK 基线与
 *    维护状态的差别，`org.bouncycastle.jce.provider.BouncyCastleProvider` 包路径、
 *    `Cipher.getInstance(algorithm, "BC")` 的用法、以及 `RSA/NONE/PKCS1Padding` 的
 *    实现语义均未变化，密文字节完全等价。
 *  - 十六进制编码：commons-codec 的 `Hex.encodeHexString` → [toHex]。前者默认输出
 *    小写、每字节两位高位补零，[toHex] 行为与之一致，输出字符串等价。
 */
internal object MiApiSigner {

    /** 小米下发的证书密钥长度，固定 1024 位（详见类注释） */
    private const val KEY_SIZE: Int = 1024

    /** 单个密文分组长度 = 密钥长度 / 8 */
    private const val GROUP_SIZE: Int = KEY_SIZE / 8

    /** 单个明文分组长度。11 字节是 PKCS#1 v1.5 padding 的固定开销 */
    private const val ENCRYPT_GROUP_SIZE: Int = GROUP_SIZE - 11

    private const val KEY_ALGORITHM: String = "RSA/NONE/PKCS1Padding"

    private const val PROVIDER: String = "BC"

    private const val LOG_TAG = "小米加密"

    /**
     * 注册 BouncyCastle。
     *
     * 放在 `init` 里依赖 Kotlin object 的懒加载，首次用到签名时才注册，
     * 不影响其他渠道的启动路径；`addProvider` 对已注册的 provider 是幂等的。
     */
    init {
        if (Security.getProvider(PROVIDER) == null) {
            Security.addProvider(BouncyCastleProvider())
        }
    }

    /**
     * 从 X.509 证书文本中取出公钥。
     *
     * 原实现在这里 `e.printStackTrace()` 之后再抛原异常，绕过日志系统，
     * 并且抛出的是 `CertificateException` 之类的底层异常 —— 上层看到的是一句英文
     * 堆栈，无法判断到底是证书填错了还是接口挂了。证书无效属于凭据问题，
     * 这里归类为 [PublishError.credential]。
     *
     * 注意：异常信息里绝不能带证书内容本身。
     */
    private fun readPublicKey(certificate: String): PublicKey {
        if (certificate.isBlank()) {
            throw PublishError.credential("小米公钥证书为空，请检查 publicKey 参数", MI_CHANNEL_ID)
        }
        try {
            val factory = CertificateFactory.getInstance("X.509")
            val cert = factory.generateCertificate(certificate.byteInputStream())
            return cert.publicKey
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            AppLogger.error(LOG_TAG, "解析小米公钥证书失败（证书内容不记录）", e)
            throw PublishError.credential(
                "小米公钥证书解析失败，请确认填入的是开放平台下载的 .cer 证书文件：${e.message}",
                MI_CHANNEL_ID,
            )
        }
    }

    /**
     * 用公钥分组加密，返回小写十六进制字符串。
     *
     * 循环逐字对齐原实现：复用同一个 `segment` 缓冲区，每轮只把有效长度
     * `(0, segsize)` 交给 `doFinal`，因此末尾残留的上一轮数据不会进入密文。
     */
    fun encrypt(content: String, certificate: String): String {
        val data = content.toByteArray()
        val output = ByteArrayOutputStream()
        val segment = ByteArray(ENCRYPT_GROUP_SIZE)
        val cipher = Cipher.getInstance(KEY_ALGORITHM, PROVIDER)
        cipher.init(Cipher.ENCRYPT_MODE, readPublicKey(certificate))
        var index = 0
        while (index < data.size) {
            val remain = data.size - index
            val segmentSize = minOf(remain, ENCRYPT_GROUP_SIZE)
            System.arraycopy(data, index, segment, 0, segmentSize)
            output.write(cipher.doFinal(segment, 0, segmentSize))
            index += segmentSize
        }
        return output.toByteArray().toHex()
    }
}
