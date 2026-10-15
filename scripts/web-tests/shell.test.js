'use strict';

// 静态外壳检查：相对资源路径、无凭据落地、无 emoji、无 innerHTML、五个标签页齐全。
const test = require('node:test');
const assert = require('node:assert/strict');
const { readAsset } = require('./helpers');

const html = readAsset('index.html');
const css = readAsset('style.css');
const js = readAsset('app.js');

const EMOJI = /[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}\u{2B00}-\u{2BFF}]/u;

test('index.html 仅引用相对资源，且不含任何凭据或账务数据', () => {
  const refs = [...html.matchAll(/(?:href|src)="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(refs.sort(), ['./app.js', './style.css']);
  assert.ok(!/https?:\/\//.test(html), '静态外壳不得引用外部 CDN');
  assert.ok(!/sk-[A-Za-z0-9]/.test(html), '静态外壳不得内嵌密钥');
  assert.ok(!/Bearer\s/i.test(html), '静态外壳不得内嵌凭据');
  assert.ok(!/localStorage\s*[.[]|sessionStorage\s*[.[]/.test(html), '静态外壳不得访问存储 API');
  assert.ok(!/localStorage\s*=|sessionStorage\s*=/.test(html));
  assert.equal(html.match(/<meta charset="utf-8">/g).length, 1);
  assert.ok(html.includes('lang="zh-CN"'));
});

test('五个标签页在 HTML 与脚本中一致', () => {
  const htmlTabs = [...html.matchAll(/data-tab="([a-z]+)"/g)].map((m) => m[1]);
  assert.deepEqual(htmlTabs, ['usage', 'keys', 'prices', 'credentials', 'health']);
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

test('脚本不写入存储、不使用 innerHTML/eval、始终携带 Authorization', () => {
  assert.ok(!/localStorage|sessionStorage/.test(js), '管理密钥不得落地存储');
  assert.ok(!/document\.cookie/.test(js), '不得使用 Cookie 保存密钥');
  assert.ok(!/innerHTML\s*=/.test(js), '不得用 innerHTML 写入服务端数据');
  assert.ok(!/insertAdjacentHTML|outerHTML\s*=|document\.write/.test(js));
  assert.ok(!/\beval\(/.test(js));
  assert.ok(js.includes("Authorization: 'Bearer ' + state.key"), '管理请求必须携带 Authorization');
  assert.ok(js.includes("credentials: 'same-origin'"));
  assert.ok(js.includes('cache: \'no-store\''));
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
  for (const required of ['partial', 'unknown', 'active', 'pending', 'settled', 'succeeded', 'rejected', 'canceled', 'adjustment_micro_usd']) {
    assert.ok(js.includes(required), `缺少契约取值 ${required}`);
  }
});

test('安全渲染辅助函数存在且服务端数据只走 textContent', () => {
  assert.ok(js.includes('function appendChildren'), '必须使用安全 DOM 构造器');
  assert.ok(js.includes("node.appendChild(document.createTextNode(String(child)))"));
  assert.ok(js.includes('function prettyJSONLossless'));
  assert.ok(js.includes('function parseJSON'));
});
