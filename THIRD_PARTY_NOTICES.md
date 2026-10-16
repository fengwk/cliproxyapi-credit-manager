# 第三方声明

本文件列出 `cliproxyapi-credit-manager` 中包含或依赖的第三方组件及其许可。

## 本仓库自身许可

本仓库源码采用 MIT 许可，Copyright (c) 2026 yuluo688，完整许可文本见根目录
[`LICENSE`](LICENSE)。上游许可原文保留不变。

## 改编的第三方源码

### cliproxyapi-observer（MIT）

`internal/plugin/accounting/accounting.go` 中的 adapter cohort token 修复逻辑改编自
`github.com/fengwk/cliproxyapi-observer/internal/observer/adapter_accounting.go`
（MIT License, Copyright (c) 2026 fengwk）。仅保留其“窄兼容范围”“唯一完整候选”
和拒绝畸形计数的行为；文件头已保留原始出处与许可说明。

```
MIT License

Copyright (c) 2026 fengwk

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## 依赖组件

以下组件由 `go.mod` 直接依赖，均以各自的原始许可随构建产物或源码分发；此处仅作
告知，不修改其条款。完整条款以上游仓库为准。

| 组件 | 版本 | 许可 |
| --- | --- | --- |
| `github.com/router-for-me/CLIProxyAPI/v8`（SDK） | v8.0.30 | MIT License（Copyright (c) 2025 Luis Pater） |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT 与 Apache License 2.0 双许可（含 NOTICE，Copyright 2011-2016 Canonical Ltd.） |
| `modernc.org/sqlite` | v1.46.1 | BSD-3-Clause（Copyright (c) 2017 The Sqlite Authors）；另附 `SQLITE-LICENSE`，SQLite 本身属公有领域 |

## 内嵌的第三方前端资源

### Apache ECharts（Apache License 2.0）

趋势图使用 Apache ECharts 5.6.0 渲染。该库以**未经修改的原始发行文件**内嵌在插件中，
不使用任何 CDN、打包器或外部网络请求；界面资源全部由 `go:embed` 打进插件二进制。

| 文件 | 说明 |
| --- | --- |
| `internal/web/assets/vendor/echarts-5.6.0.min.js` | 官方压缩发行文件（与上游逐字节一致） |
| `internal/web/assets/vendor/echarts-LICENSE` | Apache License 2.0 全文 |
| `internal/web/assets/vendor/echarts-NOTICE` | 上游 NOTICE |

许可：Apache License 2.0，Copyright 2017-2024 The Apache Software Foundation。
完整条款与归属见上述 `echarts-LICENSE` 与 `echarts-NOTICE`，本仓库未修改其条款。
文件来源为原版 credit-manager 管理界面同版本发行文件
（`credit-manager/internal/management/web/vendor/`），md5 与本仓库副本一致。

## 运行时宿主

本插件是 CLIProxyAPI v8 的原生插件，由宿主进程以 `c-shared` 动态库形式加载
（`CGO_ENABLED=1`，Go 1.26）。宿主本身的许可由其自身仓库声明，不在本仓库范围内。

## 字体、图标与前端资源

管理界面为纯 HTML/CSS/JS，不引入任何 CDN、打包器、字体或第三方图标库。唯一的第三方
前端资源是上文内嵌的 Apache ECharts（本地 `vendor/` 副本，非 CDN）；界面资源全部内嵌
在插件二进制中。
