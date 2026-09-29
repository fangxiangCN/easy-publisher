# Go 版与 Kotlin 版的差异

两个实现都在仓库里。本文记录它们**有意保留**的差异，以及对齐时必须注意的地方。

先给结论：**功能等价，行为差异集中在三处 —— 启动性能、help 文案格式、以及几个我移植时有意改进的文案。** 核心契约（退出码、`--json` 结构、错误分类、签名算法）逐字节一致。

## 实测数据

用同一份配置、同一条命令跑两个版本：

| 项 | Go 版 | Kotlin 版 |
|---|---|---|
| CLI 产物 | 11.2 MB 静态二进制 | 18.6 MB fat jar |
| MCP 产物 | 13.2 MB 静态二进制 | 13.8 MB fat jar |
| **冷启动（app list）** | **4 ms** | **496 ms** |
| 运行时依赖 | 无 | JDK 17 |

启动时间差 **124 倍**。这不是微优化 —— 它是重写的主要动机。CLI 会被脚本、CI、agent 反复调用，每次半秒的 JVM 启动在批量场景下很显眼。

## 核心契约：逐字节一致

用同一份配置跑了 11 个命令，逐字节比对（`cmp` 而非 `diff`，原因见文末）：

| 命令 | stdout | 退出码 |
|---|---|---|
| `app list` | 字节一致 | 0 / 0 |
| `app list --json` | 字节一致 | 0 / 0 |
| `channel list` | **有差异**（仅华为 note，见下） | 0 / 0 |
| `channel list --json` | 同上 | 0 / 0 |
| `channel list --app` | 同上 | 0 / 0 |
| `status`（不存在的应用） | 字节一致 | 1 / 1 |
| `status --json` | 字节一致 | 1 / 1 |
| `app add`（非法包名） | 字节一致 | 1 / 1 |
| `channel set`（未知渠道） | 字节一致 | 1 / 1 |
| `app add`（缺必填参数） | 字节一致 | 1 / 1 |
| 未知子命令 | 字节一致 | 1 / 1 |

一致的部分包括退出码映射（0-7）、`--json` 的字段名与嵌套结构、错误输出的 `kind`/`phase`/`retryable`、以及全部 20 个渠道参数的描述文案。

签名算法另有黄金向量守着（`go/testdata/golden/signing.json`）：OPPO 与 vivo 的待签串与 HMAC 签名、小米的 RequestData JSON 与 MD5，两版逐字节相同。

## 差异一：help 输出的格式（框架决定，不对齐）

```
Go 版（cobra）                    Kotlin 版（Clikt）
Usage:                            Usage: easy-publisher [<options>] <command>...
  easy-publisher [command]
Available Commands:               Commands:
Flags:                            Options:
```

两处额外差异：

- cobra 自带 `completion` 与 `help` 两个子命令，Clikt 没有
- cobra 把 `--verbose` 等持久 flag 列在根命令下

这是框架差异，不值得为对齐而改。**脚本不应解析 help 文本** —— 要解析的是 `--json` 的输出，那是逐字节一致的。

## 差异二：参数描述文案（已对齐）

移植时我改动了几处描述但没同步回 Kotlin，导致两版对同一参数给出不同说明。逐项核对后**取各自更好的表述**，而不是单边迁就：

| 参数 | 问题 | 处理 |
|---|---|---|
| 华为 `client_id`/`client_secret` | Kotlin 是「客户端ID」「**秘钥**」—— 后者是错别字 | 取 Go 文案 |
| 荣耀 `client_id`/`client_secret` | Kotlin 同上（旧文案 + 错别字） | 取 Go 文案 |
| OPPO `client_id` | Go 版移植时**精简过度**，丢了「在「账号管理 - API 密钥」中获取」 | 取 Kotlin 文案（补回 Go） |
| OPPO `client_secret` | Kotlin 只是「与 client_id 成对获取」，Go 版有 URL query 风险提示 | 取 Go 文案（补到 Kotlin） |

最后一处值得说明：那条风险提示（`client_secret` 会出现在取 token 的 URL query 上）是安全相关信息，比「成对获取」有价值得多，所以补进 Kotlin 而非反向去掉。

现在 20 个参数的描述**逐字节一致**。

## 差异二之附：`--json` 的字段（已对齐）

Go 版最初在 `capability` 里多输出了三个中文标签字段（`riskLevelLabel` / `withdrawalLabel` / `evidenceLabel`），Kotlin 版没有。

虽然多字段通常向后兼容，但这会让脚本在两版间切换时取到 `null`。已补齐 Kotlin 版，两版 JSON 结构现在一致。

## 差异三：华为渠道的 note（唯一保留的差异）

`channel list` 仍有差异，只有一行 —— 华为渠道的 note：

```
go: ……（最长 3 分钟）。AGC 文档提供「撤销审核」接口，但本工具未实现该调用。
kt: ……（最长 3 分钟）。
```

由来：加鸿蒙渠道时核对华为文档，发现上游 issue #16 的结论「各商店 API 都不提供撤销」**不准确** —— AGC 文档里确实有 `on-shelf-cancel` 接口。因此把 `Withdrawal` 标成 `ApiSupported` 并在 note 里说明「接口存在但本工具未实现」，避免使用者误以为能撤回。

只改了 Go 版。**这条保留为差异**：Go 版是主实现，note 更准确；Kotlin 版作为参照实现不必追平文案。

表格列宽随之变化（最长描述多了一句），这是副作用，不是问题 —— 脚本应解析 `--json`，不该解析表格。

## 移植时有意做的改进（不要「修正」回去）

如果将来有人要对齐两版代码，下面这些是**故意的**，不是漏改：

- **Go 版新增 `internal/output` 包**：退出码表、表格渲染、错误输出统一收口。Kotlin 版这些散在 `Support.kt` 与 `Output.kt` 里。
- **`logx` 在构造时把 slog handler 绑到 stderr**，包内无写 stdout 的路径。Kotlin 版靠 `System.setOut` 重定向兜底 —— 那是补救措施。
- **`channel` 包不引用具体渠道实现**，由 `internal/builtin` 统一注册，打破依赖环。Kotlin 版的 `ChannelRegistry` 直接 list 具体类。
- **`httpx.Upload` 用 `http.NewRequestWithContext`**，ctx 取消直接中断在途请求。Kotlin 版的 `ProgressBody` 要靠 `Thread.interrupted()` 轮询。
- **小米的 `Result` 用 `*int`**：Go 里 `int` 的零值是 0，而 0 恰好是「成功」的码，缺失字段会静默变成成功。
- **`marshalNoHTMLEscape`**：Go 的 `encoding/json` 默认转义 `& < >`，Moshi 不转义。不关掉会让 RequestData 的 MD5 对不上。

## 交付方式（已对齐）

两个版本都用 `easy-publisher` / `easy-publisher-mcp` 作为命令名，因此：

- `skills/easy-publisher/SKILL.md` 不用改调用方式
- 配置文件格式互通（JSON 字段名逐字一致，可直接换用）
- 退出码与 `--json` 结构一致，脚本无需改动

## 复现这套对比

```bash
cd go && go build -o /tmp/ep-go ./cmd/easy-publisher
cd .. && ./gradlew :cli:shadowJar

GO=/tmp/ep-go
JAR=cli/build/libs/easy-publisher-1.0.0.jar

# 用同一份配置跑同一命令，按字节比对
cmp_case() {
  local desc="$1"; shift
  EP_HOME=/tmp/cmp-go $GO "$@" >/tmp/g.out 2>/dev/null; local grc=$?
  EP_HOME=/tmp/cmp-kt java -jar "$JAR" "$@" >/tmp/k.out 2>/dev/null; local krc=$?
  cmp -s /tmp/g.out /tmp/k.out && local s=一致 || local s=有差异
  printf "%-30s %s  退出码 %s/%s\n" "$desc" "$s" "$grc" "$krc"
}
cmp_case "app list" app list
cmp_case "channel list" channel list
cmp_case "status --json" status --app com.example.absent --json
```

### 两个踩过的坑

**用 `cmp` 而不是 `diff`。** macOS 的 `diff` 处理超长中文行时可能**不报差异** ——
我在做对比时遇到过 `diff` 显示无差异、而 `cmp` 报第 749 字节不同。
行内容相同但行尾空白或换行不同时也会这样。

**这个执行环境里未加引号的变量不做词分割。** `cmd="app list"; $GO $cmd`
会把 `app list` 当成**一个**参数传给 cobra，报 `unknown command "app list"`。
两个版本都会报同样的错，于是比对结果看起来是「字节一致」—— 实为假一致。
用 `"$@"` 传参，或显式 `read -ra args <<< "$cmd"` 分词。

第二条尤其值得记：**它让一次对比全部假通过**，而假通过比报错更难发现。
