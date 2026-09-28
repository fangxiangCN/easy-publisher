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
    fun `注册了六个渠道`() {
        assertEquals(
            listOf("huawei", "mi", "oppo", "vivo", "honor", "harmony"),
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
     * 未验证不等于不支持。
     *
     * 鸿蒙送审走 v3 端点，同样没有真实凭据验证过，但它的 evidence 标注
     * 已经如实说明了这一点；用「不实现」来表达不确定性会让用户少了这个能力，
     * 而正确的做法是实现它并标注证据等级。
     */
    @Test
    fun `所有渠道都支持送审且证据等级如实标注`() {
        channels.forEach { channel ->
            assertTrue(
                channel.capability.supports(ReleaseStage.SubmitReview),
                "${channel.id} 应当支持送审",
            )
            assertEquals(
                Evidence.CodeObservation,
                channel.capability.evidence,
                "${channel.id} 尚未用真实凭据验证，evidence 不应被抬高",
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

    /**
     * 上游 issue #16 的结论是「各商店 API 都不提供撤销版本更新」，
     * 但华为 AGC 文档里确实有「撤销审核」接口。所以 AGC 系（华为、鸿蒙）
     * 标为 ApiSupported，其余渠道没人逐个确认过，仍是 NotVerified。
     *
     * 注意 ApiSupported 说的是**平台**能力，不代表本工具实现了该调用 —— 目前没实现。
     */
    @Test
    fun `AGC 系渠道标为 API 支持撤回，其余仍未验证`() {
        val agc = listOf("huawei", "harmony")
        channels.forEach { channel ->
            val expected = if (channel.id in agc) Withdrawal.ApiSupported else Withdrawal.NotVerified
            assertEquals(expected, channel.capability.withdrawal, "${channel.id} 的撤回能力标注不对")
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
        ChannelRegistry.require("harmony").requireSupportedStage(ReleaseStage.CreateDraft)
        // 能送审的渠道都应当接受 SubmitReview
        channels.filter { it.capability.supports(ReleaseStage.SubmitReview) }
            .forEach { it.requireSupportedStage(ReleaseStage.SubmitReview) }
    }

    @Test
    fun `鸿蒙支持送审也支持停在草稿`() {
        val harmony = ChannelRegistry.require("harmony")
        harmony.requireSupportedStage(ReleaseStage.SubmitReview)
        harmony.requireSupportedStage(ReleaseStage.CreateDraft)
        harmony.requireSupportedStage(ReleaseStage.UploadArtifact)
        assertEquals(
            listOf(ReleaseStage.UploadArtifact, ReleaseStage.CreateDraft, ReleaseStage.SubmitReview),
            harmony.capability.supportedStages,
        )
    }

    // ---- 鸿蒙渠道特有约束 ----

    @Test
    fun `鸿蒙只接受 app 格式`() {
        val harmony = ChannelRegistry.require("harmony")
        assertEquals(listOf("app"), harmony.artifactExtensions)
        // 其余渠道仍是 apk，默认值不该被改动
        channels.filter { it.id != "harmony" }.forEach { channel ->
            assertEquals(listOf("apk"), channel.artifactExtensions, "${channel.id} 的制品类型变了")
        }
    }

    @Test
    fun `鸿蒙要求显式配置 appId`() {
        // 鸿蒙应用在 AGC 里是独立记录，用包名反查会拿到 Android 应用的 id
        val params = ChannelRegistry.require("harmony").params
        val names = params.map { it.name }
        assertTrue("app_id" in names, "鸿蒙必须要求显式配置 app_id，实际：$names")
        assertTrue("client_id" in names && "client_secret" in names)
        // 主体登记信息是可选的：只有部分应用送审时需要
        assertTrue(params.first { it.name == "app_id" }.required)
        assertFalse(params.first { it.name == "registered_id_type" }.required)
        assertFalse(params.first { it.name == "registered_id_number" }.required)
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
