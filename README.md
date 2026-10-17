# cliproxyapi-credit-manager

CLIProxyAPI v8 的**原生插件**：用插件自建的客户端 Key 做前端鉴权，按实际保留的用量日志
和本地参考价格展示参考成本，并提供一个中文管理界面。

- **它做什么**：独占式客户端鉴权、Key/Token 配额/展示用美元预算管理、价格目录、
  用量报表、请求/Token/成本趋势、客户自助查询、可选的请求 Header/Body 快照（诊断用）。
- **它不做金额准入**：价格和四个美元预算只用于展示；是否放行只由 Key 权限、过期、
  Token 配额和并发决定。成本是查询时按当前本地价格重算的参考值，不是扣款账本，
  也不等同于上游最终账单。
- **它不做什么**：不接管普通模型请求的执行、路由或调度；不修改转发给上游的报文、请求头
  或账号选择；不管理上游凭据；不改动任何其它 provider 的页面或表单；不发起后台模型请求，
  也没有宿主模型执行回调。

## 环境要求

| 项目 | 要求 |
| --- | --- |
| 宿主 | CLIProxyAPI **v8.0.30**（原生插件 ABI 1 / RPC schema 6） |
| 运行形态 | 原生动态库，由宿主进程内加载；**单实例**：一个数据目录同一时刻只允许一个进程打开 |
| 构建（仅自行编译时需要） | Go 1.26 + CGO（`-buildmode=c-shared`） |
| 存储 | 本地 SQLite，仅支持当前格式；首次运行使用空数据目录，无外部数据库、无消息队列 |

## 快速开始

### 方式 A：通过 CPA 插件商店（推荐）

在 CPA 的 `config.yaml` 中配置自定义插件源：

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  store-sources:
    - "https://raw.githubusercontent.com/fengwk/cliproxyapi-credit-manager/main/registry.json"
```

保存后即可在 CPA Web 控制面板的「插件商店」中一键安装本插件。

### 方式 B：手动下载安装

**1. 下载归档**：从 [Releases](https://github.com/fengwk/cliproxyapi-credit-manager/releases)
取对应平台的 zip（`linux/amd64`、`linux/arm64`、`darwin/arm64`），并用同页的
`checksums.txt` 校验 SHA256。

**2. 解压并只复制动态库**：归档是扁平的，解压后只需把库文件复制到 CPA 的
插件目录（形如 `<plugins.dir>/<goos>/<goarch>/`）：

```sh
# linux
cp cliproxyapi-credit-manager.so  "$CPA_DIR/plugins/linux/amd64/"
# macOS
# cp cliproxyapi-credit-manager.dylib "$CPA_DIR/plugins/darwin/arm64/"
```

文件名决定宿主侧的**实例 id**（此处为 `cliproxyapi-credit-manager`）；`LICENSE`、
`README.md`、`metadata.json` 是发布记录，不需要复制。**不要重命名动态库**：插件只接受
正式发布的实例 id；其它 id 会停止新请求准入，避免执行后无法归属用量。

**3. 配置**：在 CPA 的 `config.yaml` 中加一段实例配置（完整示例见
[`config.example.yaml`](config.example.yaml)）：

```yaml
access:
  api-keys:
    - "REPLACE-WITH-STRONG-UNDISTRIBUTED-GUARD-KEY"
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    cliproxyapi-credit-manager:
      enabled: true
      data_dir: "./data/cliproxyapi-credit-manager"
```

**首次运行必须使用空的 `data_dir`。** 插件以单条 DDL 一次性创建完整当前格式
（`PRAGMA user_version=2`），不自动迁移、补列、转换或清空任何旧数据。当前格式可正常重开；
其它格式（包括 v0.1.5 的 format 1）会被拒绝且不会被修改。升级或重新开始时请停止 CPA、
冷备原 `data_dir` 作为独立档案，再指向新的空目录并重新创建客户端 Key、价格和调用方；
**不要直接删除旧库**。

**必须保留一把从未分发的 CPA 原生守卫 Key。** 不要把插件 Key 写入
`access.api-keys`；原生 Key 列表为空时，插件缺失可能导致 CPA 放行匿名请求。
客户端只能使用插件签发的 Key。首次部署要验证“插件禁用 → 客户 Key 被拒绝”。

**4. 重启 CPA**：宿主加载库并调用插件的注册流程（`plugin.register`）。

**5. 打开界面**：在 CPA 菜单里点 **Credit Manager**，或直接访问

```
http://<cpa-host>:8317/v0/resource/plugins/cliproxyapi-credit-manager/index.html
```

用 CPA 的**管理密钥**登录。价格不是分发 Key 的前提：

1. 在「客户端 Key」页创建并分发 Key；调用方是可选的归属元数据，显示名称必填，ID 可由服务端生成。
2. 如需成本展示，在「价格规则」为用量日志中的**实际执行模型 ID**配置价格，或从参考目录
   导入并确认保存。
3. 用「价格预览」输入模型 ID 核对匹配规则和费率。预览只按给定 token 数试算，
   不生成日志，也不等于实际日志费用。

**仅创建 Key 不会自动定价。** 「允许全部模型」只是权限，不表示免费或已经有价格。
新账本的价格表和规则表均为空。未匹配价格时，请求**不会因缺价被拒绝**，该次执行
在报表中显示为未定价（`cost_micro_usd: null`）。禁用规则会被跳过；其他生效规则或
价格表仍可提供价格。价格和美元预算都不参与准入；Key 权限和 Token 等限制仍然生效。

每把 Key 都保存 HMAC 校验值和 AES-GCM 密文；新建/轮换时显示明文，也可通过管理鉴权后的
「查看密钥」再次展示并记审计。数据库与 pepper 均须妥善保护。
默认在当前标签的 `sessionStorage` 保持登录，刷新无需重输；显式勾选「记住密钥」
才写入 `localStorage`。不要在共享设备上记住管理密钥。

> 两个 id 容易混淆：界面的**管理 API 前缀是固定的**
> `/v0/management/plugins/credit-manager/api/...`（插件声明的路由段）；
> 而**资源/界面地址用的是实例 id**（库文件名），即
> `/v0/resource/plugins/<library-id>/index.html`。

## 用 HTTP 接口管理 Key 与价格

所有 `api` 请求都走宿主鉴权，需要 `Authorization: Bearer <管理密钥>`；
非 GET 请求还必须带 `Content-Type: application/json`。下面用占位值演示。

**建一把客户端 Key**（返回的 `key` 是客户端凭据，不会出现在列表或审计里）：

```sh
curl -sS -X POST 'http://127.0.0.1:8317/v0/management/plugins/credit-manager/api/keys' \
  -H 'Authorization: Bearer REPLACE-WITH-MANAGEMENT-KEY' \
  -H 'Content-Type: application/json' \
  -d '{
        "label": "example-client",
        "enabled": true,
        "quota_micro_usd": 5000000,
        "daily_micro_usd": 0,
        "weekly_micro_usd": 0,
        "monthly_micro_usd": 1000000,
        "max_concurrent": 4,
        "allowed_models": ["gpt-5"],
        "model_token_limits": [{"model": "gpt-5", "total": 0, "daily": 2000000, "weekly": 0, "monthly": 0}]
      }'
# => {"item":{...},"key":"sk-REPLACE-WITH-ONE-TIME-PLAINTEXT"}
```

四个美元字段（`quota_micro_usd`、`daily_micro_usd`、`weekly_micro_usd`、
`monthly_micro_usd`）是**展示预算**，字段名保留但**不限制请求**；`0` 表示未设置预算，
不是“无限额度”。`model_token_limits`、`max_concurrent`、模型白名单和过期时间仍会限制调用。
`allowed_models` 为空表示允许全部模型；`enabled` 必须显式给出。

**设置模型价格**（单位：µUSD / 100 万 tokens；`reasoning` 省略时按 `output` 计）：

```sh
curl -sS -X PUT 'http://127.0.0.1:8317/v0/management/plugins/credit-manager/api/prices' \
  -H 'Authorization: Bearer REPLACE-WITH-MANAGEMENT-KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","rates":{"input":1250000,"output":10000000,"cache_read":125000,"cache_write":0},"version":1}'
# => {"ok":true}
```

**用这把 Key 调代理**（走 CPA 的常规接口，鉴权位置支持五种：`Authorization: Bearer`、
`X-Goog-Api-Key`、`X-Api-Key`，以及 `?key=`、`?auth_token=`）：

```sh
curl -sS -X POST 'http://127.0.0.1:8317/v1/chat/completions' \
  -H 'Authorization: Bearer sk-REPLACE-WITH-ONE-TIME-PLAINTEXT' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}'
```

**查用量**：

```sh
curl -sS 'http://127.0.0.1:8317/v0/management/plugins/credit-manager/api/usage?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&bucket=day&page=1&page_size=20' \
  -H 'Authorization: Bearer REPLACE-WITH-MANAGEMENT-KEY'
```

其它接口：`GET /api/keys`、`PUT /api/keys`、`POST /api/rotate`、`POST /api/reveal`、
`POST /api/delete-key`、`POST /api/reset-tokens`、`GET /api/prices`、
`GET /api/pricing-rules`、`GET /api/price-preview`、`GET /api/overview`、
`GET /api/capture?id=<请求id>`、`GET /api/health`。`POST /api/reset-tokens` 只重置
**Token 用量基线**，不修改日志、派生费用或美元预算。`/api/resolve` 与
`/api/reset-spend` 已删除，不再注册。

## 价格来源与模型匹配

参考目录优先读取 `https://models.dev/api.json`，失败时尝试
`https://models.opencode.ai/api.json`，缓存 6 小时。刷新只更新参考目录，
**不会自动写入或更新价格配置**。导入会打开一条 `exact` 规则草稿
（默认优先级 10），管理员核对模型 ID、费率和档位并确认保存后才生效。
目录只在管理员显式操作时联网；推理/准入路径不联网，也不自动关联别名。

**成本在查询时计算**：使用 CPA UsageRecord 中的实际执行模型
（`usage.model` 的完整精确 ID）、规范化五桶 Token 和 `service_tier` 匹配当前价格。
请求别名、`RequestedModel`、`ResponseModel` 都不用于猜价。请求别名与
`usage.model` 不同时，按后者计价；`ResponseModel` 不会冒充计价模型。
请求模型仍用于权限、Token 限制和请求列表/模型分组；分组成本是组内各执行
按自身实际模型计算的成本之和。

规则候选按以下顺序排序，第一条获胜：

1. 优先级从高到低。
2. 同优先级下，`exact`（精确）优先于 `glob`（通配），再优先于 `regexp`（正则）。
3. 同优先级、同匹配类型下，显式规则优先于价格表；之后按规则 ID 升序。

价格表中的精确模型价作为优先级 0 的 exact 候选参与比较。
`disabled` 规则**只是不参与价格匹配**，会像不存在一样跳过；它不是模型权限黑名单，
不会拒绝请求。没有候选或无法根据实际用量计算价格时，该次执行显示为未定价
（`cost_micro_usd: null`），请求照常执行。

金额为整数 µUSD / 每百万 Token。基础费率显式 0 表示免费；
档位的输入、输出、缓存费率 0 表示继承基础费率。思考费率留空继承输出单价，显式 0 为免费。
修改、添加或删除价格/规则后，下一次查询会立即按当前价格重算所有仍保留的日志；
不存在请求级冻结价格快照。「价格预览」是按输入 token 数做的试算，不等于日志中
按实际 token 计算出的费用。

## 界面、趋势与客户日志

- 管理界面使用 CM 标识，与 CPA `white` / `light` / `dark` / `auto` 主题联动；
  图表使用本地打包的 ECharts，不依赖 CDN。
- 请求/执行次数、五类互斥 Token、**已定价执行小计**与**未定价执行数**分别展示；
  支持 UTC 时、日、**自然月**聚合。统计与图表来自同一筛选范围，不拿当前页冒充全量趋势。
- 支持 Token 单位与 USD/CNY 展示换算；汇率由用户手动设定，不声称实时汇率，
  价格输入仍是整数微美元或美元，不随展示币种改变。
- 客户入口：`/v0/resource/plugins/cliproxyapi-credit-manager/lookup.html`。
  客户输入自己的插件 Key，无需 CPA 管理密钥，只能查询自己的预算、Token 用量、
  趋势、未定价执行数及快照。
  该 Key 只通过 Bearer 请求头发送；停用、过期、轮换旧 Key 都不能继续查询。
- 快照默认关闭；启用 `request_capture.enabled` 后，管理员在请求详情中查看，
  客户在自助页查看自己请求的 Header/Body。到期或容量淘汰后可能不可用。

完整功能、成本边界及验收入口见 [`docs/features.md`](docs/features.md)。

## 成本口径（核心规则）

- 单价是**整数 µUSD / 每 100 万 tokens**，不使用浮点美元。
- 成本**只来自仍在本地保留的日志**：查询时按每条执行的实际 `usage.model`、规范化
  Token 和 `service_tier` 匹配**当前**价格并精确计算。改价、加价或删价会让已保留日志
  立即重算，没有金额或价格快照落盘。成本只反映所选时间段/保留窗口，不是永久全历史总支出。
- **缺价、不完整/不可信用量、整数溢出、或 `per_image` 价格但缺少真实出图数量**时，
  该执行显示 `cost_micro_usd: null` 并计入 `unpriced_executions`；summary 的
  `cost_micro_usd` 只是已定价执行的小计。显式全零费率在完整用量下是**已定价的 0（免费）**，
  与“未定价”明确区分。
- 成本计算的 Token 桶为五类且互不重叠：`input`（**未缓存的**输入）、`cache_read`、`cache_write`、
  `output`（**不含** reasoning）、`reasoning`（未配置费率时按 `output`）。
- 各桶分别按单价相乘后用大整数精确求和，**只在最后向上取整一次**。
- **价格和四个美元预算只展示，不限制请求**。KeySpec 的
  `quota_micro_usd`/`daily_micro_usd`/`weekly_micro_usd`/`monthly_micro_usd`
  字段名保留，但表示预算；`0` 是未设置。Key 没有 `spent_micro_usd`、
  `quota_used_micro_usd` 等永久金额字段，也没有人工金额调整。
- 准入只做三件事：鉴权、检查 **Token 配额**（按实际已用 Token）、Key 权限/过期与显式配置的
  并发；**不读价格、不计算金额、不做任何预估、预留或占用**。
- **Token 配额是软准入限制**：配置的限制为正且**实际已用 Token** ≥ 限制时，才拒绝
  **下一次**请求。只要剩余配额为正，声明多大的 `max_tokens`、带多大的请求体都会被放行。
- 并发/流式/在途请求**可能超出 Token 配额**：真实 Token 用量一律**足额记录**，不封顶、
  不猜测、不跳过、不重复，也不会为了配额去取消、重试或改路由。
- 未知或不可信的用量不会被伪造成 0：请求保持 `pending`（“用量待完整”），但**不冻结金额、
  不阻塞后续准入**。完整但未定价的用量可以正常 `settled`；价格不能改变生命周期状态。
- 成本计算在查询时会对所选执行流做一次额外的精简扫描（只取分组、模型、Token、tier 等字段），
  内存随分组和当前页大小增长，不把结果当成长驻预聚合。实际数字是基于本地参考价格的
  用量成本，不等同上游最终账单。
- CPA 的用量投递队列仅在内存中。本插件保证已接收账本事务的原子性与保留期内幂等，
  **不承诺宿主崩溃时事件不丢失，也不承诺端到端 exactly-once**。

## 安全边界（务必阅读）

- **守卫部署是 fail-closed 的前提**：保留至少一把从未分发的 CPA 原生 Key，且客户
  Key 仅存在于本插件时，插件没加载（`plugins.enabled: false` 或库缺失）仍会拒绝客户
  Key。这不是插件在缺席时仍能执行鉴权；是 CPA 原生守卫拒绝未知 Key。原生 Key 列表
  为空可能放行匿名请求；分发原生 Key 则允许绕过插件鉴权与用量归属，二者都不安全。
- **客户端 Key 只存在于插件内**：数据库里保存的是 HMAC 校验值，明文不落库；校验依赖
  数据目录中的 `pepper`（权限 0600，首次启动自动生成）。
- **凭据存储由用户选择**：默认 `sessionStorage`，显式记住才写入 `localStorage`；
  登出或鉴权失效会清理。管理密钥和客户查询 Key 使用不同存储前缀，不读取或猜测 CPA
  登录凭据；不写 Cookie/URL。Web Storage 可被同源脚本访问，记住密钥并不能防御 XSS。
- **快照不是 DLP**：请求快照只做已知敏感字段脱敏（`Authorization`、`X-Api-Key`、
  `X-Goog-Api-Key`、`Cookie`、`Set-Cookie` 及常见 JSON 敏感键），不是匿名化承诺，也可能
  因保留期或容量限制提前被丢弃；**不要把它当作费用凭证**。
- 管理 API 只挂在受宿主管理鉴权的 management 路由下；资源路由除静态文件外，
  只开放自行校验客户 Key 的 `lookup/data` 与 `lookup/capture`，不开放管理写操作。

更完整的运维与备份要求见 [`docs/operations.md`](docs/operations.md)。

## 构建与测试

```sh
make help     # 列出全部目标与当前版本
make build    # 构建当前平台的原生库到 dist/<os>/<arch>/
make test     # go vet + go test + Node 界面测试
make race     # go test -race
make static   # 无需 C 工具链的交付检查（脚本语法、打包、校验器、工作流契约）
make browser  # 隔离 Chrome：管理页与客户自助页自动化验收
make package  # 打包当前平台的 zip（含 metadata.json 与 sha256）
```

`make build` 的版本号来自 `PLUGIN_VERSION` 环境变量、HEAD 上的精确 git tag，或
`0.1.0-dev+<短SHA>`；SDK 版本从 `go.mod` 读取。三个平台都通过**原生 runner** 构建：
`-buildmode=c-shared` 需要 CGO 交叉工具链，**本项目不提供也不声称支持伪造的交叉编译**，
本地只会构建当前平台。

端到端验收（真实 CPA 宿主 + 真实插件库 + 环回上游 mock，含浏览器检查）
见 [`tests/integration/README.md`](tests/integration/README.md)。

## 发布产物

每次发布提供 3 个平台归档与一份 `checksums.txt`（下载地址见
[Releases](https://github.com/fengwk/cliproxyapi-credit-manager/releases)）：

```
cliproxyapi-credit-manager_<version>_linux_amd64.zip
cliproxyapi-credit-manager_<version>_linux_arm64.zip
cliproxyapi-credit-manager_<version>_darwin_arm64.zip
checksums.txt
```

每个 zip 是**扁平**结构，只含库文件与发布记录：

```
cliproxyapi-credit-manager.so|.dylib   # 唯一动态库，位于归档根
LICENSE                                  # 上游 MIT 许可
README.md
metadata.json                            # id/name/version/goos/goarch/sdk/commit/abi
```

## 文档

| 文档 | 内容 |
| --- | --- |
| [`docs/operations.md`](docs/operations.md) | 全新安装、当前数据格式、配置、备份恢复、保留与清理、快照、故障处理 |
| [`docs/features.md`](docs/features.md) | 当前管理功能、成本语义与职责边界 |
| [`docs/release-notes-v0.1.6.md`](docs/release-notes-v0.1.6.md) | v0.1.6 查询时定价、预算只展示与不兼容升级说明 |
| [`docs/release-notes-v0.1.5.md`](docs/release-notes-v0.1.5.md) | v0.1.5 移除迁移与旧字段回退；历史发布事实 |
| [`docs/release-notes-v0.1.4.md`](docs/release-notes-v0.1.4.md) | v0.1.4 移除预占、软额度准入及管理/定价/分析功能恢复；含升级注意事项 |
| [`docs/release-notes-v0.1.3.md`](docs/release-notes-v0.1.3.md) | v0.1.3 修复 CPA 插件商店注入 store 配置导致注册失败问题 |
| [`docs/release-notes-v0.1.2.md`](docs/release-notes-v0.1.2.md) | v0.1.2 计费、快照隐私、边界校验与管理界面修复 |
| [`docs/release-notes-v0.1.1.md`](docs/release-notes-v0.1.1.md) | v0.1.1 请求详情布局修复 |
| [`docs/release-notes-v0.1.0.md`](docs/release-notes-v0.1.0.md) | 首次发布功能与保证边界 |
| [`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md) | 当前实现契约（评审/维护用） |
| [`config.example.yaml`](config.example.yaml) | 可直接套用的 CPA v8 配置示例 |
| [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | 第三方许可与改编说明 |
| [`tests/integration/README.md`](tests/integration/README.md) | 验收工具与用例清单 |

各版本的 release notes 记录对应版本的行为，不代表当前实现兼容旧版本的数据或接口。
可安装版本及对应产物以 [Releases](https://github.com/fengwk/cliproxyapi-credit-manager/releases)
为准。

## 许可

MIT，见 [`LICENSE`](LICENSE)（上游 MIT 原文保留）。第三方与改编代码见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
