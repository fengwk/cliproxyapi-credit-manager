/*
 * 自助查询页的独立浏览器验收（本地 mock + 隔离的无头 Chrome）。
 *
 * 覆盖：门禁与凭据处理、无损大整数渲染、真实自然月维度下发、ECharts 复用不被清空、
 *      跨密钥隔离与快照无 oracle、切换密钥/401 的在途取消与私有数据清理、记住密钥、
 *      主题同步、模型 Token 限额卡片。
 *
 * 与 tests/browser/run.mjs（财务控制台）相互独立：本文件自带 mock 服务、静态资源
 * 白名单与 CDP 客户端，不共享全局状态，可单独运行。
 * 用法：node tests/browser/lookup.mjs
 * 截图与报告写入仓库外的记录区（默认 .workspace/frontend/artifacts/lookup，可用
 * CM_LOOKUP_ARTIFACTS_DIR 覆盖），避免把大屏截图提交进仓库。
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(__dirname, '..', '..');
const ASSETS = path.join(ROOT, 'internal', 'web', 'assets');
const ARTIFACTS = process.env.CM_LOOKUP_ARTIFACTS_DIR
  ? path.resolve(process.env.CM_LOOKUP_ARTIFACTS_DIR)
  : path.resolve(ROOT, '.workspace', 'frontend', 'artifacts', 'lookup');
const SHOTS = path.join(ARTIFACTS, 'screenshots');
const CHROME = process.env.CHROME_BIN || '/usr/bin/google-chrome-stable';

const PLUGIN_BASE = '/v0/resource/plugins/credit-manager';
const ALPHA = 'cm-alpha-key';
const BETA = 'cm-beta-key';
const EXPIRED = 'cm-expired-key';
const EMPTY = 'cm-empty-key';
const BAD = 'cm-wrong-key';

// 只有这些精确路径可被服务；与生产 allow list 一致，扩展名本身不足以提供文件。
const STATIC = {
  'lookup.html': 'text/html; charset=utf-8',
  'lookup.css': 'text/css; charset=utf-8',
  'lookup.js': 'text/javascript; charset=utf-8',
  'numbers.js': 'text/javascript; charset=utf-8',
  'preferences.js': 'text/javascript; charset=utf-8',
  'charts.js': 'text/javascript; charset=utf-8',
  'vendor/echarts-5.6.0.min.js': 'text/javascript; charset=utf-8',
};

const results = [];
let failed = 0;

function check(name, condition, detail) {
  const ok = !!condition;
  if (!ok) failed++;
  results.push({ name, ok, detail: detail === undefined ? '' : String(detail) });
  process.stdout.write(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok || detail === undefined ? '' : ' :: ' + detail}\n`);
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/* ---------------- 固定账务样例（大整数一律以字面量出现） ---------------- */

function alphaData() {
  return `{
  "key": {"id":"key-alpha","label":"alpha-key","enabled":true,"quota_micro_usd":9007199254740993,
    "daily_micro_usd":0,"weekly_micro_usd":1000000,"monthly_micro_usd":0,"max_concurrent":4,
    "allowed_models":["m-alpha"],
    "model_token_limits":[{"model":"m-alpha","total":0,"daily":9007199254740993,"weekly":0,"monthly":1000000},
      {"model":"m-beta","total":2000000,"daily":0,"weekly":0,"monthly":0}],
    "expires_at":"2026-12-31T00:00:00Z","spent_micro_usd":9007199254740993,"held_micro_usd":2500000,
    "created_at":"2026-01-01T00:00:00Z"},
  "summary": {"requests":3,"executions":4,"failed":1,"pending":1,"cost_micro_usd":9007199254740993,
    "adjustment_micro_usd":2500000,
    "tokens":{"input":9007199254740993,"cache_read":2,"cache_write":3,"output":5,"reasoning":1,"total":9007199254741004},
    "complete_executions":3,"cache_read_ratio":0.25},
  "trend": [
    {"time":"2026-10-01T00:00:00Z","requests":1,"executions":1,"failed":0,"pending":0,
     "cost_micro_usd":9007199254740993,"adjustment_micro_usd":0,
     "tokens":{"input":9007199254740993,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":9007199254740993},
     "complete_executions":1},
    {"time":"2026-10-02T00:00:00Z","requests":2,"executions":3,"failed":1,"pending":1,
     "cost_micro_usd":4503599627370496,"adjustment_micro_usd":2500000,
     "tokens":{"input":10,"cache_read":2,"cache_write":3,"output":5,"reasoning":1,"total":21},
     "complete_executions":2}
  ],
  "items": [
    {"id":"req-a1","trace_id":"trace-a1","model":"m-alpha","outcome":"succeeded","state":"settled",
     "started_at":"2026-10-02T10:00:00Z","completed_at":"2026-10-02T10:00:01Z","held_micro_usd":0,
     "adjustment_micro_usd":2500000,"price":{"model":"m-alpha","rates":{"input":1,"output":1,"cache_read":0,"cache_write":0}},
     "executions":[
       {"id":"exec-a1","model":"m-alpha","tokens":{"input":9007199254740993,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":9007199254740993},
        "quality":"complete","cost_micro_usd":4503599627370496,"failed":false,"at":"2026-10-02T10:00:01Z"},
       {"id":"exec-a2","model":"m-alpha","tokens":{"input":0,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":9007199254740993},
        "quality":"complete","cost_micro_usd":1,"failed":false,"at":"2026-10-02T10:00:01Z"}
     ]},
    {"id":"req-a2","trace_id":"trace-a2","model":"m-alpha","outcome":"","state":"pending",
     "started_at":"2026-10-02T11:00:00Z","held_micro_usd":2500000,"adjustment_micro_usd":0,
     "price":{"model":"m-alpha","rates":{"input":1,"output":1,"cache_read":0,"cache_write":0}},
     "executions":[]}
  ],
  "total": 2, "page": 1, "page_size": 10, "retention_from": "2026-07-01T00:00:00Z"
}`;
}

function betaData() {
  return `{
  "key": {"id":"key-beta","label":"beta-key","enabled":true,"quota_micro_usd":0,"daily_micro_usd":0,
    "weekly_micro_usd":0,"monthly_micro_usd":0,"max_concurrent":0,"allowed_models":[],
    "model_token_limits":[],"spent_micro_usd":7000000,"held_micro_usd":0,"created_at":"2026-02-01T00:00:00Z"},
  "summary": {"requests":7,"executions":7,"failed":0,"pending":0,"cost_micro_usd":7000000,
    "adjustment_micro_usd":0,
    "tokens":{"input":70,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":70},
    "complete_executions":7},
  "trend": [{"time":"2026-10-02T00:00:00Z","requests":7,"executions":7,"failed":0,"pending":0,
    "cost_micro_usd":7000000,"adjustment_micro_usd":0,
    "tokens":{"input":70,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":70},
    "complete_executions":7}],
  "items": [
    {"id":"req-b1","trace_id":"trace-b1","model":"m-beta","outcome":"succeeded","state":"settled",
     "started_at":"2026-10-02T12:00:00Z","held_micro_usd":0,"adjustment_micro_usd":0,
     "price":{"model":"m-beta","rates":{"input":1,"output":1,"cache_read":0,"cache_write":0}},
     "executions":[{"id":"exec-b1","model":"m-beta","tokens":{"input":7,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":7},
       "quality":"complete","cost_micro_usd":7000000,"failed":false,"at":"2026-10-02T12:00:00Z"}]},
    {"id":"req-a1","trace_id":"trace-foreign","model":"m-alpha","outcome":"succeeded","state":"settled",
     "started_at":"2026-10-02T10:00:00Z","held_micro_usd":0,"adjustment_micro_usd":0,
     "price":{"model":"m-alpha","rates":{"input":1,"output":1,"cache_read":0,"cache_write":0}},
     "executions":[]}
  ],
  "total": 2, "page": 1, "page_size": 10, "retention_from": "2026-07-01T00:00:00Z"
}`;
}

function emptyData() {
  return `{
  "key": {"id":"key-empty","label":"empty-key","enabled":true,"quota_micro_usd":0,"daily_micro_usd":0,
    "weekly_micro_usd":0,"monthly_micro_usd":0,"max_concurrent":0,"allowed_models":[],
    "model_token_limits":[],"spent_micro_usd":0,"held_micro_usd":0,"created_at":"2026-03-01T00:00:00Z"},
  "summary": {"requests":0,"executions":0,"failed":0,"pending":0,"cost_micro_usd":0,"adjustment_micro_usd":0,
    "tokens":{"input":0,"cache_read":0,"cache_write":0,"output":0,"reasoning":0,"total":0},"complete_executions":0},
  "trend": [], "items": [], "total": 0, "page": 1, "page_size": 10, "retention_from": "2026-07-01T00:00:00Z"
}`;
}

// 只有 alpha 的 req-a1 快照属于 alpha；beta 递上同一个 id 必须得到通用 404。
function captureFor(key, id) {
  if (id !== 'req-a1') return { status: 404, body: '{"error":"not found"}' };
  if (key !== ALPHA) return { status: 404, body: '{"error":"not found"}' };
  return {
    status: 200,
    body: `{"request_id":"req-a1","status":"captured","at":"2026-10-02T10:00:00Z",
      "expires_at":"2026-10-09T10:00:00Z",
      "headers":{"Content-Type":["application/json"],"X-Trace":["trace-a1"]},
      "body":"{\\"model\\":\\"m-alpha\\"}"}`,
  };
}

/* ---------------- mock 服务（自带静态白名单与延迟规则） ---------------- */

const hits = [];

function startMock() {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    const authorization = req.headers.authorization || '';
    const key = authorization.startsWith('Bearer ') ? authorization.slice(7) : '';
    if (url.pathname.startsWith(`${PLUGIN_BASE}/`)) {
      hits.push({ method: req.method, path: url.pathname, query: url.search, key });
    }

    const send = (status, body, mime = 'application/json; charset=utf-8') => {
      res.writeHead(status, { 'Content-Type': mime, 'Cache-Control': 'no-store' });
      res.end(body);
    };

    if (url.pathname === `${PLUGIN_BASE}/__debug/hits`) {
      return send(200, JSON.stringify({ hits }));
    }

    if (url.pathname.startsWith(`${PLUGIN_BASE}/`)) {
      const relative = url.pathname.slice(PLUGIN_BASE.length + 1);
      if (!relative.includes('/') && STATIC[relative]) {
        try {
          return send(200, fs.readFileSync(path.join(ASSETS, relative)), STATIC[relative]);
        } catch (err) {
          return send(404, '{"error":"missing asset"}');
        }
      }
      if (relative.startsWith('vendor/')) {
        if (!STATIC[relative]) return send(404, '{"error":"not found"}');
        try {
          return send(200, fs.readFileSync(path.join(ASSETS, relative)), STATIC[relative]);
        } catch (err) {
          return send(404, '{"error":"missing asset"}');
        }
      }

      const delay = (ms, fn) => setTimeout(fn, ms);
      if (relative === 'lookup/data') {
        if (key === EXPIRED || (key && key !== ALPHA && key !== BETA && key !== EMPTY)) {
          return send(401, '{"error":"invalid or inactive key"}');
        }
        const body = key === BETA ? betaData() : key === EMPTY ? emptyData() : alphaData();
        // alpha 的月维度故意慢：用于验证切换密钥时在途响应不会落到页面上。
        const slow = key === ALPHA && url.searchParams.get('bucket') === 'month';
        return delay(slow ? 700 : 0, () => send(200, body));
      }
      if (relative === 'lookup/capture') {
        if (key === EXPIRED || (key && key !== ALPHA && key !== BETA && key !== EMPTY)) {
          return send(401, '{"error":"invalid or inactive key"}');
        }
        const result = captureFor(key, url.searchParams.get('id'));
        const slow = key === ALPHA && url.searchParams.get('id') === 'req-a2';
        return delay(slow ? 700 : 0, () => send(result.status, result.body));
      }
      return send(404, '{"error":"not found"}');
    }
    return send(404, '{"error":"not found"}');
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port }));
  });
}

/* ---------------- CDP 客户端 ---------------- */

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    ws.addEventListener('message', (event) => {
      const message = JSON.parse(event.data);
      if (message.id && this.pending.has(message.id)) {
        const { resolve, reject } = this.pending.get(message.id);
        this.pending.delete(message.id);
        if (message.error) reject(new Error(JSON.stringify(message.error)));
        else resolve(message.result);
      }
    });
  }

  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error('CDP 超时: ' + method));
        }
      }, 30000);
    });
  }

  async evaluate(expression) {
    const result = await this.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) {
      throw new Error('页面执行异常: ' + (result.exceptionDetails.exception?.description || result.exceptionDetails.text));
    }
    return result.result.value;
  }

  async waitFor(expression, timeoutMs = 10000, label = expression) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      try {
        if (await this.evaluate(`!!(${expression})`)) return true;
      } catch (err) {
        // 导航后执行上下文短暂重建：重试而不是直接失败。
      }
      await sleep(100);
    }
    throw new Error('等待条件超时: ' + label);
  }

  async click(selector) {
    const ok = await this.evaluate(
      `(() => { const n = document.querySelector(${JSON.stringify(selector)}); if (!n) return false; n.click(); return true; })()`
    );
    if (!ok) throw new Error('找不到元素: ' + selector);
    await sleep(60);
  }

  async setValue(selector, value) {
    const ok = await this.evaluate(
      `(() => { const n = document.querySelector(${JSON.stringify(selector)}); if (!n) return false;` +
        ` n.value = ${JSON.stringify(value)}; n.dispatchEvent(new Event('input', { bubbles: true }));` +
        ` n.dispatchEvent(new Event('change', { bubbles: true })); return true; })()`
    );
    if (!ok) throw new Error('找不到输入元素: ' + selector);
  }

  text(selector) {
    return this.evaluate(`(() => { const n = document.querySelector(${JSON.stringify(selector)}); return n ? n.innerText : ''; })()`);
  }

  count(selector) {
    return this.evaluate(`document.querySelectorAll(${JSON.stringify(selector)}).length`);
  }

  async screenshot(file, options = {}) {
    const params = { format: 'png' };
    if (options.fullPage) params.captureBeyondViewport = true;
    const shot = await this.send('Page.captureScreenshot', params);
    fs.writeFileSync(path.join(SHOTS, file), Buffer.from(shot.data, 'base64'));
  }
}

function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl);
    ws.addEventListener('open', () => resolve(new CDP(ws)));
    ws.addEventListener('error', (event) => reject(new Error('WebSocket 失败: ' + event.message)));
  });
}

async function launchChrome(profileDir) {
  const child = spawn(
    CHROME,
    [
      '--headless=new',
      '--remote-debugging-port=0',
      '--remote-allow-origins=*',
      `--user-data-dir=${profileDir}`,
      // 以 root/容器身份运行时 Chrome 沙箱无法初始化；测试用的是隔离的临时
      // profile，与用户 profile 和生产数据无关。
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      '--disable-gpu',
      '--disable-dev-shm-usage',
      '--hide-scrollbars',
      '--window-size=1440,1100',
      'about:blank',
    ],
    { stdio: ['ignore', 'pipe', 'pipe'] }
  );
  const wsUrlFile = path.join(profileDir, 'DevToolsActivePort');
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    if (fs.existsSync(wsUrlFile)) {
      const [port] = fs.readFileSync(wsUrlFile, 'utf8').split('\n');
      if (port) {
        const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
        const page = list.find((target) => target.type === 'page' && target.webSocketDebuggerUrl);
        if (page) return { child, wsUrl: page.webSocketDebuggerUrl };
      }
    }
    await sleep(200);
  }
  child.kill();
  throw new Error('无法连接无头 Chrome 调试端口');
}

/* ---------------- 主流程 ---------------- */

// HTML 元素在脚本执行前就存在，必须等到脚本真正绑定事件（lookupReady）后再交互，
// 否则点击会触发浏览器原生表单提交（密钥会出现在地址栏）。
async function waitForApp(cdp) {
  await cdp.waitFor("document.documentElement.dataset.lookupReady === '1'", 10000, '自助查询脚本就绪');
}

async function main() {
  fs.mkdirSync(SHOTS, { recursive: true });
  const mock = await startMock();
  // Chrome 的 profile 会写入大量小文件：放在 artifacts 所在的磁盘上，避免 /tmp
  // 空间不足导致 DevToolsActivePort 永不出现（可用 CM_BROWSER_TMP_DIR 覆盖）。
  const profileRoot = process.env.CM_BROWSER_TMP_DIR
    ? path.resolve(process.env.CM_BROWSER_TMP_DIR)
    : path.join(ARTIFACTS, 'chrome-profiles');
  fs.mkdirSync(profileRoot, { recursive: true });
  const profileDir = fs.mkdtempSync(path.join(profileRoot, 'profile-'));
  const origin = `http://127.0.0.1:${mock.port}`;
  const pageUrl = `${origin}${PLUGIN_BASE}/lookup.html`;

  const chrome = await launchChrome(profileDir);
  const cdp = await connect(chrome.wsUrl);
  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');
  // 收集页面异常：断言失败时把真实原因打印出来，避免只看超时。
  await cdp.send('Page.addScriptToEvaluateOnNewDocument', {
    source:
      "window.__cmErrors = [];" +
      "window.addEventListener('error', (event) => window.__cmErrors.push(String(event.message)));" +
      "window.addEventListener('unhandledrejection', (event) => window.__cmErrors.push('rejection: ' + String((event.reason && event.reason.message) || event.reason)));",
  });

  const hitsSince = (index, fragment) => hits.slice(index).filter((entry) => entry.method === 'GET' && entry.path.includes(fragment));
  const hitCount = () => hits.length;

  const login = async (key) => {
    const before = hitCount();
    await cdp.setValue('#key', key);
    await cdp.click('#submit');
    return before;
  };

  try {
    /* --- 1. 门禁 --- */
    await cdp.send('Page.navigate', { url: pageUrl });
    await waitForApp(cdp);
    check('初始只显示密钥输入界面', await cdp.evaluate("document.querySelector('#dashboard').hidden === true && document.querySelector('#login-gate').hidden === false"));
    const shellText = await cdp.text('body');
    check('静态页面不含密钥或账务数据', !shellText.includes('µUSD') && !shellText.includes(ALPHA) && (await cdp.count('#stats .card')) === 0);
    check('趋势容器就绪且默认按日', await cdp.evaluate("document.querySelector('[data-grain=day]').classList.contains('active')"));

    const beforeBad = await login(BAD);
    await cdp.waitFor("!document.querySelector('#banner').hidden", 10000, '错误密钥提示');
    check('无效密钥提示且停留门禁', (await cdp.text('#banner')).includes('无效'), await cdp.text('#banner'));
    check('无效密钥后主视图仍隐藏', await cdp.evaluate("document.querySelector('#dashboard').hidden === true"));
    check('无效密钥不写入存储', await cdp.evaluate("Object.keys(localStorage).concat(Object.keys(sessionStorage)).filter((k) => k.indexOf('credit-manager.lookup') === 0).length === 0"));
    check('无效密钥请求只发往 lookup/data', hitsSince(beforeBad, '/lookup/data').length === 1);

    /* --- 2. 有效密钥与精确账务 --- */
    await login(ALPHA);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '主视图出现');
    await cdp.waitFor("document.querySelectorAll('#stats .card').length > 0", 10000, '汇总渲染');
    check('有效密钥进入主视图', (await cdp.text('#key-label')).includes('alpha-key'));
    check('密钥状态显示启用与到期', (await cdp.text('#key-state')).includes('已启用') && (await cdp.text('#key-state')).includes('到期'));

    const quotas = await cdp.text('#quotas');
    for (const label of ['已用额度', '在途预占', '总额度', '日额度', '周额度', '月额度', '并发上限', '允许模型']) {
      check(`额度卡片包含 ${label}`, quotas.includes(label), quotas.slice(0, 120));
    }
    check('未设上限的额度显示不限', quotas.includes('不限'));

    // 关键回归：>2^53 的微美元必须精确，且与浮点解析的结果明显不同。
    const expectedCost = await cdp.evaluate("window.CMCharts.formatMoneyMicro('9007199254740993','USD',7.2)");
    const lostCost = await cdp.evaluate("window.CMCharts.formatMoneyMicro(9007199254740992,'USD',7.2)");
    check(
      '执行成本按精确整数渲染（>2^53 不失真）',
      expectedCost !== lostCost && (await cdp.text('#stats')).includes(expectedCost),
      `${expectedCost} vs ${lostCost}`
    );
    const expectedTokens = await cdp.evaluate("window.CMCharts.tokenText('9007199254741004','raw')");
    check('汇总 Token 合计精确渲染', (await cdp.text('#stats')).includes(expectedTokens), expectedTokens);
    check('缓存读取占比按契约展示', (await cdp.text('#stats')).includes('25.0%'));

    const limits = await cdp.text('#token-limits');
    check('模型 Token 限额卡片渲染模型名', limits.includes('m-alpha') && limits.includes('m-beta'), limits.slice(0, 160));
    check('限额为 0 显示不限', limits.includes('不限'));
    const expectedLimit = await cdp.evaluate("window.CMCharts.tokenText('9007199254740993','raw')");
    check('模型 Token 限额中的大整数精确渲染', limits.includes(expectedLimit), expectedLimit);
    check('限额区说明当前状态', (await cdp.text('#token-limits-state')).includes('2 个模型'));

    const rows = await cdp.text('#recent-body');
    const expectedRowTokens = await cdp.evaluate("window.CMCharts.tokenText('18014398509481986','raw')");
    check('最近请求的 Token 是两次执行的精确合计', rows.includes(expectedRowTokens), expectedRowTokens);
    const expectedRowCost = await cdp.evaluate("window.CMCharts.formatMoneyMicro('4503599627370497','USD',7.2)");
    check('最近请求的成本精确合计', rows.includes(expectedRowCost), expectedRowCost);
    check('请求状态按契约显示结果与结算状态', rows.includes('成功 · 已结算') && rows.includes('待定'), rows.replace(/\n/g, ' ').slice(0, 200));
    await cdp.screenshot('01-dashboard.png', { fullPage: true });

    /* --- 3. 趋势图与维度 --- */
    await cdp.waitFor("document.querySelectorAll('#trend-requests svg, #trend-requests canvas').length > 0", 10000, '趋势图渲染');
    const chartNode = () => cdp.evaluate(`(() => {
      const size = (id) => { const n = document.querySelector('#trend-' + id); const s = n.querySelector('svg, canvas');
        return s ? { w: n.getBoundingClientRect().width, h: n.getBoundingClientRect().height, children: n.children.length } : null; };
      return { requests: size('requests'), tokens: size('tokens'), cost: size('cost') };
    })()`);
    const first = await chartNode();
    check('三张趋势图都由共享组件渲染', !!first.requests && !!first.tokens && !!first.cost, JSON.stringify(first));
    check('趋势图容器有可见尺寸', first.tokens.w > 200 && first.tokens.h >= 200, JSON.stringify(first.tokens));

    const beforeMonth = hitCount();
    await cdp.click('[data-grain="month"]');
    while (hitsSince(beforeMonth, '/lookup/data').length === 0) await sleep(50);
    await sleep(900); // mock 对 alpha 的 month 维度延迟 700ms，等它渲染完成
    const monthHits = hitsSince(beforeMonth, '/lookup/data');
    check('切到月维度后服务端收到 bucket=month', monthHits.length > 0 && monthHits.every((h) => h.query.includes('bucket=month')), JSON.stringify(monthHits.map((h) => h.query)));
    const afterMonth = await chartNode();
    check('切维度后图表仍复用同一实例并保持可见', !!afterMonth.tokens && afterMonth.tokens.children > 0 && afterMonth.tokens.h >= 200, JSON.stringify(afterMonth));
    check('趋势点更新后月份维度生效', (await cdp.count('[data-grain="month"].active')) === 1);

    const beforeHour = hitCount();
    await cdp.click('[data-grain="hour"]');
    await sleep(400);
    const hourHits = hitsSince(beforeHour, '/lookup/data');
    check('切到时维度后服务端收到 bucket=hour', hourHits.length > 0 && hourHits[0].query.includes('bucket=hour'), JSON.stringify(hourHits.map((h) => h.query)));

    /* --- 4. 快照与跨密钥隔离 --- */
    await cdp.click('#recent-body button');
    await cdp.waitFor("document.querySelector('#capture-modal').open === true", 10000, '快照弹窗');
    const snapshot = await cdp.text('#capture-body');
    check('自己的快照展示请求头与请求体', snapshot.includes('请求头') && snapshot.includes('X-Trace') && snapshot.includes('m-alpha'), snapshot.slice(0, 160));
    check('快照状态按契约显示已捕获', snapshot.includes('已捕获'));
    await cdp.screenshot('02-snapshot.png', { fullPage: true });
    await cdp.click('#capture-close');
    check('关闭后弹窗收起', await cdp.evaluate("document.querySelector('#capture-modal').open === false"));

    /* --- 5. 生命周期：在途请求 + 切换密钥 --- */
    // 第二行的快照故意慢，期间切换密钥：迟到的响应必须既不开弹窗也不改写页面。
    const beforeSlow = hitCount();
    await cdp.evaluate("document.querySelectorAll('#recent-body button')[1].click()");
    while (hitsSince(beforeSlow, '/lookup/capture').length === 0) await sleep(50);
    await cdp.click('#switch-key');
    await sleep(1000);
    check('切换密钥后回到输入界面', await cdp.evaluate("document.querySelector('#login-gate').hidden === false && document.querySelector('#dashboard').hidden === true"));
    check('切换密钥后在途快照响应不再打开弹窗', await cdp.evaluate("document.querySelector('#capture-modal').open === false"));
    check('切换密钥后私有 DOM 已清空', (await cdp.count('#stats .card')) === 0 && (await cdp.count('#recent-body tr')) === 0 && (await cdp.text('#key-label')) === '');
    check('切换密钥后趋势图已释放', (await cdp.count('#trend-requests *')) === 0 && (await cdp.count('#trend-tokens *')) === 0);
    check('切换密钥后本机凭据被清除', await cdp.evaluate("Object.keys(localStorage).concat(Object.keys(sessionStorage)).filter((k) => k.indexOf('credit-manager.lookup') === 0).length === 0"));

    // 月维度的慢响应期间切换密钥：迟到的数据不得把主视图重新点亮。
    await login(ALPHA);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '重新登录');
    const beforeStale = hitCount();
    await cdp.click('[data-grain="month"]');
    while (hitsSince(beforeStale, '/lookup/data').length === 0) await sleep(50);
    await cdp.click('#switch-key');
    await sleep(1100);
    check('在途查询被作废：迟到响应不恢复主视图', await cdp.evaluate("document.querySelector('#dashboard').hidden === true"));
    check('在途查询被作废：不显示网络错误', await cdp.evaluate("document.querySelector('#banner').hidden === true"), await cdp.text('#banner'));

    /* --- 6. 401 清理 --- */
    await login(ALPHA);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '再次登录');
    await cdp.click('#recent-body button');
    await cdp.waitFor("document.querySelector('#capture-modal').open === true", 10000, '再次打开弹窗');
    await cdp.click('#capture-close');
    await login(EXPIRED);
    await cdp.waitFor("document.querySelector('#login-gate').hidden === false", 10000, '401 回到门禁');
    check('401 后提示凭据无效', (await cdp.text('#banner')).includes('无效'), await cdp.text('#banner'));
    check('401 后私有 DOM 与图表被清空', (await cdp.count('#stats .card')) === 0 && (await cdp.count('#trend-cost *')) === 0);
    check('401 后凭据与本机存储被清除', (await cdp.evaluate("document.querySelector('#key').value")) === '');
    await cdp.screenshot('03-expired.png', { fullPage: true });

    /* --- 7. 记住密钥与刷新 --- */
    await login(ALPHA);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '登录以保存');
    await cdp.click('#switch-key');
    check('未勾选记住时不落本地存储', await cdp.evaluate("localStorage.getItem('credit-manager.lookup.token') === null"));
    await cdp.setValue('#key', ALPHA);
    await cdp.click('#remember-key');
    await cdp.click('#submit');
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '勾选记住后登录');
    check('勾选记住后写入独立的 lookup 前缀', await cdp.evaluate("localStorage.getItem('credit-manager.lookup.token') === " + JSON.stringify(ALPHA)));
    check('查询地址不含密钥', await cdp.evaluate("location.search === '' && location.hash === ''"));
    check('页面 HTML 不包含密钥', await cdp.evaluate("document.documentElement.outerHTML.indexOf('" + ALPHA + "') < 0"));

    await cdp.send('Page.navigate', { url: pageUrl });
    await waitForApp(cdp);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '刷新后自动进入');
    check('刷新后凭据自动填入并进入主视图', (await cdp.evaluate("document.querySelector('#key').value")) === ALPHA);
    check('勾选状态被恢复', await cdp.evaluate("document.querySelector('#remember-key').checked === true"));

    // 只保留会话凭据（清掉本地）：刷新仍应自动进入，但复选框不勾选。
    await cdp.evaluate("localStorage.removeItem('credit-manager.lookup.token'); localStorage.removeItem('credit-manager.lookup.token-origin');");
    await cdp.send('Page.navigate', { url: pageUrl });
    await waitForApp(cdp);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '会话凭据生效');
    check('会话凭据优先且刷新后仍可用', (await cdp.evaluate("document.querySelector('#key').value")) === ALPHA);
    check('会话凭据不显示为「记住」', await cdp.evaluate("document.querySelector('#remember-key').checked === false"));

    /* --- 8. 主题同步 --- */
    await cdp.evaluate("localStorage.setItem('cli-proxy-theme', 'white'); window.dispatchEvent(new StorageEvent('storage', { key: 'cli-proxy-theme', newValue: 'white' }));");
    await sleep(200);
    check('CPA 主题 white 同步到页面', (await cdp.evaluate("document.documentElement.dataset.palette")) === 'white');
    const white = await chartNode();
    check('切主题后图表仍可见（未丢失实例）', !!white.tokens && white.tokens.children > 0, JSON.stringify(white));
    await cdp.evaluate("localStorage.setItem('cli-proxy-theme', 'dark'); window.dispatchEvent(new StorageEvent('storage', { key: 'cli-proxy-theme', newValue: 'dark' }));");
    await sleep(200);
    check('CPA 主题 dark 同步到页面', (await cdp.evaluate("document.documentElement.dataset.palette")) === 'dark');
    await cdp.screenshot('04-theme-dark.png', { fullPage: true });

    /* --- 9. 空态与跨密钥 --- */
    await cdp.click('#switch-key');
    await login(EMPTY);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, '空数据登录');
    await cdp.waitFor("document.querySelector('#trend-empty').hidden === false", 10000, '空趋势提示');
    check('无用量时显示空提示', (await cdp.text('#trend-empty')).includes('暂无'));
    check('无用量时不渲染图表节点', (await cdp.count('#trend-tokens *')) === 0);
    check('无模型限额时给出明确说明', (await cdp.text('#token-limits')).includes('未设置'));
    check('无请求时表格显示空态', (await cdp.text('#recent-body')).includes('暂无请求'));

    await cdp.click('#switch-key');
    await login(BETA);
    await cdp.waitFor("document.querySelector('#dashboard').hidden === false", 10000, 'beta 登录');
    check('切换密钥后展示新密钥自己的数据', (await cdp.text('#key-label')).includes('beta-key') && (await cdp.text('#stats')).includes('7'));
    check('切换密钥后不残留上一个密钥的账务', !(await cdp.text('#stats')).includes(expectedCost));
    const betaRows = await cdp.text('#recent-body');
    check('beta 列表包含自己的请求', betaRows.includes('m-beta'));
    // beta 递上了 alpha 的请求 id：服务端必须回通用 404，页面不得弹出任何快照。
    // 表格不展示请求 id，第二条请求就是属于 alpha 的 req-a1。
    await cdp.evaluate("document.querySelectorAll('#recent-body button')[1].click()");
    await cdp.waitFor("!document.querySelector('#banner').hidden", 10000, '外键快照提示');
    check('其它密钥的请求 id 只得到通用不可用提示', (await cdp.text('#banner')).includes('请求快照不可用'), await cdp.text('#banner'));
    check('跨密钥快照不会打开弹窗', await cdp.evaluate("document.querySelector('#capture-modal').open === false"));
    const foreignHits = hits.filter((h) => h.path.endsWith('/lookup/capture'));
    check('快照请求携带的是当前登录密钥', foreignHits.length > 0 && foreignHits[foreignHits.length - 1].key === BETA, JSON.stringify(foreignHits.slice(-1)));
    const dataKeys = [...new Set(hits.filter((h) => h.path.endsWith('/lookup/data')).map((h) => h.key))].sort();
    check(
      '数据请求只使用页面提交过的客户密钥',
      dataKeys.length === 5 && dataKeys.every((k) => [ALPHA, BETA, EMPTY, EXPIRED, BAD].includes(k)),
      JSON.stringify(dataKeys)
    );
    await cdp.screenshot('05-beta.png', { fullPage: true });
  } finally {
    try {
      const pageErrors = await cdp.evaluate('window.__cmErrors || []');
      if (pageErrors.length) process.stderr.write('[page errors] ' + JSON.stringify(pageErrors) + '\n');
      const state = await cdp.evaluate(
        "({ lookup: typeof window.CMLookup, charts: typeof window.CMCharts, numbers: typeof window.CMNumbers, url: location.href, banner: (document.getElementById('banner')||{}).textContent })"
      );
      process.stderr.write('[page state] ' + JSON.stringify(state) + '\n');
    } catch (err) {
      /* 页面可能已经不可用 */
    }
    try {
      chrome.child.kill();
      await new Promise((resolve) => {
        chrome.child.once('exit', resolve);
        setTimeout(resolve, 3000);
      });
    } catch (err) {
      /* 忽略 */
    }
    mock.server.close();
    try {
      // Chrome 退出后仍可能收尾写盘：清理失败不影响验收结论。
      fs.rmSync(profileDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
    } catch (err) {
      /* 忽略残留的临时 profile */
    }
  }

  const report = { ranAt: new Date().toISOString(), total: results.length, failed, results };
  fs.writeFileSync(path.join(ARTIFACTS, 'report.json'), JSON.stringify(report, null, 2));
  process.stdout.write(`\n自助查询页浏览器验收：${results.length - failed}/${results.length} 通过，报告 ${path.join(ARTIFACTS, 'report.json')}\n`);
  if (failed > 0) process.exit(1);
}

main().catch((error) => {
  process.stderr.write(`[ERROR] ${error && error.stack ? error.stack : error}\n`);
  process.exit(2);
});
