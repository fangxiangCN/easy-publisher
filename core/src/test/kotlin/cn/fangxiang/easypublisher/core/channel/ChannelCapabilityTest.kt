package cn.fangxiang.easypublisher.core.channel

import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.PublishError
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/**
 * 能力矩阵的一致性约束。
 *
 * 这些测试的作用不是验证渠道行为（那需要真实凭据），而是防止矩阵与实现脱节：
 * 有人给渠道加了新的停留点却忘了更新 capability，或者反过来宣称支持一个
 * 实现里根本没有的阶段。
 */
class ChannelCapabilityTest {

    private val channels = ChannelRegistry.all()

    @Test
    fun `注册了五个渠道`() {
        assertEquals(
            listOf("huawei", "mi", "oppo", "vivo", "honor"),
            channels.map { it.id },
        )
    }

    @Test
    fun `每个渠道都以送审作为最后一个阶段`() {
        channels.forEach { channel ->
            val stages = channel.capability.supportedStages
            assertTrue(stages.isNotEmpty(), "${channel.id} 未声明任何阶段")
            assertEquals(
                ReleaseStage.SubmitReview,
                stages.last(),
                "${channel.id} 的阶段列表必须以 SubmitReview 结尾",
            )
        }
    }

    @Test
    fun `阶段声明必须按流程顺序`() {
        val order = ReleaseStage.entries
        channels.forEach { channel ->
            val stages = channel.capability.supportedStages
            val indices = stages.map { order.indexOf(it) }
            assertEquals(
                indices.sorted(),
                indices,
                "${channel.id} 的 supportedStages 未按流程顺序排列",
            )
            assertEquals(indices.distinct(), indices, "${channel.id} 的 supportedStages 有重复")
        }
    }

    @Test
    fun `送审后一律不允许自动重试`() {
        // 服务端可能已受理而响应丢失，重试会重复送审且不可撤销
        channels.forEach { channel ->
            assertFalse(
                channel.capability.automaticRetryAfterSubmission,
                "${channel.id} 不应允许送审后自动重试",
            )
        }
    }

    @Test
    fun `送审一律要求显式确认`() {
        channels.forEach { channel ->
            assertTrue(
                channel.capability.requiresExplicitConfirmation,
                "${channel.id} 的送审必须要求显式确认",
            )
        }
    }

    /**
     * 诚实性守卫。
     *
     * 本项目的渠道逻辑从上游继承而来，**从未用真实凭据向任何商店实际提交过**。
     * 因此所有渠道的 evidence 都必须是 [Evidence.CodeObservation]。
     *
     * 当你在某个渠道上真机跑通之后，请把它改成 VerifiedInProduction 并同步更新
     * 这个测试 —— 这个断言故意写成会失败的形式，就是为了不让「已实测」
     * 被随手加上去。
     */
    @Test
    fun `所有渠道的证据等级都还是代码推断`() {
        channels.forEach { channel ->
            assertEquals(
                Evidence.CodeObservation,
                channel.capability.evidence,
                "${channel.id} 的 evidence 被改成了 ${channel.capability.evidence.label}，" +
                    "但该渠道尚未用真实凭据验证过。若确实已验证，请一并更新本测试",
            )
        }
    }

    @Test
    fun `撤回能力一律标为未验证`() {
        // 上游 issue 的结论是「各商店 API 都不提供撤销」，但没人逐个后台确认过，
        // 所以是 NotVerified 而不是 Unsupported —— 这两者对使用者的含义不同
        channels.forEach { channel ->
            assertEquals(
                Withdrawal.NotVerified,
                channel.capability.withdrawal,
                "${channel.id} 的撤回能力未经验证，不应声称支持或不支持",
            )
        }
    }

    // ---- 各渠道的粒度差异 ----

    @Test
    fun `华为与荣耀支持停在草稿态`() {
        // 这两个渠道的 API 把「绑定文件形成草稿」与「送审」分成独立调用，
        // 因此可以先建草稿、到后台人工核对、再送审
        listOf("huawei", "honor").forEach { id ->
            val channel = ChannelRegistry.require(id)
            assertTrue(
                channel.capability.supports(ReleaseStage.CreateDraft),
                "$id 应当支持停在草稿态",
            )
            assertTrue(channel.capability.canStopBefore())
        }
    }

    @Test
    fun `小米无处可停`() {
        // dev/push 把上传与送审合并成一次原子请求
        val mi = ChannelRegistry.require("mi")
        assertEquals(listOf(ReleaseStage.SubmitReview), mi.capability.supportedStages)
        assertFalse(mi.capability.canStopBefore())
        assertEquals(ChannelCapability.RiskLevel.Critical, mi.capability.riskLevel)
    }

    @Test
    fun `OPPO 与 vivo 只能停在上传安装包`() {
        // 文件上传与 app/upd、app.sync.update.app 是分开的，但后者一步完成
        // 版本更新与送审，中间没有可检视的草稿态
        listOf("oppo", "vivo").forEach { id ->
            val channel = ChannelRegistry.require(id)
            val cap = channel.capability
            assertTrue(cap.supports(ReleaseStage.UploadArtifact), "$id 应支持仅上传安装包")
            assertFalse(cap.supports(ReleaseStage.CreateDraft), "$id 没有草稿态")
            assertEquals(ChannelCapability.RiskLevel.Critical, cap.riskLevel)
        }
    }

    // ---- 停留点校验 ----

    @Test
    fun `小米请求停在草稿态时立即报错而非默默送审`() {
        // 这是整个机制最坏的失败模式：调用方以为停在草稿，实际已经提交了
        val mi = ChannelRegistry.require("mi")
        val error = assertFailsWith<PublishError> {
            mi.requireSupportedStage(ReleaseStage.CreateDraft)
        }
        assertEquals(ErrorKind.Configuration, error.kind)
        assertTrue(
            error.message!!.contains("草稿"),
            "错误信息应说明请求的是哪个阶段：${error.message}",
        )
    }

    @Test
    fun `OPPO 请求停在草稿态时报错`() {
        val error = assertFailsWith<PublishError> {
            ChannelRegistry.require("oppo").requireSupportedStage(ReleaseStage.CreateDraft)
        }
        assertEquals(ErrorKind.Configuration, error.kind)
    }

    @Test
    fun `支持的停留点通过校验`() {
        ChannelRegistry.require("huawei").requireSupportedStage(ReleaseStage.CreateDraft)
        ChannelRegistry.require("oppo").requireSupportedStage(ReleaseStage.UploadArtifact)
        channels.forEach { it.requireSupportedStage(ReleaseStage.SubmitReview) }
    }

    @Test
    fun `每个渠道都有说明文字`() {
        channels.forEach { channel ->
            assertTrue(
                channel.capability.note.length >= 20,
                "${channel.id} 的 note 过于简短，无法帮助调用方理解该渠道的特殊之处",
            )
        }
    }
}
