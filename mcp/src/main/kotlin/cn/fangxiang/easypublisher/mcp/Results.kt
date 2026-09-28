package cn.fangxiang.easypublisher.mcp

import cn.fangxiang.easypublisher.core.PublishError
import io.modelcontextprotocol.kotlin.sdk.types.CallToolResult
import io.modelcontextprotocol.kotlin.sdk.types.TextContent
import kotlinx.coroutines.CancellationException
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

private val prettyJson = Json { prettyPrint = true; encodeDefaults = true }

/**
 * 工具返回值。
 *
 * 同时给出 JSON 文本与 structuredContent —— 前者让模型能直接读懂，
 * 后者让宿主可以按字段取值。
 */
internal fun toolResult(payload: JsonObject): CallToolResult = CallToolResult(
    content = listOf(TextContent(prettyJson.encodeToString(JsonElement.serializer(), payload))),
    isError = false,
    structuredContent = payload,
)

internal fun toolError(payload: JsonObject): CallToolResult = CallToolResult(
    content = listOf(TextContent(prettyJson.encodeToString(JsonElement.serializer(), payload))),
    isError = true,
    structuredContent = payload,
)

/**
 * 统一把异常翻译成结构化错误。
 *
 * 失败不抛给传输层，而是作为 `isError = true` 的结果返回 —— 这样调用方
 * （通常是模型）能读到原因并自行决定重试、换渠道还是交给人处理。
 */
internal suspend fun guarded(block: suspend () -> CallToolResult): CallToolResult =
    try {
        block()
    } catch (e: CancellationException) {
        throw e
    } catch (e: PublishError) {
        toolError(
            buildJsonObject {
                put("ok", false)
                put("kind", e.kind.name)
                put("message", e.message ?: "未知错误")
                e.channel?.let { put("channel", it) }
                e.code?.let { put("code", it) }
                put("retryable", e.retryable)
                // phase 解释「为什么不可重试」。越过送审点后 retryable 恒为 false，
                // 但原因不是错误不可恢复，而是无法确定服务端是否已受理 ——
                // 调用方（通常是模型）需要据此决定是去后台确认，而不是换个姿势重试。
                put("phase", e.phase.name)
                e.raw?.takeIf { it.isNotBlank() }?.let { put("rawResponse", it.take(1000)) }
            }
        )
    } catch (e: Exception) {
        toolError(
            buildJsonObject {
                put("ok", false)
                put("kind", "Unknown")
                put("message", e.message ?: e::class.simpleName ?: "未知错误")
                put("retryable", false)
            }
        )
    }
