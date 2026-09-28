package cn.fangxiang.easypublisher.core.channel.huawei

import cn.fangxiang.easypublisher.core.channel.ReviewState
import cn.fangxiang.easypublisher.core.net.Json
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull

/**
 * 回归测试：华为草稿态应用的状态查询。
 *
 * 上游 issue #7 —— 应用已在商店上线，但开发者又建了一个新版本草稿且尚未上传 APK
 * （releaseState=7）时，华为的 app-info 接口不返回 versionCode。
 * 原实现把该字段声明为非空，Moshi 在反射构造阶段直接抛
 * `JsonDataException: Required value 'versionCode' missing at $.appInfo`，
 * 用户完全无法通过工具发版。原作者已确认这是工具的 bug，但一年多未修。
 *
 * 这几个用例锁死修复后的行为：**缺字段不抛异常，缺版本号就报 null**。
 */
class HuaweiDraftStateTest {

    private fun parse(body: String): HWAppInfoResp =
        Json.parse<HWAppInfoResp>("huawei", body)

    @Test
    fun `草稿态缺少 versionCode 时不抛异常`() {
        // issue #7 报告人实测到的响应形状：releaseState=7，无 versionCode
        val body = """
            {
              "ret": {"code": 0, "msg": "success"},
              "appInfo": {
                "releaseState": 7,
                "versionNumber": "1.2.0"
              }
            }
        """.trimIndent()

        val resp = parse(body)
        val info = assertNotNull(resp.appInfo, "appInfo 应当解析成功")
        val market = info.toMarketInfo("huawei")

        assertEquals(ReviewState.Draft, market.reviewState)
        assertNull(market.lastVersion, "没有 versionCode 时不应伪造版本号")
        assertEquals("7", market.rawState)
    }

    @Test
    fun `appInfo 整体缺失时不抛异常`() {
        // 应用不存在或凭据无权访问时，华为可能只回 ret
        val body = """{"ret": {"code": 0, "msg": "success"}}"""
        val resp = parse(body)
        assertNull(resp.appInfo)
    }

    @Test
    fun `正常上架应用解析出版本号`() {
        val body = """
            {
              "ret": {"code": 0, "msg": "success"},
              "appInfo": {
                "releaseState": 0,
                "versionCode": 1020,
                "versionNumber": "1.2.0"
              }
            }
        """.trimIndent()

        val market = assertNotNull(parse(body).appInfo).toMarketInfo("huawei")
        assertEquals(ReviewState.Online, market.reviewState)
        assertEquals(1020L, market.lastVersion?.code)
        assertEquals("1.2.0", market.lastVersion?.name)
    }

    @Test
    fun `审核中状态不允许提交`() {
        val body = """
            {
              "ret": {"code": 0, "msg": "success"},
              "appInfo": {"releaseState": 4, "versionCode": 1020, "versionNumber": "1.2.0"}
            }
        """.trimIndent()

        val market = assertNotNull(parse(body).appInfo).toMarketInfo("huawei")
        assertEquals(ReviewState.UnderReview, market.reviewState)
    }

    @Test
    fun `未知状态码保留原始值`() {
        // 华为将来新增状态码时，至少要能把原始值交到用户手里
        val body = """
            {
              "ret": {"code": 0, "msg": "success"},
              "appInfo": {"releaseState": 99}
            }
        """.trimIndent()

        val market = assertNotNull(parse(body).appInfo).toMarketInfo("huawei")
        assertEquals(ReviewState.Unknown, market.reviewState)
        assertEquals("99", market.rawState)
    }

    @Test
    fun `只有 versionCode 没有 versionNumber 时不构造版本`() {
        val body = """
            {
              "ret": {"code": 0, "msg": "success"},
              "appInfo": {"releaseState": 0, "versionCode": 1020}
            }
        """.trimIndent()

        val market = assertNotNull(parse(body).appInfo).toMarketInfo("huawei")
        assertNull(market.lastVersion)
    }
}
