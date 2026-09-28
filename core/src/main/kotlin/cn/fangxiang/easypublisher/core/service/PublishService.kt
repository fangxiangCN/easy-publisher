package cn.fangxiang.easypublisher.core.service

import cn.fangxiang.easypublisher.core.ArtifactInfo
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.Channel
import cn.fangxiang.easypublisher.core.channel.ChannelCredentials
import cn.fangxiang.easypublisher.core.channel.ChannelRegistry
import cn.fangxiang.easypublisher.core.channel.MarketInfo
import cn.fangxiang.easypublisher.core.channel.MarketQuery
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.channel.requireSupportedStage
import cn.fangxiang.easypublisher.core.channel.UploadRequest
import cn.fangxiang.easypublisher.core.config.AppConfig
import cn.fangxiang.easypublisher.core.config.AppConfigStore
import cn.fangxiang.easypublisher.core.config.LayeredCredentialStore
import cn.fangxiang.easypublisher.core.log.AppLogger
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.readArtifactInfo
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
    /**
     * 可用渠道。默认取注册表，测试可注入替身 —— 否则 service 层的编排逻辑
     * （阶段记录、失败隔离、前置校验时机）完全无法覆盖。
     */
    private val channels: List<Channel> = ChannelRegistry.all(),
    /**
     * APK 元信息读取。默认走 apk-parser，测试可注入替身 ——
     * 否则编排逻辑的测试必须先准备一个真实可解析的 APK。
     */
    private val artifactInfoReader: suspend (File) -> ArtifactInfo = ::readArtifactInfo,
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

    fun channels(): List<Channel> = channels

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
        artifactPath: File,
        releaseParams: ReleaseParams,
        channelIds: List<String>? = null,
        versionRule: PublishPolicy.VersionRule = PublishPolicy.VersionRule.Strict,
        timeouts: HttpTimeouts = HttpTimeouts.DEFAULT,
        /**
         * 流程走到哪一步就停下。
         *
         * null 表示「走到该渠道能到的最远阶段」—— 多数渠道是送审，
         * 但鸿蒙渠道的送审未经验证，最远只到草稿。这样默认行为对每个渠道都是
         * 它能安全做到的最大值，而不是一律假设可以送审。
         */
        stopAfter: ReleaseStage? = null,
    ): String {
        val config = configStore.require(applicationId)
        val targets = resolveChannels(config, channelIds)
        if (targets.isEmpty()) {
            throw PublishError.configuration("没有启用任何渠道：$applicationId")
        }

        // 停留点是否被支持，必须在做任何实际工作之前判定。
        // 否则小米这类「无处可停」的渠道会先解析制品、查完市场状态，
        // 甚至开始上传，才告诉调用方这个请求根本不成立。
        if (stopAfter != null) {
            targets.forEach { it.requireSupportedStage(stopAfter) }
        }

        // 逐渠道解析出实际要走的阶段：显式指定就用它，否则取该渠道支持的最远阶段
        val resolved: Map<String, ReleaseStage> = targets.associate { channel ->
            channel.id to (stopAfter ?: channel.capability.supportedStages.last())
        }

        // 预解析：任何一个渠道的制品缺失或配置错误都应在返回 jobId 之前暴露，
        // 而不是留到后台执行时才失败 —— 否则调用方只能从轮询结果里发现参数写错了。
        val plans = targets.map { channel ->
            val file = ArtifactLocator.locate(artifactPath, channel, config.multiChannelApk)
            ChannelPlan(
                channel = channel,
                artifactFile = file,
                artifactInfo = artifactInfoReader(file),
                credentials = credentialsFor(config, channel),
                stage = resolved.getValue(channel.id),
            )
        }

        val reference = plans.first().artifactInfo
        if (reference.applicationId != config.applicationId) {
            throw PublishError.configuration(
                "制品的包名 ${reference.applicationId} 与配置的 ${config.applicationId} 不一致"
            )
        }

        val handle = JobHandle(
            UploadJob(
                id = UUID.randomUUID().toString().take(8),
                applicationId = config.applicationId,
                artifactPath = artifactPath.absolutePath,
                versionCode = reference.versionCode,
                versionName = reference.versionName,
                channels = plans.map {
                    ChannelProgress(it.channel.id, it.channel.displayName, ChannelStage.Waiting)
                },
                requestedStage = stopAfter,
                startedAt = System.currentTimeMillis(),
            )
        )
        jobs[handle.snapshot.id] = handle

        handle.job = scope.launch {
            runUpload(handle, plans, releaseParams, versionRule, timeouts)
        }
        return handle.snapshot.id
    }

    /**
     * 解析出每个渠道本次会走到哪一步。
     *
     * 用途是让调用方在动手之前就知道后果 —— 尤其是鸿蒙这类「最远只到草稿」的渠道，
     * 以及判断这次操作是否真的包含不可撤销的送审。
     */
    suspend fun resolvedStages(
        applicationId: String,
        channelIds: List<String>? = null,
        stopAfter: ReleaseStage? = null,
    ): Map<String, ReleaseStage> {
        val config = configStore.require(applicationId)
        return resolveChannels(config, channelIds).associate { channel ->
            channel.id to (stopAfter ?: channel.capability.supportedStages.last())
        }
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
        val artifactFile: File,
        val artifactInfo: ArtifactInfo,
        val credentials: ChannelCredentials,
        /** 本渠道本次要走到哪一步，已在 submit 里解析并校验过 */
        val stage: ReleaseStage,
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
        val stopAfter = plan.stage
        try {
            // 只上传安装包时不做版本号校验：此时不产生任何版本，
            // 拦截只会妨碍「验证凭据与签名是否可用」这个用途
            if (stopAfter != ReleaseStage.UploadArtifact) {
                handle.update(channel.id, ChannelStage.Working("检查渠道状态"))
                val marketInfo = runCatching {
                    channel.queryMarket(
                        MarketQuery(plan.artifactInfo.applicationId, plan.credentials, timeouts)
                    )
                }.getOrNull()

                PublishPolicy.reject(plan.artifactInfo, marketInfo, versionRule)?.let { throw it }
            }

            handle.update(channel.id, ChannelStage.Working("请求中"))
            val reached = channel.upload(
                UploadRequest(
                    artifactFile = plan.artifactFile,
                    artifactInfo = plan.artifactInfo,
                    credentials = plan.credentials,
                    releaseParams = releaseParams,
                    timeouts = timeouts,
                    onProgress = { fraction ->
                        handle.update(channel.id, ChannelStage.Uploading(fraction))
                    },
                    stopAfter = stopAfter,
                )
            )
            // 以渠道返回的实际阶段为准，不假设请求里的 stopAfter 已达成
            handle.update(channel.id, ChannelStage.Succeeded(reached))
            AppLogger.info(channel.displayName, "${reached.label} 完成：${plan.artifactInfo}")
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
            return config.enabledChannels().mapNotNull { enabled ->
                channels.firstOrNull { it.id.equals(enabled.name, ignoreCase = true) }
            }
        }
        return channelIds.map { id ->
            val channel = channels.firstOrNull { it.id.equals(id, ignoreCase = true) }
                ?: throw PublishError.configuration(
                    "未知渠道：$id（可用渠道：${channels.joinToString(", ") { it.id }}）"
                )
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
