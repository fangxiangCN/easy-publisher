# 重构规划：脱离 JVM

当前实现是 Kotlin + Gradle，运行需要 JDK 17。本文规划用 Go 重写，产物是单个静态二进制。

## 先确认目标：三个选项，代价差一个数量级

「依赖 JDK、太重」可以指两件不同的事，解法完全不同。

| 选项 | 解决什么 | 工作量 | 代价 |
|---|---|---|---|
| **jlink / jpackage** | 用户不需要装 JDK | ~1 天，**零代码改动** | 产物 40-60MB，仍有 JVM 启动开销（约 0.5-1s） |
| **GraalVM native-image** | 单二进制、启动快 | ~1 周 | OkHttp 与 Moshi 的反射需要 reachability metadata，Moshi 要换成 kotlinx-serialization；构建链复杂，跨平台编译麻烦 |
| **Go 重写** | 单二进制、无运行时、交叉编译零成本、开发链轻 | ~10 个工作日 | 全部代码重写，72 个测试要移植，已积累的实现知识有丢失风险 |

**如果痛点是「用户要装 JDK」，jlink 今天就能解决，不该重写。**
**如果痛点是「不想维护 JVM 项目 / 要小产物 / 要快启动 / 交叉编译给 Windows 用户」，才值得重写。**

下面按 Go 重写规划。

## 为什么是 Go

不是泛泛的「Go 性能好」，是这个项目的具体约束：

**加密全部落在标准库里，能彻底去掉 BouncyCastle。**
小米的 `RSA/NONE/PKCS1Padding` 对应 `crypto/rsa.EncryptPKCS1v15`，证书解析对应
`crypto/x509.ParseCertificate`，117 字节分组自己切。OPPO/vivo 的 HMAC-SHA256 是
`crypto/hmac` + `crypto/sha256`。签名是整个工具最不容错的部分，用标准库意味着少一层不确定性。

**APK 解析有可用库。** [`avast/apkparser`](https://github.com/avast/apkparser)
（约 154 star，2026 年 4 月仍有提交）解析二进制 AndroidManifest，自带 `axml2xml`
工具可做交叉验证。这原本是我最担心的点 —— 参照实现 app-ship 就是因为没解决它，
选择 shell out 到 `aapt2`（那等于把 JDK 依赖换成 Android SDK 依赖，更糟）。

**`encoding/json` 的零值语义正好匹配我们的模型设计。**
现有 Kotlin 模型全部是「字段可空 + 默认值」，就是为了渠道少返回一个字段不崩溃。
Go 的 `json.Unmarshal` 对缺失字段天然填零值，不需要 Moshi 那套 `KotlinJsonAdapterFactory`，
也不需要 Gson 手写解析。

**官方 MCP SDK 存在**：[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)，
支持 stdio，不用自己实现 JSON-RPC。

**取消语义比协程更干净。** 现在的 `ProgressBody` 要靠 `Thread.interrupted()` 轮询才能中断上传；
Go 的 `http.NewRequestWithContext` 让 context 取消直接中断 in-flight 请求，
「取消上传后大文件仍在后台传完」这个问题从根上消失。

**交叉编译零工具链**：`GOOS=windows GOARCH=amd64 go build` 就能出 Windows 包。

Rust 的唯一实质优势是二进制更小、无 GC，对这个每次运行数分钟、瓶颈全在网络 IO 的工具没有意义，
却要付出所有权模型的学习成本。TypeScript 不解决问题 —— app-ship 就是 TS，它要求 Node ≥ 16，
只是把 JDK 依赖换成了 Node 依赖。

## 模块映射

```
easy-publisher/                 (Go)
├── go.mod
├── internal/
│   ├── artifact/               ArtifactInfo, ArtifactKind, APK/.app 解析, ApplicationId 校验
│   ├── channel/                Channel 接口, ChannelCapability, ReleaseStage, 注册表
│   │   ├── huawei/  mi/  oppo/  vivo/  honor/  harmony/
│   ├── config/                 AppConfig, Store(0600+原子写), CredentialStore(环境变量优先)
│   ├── httpx/                  超时配置, requireHTTPS, 进度 Body, 响应一律关闭
│   ├── publish/                PublishError, FailurePhase, Policy, Service, Job
│   └── logx/                   slog 封装, 脱敏, 文件轮转
├── cmd/
│   ├── easy-publisher/         CLI (cobra)
│   └── easy-publisher-mcp/     MCP server
└── skills/easy-publisher/      SKILL.md（直接复用，只改二进制调用方式）
```

概念对应关系：

| Kotlin | Go | 说明 |
|---|---|---|
| `Channel` 接口 | `type Channel interface` | 方法签名基本一致 |
| `PublishError` (class) | `type PublishError struct` + `Error() string` | 用 `errors.As` 做类型判断 |
| `FailurePhase` | 同名字段的常量 | `atSubmissionPoint{}` 包装器变成一个 `markSubmitted(err)` 函数 |
| `CancellationException` 重抛 | `ctx.Err()` / `context.Canceled` | Go 里不需要「先 catch 再重抛」这个陷阱 |
| `supervisorScope + async` | `sync.WaitGroup` + 结果切片 | 保留「单渠道失败不影响其他」的语义 |
| `AppLogger`（手写） | `log/slog` 写 `os.Stderr` | 级别过滤、结构化字段都是现成的 |
| `Dispatchers.IO` | 不需要 | goroutine 天然处理阻塞 IO |
| Moshi data class | struct + `json:"..."` tag | 全部字段用指针或 `omitempty` 表达可选 |
| Clikt | `spf13/cobra` | 子命令树、`--json` flag、退出码 |

`logx` 用 slog 是个明显收益：「日志绝不写 stdout」这个 MCP 硬约束从「靠纪律维持」
变成「构造时就把 handler 绑到 stderr」，结构上无法违反。现在 Kotlin 版是靠
`System.setOut` 重定向来兜底的，那是补救措施而不是设计。

## 风险排序与对策

### R1 签名字节等价性 — 后果最严重

一个字节差异就是鉴权失败，而渠道只回一个笼统的签名错误码，极难定位。

**对策：黄金向量测试（golden vectors）。** 在重写之前，用 Kotlin 版对固定输入
（固定 secret、固定 timestamp、固定参数集）产出「待签串 + 签名结果」，
存成 JSON fixture 提交到 Go 仓库，Go 侧断言逐字节相同。

OPPO/vivo 的 HMAC 是确定性的，可以做真正的字节相等断言。

**小米的 RSA 不行** —— PKCS#1 v1.5 填充带随机数，同一明文每次密文都不同。
所以小米只能验三件事：
1. 被加密的 SIG JSON 明文字节相等（这个是确定性的，也是最容易出错的地方）
2. 用测试密钥对做加解密回环，断言还原出的明文一致
3. 分组行为正确：输入 n 字节 → 输出 `ceil(n/117)*128` 字节

### R2 APK 解析保真度 — 概率最高

`avast/apkparser` 与 `net.dongliu:apk-parser` 是两个独立实现，对边缘 APK
（老构建工具、非常规 resource 编码、多 dex）的处理可能不同。

**对策**：拿一批真实 APK（覆盖不同 AGP 版本、minSdk、是否 split）跑双实现，
逐字段比对 `packageName` / `versionCode` / `versionName`。
这一步必须在写业务代码之前做完 —— 解析不出正确版本号，整个 `PublishPolicy`
的版本比对就是错的，而它错的方式是静默的。

`.app` 解析无风险：zip + `pack.info`（纯 JSON），标准库就够，
现有 Kotlin 测试已经用真实构造的 zip 验证过。

### R3 实现知识丢失 — 最容易被低估

代码注释里存着不少踩出来的结论，重写时如果只搬 API 形状就会丢掉：

- OPPO 的 `app/upd` 是全量更新语义，19 个字段必须原样回传，漏一个就被清空或拒绝
- 小米 multipart 里 apk part 的文件名是**空串**，看着像 bug 但不能改
- OPPO 必须**先判 errno 再解强类型 data**，否则失败响应的 data 形状不同会把真实错误码吞掉
- vivo 的 `code`/`subCode` JSON 类型不稳定（限流走数字、部分鉴权失败走字符串）
- `SimpleDateFormat` 必须显式指定 Locale，否则泰语区域按佛历格式化定时发布时间
- 鸿蒙走 v3、Android 走 v2，v3 是「appId 走 query + 负载走 body」
- 华为草稿态（releaseState=7）不返回 versionCode（上游 issue #7）

**对策**：Phase 1 产出一份语言无关的渠道规格文档，每个渠道记录
「调用序列 / 端点 / 方法与参数位置 / 成功判据 / 已知陷阱」。
这份文档本身就有价值，即使不重写也该有。

### R4 鸿蒙分片上传 — 细节密集

`nspPartMinSize` 必须用华为返回的值、分片键名是 `additionalProp1..N`（Swagger 占位符）、
签名地址的请求头必须原样转发、ETag 含引号也要原样回传。

**对策**：分片切分与 descriptor 构造是纯函数，可以脱离网络单测（给定文件大小与分片大小，
断言片数、每片长度、偏移量）。HTTP 部分无法在没有凭据的情况下验证，如实标注。

### R5 行为漂移

ReviewState 映射表、错误码解释、退出码语义都可能在重写中走样。

**对策**：72 个 Kotlin 测试是现成的验收清单，逐个移植。
它们大多是纯逻辑测试（策略、解析、能力矩阵），移植成本低。

## 执行顺序

**Phase 0 — 先用 Kotlin 版验证一个真实渠道（前置，不可跳过）**

现在六个渠道**没有一个用真实凭据跑过**。直接重写等于把未验证的逻辑翻译成另一种语言，
出问题时无法判断是原逻辑错了还是移植错了，不确定性翻倍。

```bash
easy-publisher status --app <包名> --channel huawei    # 只读，验证鉴权+签名+解析三环
```

这一步能验证的东西比我做过的所有静态检查都多，而且零副作用。

**Phase 1 — 提取黄金向量与渠道规格（1-2 天）**

- 跑 Kotlin 版产出签名 fixture（OPPO/vivo 的待签串与签名、小米的 SIG 明文）
- 写 `docs/CHANNEL-SPEC.md`，六个渠道各一节
- 双实现比对 APK 解析（R2 的验证）

**这一步的产物即使不重写也该留着**，它是把隐性知识变成显性文档。

**Phase 2 — Go 基础设施（3-5 天）**

`artifact` / `config` / `httpx` / `publish` / `logx`，以及移植过来的测试。
不含任何渠道，先把骨架和契约立起来。

**Phase 3 — 六个渠道（5-8 天）**

按风险从低到高：vivo → oppo → honor → huawei → harmony → mi。
每个渠道的验收标准是黄金向量逐字节相等 + 调用序列与规格文档一致。
小米放最后，因为它的加密最需要对照验证。

**Phase 4 — CLI + MCP（2-3 天）**

cobra 的命令树与 flag 直接照搬现有语义（含退出码表、`--json` 结构、`phase` 字段）。
MCP 六个工具的 schema 也照搬。SKILL.md 只需改二进制调用方式。

**Phase 5 — 处置 Kotlin 版**

建议**保留仓库不删**，作为参照实现和黄金向量的生成器。
在 README 里说明两者关系。

## 不要在重写期间同时扩渠道

app-ship 还有四个渠道我们没有（腾讯应用宝、蒲公英、Google Play、Apple）。
重写是补它们的好时机，但**不要和重写混在一起做** ——
否则出问题时无法区分是移植引入的还是新渠道本身的。

先 1:1 移植到行为等价，再逐个加新渠道。

其中 Apple 有架构冲突需要提前决定：IPA 实际上传依赖 Apple 的
`altool` / `iTMSTransporter` 外部工具，这会重新引入本次重写想摆脱的外部运行时依赖。

## 验证边界

重写完成后能验证的：编译、单元测试、黄金向量、CLI 端到端、MCP 协议握手、
凭据文件权限、APK/.app 解析。

**仍然不能验证的：真实向商店提交。** 这一点不会因为是 Go 版而改变。
所以 Phase 0 的价值不只是「给重写一个参照」，它本身就是当前最该做的事。
