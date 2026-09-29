# 渠道规格

给跨语言重写用的语言无关规格。每个渠道记录：调用序列、端点、鉴权方式、成功判据、**已知陷阱**。

陷阱部分是本文档的重点。API 形状可以从代码里读出来，但「为什么必须这样」的结论是踩出来的，
只存在于代码注释里 —— 重写时只搬 API 形状就会把它们丢掉，然后重新踩一遍。

签名的可复现输入输出见 `go/testdata/golden/signing.json`（由 `GoldenVectorTest` 生成）。

## 跨渠道的公共约定

**日期格式只有两种**，不要搞混：

| 格式 | 用于 |
|---|---|
| `yyyy-MM-dd'T'HH:mm:ssZZ`（如 `2026-10-01T10:00:00+0800`） | 华为、荣耀、鸿蒙的定时发布 |
| `yyyy-MM-dd HH:mm:ss`（如 `2026-10-01 10:00:00`） | OPPO、vivo 的定时上架 |

格式化**必须显式指定 Locale**。默认 Locale 在泰语等区域会按佛历输出年份（2569 而非 2026），
签名本身仍然自洽，但服务端解析出的日期是错的 —— 表现为定时发布必然失败，且极难定位。

**所有响应字段一律可空带默认值。** 渠道在不同状态下返回的字段集合并不一致，
缺字段应当走到带说明的业务校验，而不是让反序列化抛异常。这不是防御性编程，
是被上游 issue #7 教育过的：华为在草稿态不返回 `versionCode`，声明为非空就直接崩。

**服务端下发的上传地址必须校验 https。** 华为、荣耀、鸿蒙、OPPO 的上传地址都来自接口响应。
明文地址会导致整个安装包连同 `Authorization` 头一起裸奔。

**响应一律关闭。** 异常路径也要关。

**取消优先于一切错误处理。** 协程里先重抛 `CancellationException`；Go 里检查 `ctx.Err()`。
否则用户取消会被上报成「上传失败」并提示重试。

**凭据不进日志。** token、secret、证书、SIG 一律不打印。需要时用 `redact()`（保留前 4 后 2 + 长度）。
特别注意：不要用「统一打印返回值」这类日志辅助函数 —— 上游就是这么把裸 token 写进日志的。

**送审点之后的失败不可重试。** 见 `FailurePhase`。判断依据是「结果是否确定」，
不是「错误看起来是否可恢复」：渠道明确拒绝（有业务错误码）→ 结果确定 → 可安全重试；
网络超时 → 不知道服务端有没有受理 → 绝不自动重试。

---

## 华为 AppGallery（`huawei`）

**制品** `.apk` ｜ **鉴权** `client_id` header + `Authorization: Bearer <token>` ｜ **请求签名** 无
**baseUrl** `https://connect-api.cloud.huawei.com/`

调用序列：

```
POST api/oauth2/v1/token                    grant_type=client_credentials，body 形式
GET  api/publish/v2/appid-list?packageName= 包名 → appId
GET  api/publish/v2/app-info?appId=          查状态（仅 status 用）
GET  api/publish/v2/upload-url/for-obs       → 签名上传地址 + headers
PUT  <上传地址>                              body 是文件，headers 原样透传华为给的
PUT  api/publish/v2/app-file-info?appId=     绑定，fileType=5
GET  api/publish/v2/package/compile/status   轮询编译，3 分钟上限 / 10 秒间隔
PUT  api/publish/v2/app-language-info?appId= 更新说明，lang=zh-CN
POST api/publish/v2/app-submit?appId=        送审，releaseTime 走 query
```

成功判据：`ret.code == 0`。**取 token 那一步华为不返回 `ret`**，只判 `access_token` 是否存在。

陷阱：

- **草稿态没有 versionCode**（上游 issue #7，原作者确认是 bug 且挂了一年多）。
  应用有一个尚未上传 APK 的新版本草稿时 `releaseState=7`，华为不返回 `versionCode`。
  版本信息缺失时 `lastVersion` 传 null，不要伪造 0，也不要抛异常。
- `releaseState` 映射：0 已上架 / 4,5 审核中 / 7 草稿 / 8 审核不通过 / 2,6,10 已下架 / 其他 未知。
  上游只认 0/4/5/8，草稿一律显示「状态未知」。
- 轮询要**先查再等**。上游把 `delay(10s)` 放在检查之前，首次检查必然白等 10 秒。
- `pkgStateList` 可能为空数组，`.first()` 会抛 `NoSuchElementException`。
- 上传地址的 headers 必须原样透传，不能自己构造。

## 荣耀（`honor`）

**制品** `.apk` ｜ **鉴权** `Authorization: Bearer <token>` ｜ **请求签名** 无
**baseUrl** `https://appmarket-openapi-drcn.cloud.honor.com/`

调用序列：

```
POST https://iam.developer.honor.com/auth/token   绝对地址覆盖 baseUrl，POST form
GET  openapi/v1/publish/get-app-id?pkgName=        → appId
GET  openapi/v1/publish/get-app-detail             取语言信息（发布流程要用，不只是查状态）
POST openapi/v1/publish/get-file-upload-url        → 上传地址 + objectId，fileType=100
POST <上传地址>                                    multipart，字段名 "file"
POST openapi/v1/publish/update-file-info           绑定，需 fileSha256
POST openapi/v1/publish/update-language-info       更新说明（依赖上一步取到的语言信息）
POST openapi/v1/publish/submit-audit               送审
```

`get-app-current-release` 仅用于查审核状态，不在发布流程里。
注意 `get-app-detail` 取到的语言信息是后面 `update-language-info` 的输入，
所以这一步必须在上传之前完成，不能省。

成功判据：`code == 0`。

陷阱：

- token 端点是**绝对 URL**，与业务接口的 baseUrl 不同域。
- `auditResult` 映射：0 审核中 / 1 审核通过 / 2 审核不通过 / 3 其他非审核状态 / 4 编辑中未提审。
  上游只映射 0/1/2，把 4 归为「未知」—— 实际是草稿态。
- `languageInfo` 可能为空列表，`.first()` 会抛异常。
- `releaseType`：1 审核通过后立即发布，2 定时发布（此时 `releaseTime` 必填）。
- 上传用 multipart 表单（字段名 `file`），与华为的裸 PUT 不同。

## 鸿蒙 AppGallery（`harmony`）

**制品** `.app`（App Pack） ｜ **鉴权** 与华为完全同源 ｜ **请求签名** 无
**baseUrl** `https://connect-api.cloud.huawei.com/`

与华为共用 token 端点、请求体、响应字段，因此**复用华为的 token 模型**。
但发布流程完全不同：`.app` 走 Upload Management API 分片上传，关联草稿用 v3 接口。

调用序列：

```
POST api/oauth2/v1/token                          同华为
POST api/publish/v2/upload/multipart/init         query: appId fileName contentType
                                                       fileType=1 releaseType=1
                                                  → objectId, nspUploadId, nspPartMinSize
     （本地：按 nspPartMinSize 切片，逐片算 sha256）
POST api/publish/v2/upload/multipart/parts        query: objectId nspUploadId
                                                  body: {additionalProp1:{sha256,length},...}
                                                  → uploadInfoMap
PUT  <每片的签名地址>                              headers 原样转发，收 ETag
POST api/publish/v2/upload/multipart/compose      query: objectId nspUploadId
                                                  body: {additionalProp1:{partObjectId,etag},...}
PUT  api/publish/v3/app-package-info?appId=       body: {fileName, objectId} → packageId
POST api/publish/v3/app-submit?appId=             body: {remark?, releaseTime?,
                                                       registeredIdType?, registeredIdNumber?}
```

成功判据：`ret.code == 0`。

陷阱（这个渠道最多，因为 API 最新、文档最少）：

- **v3 与 v2 不可混用。** 鸿蒙走 v3（`app-package-info`、`app-submit`），Android 走 v2。
  v3 系列统一是「appId 走 query + 负载走 JSON body」，而 v2 的 `app-submit` 把 `releaseTime`
  放 query。判断依据：社区实测报错 `registeredIdType and registeredIdNumber can not be null`
  说明服务端在解析 body。
- **分片大小必须用华为返回的 `nspPartMinSize`**，自己选一个会导致 parts 返回的地址与实际分片不匹配。
- **分片键名是 `additionalProp1..N`** —— Swagger 占位符命名，看着像没改完的代码，但必须逐字匹配。
- 分片上传地址是短时效签名地址，**headers 必须原样转发**，增删都可能导致签名校验失败。
- **ETag 含引号也要原样回传**给 compose，不要加工。
- 合并成功只代表文件进了华为文件服务，**必须再调 v3 `app-package-info` 才会出现在后台草稿里**。
- `appId` 必须显式配置。HarmonyOS NEXT 应用在 AGC 里是**独立的应用记录**，
  用包名反查会拿到同名 Android 应用的 id，关联到错误的草稿。
- `204144660` = 软件包尚未编译完成就提交。这是**明确拒绝**，结果确定，可安全重试
  （实现里限 6 次 × 20 秒）。与网络超时的性质相反。
- `registeredIdType` / `registeredIdNumber`（主体登记信息）做成可选：社区实测某些应用缺失会被拒，
  但并非所有应用都需要。
- `releaseTime` 格式有两种说法（字符串 vs 毫秒时间戳）。采用字符串，因为官方 v2 文档与
  一个可用的 v3 封装实现都这么写。**若定时发布报「时间格式有误」，首先怀疑这里。**
- 商店里的「新版本介绍」需人工维护：v3 语言信息接口未经验证，暂未实现。
  `updateDesc` 只在长度符合 `remark` 要求的 10-300 字时作为提审备注提交，否则跳过并记日志。
- `.app` 的元信息在 zip 根目录的 `pack.info`（JSON），结构 `summary.app.{bundleName, version.{code,name}}`。
  HSP 的 `pack.info` 不含 `summary/app`，缺字段要给明确错误而不是崩。

## 小米（`mi`）

**制品** `.apk` ｜ **鉴权** RSA 加密的 SIG ｜ **请求签名** RSA（不是 HMAC）
**baseUrl** `https://api.developer.xiaomi.com/devupload`（**必须 https**，早期版本是 http）

参数：`account`（邮箱）、`publicKey`（开放平台下载的 `.cer` 证书）、`privateKey`（API 密码）。

调用序列：

```
POST dev/query   form: RequestData + SIG          查应用信息（拿 appName/packageName）
POST dev/push    multipart: apk + RequestData + SIG   上传并送审（原子，一步完成）
```

成功判据：`result == 0`。

陷阱：

- **`dev/push` 是原子的** —— 上传与送审合并成一次请求，中间没有任何可停下的位置。
  因此该渠道的 `supportedStages` 只有 `SubmitReview`，请求停在草稿态必须立刻报错，
  不能默默走到送审。
- **apk part 的文件名是空串** `addFormDataPart("apk", "", body)`。看着像 bug，
  但小米服务端按 part 名 `apk` 取文件，填入真实文件名未经验证，保持原样。
- **RequestData 必须只序列化一次** —— 表单里发的和参与 MD5 的必须是同一份字符串，
  否则服务端算出的 hash 与我们提交的不一致，表现为笼统的鉴权失败。
- SIG 的明文结构：`{"password":"<API密码>","sig":[{"name":"RequestData","hash":"<RequestData的MD5>"},{"name":"apk","hash":"<APK的MD5>"}]}`。
- RSA 参数**一个都不能动**：`RSA/NONE/PKCS1Padding` + provider `BC` + 1024 位密钥 +
  明文分组 117 字节 / 密文分组 128 字节（11 字节是 PKCS#1 v1.5 的固定开销）。
  1024 位与 v1.5 填充都低于当今推荐强度，但密钥由小米下发、padding 由服务端决定，
  **客户端无从选择**。改了就直接鉴权失败，而且我们没有真实凭据可以验证。
- MD5 在这里是小米规定的**数据指纹字段**，不是安全签名 —— 它被包在 RSA 加密的 SIG 里传输，
  外层还有 TLS。所以用 MD5 是可接受的，不要「升级」成 SHA-256。
- 证书解析失败要归类为凭据问题并给中文提示，且**异常信息里绝不能带证书内容**。

## OPPO（`oppo`）

**制品** `.apk` ｜ **鉴权** access_token + api_sign（HMAC-SHA256） ｜ **请求签名** 有
**baseUrl** `https://oop-openapi-cn.heytapmobi.com`

调用序列：

```
GET  developer/v1/token?client_id=&client_secret=   取 access_token
GET  resource/v1/app/info                           读回商店现有资料
GET  resource/v1/upload/get-upload-url              → 上传地址
PUT/POST <上传地址>                                 上传 APK → url + md5
POST resource/v1/app/upd                            送审（异步任务，只代表入队）
POST resource/v1/app/task-state                     轮询任务结果 ← 缺这步会把失败当成功
```

成功判据：`app/upd` 的 `errno == 0` **只代表任务入队**；版本是否真的创建成功，
必须轮询 `task-state` 得到 `task_state == "2"`。见下方陷阱。

签名规则：参数按 key 字典序排序 → `k=v` 用 `&` 拼接 → HMAC-SHA256(secret) → 小写 hex。
value 为 null 的项**整体跳过**（连键名也不参与）。参与签名的集合始终是
「业务参数 + access_token + timestamp」，无论业务参数放 query 还是 body。

陷阱：

- **`client_secret` 在 URL query 上。** 这是 OPPO 接口的设计 —— 独立实现
  [flu-cli/app-ship](https://gitee.com/flu-cli/app-ship) 也是 `GET` + query，
  未能查证是否支持 POST form。残余风险：凭据会进代理日志与网关审计。
  必须用 `addQueryParameter` 而非字符串插值：secret 含 `&` 会被拦腰截断
  （服务端收到半个 secret，鉴权失败且原因完全不可见），含空格直接抛异常。
- **`app/upd` 是全量更新语义。** 必须把从 `app/info` 读回的字段原样回传
  （19 个固定字段 + 定时发布时的 `sche_online_time`）：
  图标、截图、一句话介绍、软件介绍、隐私政策地址、二三级分类 id、软著地址、商务联系方式等。
  **漏任何一个字段就会被清空或被拒。** 这不是冗余，不要「优化」掉。
- **必须先判 errno 再解强类型 data。** 失败响应里 `data` 的形状与成功时不同，
  若先按成功结构解析，抛出的是解析异常，真正的错误码和 `data.message` 全被吞掉。
  这是上游 issue #18/#19 那类报错难以诊断的直接原因。
- `copyright_url` 有回退：为空时用 `electronic_cert_url`（OPPO 要求非空，而多数开发者只上传电子版）。
- **`app/upd` 是异步接口，`errno == 0` 不等于提交成功。** 这是本项目踩过的最贵的坑：
  接口返回成功只表示任务已入队，任务随后可能因为缺必传参数、apk 包名不符、
  截图尺寸超标等原因**静默失败**，线上版本号纹丝不动。
  必须轮询 `POST resource/v1/app/task-state`（参数 `pkg_name` + `version_code`）：
  `task_state` 为 `1` 待处理 / `2` 处理成功 / `3` 处理失败（失败时 `err_msg` 给原因）。
  文档称处理可能较耗时，建议等待 10 秒以上；本项目取 3 分钟上限。
  **超时不要报成失败** —— 任务可能仍在处理，报失败会诱使重试而产生重复版本。
- **`app_name` / `age_level` / `adaptive_equipment` 是必传字段**（文档 id=10999）。
  它们不在早期实现的参数表里，漏传会让异步任务失败 —— 而 `errno` 仍是 0。
  `app/info` 里能读回这些值（`app_name` / `age_level` / `adaptive_equipment`）。
- `summary` 限 13 字符以内且不能含标点与空格；`detail_desc` 不少于 20 字。
  超限同样走异步失败路径，不会在 `app/upd` 的响应里报出来。
- 区分两类缺失：**结构性缺失**（access_token、upload_url、sign）→ 接口变更；
  **商店资料缺失**（icon_url、summary、分类 id）→ 给可执行的中文提示
  「请先在 OPPO 开放平台补全应用信息」。
- `audit_status` 缺失映射为「未知」而非「审核中」—— 接口没返回状态和商店确实在审核是两件事。

## vivo（`vivo`）

**制品** `.apk` ｜ **鉴权** access_key + sign（HMAC-SHA256） ｜ **请求签名** 有
**baseUrl** `https://developer-api.vivo.com.cn/router/rest`（沙箱 `https://sandbox-developer-api.vivo.com.cn/router/rest`）

`router/rest` 风格：所有接口共用一个路径，靠 `method` 参数区分。

调用序列：

```
GET ?method=app.sync.getappinfo    查应用详情（先查再传，提前暴露包名不属于本账号）
POST <上传地址>                    multipart，字段名 "file" → serialnumber
GET ?method=app.sync.update.app    送审
```

成功判据：`code == 0` **且** `subCode == 0`。

签名规则：在业务参数上补齐 7 个公共参数（`access_key`、`timestamp`、`method`、`v=1.0`、
`sign_method=HMAC-SHA256`、`format=json`、`target_app_key=developer`），
按 key 字典序拼 `k=v&k=v`，HMAC-SHA256(accessSecret)，小写 hex。
**`sign` 必须在拼串之后才放进 map**，否则它会参与自己的计算。

陷阱：

- **`timestamp` 是毫秒**，不是秒。
- **`code` / `subCode` 都可能缺失，且 JSON 类型不稳定**（限流走数字、部分鉴权失败走字符串）。
  上游 `get("subCode").asString` 在缺失时直接 NPE，**把本该抛出的业务异常顶掉了**，
  用户看不到真实错误码 —— 这是 vivo 报错难诊断的直接原因。
  用 `Any?` 接住再归一化；声明成 `Int` 会因类型不匹配再次把真实错误顶掉。
- **两个码都缺失时必须按失败处理，不能当成功。** `submit` 之后没有 data 校验兜底，
  判成成功会谎报发版成功 —— 比抛 NPE 更危险。
- 签名参数全部走 query（`router/rest` 网关的强制要求，连 multipart 上传也是 body 只放文件）。
- `onlineType`：1 审核通过后立即上架，2 定时上架（此时 `scheOnlineTime` 必填）。

---

## 能力矩阵汇总

| 渠道 | 可停阶段 | 风险 | 撤回 | 证据 |
|---|---|---|---|---|
| 华为 | 上传 / 草稿 / 送审 | 高 | API 支持¹ | 已实测（不含送审） |
| 荣耀 | 上传 / 草稿 / 送审 | 高 | 未验证 | 已实测（不含送审） |
| 鸿蒙 | 上传 / 草稿 / 送审 | 高 | API 支持¹ | 已实测（不含送审） |
| OPPO | 上传 / 送审 | 极高 | 未验证 | 已实测（不含送审） |
| vivo | 上传 / 送审 | 极高 | 未验证 | 已实测（不含送审） |
| 小米 | 送审 | 极高 | 未验证 | 代码推断 |

¹ 华为 AGC 文档里有「撤销审核」接口（`agc-help-publish-api-on-shelf-cancel`），
所以平台层面支持撤回 —— 上游 issue #16「各商店 API 都不提供撤销」的结论不准确。
但本工具**尚未实现**该调用，且其适用条件（是否仅限审核中状态）未核实。

「已实测」的范围是**鉴权与上传/建草稿**（提交 `cf52205`）。
**送审路径六个渠道一个都没验证过**，而送审恰恰是不可撤销的那一步。小米则完全未验证。
