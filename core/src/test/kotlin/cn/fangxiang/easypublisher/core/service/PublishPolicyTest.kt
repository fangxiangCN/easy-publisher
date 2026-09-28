package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ArtifactInfo
import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.ReviewState
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull

class PublishPolicyTest {

    private fun apk(versionCode: Long) = ArtifactInfo(
        path = "/tmp/app.apk",
        applicationId = "com.example.app",
        versionCode = versionCode,
        versionName = "1.0.0",
        sizeBytes = 1024,
    )

    private fun market(
        versionCode: Long?,
        state: ReviewState = ReviewState.Online,
        canSubmit: Boolean = true,
    ) = MarketInfo(
        channelId = "huawei",
        reviewState = state,
        lastVersion = versionCode?.let { MarketInfo.Version(it, "1.0.0") },
        canSubmit = canSubmit,
    )

    @Test
    fun `版本号更高时允许提交`() {
        assertNull(PublishPolicy.reject(apk(11), market(10)))
    }

    @Test
    fun `版本号相同时默认拒绝`() {
        val error = PublishPolicy.reject(apk(10), market(10))
        assertNotNull(error)
        assertEquals(ErrorKind.Precondition, error.kind)
    }

    @Test
    fun `版本号相同时 AllowSame 放行`() {
        assertNull(
            PublishPolicy.reject(apk(10), market(10), PublishPolicy.VersionRule.AllowSame)
        )
    }

    @Test
    fun `版本号更低时即使 AllowSame 也拒绝`() {
        // 这是与上游 PR 的关键差别：那个实现跳过整个判断，连低于线上版本也放行，
        // 而版本号变低几乎总是选错了 APK 文件。
        val error = PublishPolicy.reject(apk(9), market(10), PublishPolicy.VersionRule.AllowSame)
        assertNotNull(error)
        assertEquals(ErrorKind.Precondition, error.kind)
    }

    @Test
    fun `Skip 跳过全部版本号校验`() {
        assertNull(PublishPolicy.reject(apk(1), market(10), PublishPolicy.VersionRule.Skip))
    }

    @Test
    fun `线上版本未知时不阻断`() {
        // 应用在商店只有未上传 APK 的草稿版本时，渠道不返回版本号。
        // 这正是上游 issue #7 的场景：原实现把该字段声明为非空，直接抛 JsonDataException。
        assertNull(PublishPolicy.reject(apk(1), market(null)))
    }

    @Test
    fun `渠道声明不可提交时拒绝`() {
        val error = PublishPolicy.reject(apk(11), market(10, canSubmit = false))
        assertNotNull(error)
        assertEquals(ErrorKind.Precondition, error.kind)
    }

    @Test
    fun `审核中时拒绝`() {
        val error = PublishPolicy.reject(
            apk(11),
            market(10, state = ReviewState.UnderReview, canSubmit = false),
        )
        assertNotNull(error)
    }

    @Test
    fun `没有渠道状态时不阻断`() {
        // 状态查询失败不应阻止提交尝试，让渠道自己拒绝并给出真实原因
        assertNull(PublishPolicy.reject(apk(1), null))
    }
}
