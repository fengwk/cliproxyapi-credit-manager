# cliproxyapi-credit-manager

CLIProxyAPI v8 的**原生插件**：用插件自建的客户端 Key 做前端鉴权，按真实用量记账扣费，
并提供一个中文财务管理界面。

- **它做什么**：独占式客户端鉴权、按模型单价记账、Key/额度/价格管理、用量报表、
  可选的一次性请求快照（诊断用）。
- **它不做什么**：不接管普通模型请求的执行、路由或调度；不修改转发给上游的报文、请求头
  或账号选择；不管理上游凭据；不改动任何其它 provider 的页面或表单。
  可选后台预热默认关闭，启用后会通过宿主执行真实探测请求。

## 环境要求

| 项目 | 要求 |
| --- | --- |
| 宿主 | CLIProxyAPI **v8.0.30**（原生插件 ABI 1 / RPC schema 6） |
| 运行形态 | 原生动态库，由宿主进程内加载；**单实例**：一个数据目录同一时刻只允许一个进程打开 |
| 构建（仅自行编译时需要） | Go 1.26 + CGO（`-buildmode=c-shared`） |
| 存储 | 本地 SQLite，无外部数据库、无消息队列 |

## 快速开始

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
`README.md`、`metadata.json` 是发布记录，不需要复制。

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

**必须保留一把从未分发的 CPA 原生守卫 Key。** 不要把插件 Key 写入
`access.api-keys`；原生 Key 列表为空时，插件缺失可能导致 CPA 放行匿名请求。
客户端只能使用插件签发的 Key。首次部署要验证“插件禁用 → 客户 Key 被拒绝”。

**4. 重启 CPA**：宿主加载库并调用插件的注册流程（`plugin.register`）。

**5. 打开界面**：在 CPA 菜单里点 **Credit Manager**，或直接访问

```
http://<cpa-host>:8317/v0/resource/plugins/cliproxyapi-credit-manager/index.html
```

用 CPA 的**管理密钥**登录（只在内存中保存），然后在「客户端 Key」页建第一把 Key。
明文只显示一次，请立即保存。

> 两个 id 容易混淆：界面的**财务 API 前缀是固定的**
> `/v0/management/plugins/credit-manager/api/...`（插件声明的路由段）；
> 而**资源/界面地址用的是实例 id**（库文件名），即
> `/v0/resource/plugins/<library-id>/index.html`。

## 用 HTTP 接口管理 Key 与价格

所有 `api` 请求都走宿主鉴权，需要 `Authorization: Bearer <管理密钥>`；
非 GET 请求还必须带 `Content-Type: application/json`。下面用占位值演示。

**建一把客户端 Key**（返回的 `key` 是唯一一次明文）：

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

金额单位是 **µUSD（百万分之一美元）**；`0` 表示该项**不限**；`allowed_models` 为空表示
允许全部模型；`enabled` 必须显式给出。

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

其它接口：`GET /api/keys`、`PUT /api/keys`、`POST /api/rotate`、`GET /api/prices`、
`GET /api/capture?id=<请求id>`、`POST /api/resolve`、`GET /api/health`。

## 计费口径（核心规则）

- 单价是**整数 µUSD / 每 100 万 tokens**，不使用浮点美元。
- 按**客户端请求的模型名精确匹配**价格；没有配置价格 → **拒绝该请求**（不会静默按 0 计费）；
  显式配置的全部费率为 0 → 该模型**免费**。
- 计费桶为五类且互不重叠：`input`（**未缓存的**输入）、`cache_read`、`cache_write`、
  `output`（**不含** reasoning）、`reasoning`（未配置费率时按 `output`）。
- 各桶分别按单价相乘后用大整数精确求和，**只在最后向上取整一次**。
- 未知或不可信的用量**不会按 0 结算**：请求保持 `pending` 并保留金额占用，等管理员裁决。
- 预授权使用请求字节数和输出上限做估算，**不是最终账单的硬上限**。真实用量超过估算
  仍会扣费并阻止后续超额准入；终身/当前周期计数独立保存，清理历史不会恢复额度。
- CPA 的用量投递队列仅在内存中。本插件保证已接收账务事务的原子性与保留期内幂等，
  **不承诺宿主崩溃时事件不丢失，也不承诺端到端 exactly-once 计费**。

## 安全边界（务必阅读）

- **守卫部署是 fail-closed 的前提**：保留至少一把从未分发的 CPA 原生 Key，且客户
  Key 仅存在于本插件时，插件没加载（`plugins.enabled: false` 或库缺失）仍会拒绝客户
  Key。这不是插件在缺席时仍能执行鉴权；是 CPA 原生守卫拒绝未知 Key。原生 Key 列表
  为空可能放行匿名请求；分发原生 Key 则允许绕过插件计费，二者都不安全。
- **客户端 Key 只存在于插件内**：数据库里保存的是 HMAC 校验值，明文不落库；校验依赖
  数据目录中的 `pepper`（权限 0600，首次启动自动生成）。
- **管理密钥只存在于内存**：界面不写 `localStorage`/`sessionStorage`/Cookie，也不放进
  URL。刷新页面需重新输入。
- **快照不是 DLP**：请求快照只做已知敏感字段脱敏（`Authorization`、`X-Api-Key`、
  `X-Goog-Api-Key`、`Cookie`、`Set-Cookie` 及常见 JSON 敏感键），不是匿名化承诺，也可能
  因保留期或容量限制提前被丢弃；**不要把它当作财务凭证**。
- 财务 API 只挂在受管理的 management 路由下；公开资源路由只返回内嵌的静态外壳。

更完整的运维与备份要求见 [`docs/operations.md`](docs/operations.md)。

## 构建与测试

```sh
make help     # 列出全部目标与当前版本
make build    # 构建当前平台的原生库到 dist/<os>/<arch>/
make test     # go vet + go test + Node 界面测试
make race     # go test -race
make static   # 无需 C 工具链的交付检查（脚本语法、打包、校验器、工作流契约）
make package  # 打包当前平台的 zip（含 metadata.json 与 sha256）
```

`make build` 的版本号来自 `PLUGIN_VERSION` 环境变量、HEAD 上的精确 git tag，或
`0.1.0-dev+<短SHA>`；SDK 版本从 `go.mod` 读取。三个平台都通过**原生 runner** 构建：
`-buildmode=c-shared` 需要 CGO 交叉工具链，**本项目不提供也不声称支持伪造的交叉编译**，
本地只会构建当前平台。

端到端验收（真实 CPA 宿主 + 真实插件库 + 环回上游 mock，共 39 个用例，含浏览器检查）
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
| [`docs/operations.md`](docs/operations.md) | 安装/升级、配置、备份恢复、保留与清理、快照、预热、故障处理 |
| [`docs/release-notes-v0.1.0.md`](docs/release-notes-v0.1.0.md) | v0.1.0 发布说明 |
| [`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md) | 实现契约（评审/维护用） |
| [`config.example.yaml`](config.example.yaml) | 可直接套用的 CPA v8 配置示例 |
| [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | 第三方许可与改编说明 |
| [`tests/integration/README.md`](tests/integration/README.md) | 验收工具与用例清单 |

## 许可

MIT，见 [`LICENSE`](LICENSE)（上游 MIT 原文保留）。第三方与改编代码见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
