package cn.fangxiang.easypublisher.mcp

import cn.fangxiang.easypublisher.core.ApplicationId
import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.channel.ChannelParam
import cn.fangxiang.easypublisher.core.channel.ChannelRegistry
import cn.fangxiang.easypublisher.core.channel.ReleaseParams
import cn.fangxiang.easypublisher.core.channel.ReleaseStage
import cn.fangxiang.easypublisher.core.config.EnvCredentialStore
import cn.fangxiang.easypublisher.core.net.HttpTimeouts
import cn.fangxiang.easypublisher.core.readArtifactInfo
import cn.fangxiang.easypublisher.core.service.ChannelStage
import cn.fangxiang.easypublisher.core.service.JobState
import cn.fangxiang.easypublisher.core.service.PublishPolicy
import cn.fangxiang.easypublisher.core.service.PublishService
import io.modelcontextprotocol.kotlin.sdk.server.Server
import io.modelcontextprotocol.kotlin.sdk.types.CallToolRequest
import io.modelcontextprotocol.kotlin.sdk.types.ToolAnnotations
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import kotlinx.serialization.json.putJsonObject
import java.io.File

/**
 * 注册全部工具。
 *
 * 安全约束（写进实现，不只写文档）：
 *  - **没有任何工具接受凭据参数。** 凭据由 core 从 `~/.easy-publisher/` 或环境变量读取，
 *    绝不经过模型的上下文。
 *  - 只读工具与写工具通过 [ToolAnnotations] 区分，写工具标注为非幂等且有副作用。
 *  - [registerUploadApk] 需要显式确认参数，理由见该函数注释。
 */
internal fun Server.registerTools(service: PublishService) {
    registerListApps(service)
    registerListChannels()
    registerGetMarketState(service)
    registerCheckRelease(service)
    registerUploadApk(service)
    registerGetUploadStatus(service)
}

private fun Server.registerListApps(service: PublishService) = addTool(
    name = "list_apps",
    description = "列出已配置的应用及其启用的渠道。只返回参数名，不返回凭据值。",
    inputSchema = Schema.empty,
    toolAnnotations = ToolAnnotations(readOnlyHint = true, openWorldHint = false),
) { _ ->
    guarded {
        val apps = service.listApps()
        toolResult(
            buildJsonObject {
                put("ok", true)
                putJsonArray("apps") {
                    apps.forEach { app ->
                        add(
                            buildJsonObject {
                                put("applicationId", app.applicationId)
                                put("name", app.name)
                                put("multiChannelApk", app.multiChannelApk)
                                putJsonArray("channels") {
                                    app.channels.forEach { channel ->
                                        add(
                                            buildJsonObject {
                                                put("id", channel.name)
                                                put("enabled", channel.enabled)
                                                putJsonArray("configuredParams") {
                                                    channel.params.forEach { add(it.name) }
                                                }
                                            }
                                        )
                                    }
                                }
                            }
                        )
                    }
                }
            }
        )
    }
}

private fun Server.registerListChannels() = addTool(
    name = "list_channels",
    description = "列出支持的应用商店渠道及各渠道所需的凭据参数名。用于了解需要配置什么。",
    inputSchema = Schema.empty,
    toolAnnotations = ToolAnnotations(readOnlyHint = true, openWorldHint = false),
) { _ ->
    guarded {
        toolResult(
            buildJsonObject {
                put("ok", true)
                putJsonArray("channels") {
                    ChannelRegistry.all().forEach { channel ->
                        add(
                            buildJsonObject {
                                put("id", channel.id)
                                put("displayName", channel.displayName)
                                put("fileNameTag", channel.fileNameTag)
                                putJsonObject("capability") {
                                    val cap = channel.capability
                                    put("riskLevel", cap.riskLevel.name)
                                    put("riskLevelLabel", cap.riskLevel.label)
                                    put("withdrawal", cap.withdrawal.name)
                                    put("withdrawalLabel", cap.withdrawal.label)
                                    put(
                                        "requiresExplicitConfirmation",
                                        cap.requiresExplicitConfirmation,
                                    )
                                    put(
                                        "automaticRetryAfterSubmission",
                                        cap.automaticRetryAfterSubmission,
                                    )
                                    put("evidence", cap.evidence.name)
                                    put("evidenceLabel", cap.evidence.label)
                                    put("verifiedScope", cap.verifiedScope)
                                    put("note", cap.note)
                                    putJsonArray("supportedStages") {
                                        cap.supportedStages.forEach { add(it.name) }
                                    }
                                }
                                putJsonArray("params") {
                                    channel.params.forEach { param ->
                                        add(
                                            buildJsonObject {
                                                put("name", param.name)
                                                put("description", param.description)
                                                put("required", param.required)
                                                put(
                                                    "type",
                                                    when (val type = param.type) {
                                                        is ChannelParam.ParamType.Text -> "text"
                                                        is ChannelParam.ParamType.TextFile ->
                                                            "file(.${type.extension})"
                                                    },
                                                )
                                                put(
                                                    "envVar",
                                                    EnvCredentialStore.envName(channel.id, param.name),
                                                )
                                            }
                                        )
                                    }
                                }
                            }
                        )
                    }
                }
            }
        )
    }
}

private fun Server.registerGetMarketState(service: PublishService) = addTool(
    name = "get_market_state",
    description = """
        查询应用在各渠道的审核状态与线上版本号。发布前应先调用此工具：
        若某渠道正在审核中，提交新版本会被拒绝。
        lastVersionCode 可能为 null（应用在商店只有未上传 APK 的草稿版本时）。
    """.trimIndent(),
    inputSchema = Schema.obj(
        "applicationId" to Schema.string("包名，如 com.example.app"),
        "channels" to Schema.stringArray("只查指定渠道；省略则查全部已启用渠道"),
        "timeoutSeconds" to Schema.integer("单次请求超时秒数，默认 120"),
        required = listOf("applicationId"),
    ),
    toolAnnotations = ToolAnnotations(readOnlyHint = true, openWorldHint = true),
) { request ->
    guarded {
        val applicationId = request.requireString("applicationId").let(ApplicationId::validate)
        val results = service.marketStates(
            applicationId = applicationId,
            channelIds = request.stringList("channels"),
            timeouts = request.timeouts(),
        )
        toolResult(
            buildJsonObject {
                put("ok", true)
                put("applicationId", applicationId)
                putJsonArray("channels") {
                    results.forEach { (id, result) ->
                        add(
                            buildJsonObject {
                                put("id", id)
                                result.fold(
                                    onSuccess = { info ->
                                        put("ok", true)
                                        put("reviewState", info.reviewState.name)
                                        put("reviewStateLabel", info.reviewState.label)
                                        put("canSubmit", info.canSubmit)
                                        info.lastVersion?.let {
                                            put("lastVersionCode", it.code)
                                            put("lastVersionName", it.name)
                                        }
                                        info.rawState?.let { put("rawState", it) }
                                    },
                                    onFailure = { error ->
                                        put("ok", false)
                                        val publishError = error as? PublishError
                                        put("kind", publishError?.kind?.name ?: "Unknown")
                                        put("message", error.message ?: "查询失败")
                                        put("retryable", publishError?.retryable ?: false)
                                    },
                                )
                            }
                        )
                    }
                }
            }
        )
    }
}

/**
 * 发布前的只读预检。
 *
 * 存在的理由：[registerUploadApk] 是不可撤销的操作，模型应当有一个**零副作用**的方式
 * 先确认「这个包发到这些渠道会不会被拒」。没有这个工具，模型只能靠真发一次来试。
 */
private fun Server.registerCheckRelease(service: PublishService) = addTool(
    name = "check_release",
    description = """
        发布预检（只读，不会提交任何东西）。解析 APK、查询各渠道状态，
        逐渠道判断是否满足发布前置条件，并说明不满足的原因。
        建议在 upload_apk 之前调用。
    """.trimIndent(),
    inputSchema = Schema.obj(
        "applicationId" to Schema.string("包名"),
        "artifactPath" to Schema.string("APK 文件路径，或存放多渠道包的目录"),
        "channels" to Schema.stringArray("只检查指定渠道；省略则检查全部已启用渠道"),
        "allowSameVersion" to Schema.boolean("允许版本号与线上相同", default = false),
        required = listOf("applicationId", "artifactPath"),
    ),
    toolAnnotations = ToolAnnotations(readOnlyHint = true, openWorldHint = true),
) { request ->
    guarded {
        val applicationId = request.requireString("applicationId").let(ApplicationId::validate)
        val artifactFile = request.requireFile("artifactPath")
        val rule = if (request.boolean("allowSameVersion") == true) {
            PublishPolicy.VersionRule.AllowSame
        } else {
            PublishPolicy.VersionRule.Strict
        }

        val artifactInfo = if (artifactFile.isFile) readArtifactInfo(artifactFile) else null
        val states = service.marketStates(applicationId, request.stringList("channels"))

        var blocked = 0
        val payload = buildJsonObject {
            put("ok", true)
            put("applicationId", applicationId)
            if (artifactInfo != null) {
                putJsonObject("apk") {
                    put("path", artifactInfo.path)
                    put("applicationId", artifactInfo.applicationId)
                    put("versionCode", artifactInfo.versionCode)
                    put("versionName", artifactInfo.versionName)
                    put("sizeBytes", artifactInfo.sizeBytes)
                }
                if (artifactInfo.applicationId != applicationId) {
                    put(
                        "warning",
                        "APK 的包名 ${artifactInfo.applicationId} 与配置的 $applicationId 不一致",
                    )
                }
            } else {
                put("note", "artifactPath 是目录，将按渠道标识匹配多渠道包，此处不逐个解析")
            }
            putJsonArray("channels") {
                states.forEach { (id, result) ->
                    val marketInfo = result.getOrNull()
                    val rejection = artifactInfo?.let { PublishPolicy.reject(it, marketInfo, rule) }
                    if (rejection != null) blocked++
                    add(
                        buildJsonObject {
                            put("id", id)
                            put("canRelease", rejection == null && result.isSuccess)
                            marketInfo?.let {
                                put("reviewState", it.reviewState.name)
                                put("reviewStateLabel", it.reviewState.label)
                                it.lastVersion?.let { v -> put("lastVersionCode", v.code) }
                            }
                            rejection?.let { put("blockedReason", it.message) }
                            result.exceptionOrNull()?.let {
                                put("stateQueryFailed", it.message ?: "查询失败")
                            }
                        }
                    )
                }
            }
            put("blockedCount", blocked)
        }
        toolResult(payload)
    }
}

/**
 * 提交新版本。
 *
 * ## 为什么需要 confirm 参数
 *
 * 各应用商店的 API **都不提供撤销版本更新的接口**，提交即不可逆。CLI 场景下
 * 有人类在键盘前敲命令，误触的代价有限；但作为 MCP 工具，调用方是自主决策的模型，
 * 一次误判就会把一个未经审阅的包推到正式渠道，且无法回滚。
 *
 * 因此这里要求显式传 `confirm: true`。这不是防御恶意调用（模型完全可以传 true），
 * 而是把「这一步不可逆」变成 schema 层面必须正视的事实，而不是藏在 description 里
 * 的一句提醒 —— 模型填这个参数时会看到它的说明。
 *
 * 宿主若需要人类确认，应当在批准工具调用时介入；本工具只保证不会因为参数省略
 * 而意外提交。
 */
private fun Server.registerUploadApk(service: PublishService) = addTool(
    name = "upload_apk",
    description = """
        上传 APK 并向应用商店提交新版本。

        警告：此操作不可撤销 —— 各应用商店均未提供撤销版本更新的 API。
        提交后若要停止发布，只能登录各商店后台手动操作。

        必须显式传 confirm=true 才会执行。建议先调用 check_release 预检。
        本工具立即返回 jobId，不等待上传完成；用 get_upload_status 轮询进度。
    """.trimIndent(),
    inputSchema = Schema.obj(
        "applicationId" to Schema.string("包名"),
        "artifactPath" to Schema.string("APK 文件路径，或存放多渠道包的目录"),
        "updateDesc" to Schema.string("更新说明，会提交给商店审核"),
        "stopAfter" to Schema.string(
            "流程走到哪一步就停下。artifact=仅上传安装包，不创建任何版本，" +
                "可用于验证凭据与签名是否可用；draft=停在草稿态，可先到渠道后台核对再送审；" +
                "submit=一路走到送审。不指定则各渠道走到各自能到的最远阶段 —— " +
                "多数渠道是送审，鸿蒙因送审未经验证只到草稿。" +
                "注意：并非所有渠道都支持中途停下，小米的 dev/push 是原子请求只能 submit，" +
                "OPPO/vivo 没有草稿态只支持 artifact 或 submit，鸿蒙不支持 submit。" +
                "调用前请先看 list_channels 返回的 capability.supportedStages",
            enum = listOf("artifact", "draft", "submit"),
        ),
        "confirm" to Schema.boolean(
            "当本次操作包含送审时必须为 true（stopAfter=submit，或未指定 stopAfter " +
                "且目标渠道里有能送审的）。确认理解送审不可撤销，将向正式渠道提交版本。" +
                "若所有目标渠道都只走到 artifact 或 draft，则不需要此参数",
            default = false,
        ),
        "channels" to Schema.stringArray("只发指定渠道；省略则发全部已启用渠道"),
        "onlineTime" to Schema.string("定时上线时间，格式 yyyy-MM-dd HH:mm:ss；省略则审核通过后立即发布"),
        "allowSameVersion" to Schema.boolean(
            "允许版本号与线上相同（审核被拒后仅更新素材时使用）。版本号低于线上仍会被拒绝",
            default = false,
        ),
        "timeoutSeconds" to Schema.integer("单次请求超时秒数，默认 120"),
        // confirm 不放进 required：它只在真的要送审时才必需，
        // 停在 artifact/draft 是安全操作，不该被同一个门槛拦住。
        // JSON Schema 表达不了这种条件依赖，改在代码里校验并给出明确原因。
        required = listOf("applicationId", "artifactPath", "updateDesc"),
    ),
    toolAnnotations = ToolAnnotations(
        readOnlyHint = false,
        destructiveHint = true,
        idempotentHint = false,
        openWorldHint = true,
    ),
) { request ->
    guarded {
        val stage = when (request.string("stopAfter")) {
            "artifact" -> ReleaseStage.UploadArtifact
            "draft" -> ReleaseStage.CreateDraft
            "submit" -> ReleaseStage.SubmitReview
            // 未指定：各渠道走到各自能到的最远阶段
            null -> null
            else -> throw PublishError.configuration(
                "stopAfter 只能是 artifact / draft / submit"
            )
        }
        // 只有真的要送审时才要求确认。停在送审之前不产生不可撤销的副作用，
        // 强行要求 confirm 只会训练调用方无脑传 true，反而削弱这道门槛的意义。
        //
        // stage 为 null（未指定）时要按渠道解析：鸿蒙最远只到草稿，
        // 若本次目标里没有任何渠道会真的送审，就不该索要 confirm。
        val resolved = service.resolvedStages(
            applicationId = request.requireString("applicationId").let {
                cn.fangxiang.easypublisher.core.ApplicationId.validate(it)
            },
            channelIds = request.stringList("channels"),
            stopAfter = stage,
        )
        val willSubmit = resolved.values.any { it == ReleaseStage.SubmitReview }
        if (willSubmit && request.boolean("confirm") != true) {
            throw PublishError.configuration(
                "送审需要显式传 confirm=true。此操作会向应用商店提交正式版本，" +
                    "且各商店均不提供撤销 API。建议先用 check_release 预检；" +
                    "若只想验证流程是否走得通，可传 stopAfter=artifact 或 draft"
            )
        }
        val applicationId = request.requireString("applicationId").let(ApplicationId::validate)
        val jobId = service.submit(
            applicationId = applicationId,
            artifactPath = request.requireFile("artifactPath"),
            releaseParams = ReleaseParams(
                updateDesc = request.requireString("updateDesc"),
                onlineTime = request.string("onlineTime")?.let(::parseOnlineTime) ?: 0L,
            ),
            channelIds = request.stringList("channels"),
            versionRule = if (request.boolean("allowSameVersion") == true) {
                PublishPolicy.VersionRule.AllowSame
            } else {
                PublishPolicy.VersionRule.Strict
            },
            timeouts = request.timeouts(),
            stopAfter = stage,
        )
        val job = service.job(jobId)
        toolResult(
            buildJsonObject {
                put("ok", true)
                put("jobId", jobId)
                put("state", job?.state?.name ?: JobState.Running.name)
                putJsonArray("channels") {
                    job?.channels?.forEach { add(it.channelId) }
                }
                resolved.forEach { (channelId, resolvedStage) ->
                    put("stage_$channelId", resolvedStage.name)
                }
                put(
                    "note",
                    if (willSubmit) {
                        "用 get_upload_status 轮询进度。上传大包可能需要数分钟到数十分钟。"
                    } else {
                        "本次不含送审。用 get_upload_status 轮询；" +
                            "完成后各渠道的 reachedStage 会显示实际到达的阶段。"
                    },
                )
            }
        )
    }
}

private fun Server.registerGetUploadStatus(service: PublishService) = addTool(
    name = "get_upload_status",
    description = """
        查询 upload_apk 返回的任务进度。
        state 为 Running 时应稍后再查（建议间隔 5 秒以上）；
        Succeeded / PartiallyFailed / Failed / Cancelled 为终态。
    """.trimIndent(),
    inputSchema = Schema.obj(
        "jobId" to Schema.string("upload_apk 返回的任务 id"),
        required = listOf("jobId"),
    ),
    toolAnnotations = ToolAnnotations(readOnlyHint = true, openWorldHint = false),
) { request ->
    guarded {
        val jobId = request.requireString("jobId")
        val job = service.job(jobId)
            ?: throw PublishError.configuration("任务不存在：$jobId")

        toolResult(
            buildJsonObject {
                put("ok", job.state == JobState.Succeeded)
                put("jobId", job.id)
                put("state", job.state.name)
                put("done", job.done)
                put("applicationId", job.applicationId)
                put("versionCode", job.versionCode)
                put("versionName", job.versionName)
                // null 表示「各渠道走到各自能到的最远阶段」（鸿蒙止于草稿，其余到送审），
                // 此时没有统一的请求阶段；每个渠道实际到达的阶段见 reachedStage
                put("requestedStage", job.requestedStage?.name)
                putJsonArray("channels") {
                    job.channels.forEach { progress ->
                        add(
                            buildJsonObject {
                                put("id", progress.channelId)
                                put("displayName", progress.displayName)
                                put("stage", progress.stage::class.simpleName ?: "Unknown")
                                (progress.stage as? ChannelStage.Succeeded)?.let {
                                    put("reachedStage", it.reached.name)
                                    put("reachedStageLabel", it.reached.label)
                                }
                                put("message", progress.stage.label)
                                (progress.stage as? ChannelStage.Failed)?.let { failed ->
                                    put("kind", failed.kind.name)
                                    put("kindLabel", failed.kind.label)
                                    failed.code?.let { put("code", it) }
                                    put("retryable", failed.retryable)
                                    put("phase", failed.phase.name)
                                    put("detail", failed.message)
                                }
                                (progress.stage as? ChannelStage.Uploading)?.let {
                                    put("progress", it.fraction)
                                }
                            }
                        )
                    }
                }
            }
        )
    }
}

// ---- 参数读取 ----

private fun CallToolRequest.arg(name: String): kotlinx.serialization.json.JsonElement? =
    params.arguments?.get(name)

private fun CallToolRequest.string(name: String): String? =
    arg(name)?.let { runCatching { it.jsonPrimitive.content }.getOrNull() }?.takeIf { it.isNotBlank() }

private fun CallToolRequest.requireString(name: String): String =
    string(name) ?: throw PublishError.configuration("缺少必填参数 $name")

private fun CallToolRequest.boolean(name: String): Boolean? =
    arg(name)?.let { runCatching { it.jsonPrimitive.content.toBooleanStrict() }.getOrNull() }

private fun CallToolRequest.stringList(name: String): List<String>? =
    arg(name)?.let { element ->
        runCatching {
            element.jsonArray.map { it.jsonPrimitive.content }.filter { it.isNotBlank() }
        }.getOrNull()
    }?.takeIf { it.isNotEmpty() }

private fun CallToolRequest.requireFile(name: String): File {
    val path = requireString(name)
    val file = File(path)
    if (!file.exists()) {
        throw PublishError.localFile("路径不存在：$path")
    }
    return file
}

private fun CallToolRequest.timeouts(): HttpTimeouts {
    val seconds = arg("timeoutSeconds")
        ?.let { runCatching { it.jsonPrimitive.content.toLong() }.getOrNull() }
    return seconds?.let { HttpTimeouts.ofSeconds(it) } ?: HttpTimeouts.DEFAULT
}

private fun parseOnlineTime(text: String): Long {
    val format = java.text.SimpleDateFormat("yyyy-MM-dd HH:mm:ss", java.util.Locale.US)
    format.isLenient = false
    val parsed = runCatching { format.parse(text) }.getOrNull()
        ?: throw PublishError.configuration("定时上线时间格式不正确：$text，应为 yyyy-MM-dd HH:mm:ss")
    if (parsed.before(java.util.Date())) {
        throw PublishError.configuration("定时上线时间不能早于当前时间：$text")
    }
    return parsed.time
}

private fun kotlinx.serialization.json.JsonArrayBuilder.add(value: String) {
    add(kotlinx.serialization.json.JsonPrimitive(value))
}

private fun kotlinx.serialization.json.JsonArrayBuilder.add(value: JsonObject) {
    add(value as kotlinx.serialization.json.JsonElement)
}
