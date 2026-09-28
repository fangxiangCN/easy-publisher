package cn.fangxiang.easypublisher.cli

import com.squareup.moshi.Moshi
import com.squareup.moshi.Types

/**
 * 结构化输出。
 *
 * 约定：**stdout 只放结果，日志一律走 stderr**（core 的 AppLogger 已保证这一点）。
 * 这样 `easy-publisher status --json | jq` 才不会被日志行污染，
 * 也是 MCP stdio transport 能复用同一套 core 的前提。
 */
object Output {

    private val moshi = Moshi.Builder().build()

    private val adapter = moshi
        .adapter<Map<String, Any?>>(
            Types.newParameterizedType(Map::class.java, String::class.java, Any::class.java)
        )
        .indent("  ")
        .serializeNulls()

    fun json(value: Map<String, Any?>): String = adapter.toJson(value)

    /** 失败结果的统一形状，便于脚本判断 */
    fun errorJson(
        kind: String,
        message: String,
        channel: String? = null,
        code: String? = null,
        retryable: Boolean = false,
    ): String = json(
        mapOf(
            "ok" to false,
            "kind" to kind,
            "channel" to channel,
            "code" to code,
            "message" to message,
            "retryable" to retryable,
        )
    )
}

/** 表格化输出，列宽按内容自适应 */
fun renderTable(headers: List<String>, rows: List<List<String>>): String {
    if (rows.isEmpty()) return ""
    val widths = headers.indices.map { column ->
        (rows.map { it.getOrElse(column) { "" } } + headers[column])
            .maxOf { it.displayWidth() }
    }
    val line = StringBuilder()
    fun appendRow(cells: List<String>) {
        cells.forEachIndexed { index, cell ->
            line.append(cell)
            if (index != cells.lastIndex) {
                line.append(" ".repeat(widths[index] - cell.displayWidth() + 2))
            }
        }
        line.append(System.lineSeparator())
    }
    appendRow(headers)
    appendRow(widths.map { "-".repeat(it) })
    rows.forEach { appendRow(it) }
    return line.toString().trimEnd()
}

/**
 * 中文字符在等宽终端里占两列，直接用 length 会让表格错位。
 */
private fun String.displayWidth(): Int = sumOf { char ->
    if (char.code in 0x1100..0x115F || char.code in 0x2E80..0xA4CF ||
        char.code in 0xAC00..0xD7A3 || char.code in 0xF900..0xFAFF ||
        char.code in 0xFE30..0xFE6F || char.code in 0xFF00..0xFF60 ||
        char.code in 0xFFE0..0xFFE6
    ) 2 else 1
}
