package cn.fangxiang.easypublisher.core

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.test.runTest
import java.io.IOException
import java.net.SocketTimeoutException
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertFailsWith
import kotlin.test.assertNull
import kotlin.test.assertTrue

/**
 * 送审点前后的重试语义。
 *
 * ## 这个测试在防什么
 *
 * 「提交审核」是不可撤销的动作 —— 各应用商店都不提供撤销版本更新的 API。
 * 而送审请求的失败有一类特别危险：**服务端已经受理，但响应在回程丢失**
 * （超时、连接被重置）。此时错误看起来是普通的网络错误，
 * 原实现会把它标成 `retryable = true`，CLI 打印「该错误通常可重试」，
 * MCP 返回 `retryable: true`，SKILL.md 还告诉 agent「网络失败或超时 → 可以重试」。
 *
 * 照做就会重复送审 / 产生重复版本，而且无法回滚。
 *
 * 因此重试与否不只取决于错误类型，还取决于**失败发生在流程的哪个阶段**。
 */
class SubmissionRetryTest {

    // ---- 送审之前：重试安全 ----

    @Test
    fun `送审前的网络错误可重试`() {
        val error = PublishError.fromNetwork("huawei", SocketTimeoutException("read timeout"))
        assertEquals(FailurePhase.PreSubmission, error.phase)
        assertTrue(error.retryable, "取 token、查状态这类步骤失败，重试没有副作用")
    }

    @Test
    fun `送审前的未知错误可重试`() {
        val error = PublishError.fromNetwork("huawei", IllegalStateException("boom"))
        assertEquals(ErrorKind.Unknown, error.kind)
        assertTrue(error.retryable)
    }

    // ---- 送审点及之后：不得盲目重试 ----

    @Test
    fun `送审点的网络超时不可重试`() {
        // 这正是原实现的缺陷：同样的超时，只因发生在送审步骤就变得危险
        val error = PublishError
            .fromNetwork("huawei", SocketTimeoutException("read timeout"))
            .atSubmissionPoint("华为", "提交审核")

        assertEquals(FailurePhase.AtOrAfterSubmission, error.phase)
        assertFalse(error.retryable, "服务端可能已受理，重试会重复送审")
    }

    @Test
    fun `送审点的普通 IO 失败不可重试`() {
        val error = PublishError
            .fromNetwork("oppo", IOException("Connection reset"))
            .atSubmissionPoint("OPPO", "提交版本")

        assertFalse(error.retryable)
        assertEquals(ErrorKind.Network, error.kind, "错误类型本身不应被改写")
    }

    @Test
    fun `提示语引导先确认而非直接重试`() {
        val error = PublishError
            .fromNetwork("huawei", SocketTimeoutException())
            .atSubmissionPoint("华为", "提交审核")

        val message = error.message ?: ""
        assertTrue(
            message.contains("确认"),
            "必须提示先到后台确认是否已提交，否则使用者只会看到「超时」然后重试：$message",
        )
        assertTrue(
            message.contains("提交审核"),
            "提示里应说明是哪一步越过了送审点：$message",
        )
    }

    @Test
    fun `atSubmissionPoint 保留原有诊断信息`() {
        val original = PublishError.rejected(
            channel = "vivo",
            code = "20000",
            message = "提交更新失败：限流",
            raw = """{"code":20000}""",
        )
        val tagged = original.atSubmissionPoint("vivo", "提交更新")

        assertEquals("vivo", tagged.channel)
        assertEquals("20000", tagged.code)
        assertEquals("""{"code":20000}""", tagged.raw)
        assertEquals(ErrorKind.ChannelRejected, tagged.kind)
        assertTrue(tagged.message!!.contains("限流"), "原始 message 不能被提示语挤掉")
        assertFalse(tagged.retryable)
    }

    @Test
    fun `渠道拒绝的错误本来就不可重试，标记后仍不可重试`() {
        val error = PublishError
            .rejected("mi", "131004", "版本号已存在")
            .atSubmissionPoint("小米", "推送版本")
        assertFalse(error.retryable)
    }

    // ---- 包装器 ----

    @Test
    fun `包装器把送审步骤的网络异常标为不可重试`() = runTest {
        val error = assertFailsWith<PublishError> {
            atSubmissionPoint("华为", "提交审核") {
                throw SocketTimeoutException("read timeout")
            }
        }
        assertEquals(FailurePhase.AtOrAfterSubmission, error.phase)
        assertFalse(error.retryable)
    }

    @Test
    fun `包装器保留已结构化的 PublishError 并补上阶段`() = runTest {
        val error = assertFailsWith<PublishError> {
            atSubmissionPoint("OPPO", "提交版本") {
                throw PublishError.rejected("oppo", "800002", "icon_url 不允许的文件格式")
            }
        }
        assertEquals("800002", error.code)
        assertEquals(FailurePhase.AtOrAfterSubmission, error.phase)
    }

    @Test
    fun `包装器原样重抛取消，不转成失败`() = runTest {
        // 取消必须能被协程框架识别，否则「取消上传」会显示成「上传失败」并提示重试
        assertFailsWith<CancellationException> {
            atSubmissionPoint("华为", "提交审核") {
                throw CancellationException("用户取消")
            }
        }
    }

    @Test
    fun `包装器透传成功结果`() = runTest {
        val result = atSubmissionPoint("华为", "提交审核") { "ok" }
        assertEquals("ok", result)
    }

    @Test
    fun `非送审步骤不受影响`() = runTest {
        val error = assertFailsWith<PublishError> {
            throw PublishError.fromNetwork("huawei", SocketTimeoutException())
        }
        assertEquals(FailurePhase.PreSubmission, error.phase)
        assertTrue(error.retryable)
        assertNull(error.code)
    }
}
