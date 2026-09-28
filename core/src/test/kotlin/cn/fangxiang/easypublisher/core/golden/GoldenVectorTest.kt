package cn.fangxiang.easypublisher.core.golden

import cn.fangxiang.easypublisher.core.channel.mi.MiPackageInfo
import cn.fangxiang.easypublisher.core.channel.mi.buildPushRequestData
import cn.fangxiang.easypublisher.core.channel.mi.buildQueryRequestData
import cn.fangxiang.easypublisher.core.channel.oppo.OppoApiSigner
import cn.fangxiang.easypublisher.core.channel.vivo.VivoApiSigner
import cn.fangxiang.easypublisher.core.net.Json
import cn.fangxiang.easypublisher.core.util.Digest
import java.io.File
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * 黄金向量：把当前实现的签名/序列化输出固化成 fixture，供跨语言重写时逐字节比对。
 *
 * ## 为什么需要这个
 *
 * 签名算法是最不容错的部分 —— 一个字节差异就是鉴权失败，而渠道只回一个笼统的
 * 错误码，极难定位。重写时如果只对照代码「看起来一样」，很容易在排序规则、
 * null 值处理、JSON 字段顺序、数字是否带引号这些细节上走样。
 *
 * 固化成 fixture 后，Go 侧只需断言「对同样输入产出同样字节」，
 * 不需要理解算法为什么这样设计。
 *
 * ## 覆盖范围与不覆盖的部分
 *
 * 覆盖：OPPO/vivo 的待签串与 HMAC-SHA256 签名、小米的 RequestData JSON 与其 MD5、
 * 小米 SIG 的 JSON 明文。这些都是确定性的。
 *
 * **不覆盖小米的 RSA 加密结果** —— PKCS#1 v1.5 填充含随机数，同一明文每次密文都不同，
 * 断言密文相等的测试会永远失败。而 Kotlin 侧要验证解密需要测试私钥，
 * 生成自签证书又需要 bcpkix（本项目只依赖 bcprov）。因此 RSA 部分改由 Go 侧
 * 用运行时生成的密钥对做加解密回环验证，本 fixture 只固化被加密的**明文**。
 *
 * ## 重新生成
 *
 * 只有在**刻意**改变签名行为时才应该重新生成：
 * ```
 * ./gradlew :core:test --tests "*GoldenVectorTest*" -Dgolden.update=true --rerun-tasks
 * ```
 * `--rerun-tasks` 是必需的：本测试通过写文件产生副作用，而 Gradle 的增量构建
 * 不知道 fixture 被外部改过，会判定任务 UP-TO-DATE 而跳过执行，
 * 结果就是「跑了生成命令但文件没变」。
 * 平时这个测试失败就意味着实现被改动了，应当先确认是有意的。
 */
class GoldenVectorTest {

    private val fixtureFile = File(FIXTURE_PATH)

    // ---- 固定输入。用假凭据，签名对任何密钥都成立 ----

    private val oppoCases = listOf(
        Case(
            name = "app-info-典型请求",
            secret = "test-oppo-secret-0123456789",
            params = linkedMapOf(
                "access_token" to "test-access-token",
                "timestamp" to "1700000000000",
                "pkg_name" to "com.example.app",
                "api_sign" to null, // 签名前尚未计算，不参与待签串
            ),
        ),
        Case(
            name = "含特殊字符与中文",
            secret = "test-secret-with-&-and-=-chars",
            params = linkedMapOf(
                "update_desc" to "修复 A&B=C 的问题，支持中文",
                "pkg_name" to "com.example.app",
                "timestamp" to "1700000000001",
            ),
        ),
        Case(
            name = "单参数",
            secret = "s",
            params = linkedMapOf("timestamp" to "1"),
        ),
    )

    private val vivoCases = listOf(
        Case(
            name = "提交更新",
            secret = "test-vivo-access-secret",
            params = linkedMapOf(
                "packageName" to "com.example.app",
                "versionCode" to "1020",
                "apk" to "serialnumber-abc",
                "fileMd5" to "d41d8cd98f00b204e9800998ecf8427e",
                "onlineType" to "1",
                "updateDesc" to "修复若干问题",
            ),
            extra = "app.sync.update.app",
        ),
        Case(
            name = "查询应用详情",
            secret = "test-vivo-access-secret",
            params = linkedMapOf("packageName" to "com.example.app"),
            extra = "app.sync.getappinfo",
        ),
    )

    private val miQueryCases = listOf(
        MiCase("查询-典型", "test@example.com", "com.example.app"),
    )

    private val miPushCases = listOf(
        MiPushCase("推送-立即发布", "test@example.com", "示例应用", "com.example.app", "修复若干问题", 0),
        MiPushCase("推送-定时发布", "test@example.com", "示例应用", "com.example.app", "修复若干问题", 1767225600000),
        MiPushCase("推送-中文与特殊字符", "test@example.com", "示例&应用", "com.example.app", "修复 A&B \"引号\" <尖括号>", 0),
    )

    private data class Case(
        val name: String,
        val secret: String,
        val params: Map<String, String?>,
        val extra: String = "",
    )

    private data class MiCase(val name: String, val account: String, val packageName: String)

    private data class MiPushCase(
        val name: String,
        val account: String,
        val appName: String,
        val packageName: String,
        val updateDesc: String,
        val onlineTime: Long,
    )

    // ---- 计算当前实现的输出 ----

    private fun compute(): String {
        val oppo = oppoCases.map { case ->
            mapOf(
                "name" to case.name,
                "secret" to case.secret,
                "params" to case.params,
                "canonical" to OppoApiSigner.canonicalize(case.params),
                "signature" to OppoApiSigner.sign(case.secret, case.params),
            )
        }
        val vivo = vivoCases.map { case ->
            @Suppress("UNCHECKED_CAST")
            val origin = case.params as Map<String, String>
            val detailed = VivoApiSigner.signDetailed(
                accessKey = "test-vivo-access-key",
                accessSecret = case.secret,
                method = case.extra,
                originParams = origin,
                timestampMillis = FIXED_TIMESTAMP,
            )
            mapOf(
                "name" to case.name,
                "accessKey" to "test-vivo-access-key",
                "accessSecret" to case.secret,
                "method" to case.extra,
                "originParams" to origin,
                "timestampMillis" to FIXED_TIMESTAMP,
                "canonical" to detailed.canonical,
                "signature" to detailed.signature,
                "signedParams" to detailed.params,
            )
        }
        val miQuery = miQueryCases.map { c ->
            val json = buildQueryRequestData(c.account, c.packageName)
            mapOf(
                "name" to c.name,
                "account" to c.account,
                "packageName" to c.packageName,
                "requestDataJson" to json,
                "requestDataMd5" to Digest.md5Hex(json),
            )
        }
        val miPush = miPushCases.map { c ->
            val json = buildPushRequestData(
                account = c.account,
                packageInfo = MiPackageInfo(
                    appName = c.appName,
                    packageName = c.packageName,
                ),
                updateDesc = c.updateDesc,
                onlineTime = c.onlineTime,
            )
            mapOf(
                "name" to c.name,
                "account" to c.account,
                "appName" to c.appName,
                "packageName" to c.packageName,
                "updateDesc" to c.updateDesc,
                "onlineTime" to c.onlineTime,
                "requestDataJson" to json,
                "requestDataMd5" to Digest.md5Hex(json),
            )
        }
        val root = mapOf(
            "_comment" to "由 GoldenVectorTest 生成，供 Go 重写时逐字节比对。重新生成见该测试的 KDoc。",
            "fixedTimestampMillis" to FIXED_TIMESTAMP,
            "oppo" to oppo,
            "vivo" to vivo,
            "miQueryRequestData" to miQuery,
            "miPushRequestData" to miPush,
        )
        // 用字符串而不是解析后的对象比对：黄金向量要的本来就是字节相等。
        // 而且 Moshi 把 JSON 数字读成 Double，1700000000000 会往返成 1.7E12，
        // 按对象比对会误报不一致
        return PRETTY.toJson(root)
    }

    @Test
    fun `黄金向量与当前实现一致`() {
        val current = compute()

        if (UPDATE_FLAG) {
            fixtureFile.parentFile.mkdirs()
            fixtureFile.writeText(current)
            println("已重新生成黄金向量：${fixtureFile.absolutePath}")
            return
        }

        assertTrue(
            fixtureFile.exists(),
            "黄金向量 fixture 不存在：${fixtureFile.absolutePath}。" +
                "用 -Dgolden.update=true 生成",
        )
        val expected = fixtureFile.readText()
        assertEquals(
            expected,
            current,
            "签名或序列化行为发生了变化。若这是有意的，用 -Dgolden.update=true 重新生成，" +
                "并同步更新 Go 侧的断言；若不是有意的，说明重写/改动引入了不兼容",
        )
    }

    @Test
    fun `待签串规则符合预期`() {
        // 这几条断言独立于 fixture，直接表达算法约定。
        // fixture 比对能发现「变了」，这几条能说明「变成了什么」
        assertEquals(
            "access_token=tok&pkg_name=com.example&timestamp=1",
            OppoApiSigner.canonicalize(
                linkedMapOf("timestamp" to "1", "access_token" to "tok", "pkg_name" to "com.example")
            ),
            "应按 key 字典序排序，与传入顺序无关",
        )
        assertEquals(
            "a=1&c=3",
            OppoApiSigner.canonicalize(linkedMapOf("a" to "1", "b" to null, "c" to "3")),
            "value 为 null 的项应整体跳过，连键名也不参与",
        )
        assertEquals(
            "a=1&b=2",
            OppoApiSigner.canonicalize(linkedMapOf("a" to "1&b=2")),
            // 已知歧义：value 里的 & 与 = 不转义，两种输入产生同一待签串。
            // 客户端是签名生成方、参数全由本程序构造，攻击者无法注入，因此不可利用；
            // 固化这个行为是为了重写时不要「顺手修正」它 —— 修正会导致与服务端不一致
        )
    }

    @Test
    fun `vivo 公共参数齐全且 sign 不参与待签串`() {
        val detailed = VivoApiSigner.signDetailed(
            accessKey = "ak",
            accessSecret = "sk",
            method = "app.sync.update.app",
            originParams = mapOf("packageName" to "com.example"),
            timestampMillis = FIXED_TIMESTAMP,
        )
        listOf(
            "access_key", "timestamp", "method", "v",
            "sign_method", "format", "target_app_key",
        ).forEach { key ->
            assertTrue(key in detailed.params, "缺少公共参数 $key")
            assertTrue(
                detailed.canonical.contains("$key="),
                "公共参数 $key 必须参与待签串",
            )
        }
        assertTrue(
            !detailed.canonical.contains("sign="),
            "sign 自身不能参与待签串，否则无法计算",
        )
        assertEquals(FIXED_TIMESTAMP.toString(), detailed.params["timestamp"])
        assertEquals("1.0", detailed.params["v"])
        assertEquals("HMAC-SHA256", detailed.params["sign_method"])
        assertEquals("developer", detailed.params["target_app_key"])
    }

    @Test
    fun `小米 RequestData 省略空的定时发布字段`() {
        val immediate = buildPushRequestData(
            "a@b.c",
            MiPackageInfo(appName = "n", packageName = "p"),
            "desc",
            0,
        )
        val scheduled = buildPushRequestData(
            "a@b.c",
            MiPackageInfo(appName = "n", packageName = "p"),
            "desc",
            1767225600000,
        )
        assertTrue(!immediate.contains("onlineTime"), "立即发布不应带 onlineTime 字段：$immediate")
        assertTrue(scheduled.contains("\"onlineTime\":1767225600000"), "定时发布应带该字段：$scheduled")
        assertTrue(immediate.contains("\"synchroType\":1"), "synchroType 应为裸数字 1：$immediate")
    }

    private companion object {
        const val FIXTURE_PATH = "../go/testdata/golden/signing.json"
        const val FIXED_TIMESTAMP = 1700000000000L
        val UPDATE_FLAG = System.getProperty("golden.update") == "true"
        val PRETTY = Json.moshi.adapter(Any::class.java).indent("  ")
    }
}
