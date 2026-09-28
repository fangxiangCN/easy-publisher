package cn.fangxiang.easypublisher.mcp

import io.modelcontextprotocol.kotlin.sdk.types.ToolSchema
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/**
 * 构造 JSON Schema 的小工具，避免手写大段 buildJsonObject 嵌套。
 */
internal object Schema {

    fun obj(
        vararg properties: Pair<String, JsonObject>,
        required: List<String> = emptyList(),
    ): ToolSchema = ToolSchema(
        properties = buildJsonObject {
            properties.forEach { (name, spec) -> put(name, spec) }
        },
        required = required,
    )

    fun string(description: String, enum: List<String>? = null): JsonObject = buildJsonObject {
        put("type", "string")
        put("description", description)
        if (enum != null) {
            put(
                "enum",
                kotlinx.serialization.json.JsonArray(enum.map { JsonPrimitive(it) }),
            )
        }
    }

    fun boolean(description: String, default: Boolean? = null): JsonObject = buildJsonObject {
        put("type", "boolean")
        put("description", description)
        if (default != null) put("default", default)
    }

    fun integer(description: String): JsonObject = buildJsonObject {
        put("type", "integer")
        put("description", description)
    }

    fun stringArray(description: String): JsonObject = buildJsonObject {
        put("type", "array")
        put("description", description)
        put("items", buildJsonObject { put("type", "string") })
    }

    val empty: ToolSchema get() = ToolSchema(properties = JsonObject(emptyMap()), required = emptyList())
}
