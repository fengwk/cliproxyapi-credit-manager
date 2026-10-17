# v0.1.6

参考 Observer，将价格与美元预算完全移出请求准入，费用改为查询时按实际用量日志和当前价格计算。
不接管 CPA 的请求执行、上游凭据选择、重试、故障转移或会话亲和。

## 升级前必读

**本版不支持从 v0.1.5 或更早版本原地升级。** 数据库格式改为单一完整 DDL、
`PRAGMA user_version=2`，旧库会被拒绝且不会被修改，不自动迁移、补列或清空。
请先停止 CPA，冷备整个原 `data_dir` 作为独立档案，再使用新的空目录并重新创建
客户端 Key、价格和调用方。**不要直接删除旧库，也不要只替换旧目录中的数据库文件。**

继续保留至少一把从未分发的 CPA 原生守卫 Key；客户端只使用插件 Key。
部署前请阅读[运维文档](https://github.com/fengwk/cliproxyapi-credit-manager/blob/v0.1.6/docs/operations.md)。

## 口径变化

- 价格和 Key 的四个美元字段（`quota_micro_usd`、`daily_micro_usd`、
  `weekly_micro_usd`、`monthly_micro_usd`）改为**只展示**：不参与准入，
  缺失价格或超出预算不会阻止请求。字段名保留，`0` 表示未设置。
- Key 不再保存 `spent_micro_usd`、`quota_used_micro_usd` 等永久金额字段；
  请求也不保存价格快照或金额。费用改为查询时按当前价格重算。
- 成本使用 CPA UsageRecord 的实际 `usage.model`、规范化 Token 和
  `service_tier` 计算，不使用请求别名、`RequestedModel` 或 `ResponseModel` 猜价。
  改价/加价/删价会重算仍保留的日志。
- 缺价、不完整/不可信用量、整数溢出、`per_image` 缺少真实出图数量时，
  单条执行的 `cost_micro_usd` 为 `null` 并计入 `unpriced_executions`；显式全零价格是
  已定价的免费 0。summary 成本只是已定价执行的小计。
- `disabled` 价格规则只是不参与价格匹配，不是模型权限黑名单。
- 价格预览按输入 token 试算，不等于实际日志费用；参考目录仍需管理员显式
  导入/保存，不在推理或准入路径联网，也不自动关联别名。
- Token 配额、Key 权限、过期和并发仍限制请求，并保持软用量语义：不预估、
  不预占、不占用；真实用量足额记录。
- `/api/resolve` 删除；`/api/reset-spend` 改为 `/api/reset-tokens`，只重置
  Token 用量基线，不影响日志、派生费用或美元预算。

## 界面

- 管理页和客户自助页统一展示预算、已定价执行小计和未定价执行数；
  全部执行未定价时不会显示成免费 0。
- Key 启用开关和选择框与 CPA Management Center 主题样式对齐，支持键盘操作和移动布局。
- 修复 Key 表单/表格布局、金额单位提示与调用方名称必填校验。

## 成本范围

- 成本只反映仍在本地保留的日志和所选时间范围，不声称永久全历史。
- 查询会对所选执行流做一次额外的精简扫描；内存随分组和分页大小增长，
  不是长期预聚合。数字是基于本地参考价格的用量成本，不等同上游最终账单。

## 验证

- 本地 Go 测试、vet、race、static 与 128 项 Web 单元测试通过。
- Chrome 管理页 332/332、客户自助页 94/94 通过。
- 真实 CPA v8.0.30 和 v8.0.32 的隔离宿主验收各 73/73 通过。
- 发布流程对标签源码重新运行测试，在 linux/amd64、linux/arm64、darwin/arm64
  原生 runner 构建，并校验三个归档的 metadata、库架构与 SHA256 后发布。
