'use strict';

// 自助查询页与无损数字模块的独立单元测试。
//
// 覆盖三件事：
//   1. numbers.js 的解析/求和绝不让 64 位账务整数经过浮点（>2^53 保持精确）；
//   2. lookup.js 的纯逻辑：趋势维度原样下发（含真实月）、范围边界、凭据存储
//      （会话优先、两侧独立 try、源站校验）；
//   3. lookup.js 的源码约束：走共享图表与无损解析、趋势重绘不清空 ECharts 容器、
//      生命周期带 epoch/AbortController。
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { ASSETS_DIR, readAsset } = require('./helpers');

// 先加载共享模块，让 intText/tokenText 走真实实现，而不是本地降级分支。
const CMCharts = require(path.join(ASSETS_DIR, 'charts.js'));
const CMNumbers = require(path.join(ASSETS_DIR, 'numbers.js'));
const CMLookup = require(path.join(ASSETS_DIR, 'lookup.js'));

const BIG = '9007199254740993'; // 2^53 + 1

// --- 1. 无损数字 -----------------------------------------------------------

test('numbers.parse 保留超过 2^53 的整数，且不改变安全范围内的取值', () => {
  const parsed = CMNumbers.parse(
    '{"big":9007199254740993,"safe":123,"str":"9007199254740993","neg":-90071992547409931,"deep":{"list":[12345678901234567890]},"float":1.5,"exp":1e3}'
  );
  assert.equal(parsed.big.cmIntText, BIG);
  assert.equal(typeof parsed.safe, 'number');
  assert.equal(parsed.safe, 123);
  assert.equal(parsed.str, '9007199254740993', '字符串字面量不得被改写');
  assert.equal(parsed.neg.cmIntText, '-90071992547409931');
  assert.equal(parsed.deep.list[0].cmIntText, '12345678901234567890');
  assert.equal(parsed.float, 1.5);
  assert.equal(parsed.exp, 1000);
  // 关键回归：原生 JSON.parse 会把它改成 9007199254740992。
  assert.notEqual(parsed.big.cmIntText, String(JSON.parse(`{"v":${BIG}}`).v));
});

test('numbers 的精确整数文本与求和绝不经过浮点', () => {
  assert.equal(CMNumbers.intText({ cmIntText: BIG }), BIG);
  assert.equal(CMNumbers.intText(9007199254740993n), BIG);
  assert.equal(CMNumbers.intText(12), '12');
  assert.equal(CMNumbers.isBigIntegerLiteral(BIG), true);
  assert.equal(CMNumbers.isBigIntegerLiteral('123'), false);
  assert.equal(CMNumbers.isBigIntegerLiteral('1.5'), false);
  // 共享图表模块存在时应使用同一实现，保证与其它页面口径一致。
  assert.equal(CMNumbers.intText({ cmIntText: BIG }), CMCharts.intText({ cmIntText: BIG }));
  const sum = CMNumbers.sumIntTexts([{ cmIntText: BIG }, 9007199254740993n, 1]);
  assert.equal(sum, '18014398509481987');
  assert.notEqual(sum, String(Number(BIG) + Number(BIG) + 1), '浮点求和会丢精度');
  assert.equal(CMNumbers.sumIntTexts([{ cmIntText: BIG }, 'not-a-number', undefined]), BIG);
});

// --- 2. 查询构造与凭据存储 -------------------------------------------------

test('趋势维度原样下发：hour/day/month 都直传服务端 bucket', () => {
  const base = 'https://host.test/v0/resource/plugins/credit-manager/lookup/data';
  const now = new Date('2026-10-16T09:30:00Z');
  for (const grain of ['hour', 'day', 'month']) {
    const url = CMLookup.buildDataURL(base, { grain, range: 'all', page: 3, pageSize: 10 }, now);
    assert.equal(url.searchParams.get('bucket'), grain, `${grain} 必须原样下发`);
    assert.equal(url.searchParams.get('page'), '3');
    assert.equal(url.searchParams.get('page_size'), '10');
    assert.equal(url.searchParams.get('from'), null, '全部保留期不带 from');
  }
  // 未知维度回落到 day，而不是继续透传非法值。
  const fallback = CMLookup.buildDataURL(base, { grain: 'week', range: 'all', page: 1, pageSize: 10 }, now);
  assert.equal(fallback.searchParams.get('bucket'), 'day');
});

test('时间范围按本地自然日计算，且请求不携带任何凭据', () => {
  const now = new Date('2026-10-16T09:30:00Z');
  const today = CMLookup.rangeBounds('today', now);
  const seven = CMLookup.rangeBounds('7', now);
  const all = CMLookup.rangeBounds('all', now);
  const start = new Date(today.from);
  assert.deepEqual([start.getHours(), start.getMinutes(), start.getSeconds()], [0, 0, 0]);
  assert.equal(today.to, now.toISOString());
  assert.equal(new Date(seven.from).getHours(), 0);
  assert.equal(
    Math.round((new Date(today.from) - new Date(seven.from)) / 86400000),
    6,
    '最近 7 天含今天共 7 个自然日'
  );
  assert.deepEqual(all, {});

  const url = CMLookup.buildDataURL(
    'https://host.test/v0/resource/plugins/credit-manager/lookup/data',
    { grain: 'day', range: '7', page: 1, pageSize: 10, model: 'm1', result: 'failed' },
    now
  );
  assert.equal(url.searchParams.get('model'), 'm1');
  assert.equal(url.searchParams.get('result'), 'failed');
  for (const forbidden of ['key', 'token', 'authorization', 'access_token']) {
    assert.equal(url.searchParams.get(forbidden), null, `URL 不得携带 ${forbidden}`);
  }
  assert.equal(CMLookup.buildDataURL('https://h.test/lookup/data', { grain: 'day', range: 'all', page: 1, pageSize: 10 }, now).toString().includes('Bearer'), false);
});

test('快照查询只带请求 id，不带密钥', () => {
  const url = CMLookup.buildCaptureURL('https://h.test/lookup/capture', 'req-001');
  assert.equal(url.searchParams.get('id'), 'req-001');
  assert.equal(url.toString().includes('Bearer'), false);
});

test('记住密钥：会话优先、本地兜底、两侧独立 try、校验源站', () => {
  const origin = 'https://host.test';
  function fakeStore(initial) {
    const data = Object.assign({}, initial || {});
    return {
      getItem: (k) => (k in data ? data[k] : null),
      setItem: (k, v) => { data[k] = String(v); },
      removeItem: (k) => { delete data[k]; },
      _data: data,
    };
  }

  // 会话优先于本地（同一浏览器里两个 tab 的语义不同）。
  let session = fakeStore({ [CMLookup.REMEMBER_KEY]: 'session-token', [CMLookup.REMEMBER_ORIGIN_KEY]: origin });
  let local = fakeStore({ [CMLookup.REMEMBER_KEY]: 'local-token', [CMLookup.REMEMBER_ORIGIN_KEY]: origin });
  assert.deepEqual(CMLookup.readStoredToken(session, local, origin), { token: 'session-token', remembered: false });
  assert.deepEqual(CMLookup.readStoredToken(fakeStore(), local, origin), { token: 'local-token', remembered: true });
  assert.deepEqual(
    CMLookup.readStoredToken(fakeStore({ [CMLookup.REMEMBER_KEY]: 'same' }), fakeStore({ [CMLookup.REMEMBER_KEY]: 'same' }), origin),
    { token: 'same', remembered: true },
    '两侧同一个凭据表示用户勾选过记住'
  );

  // 会话存储抛错时仍要读本地；本地抛错时仍要读会话。
  const throwing = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } };
  assert.deepEqual(
    CMLookup.readStoredToken(throwing, fakeStore({ [CMLookup.REMEMBER_KEY]: 'local-token' }), origin),
    { token: 'local-token', remembered: true },
    '会话存储不可用时必须继续读本地存储'
  );
  assert.deepEqual(
    CMLookup.readStoredToken(fakeStore({ [CMLookup.REMEMBER_KEY]: 'session-token' }), throwing, origin),
    { token: 'session-token', remembered: false },
    '本地存储不可用时仍要使用会话凭据'
  );

  // 源站不符的本地凭据不得使用。
  assert.deepEqual(CMLookup.readStoredToken(fakeStore(), fakeStore({ [CMLookup.REMEMBER_KEY]: 'x', [CMLookup.REMEMBER_ORIGIN_KEY]: 'https://other.test' }), origin), { token: '', remembered: false });
  assert.deepEqual(CMLookup.readStoredToken(fakeStore(), fakeStore(), origin), { token: '', remembered: false });

  // 勾选记住才落本地；未勾选时本地必须被清掉，只留会话。
  session = fakeStore(); local = fakeStore({ [CMLookup.REMEMBER_KEY]: 'old' });
  CMLookup.persistToken(session, local, origin, 'tk-1', false);
  assert.equal(session._data[CMLookup.REMEMBER_KEY], 'tk-1');
  assert.equal(local._data[CMLookup.REMEMBER_KEY], undefined);
  CMLookup.persistToken(session, local, origin, 'tk-2', true);
  assert.equal(local._data[CMLookup.REMEMBER_KEY], 'tk-2');
  assert.equal(local._data[CMLookup.REMEMBER_ORIGIN_KEY], origin);
  // 存储被禁用时不得抛错。
  assert.doesNotThrow(() => CMLookup.persistToken(throwing, throwing, origin, 'tk-3', true));
  assert.doesNotThrow(() => CMLookup.clearToken(throwing, throwing));

  // 退出/切换密钥必须把两侧凭据都清空。
  CMLookup.clearToken(session, local);
  assert.equal(session._data[CMLookup.REMEMBER_KEY], undefined);
  assert.equal(local._data[CMLookup.REMEMBER_KEY], undefined);
  assert.equal(local._data[CMLookup.REMEMBER_ORIGIN_KEY], undefined);
});

// --- 3. 展示映射 -----------------------------------------------------------

test('请求状态与模型 Token 限额按契约展示', () => {
  assert.equal(CMLookup.requestStatus({ outcome: 'failed', state: 'settled' }), '失败 · 已结算');
  assert.equal(CMLookup.requestStatus({ state: 'active' }), '进行中');
  assert.equal(CMLookup.requestStatus({}), '—');
  assert.equal(CMLookup.tokenLimitText(0), '不限');
  assert.equal(CMLookup.tokenLimitText({ cmIntText: '1000000' }), CMCharts.tokenText({ cmIntText: '1000000' }, 'raw'));
  assert.equal(CMLookup.executionTokens([{ tokens: { total: { cmIntText: BIG } } }, { tokens: { total: { cmIntText: BIG } } }]), '18014398509481986');
  assert.equal(CMLookup.executionCost([{ cost_micro_usd: { cmIntText: BIG } }, { cost_micro_usd: 1 }]), '9007199254740994');
});

test('响应体解析走无损解析，绝不使用会丢精度的 response.json', () => {
  const parsed = CMLookup.parseBody(`{"summary":{"cost_micro_usd":${BIG}}}`);
  assert.equal(parsed.summary.cost_micro_usd.cmIntText, BIG);
});

test('只调用共享模块真实导出的 API，且必需模块在页面内全部就位', () => {
  // 实际 charts 模块（不是测试替身）：越界调用会在页面上变成 undefined/异常。
  const used = [...new Set([...readAsset('lookup.js').matchAll(/charts\(\)\.([A-Za-z]+)/g)].map((m) => m[1]))];
  assert.ok(used.length >= 4, `页面应通过 charts() 调用共享图表 API：${used}`);
  for (const name of used) {
    assert.equal(typeof CMCharts[name], 'function', `共享图表模块必须导出 ${name}`);
  }
  const numbersUsed = [...new Set([...readAsset('lookup.js').matchAll(/numbers\(\)\.([A-Za-z]+)/g)].map((m) => m[1]))];
  for (const name of numbersUsed) {
    assert.equal(typeof CMNumbers[name], 'function', `共享无损数字模块必须导出 ${name}`);
  }
  // 页面依赖的格式化必须输出带一次货币后缀的精确文本（>2^53 不失真、不重复货币）。
  const money = CMCharts.formatMoneyMicro(BIG, 'USD', 7.2);
  assert.ok(money.endsWith(' USD'), `金额应带一次货币后缀：${money}`);
  assert.ok(!/USD\s+USD/.test(money), `金额不得重复货币：${money}`);
  assert.ok(money.includes('9007199254.740993'), `金额必须精确：${money}`);
  assert.equal(CMCharts.tokenText(BIG, 'raw'), '9,007,199,254,740,993');
  assert.notEqual(CMCharts.tokenText(BIG, 'k'), CMCharts.tokenText(BIG, 'raw'), '非原始单位必须改变展示文本');
  assert.equal(CMLookup.modulesReady(), true);
});

test('主题与计量偏好自带读取，跟随 CPA 键且容忍不可用存储', () => {
  function fakeStore(initial) {
    const data = Object.assign({}, initial || {});
    return { getItem: (k) => (k in data ? data[k] : null), setItem: (k, v) => { data[k] = String(v); }, _data: data };
  }
  const prefs = CMLookup.preferences;

  // CPA 同时存在纯字符串与 JSON 两种写法。
  assert.equal(prefs.readTheme(fakeStore({ 'cli-proxy-theme': 'dark' })), 'dark');
  assert.equal(prefs.readTheme(fakeStore({ 'cli-proxy-theme': '{"theme":"light"}' })), 'light');
  assert.equal(prefs.readTheme(fakeStore({ 'cli-proxy-theme': '{"state":{"theme":"white"}}' })), 'white');
  assert.equal(prefs.readTheme(fakeStore({ 'cli-proxy-theme': 'nonsense' })), 'auto');
  assert.equal(prefs.readTheme(fakeStore()), 'auto');
  assert.equal(prefs.resolvePalette('auto', true), 'dark');
  assert.equal(prefs.resolvePalette('auto', false), 'white');
  assert.equal(prefs.resolvePalette('light', true), 'light');

  const store = fakeStore({ credit_manager_token_unit: 'wan', credit_manager_currency: 'CNY', credit_manager_usd_cny_rate: '7.35' });
  assert.deepEqual(prefs.readUnits(store), { tokenUnit: 'wan', currency: 'CNY', currencyRate: 7.35 });
  assert.deepEqual(prefs.readUnits(fakeStore()), { tokenUnit: 'raw', currency: 'USD', currencyRate: 7.2 });
  prefs.writeUnits(store, { tokenUnit: 'k', currency: 'USD', currencyRate: 7.2 });
  assert.deepEqual(prefs.readUnits(store), { tokenUnit: 'k', currency: 'USD', currencyRate: 7.2 });
  const throwing = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
  assert.equal(prefs.readTheme(throwing), 'auto');
  assert.deepEqual(prefs.readUnits(throwing), { tokenUnit: 'raw', currency: 'USD', currencyRate: 7.2 });
  assert.doesNotThrow(() => prefs.writeUnits(throwing, { tokenUnit: 'k', currency: 'USD', currencyRate: 7.2 }));
});

// --- 4. 源码约束 -----------------------------------------------------------

const js = readAsset('lookup.js');
const html = readAsset('lookup.html');

test('趋势重绘不在共享渲染前清空容器，并复用共享图表与无损数字', () => {
  const start = js.indexOf('function renderTrend()');
  const cut = js.indexOf('charts().render(', start);
  assert.ok(start >= 0 && cut > start, 'renderTrend 必须调用共享渲染器');
  assert.equal(js.slice(start, cut).includes('clear('), false, '清空容器会让 ECharts 实例脱钩');
  assert.ok(js.includes('CMCharts') && js.includes('CMNumbers'));
  assert.equal(js.includes('response.json()'), false, 'response.json() 会丢失 64 位精度');
  assert.ok(js.includes('refreshTheme'), '主题切换后必须重新上色');
  // 共享模块是必需资源：不得保留自绘趋势图或退化数字分支。
  for (const gone of ['renderTrendFallback', 'fallbackBar', 'bar-fill', 'JSON.parse(text)']) {
    assert.equal(js.includes(gone), false, `必须删除降级实现 ${gone}`);
  }
  assert.ok(js.includes('页面资源加载不完整'), '缺少必需模块时必须给出可见错误');
  assert.ok(js.includes("el('submit').disabled = true"), '缺少必需模块时必须阻断提交');
});

test('展示偏好改变时整页重绘：额度/汇总/明细/趋势口径一致', () => {
  // 三个偏好控件都必须调用 renderAll()，而不是只重画图表。
  const handlers = ['token-unit', 'currency', 'rate'].map((id) => {
    const at = js.indexOf(`el('${id}').addEventListener('change'`);
    assert.ok(at >= 0, `缺少 ${id} 的偏好处理`);
    return js.slice(at, js.indexOf('});', at));
  });
  for (const body of handlers) {
    assert.ok(body.includes('renderAll()'), '偏好改变必须整页重绘');
  }
  const at = js.indexOf('function renderAll()');
  const body = js.slice(at, js.indexOf('function renderTrend()', at));
  for (const call of ['renderQuotas(', 'renderTokenLimits(', 'renderStats(', 'renderTrend()', 'renderRecent(']) {
    assert.ok(body.includes(call), `renderAll 必须重绘 ${call}`);
  }
  assert.ok(body.includes('state.payload'), '整页重绘必须基于保留的最新响应');
  assert.ok(js.slice(js.indexOf('function resetPrivateUI()'), js.indexOf('function dropSession(')).includes('state.payload = null'),
    '退出/切换密钥必须清掉保留的账务响应');
});

test('生命周期：切换密钥/401 会取消在途请求并清空私有数据', () => {
  assert.ok(js.includes('AbortController') && js.includes('abort()'));
  assert.ok(js.includes('epoch'), '迟到的响应必须按 epoch 丢弃');
  const drop = js.slice(js.indexOf('function dropSession('), js.indexOf('function dropSession(') + 900);
  for (const call of ['newEpoch()', 'clearToken(', 'resetPrivateUI()', 'showGate(']) {
    assert.ok(drop.includes(call), `dropSession 必须调用 ${call}`);
  }
  assert.ok(js.includes('response.status === 401'), '401 必须走统一的凭据清理路径');
  assert.ok(js.includes('disposeAll()'), '清理时必须释放图表实例');
});

test('页面只引用第一方相对资源，且不含内联凭据', () => {
  const refs = [...html.matchAll(/(?:href|src)="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(refs, ['./lookup.css', './vendor/echarts-5.6.0.min.js', './charts.js', './numbers.js', './lookup.js']);
  assert.equal(/https?:\/\//.test(html), false);
  assert.equal(/Bearer|localStorage|sessionStorage/.test(html), false);
  assert.ok(html.includes('id="token-limits"'), '必须展示后端返回的模型 Token 限额');
  assert.ok(html.includes('data-grain="month"') && html.includes('data-grain="hour"'));
});

test('lookup.js 模型分析与额度基线：无未定义函数，退出清理模型区与 Donut', () => {
  const code = readAsset('lookup.js');
  // 确保没有误用全局未定义变量
  assert.ok(!code.includes('formatMicroUSD(') || code.includes('function formatMicroUSD('), 'formatMicroUSD 必须有合法定义');
  assert.ok(!code.includes('formatTokens(') || code.includes('function formatTokens('), 'formatTokens 必须有合法定义');

  const resetFn = code.slice(code.indexOf('function resetPrivateUI()'), code.indexOf('function dropSession('));
  assert.ok(resetFn.includes('lookup-models-section'), 'resetPrivateUI 必须隐藏模型分析区域');
  assert.ok(resetFn.includes('lookup-model-share'), 'resetPrivateUI 必须清理模型分布图容器');
  assert.ok(resetFn.includes('lookup-model-rank'), 'resetPrivateUI 必须清空模型排行列表');

  const quotasFn = code.slice(code.indexOf('function renderQuotas('), code.indexOf('function renderTokenLimits('));
  assert.ok(quotasFn.includes('quota_used_micro_usd'), 'renderQuotas 必须支持周期生效配额字段');
  assert.ok(quotasFn.includes('累计消费') || quotasFn.includes('累计支出'), 'renderQuotas 必须明确标注累计消费');
});
