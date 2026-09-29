# easy-publisher

把 Android APK 一键提交到多个应用商店。提供 CLI 与 MCP server 两种接入方式，无图形界面。

支持渠道：**华为 AppGallery、小米、OPPO、vivo、荣耀、鸿蒙 AppGallery**。

鸿蒙渠道走 AGC 的 **v3** 接口（Android 版是 v2，两者路径不同不可混用），
支持上传 App Pack、关联草稿与送审。

> 本项目衍生自 [Xigong93/XiaoZhuan](https://github.com/Xigong93/XiaoZhuan)
> （Copyright 2024 Xigong，Apache License 2.0）。原项目是一个 Compose Desktop 图形工具，
> 渠道对接逻辑由原作者完成。本项目将其重构为 headless 形态，移除了图形界面，
> 并把渠道任务改为无状态、重做了凭据存储与错误模型。详见 [NOTICE](NOTICE)。

## 为什么是 headless

图形工具需要人在屏幕前点。而发版这件事越来越常由脚本、CI 或 AI agent 触发 ——
它们需要的是稳定的退出码、结构化的错误、以及可以被轮询的长任务，不是进度条。

因此本项目：

- **stdout 只放结果，日志一律走 stderr。** `--json` 的输出可以直接管道给 `jq`。
- **退出码按错误类型区分**，脚本据此决定重试、换渠道还是交给人。
- **凭据不经过调用方的参数**。CLI 与 MCP 的接口都不接受 secret，
  凭据由程序自己从配置文件或环境变量读取 —— 这样密钥不会进入 shell 历史、
  CI 日志或 AI 模型的对话上下文。

## 安装

需要 Go 1.24+。产物是单个静态二进制，**不需要 JDK 或任何运行时**；
交叉编译也只需换 `GOOS`/`GOARCH`。

```bash
cd go
go build -o easy-publisher ./cmd/easy-publisher
go build -o easy-publisher-mcp ./cmd/easy-publisher-mcp
```

> 早期版本还有一个 Kotlin 实现，作为参照用。它已删除 —— 详见文末
> 「为什么删掉了 Kotlin 版」。

## 快速开始

```bash
# 1. 登记应用（会打印出每个渠道需要填哪些参数）
easy-publisher app add --id com.example.app --name 我的应用 --channels huawei,mi

# 2. 填凭据
easy-publisher channel set --app com.example.app --channel huawei --key client_id --value xxx
# 敏感值建议从文件读，避免进入 shell 历史
easy-publisher channel set --app com.example.app --channel mi --key publicKey --value-file ./mi.cer

# 3. 发版前先看各渠道状态
easy-publisher status --app com.example.app

# 4. 发版
easy-publisher upload --app com.example.app --artifact ./app-release.apk --desc "修复若干问题"

# 鸿蒙（.app 包，app_id 需单独配置）
easy-publisher channel set --app com.example.harmony --channel harmony --key app_id --value 123456
easy-publisher upload --app com.example.harmony --artifact ./demo.app --desc "修复若干问题"
```

### 凭据

两种来源，**环境变量优先**：

| 来源 | 说明 |
|---|---|
| `~/.easy-publisher/apps/<包名>.json` | 权限 600，原子写入 |
| `EP_<渠道>_<参数>` | 如 `EP_HUAWEI_CLIENT_SECRET`，CI 场景无需落盘 |

`easy-publisher channel list --app <包名>` 会显示每个参数当前是「已配置 / 环境变量 / 缺失」，
但不会显示值。

配置根目录可用环境变量 `EP_HOME` 覆盖。

## 各渠道的能力差异

**这是本项目最重要的一张表。** 各商店的 API 粒度差别很大，用统一的「上传」掩盖这个差异，
会让人误以为所有渠道都有反悔的机会。

| 渠道 | 风险 | 可停在 | 撤回 | 制品 |
|---|---|---|---|---|
| 华为 | 高 | 上传 / **草稿** / 送审 | API 支持¹ | `.apk` |
| 荣耀 | 高 | 上传 / **草稿** / 送审 | 未验证 | `.apk` |
| OPPO | 极高 | 上传 / 送审 | 未验证 | `.apk` |
| vivo | 极高 | 上传 / 送审 | 未验证 | `.apk` |
| 小米 | 极高 | 送审 | 未验证 | `.apk` |
| 鸿蒙 | 高 | 上传 / **草稿** / 送审 | API 支持¹ | `.app` |

¹ 华为 AGC 文档里有「撤销审核」接口，所以平台层面支持撤回 —— 这与上游项目
「各商店 API 都不提供撤销」的结论不同。但本工具**尚未实现**该调用，
且其适用条件（是否仅限审核中状态）未核实，需要撤回时仍得到 AGC 后台操作。

- **草稿**：华为和荣耀可以先上传并绑定文件形成草稿版本，登录后台人工核对无误后再送审。
  ```bash
  easy-publisher upload --app com.example.app --artifact ./app.apk --desc "..." --stop-after draft
  ```
- **上传**：所有渠道（小米除外）都支持只把安装包传上去、不创建任何版本。
  用途是验证凭据、签名与文件是否被渠道接受 —— 比真发一版安全得多。
  ```bash
  easy-publisher upload ... --stop-after artifact
  ```
- **小米无处可停**：它的 `dev/push` 把上传与送审合并成一次原子请求。
  请求 `--stop-after draft` 会立即报错，而不是默默走到送审。
- **鸿蒙走 v3**：送审端点是 `api/publish/v3/app-submit`，Android 版是 v2。
  v3 系列统一是「appId 走 query + 负载走 JSON body」，与 v2 把 releaseTime 放 query
  的做法不同。`appId` 必须显式配置 —— 鸿蒙应用在 AGC 里是独立记录，
  用包名反查会拿到同名 Android 应用的 id。
- **撤回按渠道如实标注**：华为 AGC 有「撤销审核」接口（上游项目称各商店都不提供，
  这一点不准确），其余渠道没人逐个确认过，仍标「未验证」。
  「未验证」与「不支持」对使用者的含义不同，所以分开标。

不指定 `--stop-after` 时，各渠道走到**各自能到的最远阶段** —— 多数渠道是送审，
鸿蒙只到草稿。MCP 的 `upload_apk` 也据此判定 `confirm`：只有本次真的包含送审时才要求，
全部目标渠道都只走到 artifact/draft 时不需要。

`easy-publisher channel list` 会输出这张表及每个渠道的说明。

## MCP server

```bash
cd go && go build -o easy-publisher-mcp ./cmd/easy-publisher-mcp
```

在 MCP 客户端里配置：

```json
{
  "mcpServers": {
    "easy-publisher": {
      "command": "java",
      "args": ["-jar", "/path/to/easy-publisher-mcp-1.0.0.jar"]
    }
  }
}
```

提供 6 个工具：

| 工具 | 副作用 | 说明 |
|---|---|---|
| `list_apps` | 只读 | 已配置的应用（只返回参数名，不返回凭据值） |
| `list_channels` | 只读 | 渠道、所需参数、能力矩阵 |
| `get_market_state` | 只读 | 审核状态与线上版本号 |
| `check_release` | 只读 | 发布预检：逐渠道判断能否发版并说明原因 |
| `upload_apk` | **写** | 提交新版本，需显式 `confirm: true` |
| `get_upload_status` | 只读 | 轮询任务进度 |

两个刻意的设计：

1. **没有任何工具接受凭据参数。** secret 不会进入模型的上下文。
2. **`upload_apk` 要求 `confirm: true`，但停在送审之前时不需要。**
   提交不可撤销，而 MCP 的调用方是自主决策的模型，一次误判就会把未经审阅的包
   推到正式渠道。这道门槛不防恶意调用（模型完全可以传 true），
   而是把「不可逆」变成 schema 层面必须正视的事实。
   `stopAfter: "draft"` 或 `"artifact"` 是安全的，因此不设门槛。

stdout 被传输层独占：进程启动时先把真正的 fd 1 交给 transport，
再把 `System.out` 重定向到 stderr。这样即便依赖库里有 `println`，
也不会破坏 JSON-RPC 报文。

## Skill

`skills/easy-publisher/SKILL.md` 是给 AI agent 的使用说明，通过 Bash 调 CLI。
里面写明了退出码语义、上传耗时、以及「不要因为看到网络超时就自动重试 upload」——
后者是本工具最容易造成实际损害的误操作，详见下节。

## 错误处理与重试

退出码：

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 参数或配置错误 |
| 2 | 凭据缺失或被拒 |
| 3 | 渠道返回业务错误 |
| 4 | 网络失败或超时 |
| 5 | 不满足发布前置条件（如版本号不大于线上版本） |
| 6 | 本地文件问题 |
| 7 | 渠道响应无法解析，接口可能已变更 |

**退出码 4 不能一律重试。** `--json` 输出里的 `phase` 字段是关键：

- `PreSubmission` —— 失败发生在送审之前（取 token、查状态、上传文件、绑定草稿）。
  重试安全，远端最多留下一个草稿。
- `AtOrAfterSubmission` —— 失败发生在送审这一步或之后。**不要重试。**
  服务端可能已经受理，只是响应在回程丢失（超时与连接重置从客户端看无法区分）。
  重试会重复送审或产生重复版本，而这不可撤销。此时应当登录渠道后台确认。

部分渠道成功时退出码同样非零 —— 别因为非零就断定全部失败，读 `channels` 数组。

## 构建与测试

```bash
cd go
go build ./...
go test ./...                     # 332 个测试
go test ./... -race               # 编排层有并发，-race 不是可选项
go vet ./...

# 校验签名黄金向量与生成器是否一致
python3 scripts/gen-golden-vectors.py --check

# 验证「已提交的树」可构建（防 gitignore 误伤源码那类问题）
./scripts/verify-committed-tree.sh
```

## 已知限制

请务必读完这一节再用于生产。

**送审路径与小米渠道未经真实验证。**

已用真实凭据跑通的部分（见提交 `cf52205`）：

| 渠道 | 已实测 | 未实测 |
|---|---|---|
| 华为 | 鉴权、APK 上传、绑定草稿 | 送审 |
| 荣耀 | 鉴权、APK 上传、绑定草稿 | 送审 |
| 鸿蒙 | 鉴权、App Pack 分片上传、v3 关联草稿 | 送审 |
| OPPO | 鉴权、签名请求、文件上传 | 送审（`app/upd`） |
| vivo | 鉴权、签名请求、文件上传 | 送审 |
| 小米 | — | **全部** |

`easy-publisher channel list` 会逐渠道打印这个范围。两点要特别注意：

- **送审是不可撤销的那一步，而它一个渠道都没验证过。** 已验证的部分都是可安全重来的
  （上传文件、建草稿），风险最高的环节恰恰是没跑过的那个。
- **小米完全未验证**，而它的加密最复杂（RSA + 117 字节分组 + PKCS#1 v1.5）。
  首次使用请把小米排在最后。

首次使用建议：

```bash
# 只读操作，验证凭据、签名与响应解析三环是否都通
easy-publisher status --app <包名> --channel huawei

# 再验证文件是否被接受，不创建任何版本
easy-publisher upload --app <包名> --artifact <包> --desc x --stop-after artifact

# 最后再真发一个渠道
```

**OPPO 的 `client_secret` 在 URL query 上。** 这是 OPPO 接口的设计，
另一个独立实现（[flu-cli/app-ship](https://gitee.com/flu-cli/app-ship)）也是同样做法，
未能查证是否支持 POST form。已做 URL 编码（否则 secret 含 `&` 会被截断且原因不可见），
但凭据仍可能进入代理日志与网关审计。建议缩短该 secret 的轮换周期。

**华为与 OPPO 的 `icon_url` 报错未解决。** 部分应用提交时会遇到
`icon_url 不允许的文件格式` 或 `sensitivePermissionIconUrl is necessary`。
根因需要抓包对比才能定位。本项目只是让这类错误更好诊断：
区分「商店资料缺失」与「接口变更」，前者会给出可执行的中文提示。

**无法撤回版本。** 各商店 API 都不提供撤销版本更新的接口。

**鸿蒙送审的两处不确定。** 端点 `v3/app-submit` 与参数来自社区实现与论坛实测，
华为官方文档页是 JS 渲染的、抓不到完整字段表。具体是：

- `releaseTime` 的格式有两种说法。官方 v2 文档与一个可用的 v3 封装实现都写
  `yyyy-MM-dd'T'HH:mm:ssZZ`，个别帖子说是毫秒时间戳。本工具采用前者
  （两个较可靠来源一致，且与华为渠道已在用的格式相同）。
  **若定时发布报「时间格式有误」，首先怀疑这一点。**
- `registeredIdType` / `registeredIdNumber`（主体登记信息）做成了可选配置。
  社区实测某些应用缺失会被拒（`registeredIdType and registeredIdNumber can not be null`），
  但并非所有应用都需要，因此不强制。遇到该报错时用
  `channel set --channel harmony --key registered_id_type` 补上。

另外**商店里的「新版本介绍」需要人工维护**：v3 的语言信息接口形状未经验证，暂未实现。
`--desc` 只在长度符合华为 remark 要求的 10-300 字时作为提审备注提交，否则跳过并记日志。

**鸿蒙渠道未查询市场状态。** `status` 对鸿蒙会报错而不是返回猜测值：
复用华为的 `app-info` 查到的是同名 Android 应用的记录，把它当成鸿蒙应用的状态
报出去会误导发布决策。副作用是鸿蒙上传时跳过线上版本号比对，日志里会明确记一条。

## 与上游的差异

相对 [XiaoZhuan](https://github.com/Xigong93/XiaoZhuan) 的主要改动：

- 移除 Compose Desktop 图形界面，改为 CLI + MCP
- 渠道任务改为无状态：凭据与回调走方法入参。原实现的渠道是进程级单例却持有
  可变的 `clientId` / `clientSecret` / listener，并发场景下会用 A 应用的密钥
  上传 B 应用的包
- 修复上游 issue #7：应用有未上传 APK 的草稿版本时，华为不返回 `versionCode`，
  原实现声明为非空导致直接抛异常、无法发版
- 日志不再写 stdout，且级别过滤前置。原实现会把 access_token 与全部
  clientSecret 打印出来
- 凭据文件权限 600、原子写入、删除时真正移除（原实现只是改名为 `.bak`）
- 不移植关闭 TLS 校验的调试客户端
- 全部响应模型字段可空，避免渠道少返回一个字段就崩溃
- 结构化错误模型，区分 7 类原因并标注失败阶段
- 不依赖 gson / BouncyCastle / commons-codec：Go 版用标准库的 `encoding/json`、
  `crypto/rsa`、`crypto/md5`，顺带规避了原依赖的已知问题
  （gson 2.8.6 的 CVE-2022-25647、已停维护的 bcprov-jdk15on 等）
- 新增鸿蒙 AppGallery 渠道（上游不支持）：`.app` 走 Upload Management API
  分片上传 + v3 `app-package-info` 关联草稿，鉴权与华为同源因此复用其 token 模型
- 制品抽象从 APK 专用改为按扩展名分派。`.app` 的元信息在 zip 内的 `pack.info`
  （纯 JSON），比 APK 的二进制 AndroidManifest 简单，无需额外依赖

## 为什么删掉了 Kotlin 版

Go 版完成后，仓库里曾同时存在两个实现，Kotlin 版作为参照。**它已删除**，
原因是维护两个实现让每个 bug 的修复成本翻倍 —— 那段时间的三个渠道修复
（OPPO 异步任务、vivo 频率限制、荣耀年龄分级）每个都要改两遍。

删除前确认过三件事：

1. **Go 的构建与测试不依赖 Kotlin。** 测试读的是已提交的静态 fixture，
   不是构建产物。
2. **真实 API 已成为参照。** 上面那三个修复都是靠真实提交发现的，
   参照实现的价值随之下降 —— 它最有用的时刻是在真实调用之前。
3. **黄金向量的生成能力已被替代。** 那是唯一真正会丢失的东西：9 条签名向量
   原本由 Kotlin 生成。已改为 `scripts/gen-golden-vectors.py`，
   **并验证过它逐字节复现原有向量**（签名值全部不变）。

用 Python 而不是 Go 来生成，是为了保持独立性：如果生成器和被测实现是同一份
代码，向量就成了自证。Python 的 `hmac` / `hashlib` 与 Go 的 `crypto` 是两套
独立实现，对照才有意义。

需要旧实现时可以从 git 历史取出：`git show <删除前的提交>:core/...`。

## License

Apache License 2.0 — 见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
