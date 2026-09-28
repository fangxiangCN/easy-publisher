package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ApkInfo
import cn.fangxiang.easypublisher.core.ErrorKind
import cn.fangxiang.easypublisher.core.FailurePhase
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCapability
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.Evidence
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.ReviewState
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.channel.Withdrawal
import cn.fangxiang.easypublisher.core.config.AppConfig
import cn.fangxiang.easypublisher.core.config.AppConfigStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import java.io.File
import java.net.SocketTimeoutException
import java.nio.file.Files
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * service 层的编排逻辑。
 *
 * 用假渠道注入，覆盖的是「编排」本身：实际阶段如何记录、单渠道失败是否被隔离、
 * 前置校验在什么时机执行。渠道各自的 HTTP 细节不在这里测。
 */
class PublishServiceTest {

    private val tempDir: File = Files.createTempDirectory("ep-service-test").toFile()

    private val store = AppConfigStore(File(tempDir, "apps"))

    /** 假装是个 APK；apkInfoReader 被注入替身，不会真的去解析 */
    private val apkFile = File(tempDir, "app-1.0.0.apk").apply { writeText("not really an apk") }

    private val fakeApkInfo = ApkInfo(
        path = apkFile.absolutePath,
        applicationId = APP_ID,
        versionCode = 10,
        versionName = "1.0.0",
        sizeBytes = 17,
    )

    @AfterTest
    fun cleanup() {
        tempDir.deleteRecursively()
    }

    private class FakeChannel(
        override val id: String,
        override val displayName: String = id,
        override val fileNameTag: String = id.uppercase(),
        override val params: List<ChannelParam> = emptyList(),
        override val capability: ChannelCapability = ChannelCapability(
            supportedStages = ReleaseStage.entries,
            riskLevel = ChannelCapability.RiskLevel.Medium,
            withdrawal = Withdrawal.NotVerified,
            evidence = Evidence.CodeObservation,
            note = "测试用假渠道",
        ),
        /**
         * upload 返回的实际阶段。
         *
         * null 表示「照请求停下」—— 真实渠道的正常行为；
         * 显式赋值表示无论请求什么都只走到这一步，用于验证 service 是否如实记录
         * 渠道返回的阶段，而不是想当然地认为请求已达成。
         */
        private val reached: ReleaseStage? = null,
        private val failure: PublishError? = null,
        private val market: MarketInfo? = MarketInfo(
            channelId = id,
            reviewState = ReviewState.Online,
            // 默认线上版本低于待提交版本，让版本号校验通过
            lastVersion = MarketInfo.Version(5, "0.9.0"),
        ),
    ) : Channel {

        var uploadCalls = 0
            private set
        var lastStopAfter: ReleaseStage? = null
            private set
        var marketQueries = 0
            private set

        override suspend fun upload(request: UploadRequest): ReleaseStage {
            uploadCalls++
            lastStopAfter = request.stopAfter
            failure?.let { throw it }
            return reached ?: request.stopAfter
        }

        override suspend fun queryMarket(query: MarketQuery): MarketInfo {
            marketQueries++
            return market ?: throw PublishError.protocol(id, "无状态可查")
        }
    }

    private suspend fun seedConfig(vararg channelIds: String) {
        store.save(
            AppConfig(
                name = "测试应用",
                applicationId = APP_ID,
                channels = channelIds.map { AppConfig.ChannelConfig(name = it, enabled = true) },
            )
        )
    }

    // ---- 实际阶段的记录 ----

    @Test
    fun `记录渠道返回的实际阶段而非请求的阶段`() = runTest {
        seedConfig("huawei")
        // 渠道声称只走到上传，即使请求的是送审 —— service 必须如实记录
        val channel = FakeChannel("huawei", reached = ReleaseStage.UploadArtifact)
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(APP_ID, apkFile, ReleaseParams("测试"))
        advanceUntilIdle()

        val job = assertNotNull(svc.job(jobId))
        val stage = job.channels.single().stage
        assertTrue(stage is ChannelStage.Succeeded, "实际是 $stage")
        assertEquals(ReleaseStage.UploadArtifact, stage.reached)
        assertEquals("已上传安装包（未创建版本）", stage.label)
    }

    @Test
    fun `停在草稿态时不显示为已提交`() = runTest {
        seedConfig("honor")
        val channel = FakeChannel("honor")
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(
            APP_ID, apkFile, ReleaseParams("测试"), stopAfter = ReleaseStage.CreateDraft,
        )
        advanceUntilIdle()

        val job = assertNotNull(svc.job(jobId))
        assertEquals(ReleaseStage.CreateDraft, job.requestedStage)
        assertEquals(ReleaseStage.CreateDraft, channel.lastStopAfter)
        val stage = job.channels.single().stage as ChannelStage.Succeeded
        assertEquals("草稿已就绪（未送审）", stage.label)
    }

    // ---- 前置校验的时机 ----

    @Test
    fun `仅上传安装包时跳过版本号校验`() = runTest {
        seedConfig("huawei")
        // 线上版本 100 高于待提交的 10，正常送审会被拦；但只上传文件不产生版本，不该拦
        val channel = FakeChannel("huawei", market = onlineVersion(100))
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(
            APP_ID, apkFile, ReleaseParams("测试"), stopAfter = ReleaseStage.UploadArtifact,
        )
        advanceUntilIdle()

        val job = assertNotNull(svc.job(jobId))
        assertTrue(
            job.channels.single().stage is ChannelStage.Succeeded,
            "仅上传安装包不应被版本号校验拦住，实际：${job.channels.single().stage.label}",
        )
        assertEquals(0, channel.marketQueries, "不该去查渠道状态")
    }

    @Test
    fun `停在草稿态时仍执行版本号校验`() = runTest {
        seedConfig("huawei")
        val channel = FakeChannel("huawei", market = onlineVersion(100))
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(
            APP_ID, apkFile, ReleaseParams("测试"), stopAfter = ReleaseStage.CreateDraft,
        )
        advanceUntilIdle()

        val stage = assertNotNull(svc.job(jobId)).channels.single().stage
        assertTrue(stage is ChannelStage.Failed, "草稿也是要建版本的，版本号过低应当被拦：$stage")
        assertEquals(ErrorKind.Precondition, stage.kind)
        assertEquals(0, channel.uploadCalls, "校验不通过就不该上传")
    }

    // ---- 失败隔离与阶段标记 ----

    @Test
    fun `单渠道失败不影响其他渠道`() = runTest {
        seedConfig("huawei", "mi")
        val failing = FakeChannel(
            "mi", failure = PublishError.rejected("mi", "131004", "版本号已存在"),
        )
        val ok = FakeChannel("huawei")
        val svc = PublishService(store, this, listOf(failing, ok)) { fakeApkInfo }

        val jobId = svc.submit(APP_ID, apkFile, ReleaseParams("测试"))
        advanceUntilIdle()

        val job = assertNotNull(svc.job(jobId))
        assertEquals(JobState.PartiallyFailed, job.state)
        assertEquals(listOf("huawei"), job.succeeded())
        assertEquals("mi", job.failed().single().channelId)
        assertEquals(1, ok.uploadCalls, "成功的渠道应当照常上传")
    }

    @Test
    fun `送审点的失败带上阶段标记且不可重试`() = runTest {
        seedConfig("huawei")
        // 模拟「服务端已受理但响应丢失」：渠道内部已用 atSubmissionPoint 标记
        val channel = FakeChannel(
            "huawei",
            failure = PublishError
                .fromNetwork("huawei", SocketTimeoutException("read timeout"))
                .atSubmissionPoint("华为", "提交审核"),
        )
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(APP_ID, apkFile, ReleaseParams("测试"))
        advanceUntilIdle()

        val stage = assertNotNull(svc.job(jobId)).channels.single().stage
        assertTrue(stage is ChannelStage.Failed)
        assertEquals(FailurePhase.AtOrAfterSubmission, stage.phase)
        assertEquals(false, stage.retryable, "送审后的网络失败绝不能标记为可重试")
        assertTrue(stage.message.contains("确认"), "提示应引导先确认：${stage.message}")
    }

    @Test
    fun `送审前的网络失败仍可重试`() = runTest {
        seedConfig("huawei")
        val channel = FakeChannel("huawei", failure = PublishError.fromNetwork("huawei", SocketTimeoutException()))
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val jobId = svc.submit(APP_ID, apkFile, ReleaseParams("测试"))
        advanceUntilIdle()

        val stage = assertNotNull(svc.job(jobId)).channels.single().stage as ChannelStage.Failed
        assertEquals(FailurePhase.PreSubmission, stage.phase)
        assertTrue(stage.retryable)
    }

    // ---- 提交前的快速失败 ----

    @Test
    fun `APK 包名与配置不一致时在返回 jobId 之前失败`() = runTest {
        seedConfig("huawei")
        val mismatched = fakeApkInfo.copy(applicationId = "com.other.app")
        val svc = PublishService(store, this, listOf(FakeChannel("huawei"))) { mismatched }

        val error = assertFailsWith<PublishError> {
            svc.submit(APP_ID, apkFile, ReleaseParams("测试"))
        }
        assertEquals(ErrorKind.Configuration, error.kind)
        assertTrue(error.message!!.contains("com.other.app"))
    }

    @Test
    fun `请求未配置的渠道时快速失败`() = runTest {
        seedConfig("huawei")
        val svc = PublishService(store, this, listOf(FakeChannel("huawei"))) { fakeApkInfo }

        val error = assertFailsWith<PublishError> {
            svc.submit(APP_ID, apkFile, ReleaseParams("测试"), channelIds = listOf("vivo"))
        }
        assertEquals(ErrorKind.Configuration, error.kind)
    }

    @Test
    fun `请求不支持的停留点时在做任何工作之前就失败`() = runTest {
        seedConfig("mi")
        // 小米的 dev/push 是原子的，无处可停。这个校验必须早于 APK 解析与状态查询，
        // 否则会先白做一堆工作，甚至在某些实现里开始上传，才告诉调用方请求不成立
        val channel = FakeChannel(
            "mi",
            capability = ChannelCapability(
                supportedStages = listOf(ReleaseStage.SubmitReview),
                riskLevel = ChannelCapability.RiskLevel.Critical,
                withdrawal = Withdrawal.NotVerified,
                evidence = Evidence.CodeObservation,
                note = "dev/push 原子请求，无处可停",
            ),
        )
        val svc = PublishService(store, this, listOf(channel)) { fakeApkInfo }

        val error = assertFailsWith<PublishError> {
            svc.submit(APP_ID, apkFile, ReleaseParams("测试"), stopAfter = ReleaseStage.CreateDraft)
        }
        assertEquals(ErrorKind.Configuration, error.kind)
        assertEquals(0, channel.uploadCalls, "不该发起任何上传")
        assertEquals(0, channel.marketQueries, "不该查询市场状态")
    }

    private fun onlineVersion(code: Long) = MarketInfo(
        channelId = "huawei",
        reviewState = ReviewState.Online,
        lastVersion = MarketInfo.Version(code, "$code.0.0"),
    )

    private companion object {
        const val APP_ID = "com.example.app"
    }
}
