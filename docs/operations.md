# 运维手册

面向 CLIProxyAPI 用户与运维人员。安装、配置、备份、保留策略、故障处理都集中在这里；
更深的设计约束见 [`IMPLEMENTATION.md`](IMPLEMENTATION.md)，接口速查见
[`../README.md`](../README.md)。

## 1. 安装与升级

### 目录与 id

宿主按 `<plugins.dir>/<goos>/<goarch>/<文件名>` 发现原生库，并用**文件名去掉扩展名**
作为插件实例 id。发布归档里的库名是 `cliproxyapi-credit-manager.so`（macOS 为
`.dylib`），所以：

- 实例 id：`cliproxyapi-credit-manager`
- 配置块：`plugins.configs.cliproxyapi-credit-manager`
- 界面地址：`/v0/resource/plugins/cliproxyapi-credit-manager/index.html`
- **财务 API 前缀是插件自己声明的固定值**：`/v0/management/plugins/credit-manager/api/…`
  （与实例 id 不是同一个字符串，不要混用）

安装步骤（主机上执行）：

```sh
# 1. 校验下载物
sha256sum -c checksums.txt --ignore-missing   # 需要与 zip 同目录

# 2. 解压，只复制动态库
unzip -o cliproxyapi-credit-manager_0.1.0_linux_amd64.zip cliproxyapi-credit-manager.so
mkdir -p "$CPA_DIR/plugins/linux/amd64"
cp cliproxyapi-credit-manager.so "$CPA_DIR/plugins/linux/amd64/"

# 3. 按 config.example.yaml 增加 plugins.configs 段，然后重启 CPA
```

> 归档里的 `LICENSE`、`README.md`、`metadata.json` 是发布记录，**不需要**复制到插件目录；
> 宿主只读取动态库本身。

### 升级

1. 停止 CPA，并冷备整个插件数据目录；本版只发布 Linux/macOS 动态库。
2. 用新版本的库文件覆盖同名文件（路径与文件名保持不变，实例 id 才不变）。
3. 在同一个 `data_dir` 上启动 CPA，配置与账务、客户端 Key 全部延续。

### 卸载

删除库文件并移除 `plugins.configs` 中的配置块即可。**`data_dir` 不要随手删除**：里面
有 `pepper`，删掉之后所有客户端 Key 都无法再验证（见第 4 节）。

## 2. 配置项

配置写在 CPA 的 `config.yaml` 的 `plugins.configs.cliproxyapi-credit-manager` 下，
使用**严格解析**：出现未知键会直接报错并在日志中拒绝注册，所以拼写必须准确。

| 键 | 默认值 | 取值范围 | 说明 |
| --- | --- | --- | --- |
| `enabled` | 由宿主下发 | `true`/`false` | 宿主字段：只控制该实例开关，不影响全局 `plugins.enabled` |
| `data_dir` | `./data/cliproxyapi-credit-manager` | 非空 | 数据目录；相对路径按 CPA 工作目录解析为绝对路径；**运行期不可修改** |
| `history.retention_months` | `3` | 1..1200 | 历史保留月数（UTC 日历月） |
| `max_pending_requests` | `16` | 1..10000 | 每个 Key 允许同时在途（active+pending）的请求数 |
| `request_capture.enabled` | `false` | `true`/`false` | 是否启用请求快照 |
| `request_capture.retention` | `"24h"` | 正的 Go duration | 快照保留时长 |
| `request_capture.max_body_bytes` | `10485760` (10 MiB) | 1B..100MiB | 单个快照体积上限 |
| `request_capture.max_storage_bytes` | `1073741824` (1 GiB) | 1MiB..1TiB | 快照库物理上限（含 WAL） |
| `warmup.enabled` | `false` | `true`/`false` | 是否启用账号预热 |
| `warmup.interval` | `"1h"` | 正的 Go duration | 预热周期 |
| `warmup.targets[]` | 空 | — | 每个目标需 `auth_id`、`provider`、`model`；`protocol` ∈ `openai`/`claude`/`gemini`（缺省 `openai`） |

修改任何一项都需要宿主重新下发配置（重启 CPA，或通过宿主的管理接口触发热重载）。
`data_dir` 变更会被显式拒绝。

## 3. 数据目录与两个数据库

`data_dir` 下的文件：

```
data_dir/
├── credit.db         # 账务库（Key、价格、请求、用量、计数器）
├── credit.db-wal     # 同上，WAL
├── request-captures.db   # 请求快照库（可随时丢弃）
├── pepper            # 0600，客户端 Key 的 HMAC 密钥材料（首次启动生成）
└── lock              # 单实例目录锁
```

- **两个 SQLite 库相互独立**：账务库负责钱和额度；快照库只存诊断快照。快照库损坏或
  打不开时插件会降级为“快照关闭 + 健康告警”，**不影响**记账与推理。
- **单实例**：同一个 `data_dir` 同一时刻只允许一个进程打开（目录锁）。第二个进程会
  启动失败，这是有意设计——避免双重恢复或双记账。
- 账务库使用单连接、`synchronous=FULL`、`busy_timeout=5s`，提交即持久化。
  账务文件和 SQLite 辅助文件使用私有权限；已有账务库丢失 `pepper` 时启动失败，
  不会生成新的密钥覆盖恢复线索，应还原成对备份。

## 4. 备份与恢复（务必遵守）

必须备份的内容是**整个 `data_dir`**（尤其 `pepper` 与 `credit.db`），并且：

1. **先停止 CPA，再备份**。运行中直接拷走在途状态可能不一致。
2. **`pepper` 必须与账务库成对保存**，不要分开、不要删除、不要重建。
   - 库中保存的是 HMAC 校验值，只有同一 `pepper` 才能验证客户端 Key；
   - 更换或丢失 `pepper` = 所有客户端 Key 立刻失效，且无法找回明文（明文从未落库）。
3. **恢复时原样还原** `pepper` 与 `credit.db`（含 `-wal`），不要“重新初始化”。
4. **不做旧版本数据库迁移**：本版本首次启动会在**空目录**上自动建库；不要指向其它
   版本或其它工具创建的既有同名库，也不要依赖自动升级。
5. 快照库 (`request-captures.db`) 可以不备份；它只是诊断数据。

建议做法：停机冷备整个目录并带上时间戳；把备份与 `pepper` 一起纳入受控密钥管理；
定期做一次“还原到空主机”的演练。

## 5. 保留与清理

- 历史默认保留 **3 个 UTC 日历月**（月末按日历钳位，例如 3 月 31 日往前一个月算到
  2 月 28/29 日）。
- 每次维护会删除**早于保留窗口且已结算**的历史行（请求、用量、调整），批量删除。
- **不删除**：未决（active/pending）请求、Key、价格配置、模型 token 限额，以及
  **终身累计计数**。跨清理存活的是耐久计数，例如 Key 的累计花费/占用与 total 窗口的
  金额与 token 计数。
- 清理不会做整库 `VACUUM`，只做增量回收 + WAL 截断，且**每次回收有上限**，所以文件
  体积可能大于当前内容量。
- 如果 WAL 被占用导致检查点失败，健康状态会报告错误（不会假装成功）；下一次维护会
  再试。
- **历史幂等只在保留窗口内有效**：窗口外的旧请求行已被物理删除，迟到的用量事件无法
  “复活”这些请求；跨保留期的旧 `Reserve` 也不再支持。

维护按固定节拍（约每分钟）自动执行，不需要人工干预。

## 6. 未决请求与管理员裁决

请求生命周期：`active`（在途，占用金额与并发）→ 结束时进入 `pending`（账务未决）或
`settled`（已结算并释放占用）。

出现下列情况时请求会**保持 pending 且继续占用金额**，而不是按 0 计费：

- 完全没有收到上游用量事件；
- 用量质量不可信（unknown）或不完整（partial）；
- 上游失败但费用无法证明为零。

管理员在「使用统计」页对未决请求做裁决（也可用 `POST /api/resolve`）：

- `action: "release"`：释放尚未解决的占用，不退款已入账费用；
- `action: "charge"`：按指定 `amount_micro_usd` 扣费；
- 两个动作都**必须给出原因** `reason`；
- 只接受 `pending` 状态：对 `active` 请求裁决会返回 409 冲突（否则会绕过并发与额度）。

每次人工裁决都会追加一条**独立**的调整记录，展示时不并入执行成本：报表里执行成本、
人工调整与 Key 账本可以分别核对。

> **崩溃语义**：上游用量事件经由宿主的内存队列送达插件，宿主队列不持久化。插件对
> “已提交的账务”是持久化的（SQLite FULL 同步），但**不承诺崩溃安全的一次性计费**，
> 也不承诺任何丢失的事件一定会被补回。请把 pending + 人工裁决当作兜底手段。

## 7. 请求快照（默认关闭）

- 默认**关闭**。开启后抓取的是**执行入口的诊断快照**（请求 ID、入站请求头、入站请求
  体），**不是**最终发给上游的网络原始报文，也不是上游返回。
- 单个快照体积上限、保留时长、物理上限见配置表；快照库的物理占用按**数据库文件 + WAL**
  统计，并预留一定余量，压力大时会**先淘汰最旧的快照**。
- 快照状态可能是：`disabled`（未开启）、`captured`、`oversize`（超限被跳过）、
  `dropped`（队列满等被丢弃）、`expired`（已过期）、`unavailable`（无证据可查）。
- 快照写入失败、超限或淘汰**永远不会**导致推理失败或计费变更。
- **脱敏有边界**：只对已知的敏感请求头与常见 JSON 敏感键替换为 `[redacted]`，不是
  匿名化承诺。非 JSON 请求体会原样保存；JSON 形态但解析失败、过深或含尾随内容的
  快照会丢弃，避免未经脱敏落盘。不要把快照当作合规 DLP 手段，
  也不要把它当作财务凭证（它可能已被淘汰或过期）。
- 清理/回收依赖 SQLite 检查点；遇到 busy 时会报告错误并在下一次维护重试，**不保证
  立即完成**。

## 8. 账号预热（默认关闭，不建议默认开启）

预热会对**真实凭据**发起**真实请求**，请按需谨慎启用。

- 默认 `enabled: false`，且 `targets` 为空时不产生任何后台请求。
- 每个目标必须精确写死 `auth_id`、`provider`、`model`（`auth_id`/`provider` 请从 CPA
  的实际凭据与执行记录中取准确值）。插件**不会**替你猜测或回退到宿主侧的凭据/提供商
  选择，避免误用或干扰未指定的账号。
- 探测请求只把**输出**限制为 1 个 token。**这不代表上游只消耗 1 个 token**：输入部分
  以及模型内部的隐藏 reasoning 仍由上游提供商计费，费用由你用 CPA 的账号自行承担，
  与插件内的客户端信用费用无关。
- 探测**不预留、不独占、不排除**账号：同一凭据上的客户请求会正常并行并照常计费。
- 探测失败只写入插件健康信息（例如“某 provider 预热失败”），不会进入客户账本，也不
  阻断准入；宿主侧可能因此对凭据施加它自己的冷却。
- **写入期间不可取消**：模型的执行回调是宿主同步调用，无法被中止。因此启用预热后：
  - `plugin.shutdown` 与紧随其后的**热重载会等待在途探测真正结束**，若上游卡住，停机与
    重启可能被拖长；
  - 重新下发配置时，插件最多等待 5 秒让本轮探测空闲，否则返回 **409** 并**继续按旧
    配置和旧周期探测**，既不会留下死掉的周期任务，也不会启动第二个循环。
- 预热会占用连接与上游配额，**可能影响延迟**；请不要对外宣称“对业务无影响”。

## 9. 故障与健康

`GET /api/health` 返回 `status`（`ok`/`degraded`）、`version`、`capture` 统计、
`retention_months`、`capture_enabled`、`last_error` 与当前配置。

- **粘性健康**：插件维护/财务错误会保留到成功的重新配置或重启，成功维护不会自动清除
  该告警；快照库还提供自身的错误统计。
- **财务故障会阻断新准入**：账务库出错时，新请求会被拒绝（fail closed），而不是放行
  不计费流量。
- **维护类故障**（例如快照库异常）只影响健康状态与快照能力。

常见问题：

| 现象 | 含义与处理 |
| --- | --- |
| 客户端 401 | 没有身份或身份未知：Key 未携带、拼写错误、被禁用或已过期 |
| 客户端 403 | 该模型未配置价格，或 Key 不允许该模型；先配置价格/放行模型 |
| 客户端 429 | 额度、并发或未决请求上限触顶 |
| 客户端 5xx | 账务库不可用或准入被阻断：查看 `/api/health` 与 CPA 日志 |
| 管理 API 401 | 缺少/错误的 `Authorization: Bearer <管理密钥>` |
| 管理 API 415 | 非 GET 请求缺少 `Content-Type: application/json` |
| 管理 API 409 | 冲突：对非 pending 请求裁决、重配置被拒（配置仍为旧值且仍生效） |
| 界面要求重新登录 | 正常行为：管理密钥只存在内存中 |

## 10. 上线前安全核对清单

- [ ] 已确认 **CPA 原生 `access.api-keys` 从未分发给调用方**（保持“未分发”状态）。
- [ ] 原生 Key 列表至少有一把高强度守卫 Key，且其中不包含插件客户 Key；原生列表为空
      不是安全部署，可能在插件缺失时放行匿名请求。
- [ ] `plugins.enabled: true`，且库文件名与 `plugins.configs` 的键一致。
- [ ] `data_dir` 位于受控存储，且已纳入包含 `pepper` 的整目录冷备。
- [ ] 管理密钥为高强度口令，且只通过安全的带外渠道分发给管理员。
- [ ] 已实测误删/误停插件的后果：在上述守卫前提下，插件缺失时所有客户 Key 都被拒绝，
      上游调用为零；不把插件缺席时的安全错误归因于插件代码仍在执行。
- [ ] 快照若开启，已明确它不是 DLP、可能提前丢失，且不用于对账。
- [ ] 预热若开启，已明确它会真实消耗上游额度。
