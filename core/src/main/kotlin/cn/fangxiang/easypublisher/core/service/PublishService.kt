package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ApkInfo
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCredentials
import cn.fangxiang.easypublisher.core.channel.ChannelRegistry
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.config.AppConfig
import cn.fangxiang.easypublisher.core.config.AppConfigStore
import cn.fangxiang.easypublisher.core.config.LayeredCredentialStore
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.readApkInfo
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.supervisorScope
import java.io.File
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * 发布编排。
 *
 * 对外只接受包名、APK 路径与发布参数 —— **绝不接受凭据参数**。凭据由
 * [LayeredCredentialStore] 在 core 内部读取，避免 secret 出现在
 * CLI 参数、shell 历史或 agent 的对话上下文里。
 */
class PublishService(
    private val configStore: AppConfigStore = AppConfigStore(),
    private val scope: CoroutineScope = CoroutineScope(SupervisorJob()),
) {

    private val jobs = ConcurrentHashMap<String, JobHandle>()

    private class JobHandle(
        @Volatile var snapshot: UploadJob,
        @Volatile var job: Job? = null,
    ) {
        private val stages = ConcurrentHashMap<String, ChannelStage>()

        fun update(channelId: String, stage: ChannelStage) {
            stages[channelId] = stage
            val updated = snapshot.channels.map { progress ->
                stages[progress.channelId]?.let { progress.copy(stage = it) } ?: progress
            }
            val finished = updated.all { it.stage.terminal }
            snapshot = snapshot.copy(
                channels = updated,
                finishedAt = if (finished) System.currentTimeMillis() else null,
            )
        }
    }

    suspend fun listApps(): List<AppConfig> = configStore.list()

    suspend fun app(applicationId: String): AppConfig = configStore.require(applicationId)

    fun channels(): List<Channel> = ChannelRegistry.all()

    /**
     * 查询应用在各渠道的状态。
     *
     * 单个渠道失败不影响其他渠道 —— 用 [supervisorScope] 而非普通 scope。
     */
    suspend fun marketStates(
        applicationId: String,
        channelIds: List<String>? = null,
        timeouts: HttpTimeouts = HttpTimeouts.DEFAULT,
    ): Map<String, Result<MarketInfo>> {
        val config = configStore.require(applicationId)
        val targets = resolveChannels(config, channelIds)
        return supervisorScope {
            targets.map { channel ->
                channel.id to async {
                    runCatching {
                        channel.queryMarket(
                            MarketQuery(
                                applicationId = config.applicationId,
                                credentials = credentialsFor(config, channel),
                                timeouts = timeouts,
                            )
                        )
                    }
                }
            }.map { (id, deferred) -> id to deferred.await() }.toMap()
        }
    }

    /**
     * 提交新版本。**立即返回**，不等待上传完成。
     *
     * @return 任务 id，用 [job] 轮询进度
     */
    suspend fun submit(
        applicationId: String,
        apkPath: File,
        releaseParams: ReleaseParams,
        channelIds: List<String>? = null,
        versionRule: PublishPolicy.VersionRule = PublishPolicy.VersionRule.Strict,
        timeouts: HttpTimeouts = HttpTimeouts.DEFAULT,
    ): String {
        val config = configStore.require(applicationId)
        val targets = resolveChannels(config, channelIds)
        if (targets.isEmpty()) {
            throw PublishError.configuration("没有启用任何渠道：$applicationId")
        }

        // 预解析：任何一个渠道的 APK 缺失或配置错误都应在返回 jobId 之前暴露，
        // 而不是留到后台执行时才失败 —— 否则调用方只能从轮询结果里发现参数写错了。
        val plans = targets.map { channel ->
            val file = ApkLocator.locate(apkPath, channel, config.multiChannelApk)
            ChannelPlan(channel, file, readApkInfo(file), credentialsFor(config, channel))
        }

        val reference = plans.first().apkInfo
        if (reference.applicationId != config.applicationId) {
            throw PublishError.configuration(
                "APK 的包名 ${reference.applicationId} 与配置的 ${config.applicationId} 不一致"
            )
        }

        val handle = JobHandle(
            UploadJob(
                id = UUID.randomUUID().toString().take(8),
                applicationId = config.applicationId,
                apkPath = apkPath.absolutePath,
                versionCode = reference.versionCode,
                versionName = reference.versionName,
                channels = plans.map {
                    ChannelProgress(it.channel.id, it.channel.displayName, ChannelStage.Waiting)
                },
                startedAt = System.currentTimeMillis(),
            )
        )
        jobs[handle.snapshot.id] = handle

        handle.job = scope.launch {
            runUpload(handle, plans, releaseParams, versionRule, timeouts)
        }
        return handle.snapshot.id
    }

    fun job(id: String): UploadJob? = jobs[id]?.snapshot

    fun jobIds(): List<String> = jobs.keys().toList()

    fun cancel(id: String): Boolean {
        val handle = jobs[id] ?: return false
        handle.job?.cancel()
        return true
    }

    private class ChannelPlan(
        val channel: Channel,
        val apkFile: File,
        val apkInfo: ApkInfo,
        val credentials: ChannelCredentials,
    )

    private suspend fun runUpload(
        handle: JobHandle,
        plans: List<ChannelPlan>,
        releaseParams: ReleaseParams,
        versionRule: PublishPolicy.VersionRule,
        timeouts: HttpTimeouts,
    ) {
        supervisorScope {
            plans.map { plan ->
                async { uploadOne(handle, plan, releaseParams, versionRule, timeouts) }
            }.awaitAll()
        }
    }

    private suspend fun uploadOne(
        handle: JobHandle,
        plan: ChannelPlan,
        releaseParams: ReleaseParams,
        versionRule: PublishPolicy.VersionRule,
        timeouts: HttpTimeouts,
    ) {
        val channel = plan.channel
        try {
            handle.update(channel.id, ChannelStage.Working("检查渠道状态"))
            val marketInfo = runCatching {
                channel.queryMarket(
                    MarketQuery(plan.apkInfo.applicationId, plan.credentials, timeouts)
                )
            }.getOrNull()

            PublishPolicy.reject(plan.apkInfo, marketInfo, versionRule)?.let { throw it }

            handle.update(channel.id, ChannelStage.Working("请求中"))
            channel.upload(
                UploadRequest(
                    apkFile = plan.apkFile,
                    apkInfo = plan.apkInfo,
                    credentials = plan.credentials,
                    releaseParams = releaseParams,
                    timeouts = timeouts,
                    onProgress = { fraction ->
                        handle.update(channel.id, ChannelStage.Uploading(fraction))
                    },
                )
            )
            handle.update(channel.id, ChannelStage.Succeeded)
            AppLogger.info(channel.displayName, "提交新版本成功：${plan.apkInfo}")
        } catch (e: CancellationException) {
            // 取消必须原样向上传播，否则协程框架无法感知。
            // 原实现用 catch(Throwable) 把取消当成失败，界面显示「上传失败」并给出重试按钮，
            // 后续渠道也逐个变成 Error，取消语义完全失效。
            handle.update(channel.id, ChannelStage.Cancelled)
            throw e
        } catch (e: PublishError) {
            handle.update(channel.id, ChannelStage.of(e))
            AppLogger.error(channel.displayName, "提交新版本失败：${e.describe()}", e)
        } catch (e: Exception) {
            val error = PublishError.fromNetwork(channel.id, e)
            handle.update(channel.id, ChannelStage.of(error))
            AppLogger.error(channel.displayName, "提交新版本失败", e)
        }
    }

    private fun resolveChannels(config: AppConfig, channelIds: List<String>?): List<Channel> {
        if (channelIds.isNullOrEmpty()) {
            return config.enabledChannels().mapNotNull { ChannelRegistry.find(it.name) }
        }
        return channelIds.map { id ->
            val channel = ChannelRegistry.require(id)
            if (config.channel(channel.id) == null) {
                throw PublishError.configuration(
                    "应用 ${config.applicationId} 未配置渠道 ${channel.id}",
                    channel = channel.id,
                )
            }
            channel
        }
    }

    /**
     * 组装渠道凭据。环境变量优先于配置文件，CI 场景可完全不落盘。
     */
    private fun credentialsFor(config: AppConfig, channel: Channel): ChannelCredentials {
        val store = LayeredCredentialStore(config)
        val values = channel.params.mapNotNull { param ->
            val value = store.get(config.applicationId, channel.id, param.name)
            if (value.isNullOrBlank()) {
                if (param.required) {
                    throw PublishError.credential(
                        "渠道 ${channel.displayName} 缺少必填参数 ${param.name}（${param.description}）",
                        channel = channel.id,
                    )
                }
                null
            } else {
                param.name to value
            }
        }.toMap()
        return ChannelCredentials(values)
    }
}
