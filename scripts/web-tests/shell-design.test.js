'use strict';

// 静态外壳 CPA 恢复设计验证：验证 CPA 品牌资产、多图表、单位切换、汇率与免密配置。
const test = require('node:test');
const assert = require('node:assert/strict');
const { readAsset } = require('./helpers');

const html = readAsset('index.html');
const css = readAsset('style.css');

test('index.html 包含 CPA 品牌 Logo、防闪烁预渲染与自助查询入口', () => {
  assert.ok(html.includes('viewBox="0 0 512 512"'), '缺少 CM 矢量 Logo');
  assert.ok(html.includes('cli-proxy-theme'), '缺少 CPA 主题读取预渲染');
  assert.ok(html.includes('data-palette'), '缺少 data-palette 属性设置');
  assert.ok(html.includes('href="./lookup.html"'), '缺少自助查询入口链接');
  assert.ok(html.includes('信用管理'), '页面标题须保留信用管理关键词以兼容测试');
});

test('登录表单包含记住密钥复选框与会话保持文案', () => {
  assert.ok(html.includes('id="remember-management-key"'), '缺少记住密钥复选框');
  assert.ok(html.includes('name="remember-management-key"'));
  assert.ok(html.includes('id="management-key"'));
  assert.ok(html.includes('id="login-button"'));
  assert.ok(html.includes('id="login-error"'));
  assert.ok(!html.includes('仅保留在当前页面的内存中'), '不得断言绝对仅保留在内存中');
  assert.ok(html.includes('会话') && html.includes('localStorage'), '说明需体现会话保持与可选记住');
});

test('顶栏提供 Token 切换、费用货币切换与自定义汇率输入', () => {
  const tokenUnit = html.match(/<select id="token-unit"[^>]*>([\s\S]*?)<\/select>/);
  assert.ok(tokenUnit, '缺少 token-unit 下拉框');
  const tokenValues = [...tokenUnit[1].matchAll(/value="([^"]*)"/g)].map((m) => m[1]);
  assert.deepEqual(tokenValues, ['raw', 'k', 'wan', 'm']);

  const currencyUnit = html.match(/<select id="currency-unit"[^>]*>([\s\S]*?)<\/select>/);
  assert.ok(currencyUnit, '缺少 currency-unit 下拉框');
  const currencyValues = [...currencyUnit[1].matchAll(/value="([^"]*)"/g)].map((m) => m[1]);
  assert.deepEqual(currencyValues, ['USD', 'CNY']);

  assert.ok(html.includes('id="currency-rate"'), '缺少 currency-rate 输入框');
  assert.ok(html.includes('手动设定') || html.includes('自定义'), '汇率标签应标明手动提供而非实时承诺');
});

test('使用统计提供按月粒度与三组独立趋势图表', () => {
  const bucket = html.match(/<select id="usage-bucket"[^>]*>([\s\S]*?)<\/select>/);
  assert.ok(bucket, '缺少 usage-bucket 下拉框');
  const bucketValues = [...bucket[1].matchAll(/value="([^"]*)"/g)].map((m) => m[1]);
  assert.ok(bucketValues.includes('month'), '缺少 month 粒度选项');

  assert.ok(html.includes('id="usage-chart"'), '缺少请求与执行趋势图容器 usage-chart');
  assert.ok(html.includes('id="usage-token-chart"'), '缺少 Token 趋势图容器 usage-token-chart');
  assert.ok(html.includes('id="usage-cost-chart"'), '缺少费用趋势图容器 usage-cost-chart');
});

test('脚本按正确顺序加载 vendor echarts、charts.js 与 app.js', () => {
  const scripts = [...html.matchAll(/<script\s+src="([^"]+)"[^>]*><\/script>/g)].map((m) => m[1]);
  assert.deepEqual(scripts, [
    './vendor/echarts-5.6.0.min.js',
    './charts.js',
    './app.js'
  ]);
});

test('样式表具备完整 CPA 配色、主题变量与响应式无溢出保证', () => {
  assert.ok(css.includes('data-palette="white"'), '缺少 white 色板');
  assert.ok(css.includes('data-palette="light"'), '缺少 light 色板');
  assert.ok(css.includes('data-palette="dark"'), '缺少 dark 色板');
  assert.ok(css.includes('prefers-color-scheme: dark'), '缺少系统深色回退');
  assert.ok(css.includes('@media (max-width: 720px)'), '缺少移动端断点');
  assert.ok(css.includes('overflow-x: auto'), '缺少横向防溢出滚动定义');
});

test('图表容器具备显式高度与响应式栅格，并移除陈旧手工 SVG 样式', () => {
  assert.ok(css.includes('height: 280px'), 'ECharts 容器需显式 280px 左右高度');
  assert.ok(!css.includes('.chart svg'), '不得保留影响 ECharts 画布的 .chart svg 规则');
  assert.ok(!css.includes('.chart .grid-line'), '需移除陈旧的手工 SVG 样式');
  assert.ok(css.includes('.chart-card.chart-card-requests'), '缺少请求整行卡片选择器');
  assert.ok(css.includes('grid-column: 1 / -1'), '请求趋势图需占满整行');
  assert.ok(html.includes('chart-card-requests'));
  assert.ok(html.includes('chart-card-tokens'));
  assert.ok(html.includes('chart-card-cost'));
});

test('请求明细卡片具备紧凑高信息密度样式', () => {
  assert.ok(css.includes('#usage-items > .card'), '缺少紧凑明细卡片样式');
  assert.ok(css.includes('#usage-items .request-meta'), '缺少紧凑元数据网格样式');
  assert.ok(css.includes('#usage-items table.data'), '缺少紧凑表格样式');
});
