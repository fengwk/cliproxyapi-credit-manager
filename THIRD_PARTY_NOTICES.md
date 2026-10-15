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

## 运行时宿主

本插件是 CLIProxyAPI v8 的原生插件，由宿主进程以 `c-shared` 动态库形式加载
（`CGO_ENABLED=1`，Go 1.26）。宿主本身的许可由其自身仓库声明，不在本仓库范围内。

## 字体、图标与前端资源

管理界面为纯 HTML/CSS/JS，不引入任何 CDN、打包器、字体或第三方图标库；界面资源
全部内嵌在插件二进制中。
