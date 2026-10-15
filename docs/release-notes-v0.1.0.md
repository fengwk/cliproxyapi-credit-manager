# v0.1.0 发布说明

首个正式版本。宿主契约：**CLIProxyAPI v8.0.30**（原生插件 ABI 1 / RPC schema 6，
使用 SDK v8.0.30 构建）。

## 下载

| 平台 | 归档 |
| --- | --- |
| Linux x86_64 | `cliproxyapi-credit-manager_0.1.0_linux_amd64.zip` |
| Linux arm64 | `cliproxyapi-credit-manager_0.1.0_linux_arm64.zip` |
| macOS arm64 | `cliproxyapi-credit-manager_0.1.0_darwin_arm64.zip` |

同页提供 `checksums.txt`（`<sha256>  <归档名>`）。请先校验再解压；解压后**只把动态库
复制到** `<plugins.dir>/<goos>/<goarch>/`，文件名保持
`cliproxyapi-credit-manager.so`（macOS 为 `.dylib`），实例 id 即为
`cliproxyapi-credit-manager`。安装与配置步骤见 [README](../README.md) 与
[运维手册](operations.md)。

每个归档都是扁平结构：动态库 + `LICENSE` + `README.md` + `metadata.json`，其中
`metadata.json` 记录 `id`、`name`、`version`、`goos`、`goarch`、`sdk`、`commit`
（完整 40 位）与 `abi`。

## 本版包含

- **独占式前端鉴权**：插件签发并校验客户端 Key，支持 CPA 的五个凭据位置
  （`Authorization: Bearer`、`X-Goog-Api-Key`、`X-Api-Key`、`?key=`、`?auth_token=`），
  冲突时按 CPA 原生顺序取首个有效者。CPA 原生 `access.api-keys` 不再对外分发。
- **按真实用量记账**：整数 µUSD / 每 100 万 tokens；按客户端请求的模型名**精确匹配**
  价格；缺价拒绝（不当 0 计费），显式全零为免费；五个互不重叠的桶（未缓存 input、
  cache_read、cache_write、output、不含 reasoning 的 output、reasoning 未配置时按
  output）；大整数精确求和后只向上取整一次。
- **Key 与额度管理**：一次性明文（明文不落库，库中为 HMAC，依赖 0600 的 pepper）、
  轮换（禁止与当前材料相同）、启用开关、总额/日/周/月金额额度、每模型 token 限额、
  允许模型白名单、并发上限；`0` 表示不限。
- **未决请求与管理员裁决**：未知或不可信用量**保持 pending 并保留占用**，不会按 0 结算；
  管理员可 `release` 或 `charge`（都必须给出原因），调整记录**独立成行**，不并入执行成本。
- **用量报表**：时间窗、Key、模型、provider、`auth_id`、结果、搜索、分页与
  hour/day 趋势；完整 UUID 追踪可检索。
- **请求快照（默认关闭）**：执行入口诊断快照，已知敏感头与常见 JSON 敏感键递归脱敏，
  64 位整数按文本保真；受保留期、单条上限与物理预算约束，压力下先淘汰最旧。
- **账号预热（默认关闭）**：对指定 `auth_id` + `provider` + `model` 的定时探测；探测不
  占用客户账本、不预留账号。
- **中文管理界面**：使用统计、客户端 Key、价格、上游凭据与额度、状态共 5 个标签页；
  管理密钥只保留在内存，不写入 `localStorage`/Cookie/URL。上游凭据列表与额度查询为
  **只读动态获取**（`/v8/management/credentials` 与对应的插件额度接口），不改造导入或
  OAuth 流程。
- **保留策略**：历史默认 3 个 UTC 日历月（月末钳位）；终身累计计数与未决请求不受清理
  影响；增量回收 + WAL 截断，不做整库 VACUUM。

## 验证状态

本版记录以下**已执行**的验证（不是计划项）：

| 验证 | 结果 |
| --- | --- |
| Go 单元/集成测试 `go test ./...` | 全部 7 个包通过（13 个 `*_test.go`，静态计数 156 个顶层 Test 函数） |
| 界面测试 `node --test`（`scripts/web-tests`） | 27 项通过，0 失败 |
| 交付链路 `make static` | 通过：5 项打包回归、26 项发布资产校验、3 项端到端打包→校验、脚本语法与工作流契约 |
| 端到端宿主验收（`tests/integration`） | **39/39 通过**，两次连续干净运行（记录于 HEAD `2f028ca`）；真实 CPA 8.0.30 宿主 + 真实插件库 + 环回上游 mock，含浏览器模块 |
| 独立浏览器验收（`tests/browser`） | 112 项检查 0 失败 |

发布前，集成负责人会在完整环境（CPA v8.0.30 源码、Chromium、`sha256` 校验）**重跑
39 项宿主验收套件**，并对三个平台归档逐一执行发布资产校验；本说明不把尚未重跑的用例
当作已通过的证据。验收套件的环境要求与用例清单见
[`tests/integration/README.md`](../tests/integration/README.md)。

## 已知限制（明确不承诺）

- **不承诺崩溃安全的一次性计费**：用量事件经宿主的内存队列送达，宿主队列不持久化。
  插件对已提交账务是持久化的（SQLite FULL 同步），但事件丢失不保证自动补回；未决请求
  由管理员裁决兜底。
- **单实例**：一个 `data_dir` 同一时刻只允许一个进程（目录锁）；不提供分布式/多节点形态。
- **不迁移旧数据库**：首次运行在空目录自动建库，不读取其它版本或其它工具创建的既有库。
- **插件缺席即拒绝**：`plugins.enabled: false` 或库缺失时，所有插件 Key 在五个凭据位置
  全部被拒（fail closed），不会回退到 CPA 原生 Key。
- **快照不是 DLP**：脱敏只覆盖已知模式，非 JSON 请求体原样保存，并可能因保留期或容量
  提前丢失；不可用于对账或合规举证。
- **预热会真实消耗上游**：开启后对真实凭据发起真实请求；1 token 只限制输出，输入与模型
  隐藏 reasoning 仍由上游计费；探测期间停机/热重载会被在途调用拖长，重配置超时会返回
  409 并继续使用旧配置。
- **不伪造交叉编译**：三个平台归档均由原生 runner 构建（`-buildmode=c-shared` 需要 CGO
  交叉工具链）。

## 升级与兼容

- 本版为首次发布；没有旧版本数据迁移路径，请使用全新的 `data_dir`。
- 升级方式：停止 CPA → 覆盖同名库文件 → 在同一 `data_dir` 上启动。**`data_dir` /
  `pepper` 必须保留**，否则所有客户端 Key 立即失效（详见运维手册第 4 节）。
- 许可：MIT（上游许可原文保留），第三方与改编代码见
  [`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md)。
