package web

// 管理界面静态资源的结构性检查。这些断言把 docs/IMPLEMENTATION.md 中的硬约束
// （管理接口路径、契约枚举、安全渲染、无外部依赖）固定下来，避免界面在后续改动中
// 悄悄回退到旧别名、内联脚本或对外部 CDN 的依赖。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("assets", name))
	if err != nil {
		t.Fatalf("读取静态资源 %s 失败: %v", name, err)
	}
	return string(data)
}

// 静态外壳必须是自包含的相对路径页面，且不含任何凭据。
func TestShellIsSelfContained(t *testing.T) {
	html := readAsset(t, "index.html")

	refs := regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(html, -1)
	allowed := map[string]bool{
		"./style.css":                   true,
		"./lookup.html":                 true,
		"./vendor/echarts-5.6.0.min.js": true,
		"./charts.js":                   true,
		"./numbers.js":                  true,
		"./app.js":                      true,
	}
	if len(refs) == 0 {
		t.Fatal("index.html 未引用任何静态资源")
	}
	for _, match := range refs {
		if !allowed[match[1]] {
			t.Errorf("index.html 引用了非预期资源: %s", match[1])
		}
	}
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Error("静态外壳不得引用外部 CDN 或外部地址")
	}
	if regexp.MustCompile(`sessionStorage\s*[.=\[]|document\.cookie\s*=`).MatchString(html) {
		t.Error("静态外壳不得读写 sessionStorage 或 Cookie")
	}
	if !strings.Contains(html, `lang="zh-CN"`) {
		t.Error("界面语言应为简体中文")
	}
	if regexp.MustCompile(`sk-[A-Za-z0-9]{8,}`).MatchString(html) {
		t.Error("静态外壳不得内嵌密钥")
	}
}

// 五个标签页的面板必须与脚本中的加载器一一对应。
func TestTabsMatchPanelsAndLoaders(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	tabPattern := regexp.MustCompile(`data-tab="([a-z]+)"`)
	matches := tabPattern.FindAllStringSubmatch(html, -1)
	want := []string{"usage", "keys", "prices", "credentials", "health"}
	if len(matches) != len(want) {
		t.Fatalf("应有 %d 个标签页，实际 %d 个", len(want), len(matches))
	}
	for i, match := range matches {
		if match[1] != want[i] {
			t.Errorf("第 %d 个标签页为 %s，期望 %s", i+1, match[1], want[i])
		}
		if !strings.Contains(html, `id="panel-`+match[1]+`"`) {
			t.Errorf("缺少面板 panel-%s", match[1])
		}
		if !strings.Contains(html, `aria-controls="panel-`+match[1]+`"`) {
			t.Errorf("标签 %s 缺少 aria-controls", match[1])
		}
		if !strings.Contains(js, match[1]+": function") {
			t.Errorf("脚本缺少 %s 的标签加载器", match[1])
		}
	}
}

// 脚本必须只访问文档规定的管理接口与上游凭据接口。
func TestAppJSUsesDocumentedEndpoints(t *testing.T) {
	js := readAsset(t, "app.js")

	required := []string{
		"/v0/management/plugins/credit-manager",
		"/api/keys",
		"/api/rotate",
		"/api/prices",
		"/api/usage",
		"/api/capture",
		"/api/resolve",
		"/api/health",
		// 凭据页：CPA v8 凭据列表与插件额度查询。
		"/v8/management/credentials",
		"/v8/management/plugins/",
		"/quota",
	}
	for _, path := range required {
		if !strings.Contains(js, path) {
			t.Errorf("app.js 缺少接口路径 %s", path)
		}
	}
	// 财务与账务接口绝不能出现在公开静态资源路径下。
	if strings.Contains(js, "/v0/resource/plugins/credit-manager/api/") {
		t.Error("管理 API 不得挂在公开资源前缀下")
	}
}

// 质量/状态/结果的取值必须与 credit 契约一致，且不再保留别名。
func TestAppJSContractEnums(t *testing.T) {
	js := readAsset(t, "app.js")

	required := []string{"complete", "partial", "unknown", "active", "pending", "settled", "succeeded", "failed", "rejected", "canceled"}
	for _, value := range required {
		if !strings.Contains(js, value) {
			t.Errorf("app.js 缺少契约取值 %s", value)
		}
	}
	for _, stale := range []string{"inconsistent", "unclassified", "cancelled", "'success'", "'stopped'", "'charged'"} {
		if strings.Contains(js, stale) {
			t.Errorf("app.js 仍保留已废弃的取值 %s", stale)
		}
	}
	// 响应模型只允许展示服务端实际上报的字段，不得回退到执行模型。
	if !strings.Contains(js, "response_model") {
		t.Error("app.js 必须展示 execution.response_model")
	}
}

// 界面必须区分执行成本与人工补记，并按契约解释失败/未结算计数。
func TestAppJSAccountingWording(t *testing.T) {
	js := readAsset(t, "app.js")

	required := []string{
		"adjustment_micro_usd", // 人工补记的契约字段
		"失败请求",                 // summary.failed 是失败请求数，不是失败执行数
		"未结算请求",                // summary.pending 是 active+pending
		"执行成本",
		"人工补记",
		"已知用量照常入账",
	}
	for _, token := range required {
		if !strings.Contains(js, token) {
			t.Errorf("app.js 缺少 %q", token)
		}
	}
	// 只断言卡片标签本身，不禁止说明文案里对这些口径的正常引用。
	for _, stale := range []string{"label: '失败执行'", "label: '待定请求'", "label: '成本合计'", "不计入成本"} {
		if strings.Contains(js, stale) {
			t.Errorf("app.js 仍保留错误口径卡片 %q", stale)
		}
	}
}

// 界面必须用 textContent 渲染服务端数据，且不落地任何凭据。
func TestAppJSRenderSafety(t *testing.T) {
	js := readAsset(t, "app.js")

	// 只禁止实际写入/调用，允许注释中提及这些 API（用于说明为何不使用）。
	// 管理密钥与主题/单位偏好允许通过 Origin 隔离的 sessionStorage / localStorage 规范读写；
	// 但严禁 Cookie 与 innerHTML/eval 等高危 API。
	forbidden := []string{
		`\.innerHTML\s*=`,
		`\.outerHTML\s*=`,
		`insertAdjacentHTML`,
		`document\.write`,
		`document\.cookie\s*=`,
		`\beval\s*\(`,
	}
	for _, token := range forbidden {
		if regexp.MustCompile(token).MatchString(js) {
			t.Errorf("app.js 不得使用 %s", token)
		}
	}
	if !strings.Contains(js, "document.createTextNode") {
		t.Error("服务端数据必须通过 createTextNode/textContent 写入")
	}
	if !strings.Contains(js, "Authorization: 'Bearer '") {
		t.Error("管理请求必须携带 Authorization 头")
	}
	if !strings.Contains(js, "credentials: 'same-origin'") || !strings.Contains(js, "cache: 'no-store'") {
		t.Error("管理请求应限制为同源且不缓存")
	}
}

// 样式表必须支持窄屏：内部滚动而不是页面横向溢出。
func TestStyleSupportsNarrowScreens(t *testing.T) {
	css := readAsset(t, "style.css")

	if !strings.Contains(css, "@media (max-width: 720px)") {
		t.Error("缺少移动端断点")
	}
	if !strings.Contains(css, "overflow-x: auto") {
		t.Error("宽表格/图表必须在容器内横向滚动")
	}
	if regexp.MustCompile(`url\(\s*['"]?https?:`).MatchString(css) || strings.Contains(css, "@import") {
		t.Error("样式表不得加载外部资源")
	}
}
