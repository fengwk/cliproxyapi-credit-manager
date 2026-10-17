'use strict';

// 静态外壳检查：相对资源路径、显式凭据存储、无 innerHTML、五个标签页齐全。
const test = require('node:test');
const assert = require('node:assert/strict');
const { readAsset } = require('./helpers');

const html = readAsset('index.html');
const css = readAsset('style.css');
const js = readAsset('app.js');

const EMOJI = /[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}\u{2B00}-\u{2BFF}]/u;

test('index.html 仅引用相对资源，且不含任何凭据或账务数据', () => {
  const refs = [...html.matchAll(/(?:href|src)="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(refs.sort(), ['./admin-pricing.js', './app.js', './charts.js', './lookup.html', './numbers.js', './style.css', './vendor/echarts-5.6.0.min.js']);
  assert.ok(!/https?:\/\//.test(html), '静态外壳不得引用外部 CDN');
  assert.ok(!/sk-[A-Za-z0-9]{8,}/.test(html), '静态外壳不得内嵌密钥');
  assert.ok(!/Bearer\s+[A-Za-z0-9]{16,}/i.test(html), '静态外壳不得内嵌凭据');
  assert.ok(!/sessionStorage\s*[.=[\]]|document\.cookie\s*=/.test(html), '静态外壳不得读写 sessionStorage 或 Cookie');
  assert.equal(html.match(/<meta charset="utf-8">/g).length, 1);
  assert.ok(html.includes('lang="zh-CN"'));
});

test('标签页在 HTML 与脚本中一致（动态派生标签 ID）', () => {
  const htmlTabs = [...html.matchAll(/data-tab="([a-z]+)"/g)].map((m) => m[1]);
  assert.ok(htmlTabs.length >= 6, '标签页数量应至少包含基础与概览等页面');
  const required = ['overview', 'usage', 'keys', 'prices', 'credentials', 'health'];
  for (const tab of required) {
    assert.ok(htmlTabs.includes(tab), `缺少必需标签页 ${tab}`);
  }
  for (const tab of htmlTabs) {
    assert.ok(html.includes(`id="panel-${tab}"`), `缺少面板 panel-${tab}`);
    assert.ok(js.includes(`${tab}: function`), `脚本缺少 ${tab} 的加载器`);
    assert.ok(html.includes(`aria-controls="panel-${tab}"`));
  }
});

test('结果筛选只提供契约枚举值', () => {
  const select = html.match(/<select id="usage-result">([\s\S]*?)<\/select>/);
  assert.ok(select, '缺少结果筛选下拉框');
  const values = [...select[1].matchAll(/<option value="([^"]*)"/g)].map((m) => m[1]);
  // 后端 result 参数接受 4 个 outcome 值与 active/pending 两个 state 值（pending = active+pending）。
  assert.deepEqual(values, ['', 'succeeded', 'failed', 'rejected', 'canceled', 'active', 'pending']);
});

test('凭据与偏好按规范使用存储、不使用 Cookie、不使用 innerHTML/eval、始终携带 Authorization', () => {
  assert.ok(!/document\.cookie/.test(js), '不得使用 Cookie 保存密钥');
  assert.ok(!/innerHTML\s*=/.test(js), '不得用 innerHTML 写入服务端数据');
  assert.ok(!/insertAdjacentHTML|outerHTML\s*=|document\.write/.test(js));
  assert.ok(!/\beval\(/.test(js));
  assert.ok(js.includes("Authorization: 'Bearer ' + state.key"), '管理请求必须携带 Authorization');
  assert.ok(js.includes("credentials: 'same-origin'"));
  assert.ok(js.includes('cache: \'no-store\''));
  assert.ok(js.includes('sessionStorage'), '会话凭据使用 sessionStorage 隔离在 tab 内');
  assert.ok(js.includes('credit_manager_mgmt_token'), '存储键遵循契约前缀');
});

test('脚本与样式不含 emoji，避免华而不实的符号', () => {
  for (const [name, text] of [['index.html', html], ['app.js', js], ['style.css', css]]) {
    const found = text.match(EMOJI);
    assert.equal(found, null, `${name} 含疑似 emoji：${found && found[0]}`);
  }
});

test('样式表不引用外部资源', () => {
  assert.ok(!/url\(\s*['"]?https?:/i.test(css), 'CSS 不得加载外部资源');
  assert.ok(!/@import/.test(css));
  assert.ok(css.includes('@media (max-width: 720px)'), '必须包含移动端断点');
  assert.ok(css.includes('overflow-x: auto'), '表格/图表必须在窄屏内滚动而非撑破页面');
});

test('脚本不保留已废弃的枚举别名', () => {
  // 契约取值固定为 quality complete|partial|unknown、state active|pending|settled、
  // outcome succeeded|failed|rejected|canceled；旧别名会让界面显示与服务端不一致。
  for (const stale of ['inconsistent', 'unclassified', 'cancelled', "'success'", "'stopped'", "'charged'"]) {
    assert.ok(!js.includes(stale), `不应再出现旧取值 ${stale}`);
  }
  for (const required of ['partial', 'unknown', 'active', 'pending', 'settled', 'succeeded', 'rejected', 'canceled', 'unpriced_executions']) {
    assert.ok(js.includes(required), `缺少契约取值 ${required}`);
  }
});

test('安全渲染辅助函数存在且服务端数据只走 textContent', () => {
  assert.ok(js.includes('function appendChildren'), '必须使用安全 DOM 构造器');
  assert.ok(js.includes("node.appendChild(document.createTextNode(String(child)))"));
  assert.ok(js.includes('function prettyJSONLossless'));
  assert.ok(js.includes('function parseJSON'));
});

test('脚本与页面不包含预授权与持有金额等陈旧概念与字段', () => {
  assert.equal(js.includes('held_micro_usd'), false, 'app.js 不得引用 held_micro_usd');
  assert.equal(js.includes('持有金额'), false, 'app.js 不得渲染持有金额');
  assert.equal(js.includes('归还持有金额'), false, 'app.js 不得包含归还持有金额');
  assert.equal(js.includes('预扣款'), false, 'app.js 不得包含预扣款文案');
  assert.equal(html.includes('held_micro_usd'), false, 'index.html 不得包含 held_micro_usd');
});
