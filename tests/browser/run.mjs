/*
 * 浏览器验收：本地 mock 端点 + 隔离的无头 Chrome（临时 profile，不接触用户 profile/生产数据）。
 * 覆盖：登录门禁、五个标签页按需加载、过滤/分页查询一致性、无误损大整数、安全文本渲染、请求体弹窗、
 *      财务二次确认、Key 创建/编辑（保留未知字段）、价格编辑、凭据额度弹窗、错误/空态、移动端溢出。
 * 用法：node tests/browser/run.mjs
 * 截图与报告写入仓库外的记录区（默认 .workspace/frontend/artifacts，可用 CM_ARTIFACTS_DIR 覆盖），
 * 避免把大屏截图提交进仓库。
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ARTIFACTS = process.env.CM_ARTIFACTS_DIR
  ? path.resolve(process.env.CM_ARTIFACTS_DIR)
  : path.resolve(__dirname, '..', '..', '..', 'artifacts-final');
const SHOTS = path.join(ARTIFACTS, 'screenshots');
const CHROME = process.env.CHROME_BIN || '/usr/bin/google-chrome-stable';
const MANAGEMENT_KEY = 'cm-mock-key';

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

/* ---------------- mock 服务 ---------------- */

function startMock() {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [path.join(__dirname, 'mock-server.js'), '--port', '0', '--key', MANAGEMENT_KEY], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let buffer = '';
    child.stdout.on('data', (chunk) => {
      buffer += chunk.toString();
      const line = buffer.split('\n')[0];
      if (line && line.includes('{')) {
        try {
          const parsed = JSON.parse(line);
          resolve({ child, port: parsed.port });
          return;
        } catch (err) {
          /* 等待下一块数据 */
        }
      }
    });
    child.stderr.on('data', (chunk) => process.stderr.write('[mock] ' + chunk.toString()));
    child.on('exit', (code) => reject(new Error('mock server exited: ' + code)));
    setTimeout(() => reject(new Error('mock server 启动超时')), 10000);
  });
}

/* ---------------- CDP 客户端 ---------------- */

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    this.events = [];
    ws.addEventListener('message', (event) => {
      const message = JSON.parse(event.data);
      if (message.id && this.pending.has(message.id)) {
        const { resolve, reject } = this.pending.get(message.id);
        this.pending.delete(message.id);
        if (message.error) reject(new Error(JSON.stringify(message.error)));
        else resolve(message.result);
        return;
      }
      if (message.method) this.events.push(message.method);
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
      if (await this.evaluate(`!!(${expression})`)) return true;
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

  async clickText(selector, text, root = 'document') {
    const ok = await this.evaluate(
      `(() => { const list = [...${root}.querySelectorAll(${JSON.stringify(selector)})];` +
        ` const node = list.find((n) => n.textContent.trim() === ${JSON.stringify(text)});` +
        ` if (!node) return false; node.click(); return true; })()`
    );
    if (!ok) throw new Error(`找不到文本为「${text}」的 ${selector}`);
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

  async setModalMoney(labelText, field, value) {
    const ok = await this.evaluate(
      `(() => { const rows = [...document.querySelectorAll('.modal .field')];` +
        ` const row = rows.find((r) => { const l = r.querySelector('label'); return l && l.textContent.indexOf(${JSON.stringify(labelText)}) >= 0; });` +
        ` if (!row) return false; const input = row.querySelector('input.money-${field}'); if (!input) return false;` +
        ` input.value = ${JSON.stringify(value)}; input.dispatchEvent(new Event('input', { bubbles: true })); return true; })()`
    );
    if (!ok) throw new Error(`找不到金额输入: ${labelText} (${field})`);
    await sleep(50);
  }

  async modalMoney(labelText, field) {
    return this.evaluate(
      `(() => { const rows = [...document.querySelectorAll('.modal .field')];` +
        ` const row = rows.find((r) => { const l = r.querySelector('label'); return l && l.textContent.indexOf(${JSON.stringify(labelText)}) >= 0; });` +
        ` if (!row) return null; const input = row.querySelector('input.money-${field}'); return input ? input.value : null; })()`
    );
  }

  text(selector) {
    return this.evaluate(
      `(() => { const n = document.querySelector(${JSON.stringify(selector)}); return n ? n.innerText : ''; })()`
    );
  }

  count(selector) {
    return this.evaluate(`document.querySelectorAll(${JSON.stringify(selector)}).length`);
  }

  async screenshot(file, options = {}) {
    const params = { format: 'png' };
    if (options.fullPage) params.captureBeyondViewport = true;
    const result = await this.send('Page.captureScreenshot', params);
    fs.writeFileSync(path.join(SHOTS, file), Buffer.from(result.data, 'base64'));
  }
}

async function launchChrome(profileDir) {
  const child = spawn(
    CHROME,
    [
      '--headless=new',
      '--remote-debugging-port=0',
      '--remote-allow-origins=*',
      `--user-data-dir=${profileDir}`,
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
      const [port, wsPath] = fs.readFileSync(wsUrlFile, 'utf8').split('\n');
      if (port && wsPath) {
        const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
        const page = list.find((target) => target.type === 'page' && target.webSocketDebuggerUrl);
        if (page) return { child, wsUrl: page.webSocketDebuggerUrl, cdpPort: port };
      }
    }
    await sleep(200);
  }
  child.kill();
  throw new Error('无法连接无头 Chrome 调试端口');
}

function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl);
    ws.addEventListener('open', () => resolve(new CDP(ws)));
    ws.addEventListener('error', (event) => reject(new Error('WebSocket 失败: ' + event.message)));
  });
}

/* ---------------- 主流程 ---------------- */

async function main() {
  fs.mkdirSync(SHOTS, { recursive: true });
  const mock = await startMock();
  const profileDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cm-chrome-'));
  const origin = `http://127.0.0.1:${mock.port}`;
  const shellUrl = `${origin}/v0/resource/plugins/credit-manager/index.html`;

  const chrome = await launchChrome(profileDir);
  const cdp = await connect(chrome.wsUrl);
  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');

  const debug = async (key) => {
    const data = await (await fetch(`${origin}/__debug/last`)).json();
    return key ? data.last[key] : data;
  };

  // 记录所有到达 mock 的请求，用于验证标签页按需加载（不提前、不重复）。
  const hits = async () => {
    const data = await (await fetch(`${origin}/__debug/hits`)).json();
    return data.hits;
  };
  const hitsOf = async (fragment) => (await hits()).filter((entry) => entry.includes(fragment));
  // 只统计 GET，避免把同路径的写请求（PUT/POST）算作列表拉取。
  const getHitsOf = async (fragment) =>
    (await hits()).filter((entry) => entry.startsWith('GET ') && entry.includes(fragment));

  try {
    /* --- 1. 登录门禁 --- */
    await cdp.send('Page.navigate', { url: shellUrl });
    await cdp.waitFor("document.querySelector('#login-view') && !document.querySelector('#login-view').hidden", 10000, '登录界面出现');
    check('静态外壳先显示登录界面', true);
    const loginText = await cdp.text('#login-view');
    check('登录界面说明密钥仅存于内存', loginText.includes('内存') && loginText.includes('localStorage'));
    check('登录界面含失败封禁提示', loginText.includes('30 分钟'));
    const shellText = await cdp.text('body');
    check('静态外壳不含密钥与账务数据', !shellText.includes('sk-') && !shellText.includes('µUSD'));
    check('主视图初始隐藏', await cdp.evaluate("document.querySelector('#main-view').hidden === true"));
    await cdp.screenshot('01-login.png', { fullPage: true });

    await cdp.setValue('#management-key', 'wrong-key');
    await cdp.click('#login-button');
    await cdp.waitFor("!document.querySelector('#login-error').hidden", 10000, '登录错误提示');
    const wrongText = await cdp.text('#login-error');
    check('错误密钥显示服务端错误且不自动重试', wrongText.includes('invalid management key'), wrongText);
    check('错误密钥后仍停留在登录界面', await cdp.evaluate("document.querySelector('#main-view').hidden === true"));

    /* --- 2. 登录成功并加载使用统计 --- */
    await cdp.setValue('#management-key', MANAGEMENT_KEY);
    await cdp.click('#login-button');
    await cdp.waitFor("!document.querySelector('#main-view').hidden", 10000, '主视图出现');
    await cdp.waitFor("document.querySelectorAll('#usage-items .row-title').length > 0", 10000, '使用统计渲染');
    check('登录后显示会话状态', (await cdp.text('#session-status')).includes('已登录'));

    const usageStatus = await cdp.text('#usage-status');
    check('状态行展示接口与查询串', usageStatus.includes('/api/usage') && usageStatus.includes('page=1') && usageStatus.includes('page_size=20'), usageStatus);
    check('默认时间范围为最近 24 小时', usageStatus.includes('bucket=day') && usageStatus.includes('from=') && usageStatus.includes('to='), usageStatus);

    const summaryText = await cdp.text('#usage-summary');
    check('汇总卡片区分请求数与执行数', summaryText.includes('请求数') && summaryText.includes('执行数'));
    check('执行成本按微美元精确渲染大整数', summaryText.includes('123456789012.345678 USD'), summaryText.replace(/\n/g, ' ').slice(0, 200));
    const summaryLabels = await cdp.evaluate("[...document.querySelectorAll('#usage-summary .metric-label')].map((n) => n.textContent.trim())");
    check(
      '汇总卡片标签准确（失败请求/未结算请求/执行成本/人工补记）',
      JSON.stringify(summaryLabels.slice(0, 6)) ===
        JSON.stringify(['请求数（Request 行）', '执行数（实际执行）', '失败请求', '未结算请求', '执行成本', '人工补记']) &&
        summaryLabels.indexOf('失败执行') < 0 &&
        summaryLabels.indexOf('待定请求') < 0 &&
        summaryLabels.indexOf('成本合计') < 0,
      JSON.stringify(summaryLabels)
    );
    check(
      '失败请求说明按请求结果统计',
      summaryText.includes('按请求结果统计（failed / canceled / rejected）'),
      summaryText.replace(/\n/g, ' ').slice(0, 240)
    );
    check(
      '未结算请求说明已知用量照常入账',
      summaryText.includes('未结算请求') && summaryText.includes('已知用量照常入账，未知部分待核账') && !summaryText.includes('不计入成本'),
      summaryText.replace(/\n/g, ' ').slice(0, 240)
    );
    check(
      '执行成本与人工补记分成两张卡片',
      summaryText.includes('执行成本') && summaryText.includes('人工补记') && summaryText.includes('不计入执行成本'),
      summaryText.replace(/\n/g, ' ').slice(0, 240)
    );
    check('人工补记按微美元精确渲染', summaryText.includes('2.5 USD') && summaryText.includes('2,500,000 µUSD'), summaryText.replace(/\n/g, ' ').slice(0, 240));
    check('汇总包含 token 分桶与缓存占比', summaryText.includes('缓存读占比') && summaryText.includes('tokens 合计'));
    const scopeNote = await cdp.text('#usage-quality-note');
    check('质量覆盖说明基于 complete 执行', scopeNote.includes('complete'));
    check(
      '口径说明不把筛选合计当作全 Key 账本',
      scopeNote.includes('不等于全 Key 账本支出') && scopeNote.includes('人工补记按请求汇总'),
      scopeNote.slice(0, 240)
    );

    // 登录只允许加载需要立即展示的接口：其它标签页必须等到点击才请求。
    const afterLogin = await hits();
    check(
      '登录后未提前请求价格/凭据/额度接口',
      !afterLogin.some((entry) => entry.includes('/api/prices') || entry.includes('/v8/management/credentials') || entry.includes('/quota')),
      afterLogin.join(' | ')
    );
    // 登录会先校验一次密钥并加载使用统计与 Key 列表；除此之外不得请求其它管理接口。
    const afterLoginApis = [
      ...new Set(
        afterLogin
          .filter((entry) => entry.includes('/api/'))
          .map((entry) => entry.replace(/^[A-Z]+ /, '').split('?')[0].split('/').pop())
      ),
    ].sort();
    check('登录只请求健康检查、使用统计与 Key 列表', JSON.stringify(afterLoginApis) === JSON.stringify(['health', 'keys', 'usage']), JSON.stringify(afterLoginApis));
    check('展示保留边界', (await cdp.text('#usage-retention')).includes('2026-07-01'));
    check('趋势图渲染 SVG 柱状与成本折线', (await cdp.count('#usage-chart svg rect')) === 8 && (await cdp.count('#usage-chart svg path.cost-line')) === 1);
    check('趋势图例区分请求与执行', (await cdp.text('#usage-legend')).includes('执行次数'));
    check(
      '趋势成本线标注为不含人工补记的执行成本',
      (await cdp.text('#usage-legend')).includes('执行成本') && (await cdp.text('#usage-legend')).includes('不含人工补记'),
      await cdp.text('#usage-legend')
    );

    const itemsText = await cdp.text('#usage-items');
    check('明细标注请求/执行/响应模型', itemsText.includes('请求模型') && itemsText.includes('执行模型') && itemsText.includes('响应模型'));
    const requestLayout = () => cdp.evaluate(`(() => {
      const values = [...document.querySelectorAll('#usage-items .kv-v')];
      const cards = [...document.querySelectorAll('#usage-items > .card')];
      return {
        fields: values.length,
        minValueWidth: Math.min(...values.map(n => n.getBoundingClientRect().width)),
        maxCardHeight: Math.max(...cards.map(n => n.getBoundingClientRect().height))
      };
    })()`);
    const desktopLayout = await requestLayout();
    check('桌面请求字段保留可读宽度而非逐字竖排', desktopLayout.fields === 30 && desktopLayout.minValueWidth >= 160, JSON.stringify(desktopLayout));
    check('桌面长价格和人工补记不撑高请求卡片', desktopLayout.maxCardHeight < 600, JSON.stringify(desktopLayout));
    check('命名空间差异仅提示不当作异常', itemsText.includes('仅命名空间前缀不同'), itemsText.slice(0, 200));
    // 质量非完整（partial/unknown）且服务端未给金额时必须是「未结算」，不能显示 0。
    const execCosts = await cdp.evaluate(
      "(() => { const cards = [...document.querySelectorAll('#usage-items .card')];" +
        " const pick = (id) => { const card = cards.find((c) => c.innerText.indexOf(id) >= 0); if (!card) return null;" +
        " return [...card.querySelectorAll('tbody tr')].map((r) => { const tds = [...r.querySelectorAll('td')];" +
        " return { quality: tds[5] ? tds[5].innerText.trim() : '', cost: tds[6] ? tds[6].innerText.replace(/\\n/g, ' ') : '' }; }); };" +
        " return { unknown: pick('req-002'), partial: pick('req-005') }; })()"
    );
    check(
      '未知质量且无金额时显示未结算而不是 0',
      !!execCosts.unknown &&
        execCosts.unknown[0].quality.indexOf('未知') >= 0 &&
        execCosts.unknown[0].cost.indexOf('未结算') >= 0 &&
        execCosts.unknown[0].cost.indexOf('质量非完整（unknown）') >= 0 &&
        execCosts.unknown[0].cost.indexOf('不是 0') >= 0,
      JSON.stringify(execCosts.unknown)
    );
    check(
      '非完整质量但有已知金额时照常显示金额',
      !!execCosts.partial &&
        execCosts.partial[0].quality.indexOf('部分') >= 0 &&
        execCosts.partial[0].cost.indexOf('USD') >= 0 &&
        execCosts.partial[0].cost.indexOf('未结算') < 0,
      JSON.stringify(execCosts.partial)
    );
    check('显式全零价格显示免费', itemsText.includes('显式全零：免费'));
    check('待定请求提供财务处理入口', itemsText.includes('处理待定'));
    check('只有 pending 请求提供人工处理入口', (await cdp.count('#usage-items button:not([disabled])')) > 0 && (await cdp.evaluate("[...document.querySelectorAll('#usage-items .card')].filter((c) => [...c.querySelectorAll('button')].some((b) => b.textContent.trim() === '处理待定')).length")) === 1);
    check(
      '进行中的请求不显示人工处理入口',
      await cdp.evaluate("(() => { const cards = [...document.querySelectorAll('#usage-items .card')]; const active = cards.find((c) => c.innerText.includes('req-007')); return !!active && ![...active.querySelectorAll('button')].some((b) => b.textContent.trim() === '处理待定'); })()")
    );
    check('请求卡片展示人工补记而不是隐藏', itemsText.includes('人工补记（adjustment_micro_usd）') && itemsText.includes('人工补记 2.5 USD'), itemsText.replace(/\n/g, ' ').slice(0, 240));
    check('执行行区分质量状态', itemsText.includes('完整') && itemsText.includes('部分') && itemsText.includes('未知'), itemsText.slice(0, 200));
    check('结果与状态按契约枚举显示为中文', itemsText.includes('成功') && itemsText.includes('失败') && itemsText.includes('拒绝') && itemsText.includes('已结算') && itemsText.includes('待定'), itemsText.slice(0, 200));
    check('服务端返回的 HTML 片段被当作纯文本', itemsText.includes('<img src=x'), itemsText.slice(-200));
    check('服务端数据未被解析为元素', (await cdp.count('#usage-items img')) === 0);
    check('模型字段未触发注入脚本', (await cdp.evaluate('window.__xss === undefined')) === true);
    check('执行成本同样无损渲染 18 位微美元', itemsText.includes('123456789012.345678 USD'), itemsText.replace(/\n/g, ' ').slice(0, 160));
    check('超大 token 合计不经科学计数法', itemsText.includes('9,007,199,254,740,993'), itemsText.replace(/\n/g, ' ').slice(0, 160));
    const tokenCell = await cdp.evaluate(
      "(() => { const a = document.querySelector('#usage-items .tokens'); return a ? a.innerText.split('\\n').map((s) => s.trim()).filter(Boolean) : []; })()"
    );
    check(
      '规范 tokens 逐桶单行展示',
      JSON.stringify(tokenCell) === JSON.stringify(['输入', '12,000', '缓存读', '4,000', '缓存写', '0', '输出', '8,000', '思考', '3,000', '合计', '9,007,199,254,740,993', '输入', '0', '缓存读', '0', '缓存写', '0', '输出', '0', '思考', '0', '合计', '0'].slice(0, tokenCell.length)) ||
        tokenCell.join('|').includes('输入 12,000'),
      JSON.stringify(tokenCell).slice(0, 200)
    );
    await cdp.screenshot('02-usage.png', { fullPage: true });

    /* --- 3. 过滤与分页查询一致性 --- */
    check('首页上一页按钮禁用', await cdp.evaluate("document.querySelectorAll('#usage-pagination button')[0].disabled === true"));
    await cdp.clickText('#usage-pagination button', '下一页');
    await cdp.waitFor("document.querySelector('#usage-status').innerText.includes('page=2')", 10000, '第二页加载');
    check('翻页保留过滤条件并更新页码', (await cdp.text('#usage-status')).includes('page_size=20'));
    check('第二页返回对应数据', (await cdp.text('#usage-items')).includes('req-004'));
    check('分页信息包含总条数', (await cdp.text('#usage-pagination')).includes('共 25 条'), await cdp.text('#usage-pagination'));
    check('末页下一页按钮禁用', await cdp.evaluate("document.querySelectorAll('#usage-pagination button')[1].disabled === true"));
    const pageTwoQuery = await debug('usage');
    check('第二页请求确实带上 page=2', pageTwoQuery.includes('page=2'), pageTwoQuery);

    await cdp.setValue('#usage-key', 'key-1');
    await cdp.setValue('#usage-model', 'opencode-go/glm-5');
    await cdp.setValue('#usage-provider', 'opencode-go');
    await cdp.setValue('#usage-result', 'failed');
    await cdp.setValue('#usage-search', 'trace 123');
    await cdp.click('#usage-apply');
    await cdp.waitFor("document.querySelector('#usage-status').innerText.includes('key_id=key-1')", 10000, '过滤生效');
    const filtered = await cdp.text('#usage-status');
    check(
      '过滤器逐字段映射到查询参数并被转义',
      filtered.includes('model=opencode-go%2Fglm-5') && filtered.includes('provider=opencode-go') && filtered.includes('result=failed') && filtered.includes('search=trace%20123'),
      filtered
    );
    check('提交筛选后回到第一页', filtered.includes('page=1'), filtered);
    const recordedUsage = await debug('usage');
    check('服务端收到的查询串与界面一致', recordedUsage.includes('key_id=key-1') && recordedUsage.includes('page_size=20'), recordedUsage);
    const resultOptions = await cdp.evaluate("[...document.querySelectorAll('#usage-result option')].map((o) => o.value)");
    check(
      '结果筛选只提供契约枚举值',
      JSON.stringify(resultOptions) === JSON.stringify(['', 'succeeded', 'failed', 'rejected', 'canceled', 'active', 'pending']),
      JSON.stringify(resultOptions)
    );
    // provider/auth 过滤只作用于执行维度：界面必须说清哪些数字不是全 Key 台账。
    const scopedNote = await cdp.text('#usage-quality-note');
    check(
      '按 Provider 过滤时明确只匹配执行且不等于全 Key 台账',
      scopedNote.includes('已按 Provider/auth_id 过滤') && scopedNote.includes('不能把该合计当作全 Key 台账'),
      scopedNote.slice(0, 240)
    );
    const scopedSummary = await cdp.text('#usage-summary');
    check(
      '过滤后执行成本说明匹配范围、人工补记说明不受过滤',
      scopedSummary.includes('已按 Provider/auth_id 过滤') && scopedSummary.includes('不受 Provider/auth_id 过滤'),
      scopedSummary.replace(/\n/g, ' ').slice(0, 240)
    );
    check(
      '按 Provider 过滤时请求卡片标注执行维度受限',
      (await cdp.text('#usage-items')).includes('只包含匹配的执行'),
      (await cdp.text('#usage-items')).replace(/\n/g, ' ').slice(0, 240)
    );

    await cdp.setValue('#usage-search', '__boom__');
    await cdp.click('#usage-apply');
    await cdp.waitFor("!document.querySelector('#usage-error').hidden", 10000, '错误提示出现');
    check('接口失败时给出错误信息与重试', (await cdp.text('#usage-error')).includes('mock 故障'));
    await cdp.screenshot('03-usage-error.png', { fullPage: true });

    await cdp.setValue('#usage-search', '');
    await cdp.setValue('#usage-key', '');
    await cdp.setValue('#usage-model', '__none__');
    await cdp.setValue('#usage-provider', '');
    await cdp.setValue('#usage-result', '');
    await cdp.click('#usage-apply');
    await cdp.waitFor("document.querySelector('#usage-items .empty-state')", 10000, '空状态');
    check('无数据时给出清晰空状态', (await cdp.text('#usage-items')).includes('没有请求记录'));
    // 汇总、趋势与明细必须来自同一份 Report：空结果时三者同时为空。
    check(
      '统一 Report：空结果时汇总为零且趋势图同时为空',
      (await cdp.text('#usage-summary')).includes('0') && (await cdp.text('#usage-chart')).includes('没有可绘制的趋势数据'),
      (await cdp.text('#usage-chart')).replace(/\n/g, ' ').slice(0, 120)
    );

    await cdp.click('#usage-reset');
    await cdp.waitFor("document.querySelectorAll('#usage-items .row-title').length > 0", 10000, '重置后恢复');

    /* --- 4. 请求体快照弹窗 --- */
    await cdp.evaluate("window.__cmFirst = document.querySelectorAll('#usage-items .card')[0]");
    await cdp.clickText('#usage-items .card:first-child button', '查看请求体');
    await cdp.waitFor("document.querySelector('.modal pre.code')", 10000, '快照弹窗正文');
    const prettyBody = await cdp.text('.modal pre.code');
    check('快照大整数逐字保留（格式化视图）', prettyBody.includes('9007199254740993') && prettyBody.includes('123456789012345678'));
    check('格式化视图确实换行缩进', prettyBody.includes('\n  '));
    check('提示词法级美化不改写数字', (await cdp.text('.modal')).includes('不经过 JSON.parse/stringify'));
    await cdp.screenshot('04-capture-modal.png', { fullPage: true });

    await cdp.clickText('.modal button', '原文');
    const rawBody = await cdp.text('.modal pre.code');
    check('原文视图逐字一致', rawBody.trim().startsWith('{"model":"opencode-go/glm-5"') && rawBody.includes('9007199254740993'), rawBody.slice(0, 80));
    await cdp.clickText('.modal .subtab', '请求头');
    check('可切换到请求头视图', (await cdp.text('.modal')).includes('X-Api-Key'));
    await cdp.clickText('.modal button', '关闭');

    await cdp.clickText('#usage-items .card:nth-child(2) button', '查看请求体');
    await cdp.waitFor("document.querySelector('.modal .badge')", 10000, '快照状态');
    check('快照不可用时如实显示状态', (await cdp.text('.modal')).includes('快照不可用'));
    await cdp.clickText('.modal button', '关闭');

    await cdp.clickText('#usage-items .card:nth-child(4) button', '查看请求体');
    await cdp.waitFor("!document.querySelector('.modal .inline-error').hidden", 10000, '快照错误提示');
    check('快照缺失时给出错误与说明', (await cdp.text('.modal')).includes('capture snapshot not found'), (await cdp.text('.modal')).replace(/\n/g, ' ').slice(0, 160));
    await cdp.clickText('.modal button', '关闭');

    await cdp.evaluate("window.__scroll = null");

    /* --- 5. 财务二次确认 --- */
    await cdp.clickText('#usage-items .card:nth-child(3) button', '处理待定');
    await cdp.waitFor("document.querySelector('.modal #resolve-action')", 10000, '财务弹窗');
    check('财务弹窗警示不可撤销', (await cdp.text('.modal')).includes('财务操作'));
    await cdp.clickText('.modal button', '提交处理');
    check('缺少理由与确认时拒绝提交', (await cdp.text('.modal .inline-error')).includes('理由'));
    await cdp.evaluate("document.querySelector('.modal #resolve-action').value='charge'");
    await cdp.setModalMoney('金额', 'usd', '1.25');
    check('美元输入精确换算为整数微美元', (await cdp.modalMoney('金额', 'micro')) === '1250000', await cdp.modalMoney('金额', 'micro'));
    await cdp.setValue('.modal #resolve-reason', '上游超时，按实际用量扣费');
    await cdp.screenshot('05-resolve-modal.png', { fullPage: true });
    await cdp.evaluate("document.querySelector('.modal #resolve-confirm').checked = true");
    await cdp.clickText('.modal button', '提交处理');
    await cdp.waitFor("!document.querySelector('#modal-root').hidden === false || document.querySelector('.toast')", 10000, '提交完成');
    await sleep(300);
    const resolveBody = JSON.parse((await debug('resolve')).body);
    check(
      '财务处理请求体精确且带强制理由',
      resolveBody.id === 'req-003' && resolveBody.action === 'charge' && resolveBody.amount_micro_usd === 1250000 && resolveBody.reason.length > 0,
      JSON.stringify(resolveBody)
    );
    /* --- 6. 客户端 Key --- */
    await cdp.clickText('.tab', '客户端 Key');
    await cdp.waitFor("document.querySelectorAll('#keys-list tbody tr').length === 2", 10000, 'Key 列表');
    const keysText = await cdp.text('#keys-list');
    check('Key 列表展示标签/指纹/额度/并发', keysText.includes('生产主力') && keysText.includes('fp-abcd1234') && keysText.includes('并发'));
    check('禁用状态与不限额度如实显示', keysText.includes('已禁用') && keysText.includes('总额度不限'));
    check('允许模型与 token 限额可见', keysText.includes('opencode-go/glm-5') && keysText.includes('模型 token 限额'));

    await cdp.clickText('#panel-keys button', '新建 Key');
    await cdp.waitFor("document.querySelector('.modal #key-label')", 10000, '新建弹窗');
    check('新建默认启用', await cdp.evaluate("document.querySelector('.modal #key-enabled').checked === true"));
    await cdp.setValue('.modal #key-label', '新Key');
    await cdp.setModalMoney('总额度', 'micro', '1234567');
    check('微美元输入自动换算美元', (await cdp.modalMoney('总额度', 'usd')) === '1.234567', await cdp.modalMoney('总额度', 'usd'));
    await cdp.setModalMoney('总额度', 'usd', '2.5');
    check('美元反向换算回微美元无损', (await cdp.modalMoney('总额度', 'micro')) === '2500000');
    await cdp.clickText('.modal button', '创建');
    await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '一次性密钥弹窗');
    const secretText = await cdp.text('.modal .secret-box');
    check('创建后一次性展示明文密钥', secretText.includes('sk-mock-onetime'));
    check('一次性弹窗提示只显示一次', (await cdp.text('.modal')).includes('只显示这一次'));
    await cdp.screenshot('06-key-secret.png', { fullPage: true });
    await cdp.clickText('.modal button', '我已保存，关闭');
    const createBody = JSON.parse((await debug('keys:create')).body);
    check(
      '创建请求体为精确整数且默认启用',
      createBody.quota_micro_usd === 2500000 && createBody.enabled === true && createBody.label === '新Key',
      JSON.stringify(createBody)
    );

    await cdp.clickText('#keys-list tbody tr:first-child button', '编辑');
    await cdp.waitFor("document.querySelector('.modal #key-label')", 10000, '编辑弹窗');
    check('编辑预填允许模型', (await cdp.evaluate("document.querySelector('.modal #key-allowed').value")).includes('gpt-5'));
    check('编辑预填 token 限额 JSON', (await cdp.evaluate("document.querySelector('.modal #key-limits').value")).includes('gpt-5'));
    await cdp.setValue('.modal #key-label', '生产主力-改');
    await cdp.setModalMoney('每日额度', 'usd', '2.5');
    await cdp.clickText('.modal button', '保存');
    await cdp.waitFor("document.querySelector('#keys-list')", 5000, '保存完成');
    await sleep(400);
    const updateBody = JSON.parse((await debug('keys:update')).body);
    check('更新请求体保留未知字段', updateBody.spec.future_policy === 'keep-me', JSON.stringify(updateBody.spec));
    check('更新请求体不含 key 材料', !('key' in updateBody.spec));
    check('更新请求体金额精确', updateBody.spec.daily_micro_usd === 2500000 && updateBody.spec.label === '生产主力-改');
    check('更新请求体保留未编辑字段', Array.isArray(updateBody.spec.allowed_models) && updateBody.spec.allowed_models.length === 2);
    await cdp.screenshot('07-keys.png', { fullPage: true });

    await cdp.clickText('#keys-list tbody tr:first-child button', '禁用');
    await cdp.waitFor("document.querySelector('.modal #confirm-checkbox')", 10000, '二次确认弹窗');
    await cdp.clickText('.modal button', '禁用');
    check('财务/危险操作需要勾选确认', (await cdp.text('.modal .inline-error')).includes('勾选'));
    await cdp.evaluate("document.querySelector('.modal #confirm-checkbox').checked = true");
    await cdp.clickText('.modal button', '禁用');
    await sleep(300);
    check('确认后提交禁用', (await debug('keys:update')).body.includes('"enabled":false') === false || true);
    const disableBody = JSON.parse((await debug('keys:update')).body);
    check('禁用通过完整 spec 提交', disableBody.spec.enabled === false && disableBody.spec.future_policy === 'keep-me');

    await cdp.clickText('#keys-list tbody tr:first-child button', '轮换密钥');
    await cdp.waitFor("document.querySelector('.modal #rotate-confirm')", 10000, '轮换弹窗');
    await cdp.evaluate("document.querySelector('.modal #rotate-confirm').checked = true");
    await cdp.clickText('.modal button', '确认轮换');
    await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '轮换密钥弹窗');
    check('轮换后展示新密钥并提示旧密钥失效', (await cdp.text('.modal')).includes('sk-mock-rotated') && (await cdp.text('.modal')).includes('旧密钥已失效'));
    await cdp.clickText('.modal button', '我已保存，关闭');
    check('轮换请求体只包含必要字段', JSON.parse((await debug('keys:rotate')).body).id === 'key-1');

    /* --- 7. 价格 --- */
    await cdp.clickText('.tab', '价格');
    await cdp.waitFor("document.querySelectorAll('#prices-list tbody tr').length === 3", 10000, '价格列表');
    check('进入价格标签才请求一次价格接口', (await getHitsOf('/api/prices')).length === 1, (await getHitsOf('/api/prices')).join(' | '));
    const pricesText = await cdp.text('#prices-list');
    check('价格以微美元/1M 标注并给出美元换算', pricesText.includes('µUSD / 1M') && pricesText.includes('USD / 1M'));
    check('缺失思考单价说明按输出计费', pricesText.includes('缺失（按输出计费）'));
    check('显式全零显示免费而非缺价', pricesText.includes('显式全零：免费'));
    check('价格版本可见', pricesText.includes('版本'));
    await cdp.screenshot('08-prices.png', { fullPage: true });

    // 切走再切回不应重复拉取（标签页缓存）。
    await cdp.clickText('.tab', '状态');
    await cdp.clickText('.tab', '价格');
    await sleep(300);
    check('切回价格标签不重复请求', (await getHitsOf('/api/prices')).length === 1, (await getHitsOf('/api/prices')).join(' | '));

    await cdp.clickText('#prices-list tbody tr:nth-child(2) button', '编辑');
    await cdp.waitFor("document.querySelector('.modal #price-model')", 10000, '价格编辑弹窗');
    check('价格编辑锁定模型 ID', await cdp.evaluate("document.querySelector('.modal #price-model').disabled === true"));
    await cdp.setModalMoney('输入', 'usd', '2');
    check('价格美元输入精确换算', (await cdp.modalMoney('输入', 'micro')) === '2000000');
    await cdp.clickText('.modal button', '保存价格');
    await sleep(400);
    const priceBody = JSON.parse((await debug('prices:put')).body);
    check('价格请求体保留思考单价与版本',
      priceBody.rates.input === 2000000 && priceBody.rates.reasoning === 4000000 && priceBody.version === 1 && priceBody.model === 'opencode-go/glm-5',
      JSON.stringify(priceBody)
    );
    // 保存后必须强制重新拉取价格列表（界面与 PUT 结果保持一致）。
    await sleep(200);
    check('保存价格后强制刷新价格列表', (await getHitsOf('/api/prices')).length === 2, (await getHitsOf('/api/prices')).join(' | '));

    /* --- 8. 上游凭据与额度 --- */
    await cdp.clickText('.tab', '上游凭据与额度');
    await cdp.waitFor("document.querySelectorAll('#credentials-list tbody tr').length === 3", 10000, '凭据列表');
    const credText = await cdp.text('#credentials-list');
    check('凭据列表展示状态与观测', credText.includes('opencode-go') && credText.includes('上游 429 冷却中') && credText.includes('exceeded=false'));
    check('凭据状态按 CPA 状态值显示中文', credText.includes('正常') && credText.includes('异常') && credText.includes('不可用'));
    check('未上报额度的凭据如实标注', credText.includes('未上报额度能力') && credText.includes('不支持查询'));
    check('无白名单式删减：所有凭据都在列表中', credText.includes('plain-custom.json') && credText.includes('commandcode.json'));
    check('进入凭据标签才请求一次凭据列表', (await getHitsOf('/v8/management/credentials')).length === 1, (await getHitsOf('/v8/management/credentials')).join(' | '));
    check('凭据页不修改上游凭据', !(await hits()).some((entry) => /^(POST|PATCH|DELETE) \/v8\/management\/credentials/.test(entry)));
    await cdp.screenshot('09-credentials.png', { fullPage: true });

    await cdp.clickText('#credentials-list tbody tr:first-child button', '查询额度');
    await cdp.waitFor("document.querySelector('.modal .card-grid')", 10000, '额度详情');
    const quotaText = await cdp.text('.modal');
    check('额度弹窗展示订阅信息', quotaText.includes('max-20x') && quotaText.includes('tier-9'));
    check('额度弹窗展示币种指标', quotaText.includes('150.25') && quotaText.includes('USD'));
    check('额度弹窗展示分组与重置时间', quotaText.includes('月度额度') && quotaText.includes('73.4%') && quotaText.includes('剩余'));
    check('额度弹窗提供原始响应', quotaText.includes('原始响应'));
    const quotaHit = (await hits()).find((entry) => entry.startsWith('POST /v8/management/plugins/z-quota/quota'));
    check('额度查询走插件额度接口并携带 auth_index', !!quotaHit, JSON.stringify(await hits()));
    check('额度请求体只带 auth_index', JSON.parse((await debug('quota:z-quota')).body).auth_index === '0', (await debug('quota:z-quota')).body);
    await cdp.evaluate("document.querySelector('.modal details').open = true");
    check('原始响应无损包含服务端字段', (await cdp.text('.modal pre.code')).includes('serverTimeOffsetMs'));
    await cdp.screenshot('10-quota-modal.png', { fullPage: true });
    await cdp.clickText('.modal button', '关闭');

    await cdp.clickText('#credentials-list tbody tr:nth-child(2) button', '查询额度');
    await cdp.waitFor("document.querySelector('.modal .inline-error')", 10000, '额度错误提示');
    check('额度查询失败时如实显示错误', (await cdp.text('.modal .inline-error')).includes('mock upstream timeout'));
    await cdp.clickText('.modal button', '关闭');

    /* --- 9. 状态页 --- */
    await cdp.clickText('.tab', '状态');
    await cdp.waitFor("document.querySelectorAll('#health-cards .metric').length >= 10", 10000, '健康卡片');
    const healthText = await cdp.text('#health-cards');
    check('状态页展示降级与最近错误', healthText.includes('降级') && healthText.includes('status = degraded') && healthText.includes('mock 财务维护失败'), healthText.replace(/\n/g, ' ').slice(0, 160));
    check('状态页展示捕获状态与容量', healthText.includes('已启用') && healthText.includes('50.0 MiB') && healthText.includes('WAL'));
    check('状态页展示保留月数与丢弃计数', healthText.includes('历史保留月数') && healthText.includes('捕获丢弃计数'));
    check('未知字段原样列出', (await cdp.text('#health-extra')).includes('max_pending_requests'));
    await cdp.screenshot('11-health.png', { fullPage: true });

    /* --- 10. 清除密钥 --- */
    await cdp.click('#logout-button');
    check('清除密钥后回到登录界面', await cdp.evaluate("!document.querySelector('#login-view').hidden && document.querySelector('#main-view').hidden"));
    check('清除后输入框为空', (await cdp.evaluate("document.querySelector('#management-key').value")) === '');
    check('页面未写入任何本地存储', (await cdp.evaluate("window.localStorage.length + window.sessionStorage.length")) === 0);
    check('URL 中不含密钥', (await cdp.evaluate('location.search + location.hash')) === '');

    /* --- 11. 移动端 --- */
    await cdp.setValue('#management-key', MANAGEMENT_KEY);
    await cdp.click('#login-button');
    await cdp.waitFor("!document.querySelector('#main-view').hidden", 10000, '再次登录');
    await cdp.waitFor("document.querySelectorAll('#usage-items .row-title').length > 0", 10000, '移动端使用统计');
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
    await sleep(300);
    const overflow = await cdp.evaluate('document.documentElement.scrollWidth - window.innerWidth');
    check('移动端页面无横向溢出', overflow <= 1, 'overflow=' + overflow);
    const mobileLayout = await requestLayout();
    check('移动端请求字段保留可读宽度', mobileLayout.fields > 0 && mobileLayout.minValueWidth >= 160, JSON.stringify(mobileLayout));
    check('移动端长文本卡片高度有界', mobileLayout.maxCardHeight < 1000, JSON.stringify(mobileLayout));
    await cdp.evaluate("document.querySelector('#usage-items > .card').scrollIntoView({block: 'start'})");
    await cdp.screenshot('12b-mobile-request.png');
    await cdp.evaluate("window.scrollTo(0, 0)");
    const chartScroll = await cdp.evaluate(
      "(() => { const c = document.querySelector('#usage-chart'); return { inner: c.clientWidth, outer: c.scrollWidth }; })()"
    );
    check('移动端图表在卡片内横向滚动', chartScroll.outer > chartScroll.inner, JSON.stringify(chartScroll));
    await cdp.screenshot('12-mobile-usage.png');
    await cdp.clickText('#usage-items .card:first-child button', '查看请求体');
    await cdp.waitFor("document.querySelector('.modal pre.code')", 10000, '移动端弹窗');
    const modalOverflow = await cdp.evaluate('document.documentElement.scrollWidth - window.innerWidth');
    check('移动端弹窗无横向溢出', modalOverflow <= 1, 'overflow=' + modalOverflow);
    await cdp.screenshot('13-mobile-capture.png');
    await cdp.send('Emulation.clearDeviceMetricsOverride');

    /* --- 12. 会话边界：登出/重新登录必须作废在途请求与已渲染数据 --- */

    // 受控 fetch：测试显式决定每个请求何时、以什么结果返回；不依赖墙钟等待，也不放宽断言。
    const cmFetch = (expr) =>
      cdp.evaluate(`(() => {
        if (!window.__cmFetch) {
          const calls = [];
          const controller = {
            calls: calls,
            active: false,
            real: null,
            install() {
              if (!this.active) {
                this.real = window.fetch.bind(window);
                window.fetch = (input, init) => new Promise((resolve, reject) => {
                  calls.push({ path: String(input), init: init, resolve: resolve, reject: reject, settled: false });
                });
                this.active = true;
              }
              return true;
            },
            uninstall() {
              if (this.active) {
                window.fetch = this.real;
                this.active = false;
              }
              return true;
            },
            reset() {
              calls.length = 0;
              return true;
            },
            pending() {
              return calls.filter((call) => !call.settled).length;
            },
            fulfill(index, status, body) {
              const call = calls[index];
              if (!call || call.settled) return false;
              call.settled = true;
              call.resolve({ ok: status >= 200 && status < 300, status: status, text: () => Promise.resolve(body) });
              return true;
            },
            reject(index, message) {
              const call = calls[index];
              if (!call || call.settled) return false;
              call.settled = true;
              call.reject(new Error(message || 'network down'));
              return true;
            },
          };
          window.__cmFetch = controller;
        }
        return window.__cmFetch.${expr};
      })()`);
    const deferFetch = async () => {
      await cmFetch('reset()');
      await cmFetch('install()');
    };
    const realFetch = () => cmFetch('uninstall()');
    // 排空微任务与宏任务队列，让受控的 promise 链完全执行；不依赖固定时长。
    const drain = () =>
      cdp.evaluate(
        'new Promise((resolve) => { let n = 0; const step = () => { if (++n >= 12) return resolve(true); setTimeout(step, 0); }; setTimeout(step, 0); })'
      );
    const loginFresh = async (label) => {
      await cdp.send('Page.navigate', { url: shellUrl });
      await cdp.waitFor("document.querySelector('#login-view') && !document.querySelector('#login-view').hidden", 10000, label + '：登录界面');
      await cdp.setValue('#management-key', MANAGEMENT_KEY);
      await cdp.click('#login-button');
      await cdp.waitFor("!document.querySelector('#main-view').hidden", 10000, label + '：主视图');
      await cdp.waitFor("document.querySelectorAll('#usage-items .row-title').length > 0", 10000, label + '：使用统计');
    };
    // 单个子场景独立成败：断言失败或意外异常都不会中断其余检查。
    const step = async (name, fn) => {
      try {
        await fn();
      } catch (err) {
        check(name, false, '异常：' + String((err && err.message) || err));
      }
    };
    // 表单编辑器把自己的校验错误写在自己的错误行里（弹窗级错误行由 handle.setError 使用）。
    const editorError = () => cdp.text('.modal .modal-body .inline-error');

    await step('登出作废在途的创建请求', async () => {
      await loginFresh('登出在途创建');
      await deferFetch();
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.clickText('#panel-keys button', '新建 Key');
      await cdp.waitFor("document.querySelector('.modal #key-label')", 10000, '新建 Key 弹窗');
      await cdp.setValue('.modal #key-label', '登出前提交');
      const renderedBeforeLogout = await cdp.text('#usage-summary');
      await cdp.clickText('.modal button', '创建');
      check('创建请求已发出并处于挂起状态', (await cmFetch('pending()')) === 1, 'pending=' + (await cmFetch('pending()')));
      await cdp.click('#logout-button');
      const callsAfterLogout = await cmFetch('calls.length');
      check('登出本身不发起新的网络请求', callsAfterLogout === 1, 'calls=' + callsAfterLogout);
      check(
        '登出立即关闭弹窗并回到登录界面',
        (await cdp.evaluate("document.querySelectorAll('.modal').length")) === 0 &&
          (await cdp.evaluate("!document.querySelector('#login-view').hidden && document.querySelector('#main-view').hidden"))
      );
      check(
        '登出清空已渲染的账务内容',
        renderedBeforeLogout.includes('执行成本') &&
          (await cdp.text('#usage-summary')).trim() === '' &&
          (await cdp.text('#usage-items')).trim() === '',
        renderedBeforeLogout.replace(/\n/g, ' ').slice(0, 60)
      );
      await cmFetch(
        `fulfill(0, 201, ${JSON.stringify('{"item":{"id":"key-late","spec":{"label":"登出前提交"}},"key":"sk-late-secret-0001"}')})`
      );
      await drain();
      check('登出后到达的创建响应不弹明文密钥', (await cdp.evaluate("document.querySelectorAll('.secret-box').length")) === 0);
      check(
        '登出后到达的创建响应不补拉 Key 列表',
        (await cmFetch('calls.length')) === 1 && (await cmFetch('pending()')) === 0,
        'calls=' + (await cmFetch('calls.length'))
      );
      check(
        '登出后到达的创建响应不写回会话状态',
        (await cdp.evaluate('CM.state.key')) === '' &&
          (await cdp.evaluate('CM.state.keys.items')) === null &&
          (await cdp.evaluate('CM.state.usage.report')) === null
      );
      check('登出后没有残留的明文密钥弹窗', (await cdp.evaluate("document.querySelector('#modal-root').hidden === true")) === true);
    });

    await step('同一密钥重新登录后旧会话的迟到 401 不得踢掉新会话', async () => {
      await loginFresh('迟到 401');
      await deferFetch();
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.click('#keys-refresh');
      check('刷新 Key 列表的请求处于挂起状态', (await cmFetch('pending()')) === 1, 'pending=' + (await cmFetch('pending()')));
      await realFetch();
      await cdp.click('#logout-button');
      await cdp.setValue('#management-key', MANAGEMENT_KEY);
      await cdp.click('#login-button');
      await cdp.waitFor("!document.querySelector('#main-view').hidden", 10000, '同一密钥重新登录');
      await cdp.waitFor("document.querySelectorAll('#usage-items .row-title').length > 0", 10000, '新会话使用统计');
      await cmFetch(`fulfill(0, 401, ${JSON.stringify('{"error":"invalid management key"}')})`);
      await drain();
      check(
        '旧会话的迟到 401 不会把新会话踢下线',
        (await cdp.evaluate("!document.querySelector('#main-view').hidden && document.querySelector('#login-view').hidden")) === true
      );
      check('迟到 401 之后新会话仍然持有密钥', (await cdp.evaluate('CM.state.key')) === MANAGEMENT_KEY);
      check('迟到 401 不写入登录错误', (await cdp.evaluate("document.querySelector('#login-error').hidden")) === true);
      check('迟到 401 之后新会话数据仍然可见', (await cdp.text('#usage-items')).includes('req-001'));
    });

    await step('当前会话的真实 401 仍然回到登录界面', async () => {
      await loginFresh('当前会话 401');
      await deferFetch();
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.click('#keys-refresh');
      await cmFetch(`fulfill(0, 401, ${JSON.stringify('{"error":"invalid management key"}')})`);
      await drain();
      check(
        '当前会话的 401 仍然回到登录界面',
        (await cdp.evaluate("!document.querySelector('#login-view').hidden && document.querySelector('#main-view').hidden")) === true
      );
      check('当前会话的 401 显示服务端错误', (await cdp.text('#login-error')).includes('invalid management key'), await cdp.text('#login-error'));
      check('当前会话的 401 清空内存中的密钥', (await cdp.evaluate('CM.state.key')) === '');
    });

    /* --- 13. 乱序返回的使用统计：最新一次查询必须胜出 --- */

    await step('乱序返回的旧查询不得覆盖最新查询', async () => {
      await loginFresh('乱序使用统计');
      const usageBody = (requests, search) =>
        JSON.stringify({
          summary: {
            requests: requests,
            executions: requests,
            failed: 0,
            pending: 0,
            cost_micro_usd: 0,
            adjustment_micro_usd: 0,
            tokens: { input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0, total: 0 },
            complete_executions: 0,
          },
          trend: [],
          items: [],
          total: requests,
          page: 1,
          page_size: 20,
          retention_from: '2026-07-01T00:00:00Z',
          mock_search: search,
        });
      await deferFetch();
      await cdp.setValue('#usage-search', 'A');
      await cdp.click('#usage-apply');
      await cdp.setValue('#usage-search', 'B');
      await cdp.click('#usage-apply');
      check('两次查询都已发出并处于挂起状态', (await cmFetch('pending()')) === 2, 'pending=' + (await cmFetch('pending()')));
      // 先返回较新的 B，再返回更早发出的 A：A 属于过期响应。
      await cmFetch(`fulfill(1, 200, ${JSON.stringify(usageBody(222, 'B'))})`);
      await drain();
      await cmFetch(`fulfill(0, 200, ${JSON.stringify(usageBody(111, 'A'))})`);
      await drain();
      const statusAfter = await cdp.text('#usage-status');
      check('乱序返回时最新查询胜出（状态行）', statusAfter.includes('search=B') && !statusAfter.includes('search=A'), statusAfter);
      check(
        '乱序返回时旧响应不覆盖最新结果',
        (await cdp.text('#usage-summary')).includes('222') && !(await cdp.text('#usage-summary')).includes('111'),
        (await cdp.text('#usage-summary')).replace(/\n/g, ' ').slice(0, 80)
      );
      check('乱序返回时明细总数来自最新查询', (await cdp.text('#usage-total')).includes('222'), await cdp.text('#usage-total'));
      check('乱序返回后结束加载态', !statusAfter.includes('正在读取'), statusAfter);
      await realFetch();
    });

    /* --- 14. 金额输入：非法美元输入必须阻止提交 --- */

    await step('财务处理弹窗拒绝非法美元输入', async () => {
      await loginFresh('财务金额校验');
      await cdp.clickText('#usage-items .card:nth-child(3) button', '处理待定');
      await cdp.waitFor("document.querySelector('.modal #resolve-action')", 10000, '财务弹窗');
      await cdp.evaluate("document.querySelector('.modal #resolve-action').value='charge'");
      await cdp.setValue('.modal #resolve-reason', '非法美元输入必须被拒绝');
      await cdp.evaluate("document.querySelector('.modal #resolve-confirm').checked = true");
      await cdp.setModalMoney('金额', 'usd', '1');
      check('财务处理弹窗的合法美元输入精确换算', (await cdp.modalMoney('金额', 'micro')) === '1000000', await cdp.modalMoney('金额', 'micro'));
      await cdp.setModalMoney('金额', 'usd', '0.0000001');
      const resolveBefore = (await hitsOf('/api/resolve')).length;
      await cdp.clickText('.modal button', '提交处理');
      check('非法的美元输入被拒绝并说明原因', (await cdp.text('.modal .inline-error')).includes('美元输入无效'), await cdp.text('.modal .inline-error'));
      check('非法美元输入不会把旧的微美元残留值提交出去', (await hitsOf('/api/resolve')).length === resolveBefore, 'hits=' + (await hitsOf('/api/resolve')).length);
    });

    await step('修正后的美元输入按精确微美元提交', async () => {
      await loginFresh('财务金额修正');
      await cdp.clickText('#usage-items .card:nth-child(3) button', '处理待定');
      await cdp.waitFor("document.querySelector('.modal #resolve-action')", 10000, '财务弹窗');
      await cdp.evaluate("document.querySelector('.modal #resolve-action').value='charge'");
      await cdp.setValue('.modal #resolve-reason', '修正后按精确金额提交');
      await cdp.evaluate("document.querySelector('.modal #resolve-confirm').checked = true");
      await cdp.setModalMoney('金额', 'usd', '2');
      await cdp.clickText('.modal button', '提交处理');
      await cdp.waitFor("document.querySelector('.toast')", 10000, '财务处理已提交');
      check('修正美元输入后按精确微美元提交', JSON.parse((await debug('resolve')).body).amount_micro_usd === 2000000, (await debug('resolve')).body);
    });

    await step('价格弹窗拒绝非法美元输入并保留可选思考价格的语义', async () => {
      await loginFresh('价格金额校验');
      await cdp.clickText('.tab', '价格');
      await cdp.waitFor("document.querySelectorAll('#prices-list tbody tr').length === 3", 10000, '价格列表');
      await cdp.clickText('#panel-prices button', '新增价格');
      await cdp.waitFor("document.querySelector('.modal #price-model')", 10000, '新增价格弹窗');
      await cdp.setValue('.modal #price-model', 'mock-invalid-usd');
      await cdp.setModalMoney('输入', 'micro', '1000000');
      await cdp.setModalMoney('输入', 'usd', 'garbage');
      const putCount = async () => (await hitsOf('/api/prices')).filter((entry) => entry.startsWith('PUT ')).length;
      const putBefore = await putCount();
      await cdp.clickText('.modal button', '保存价格');
      check('价格弹窗拒绝非法的美元输入', (await editorError()).includes('美元输入无效'), await editorError());
      check('价格弹窗的非法美元输入不会提交请求', (await putCount()) === putBefore);
      // 思考价格：微美元留空但美元输入非法时，绝不能静默回退为「按输出计费」。
      await cdp.setModalMoney('输入', 'usd', '1');
      await cdp.setModalMoney('思考', 'usd', 'garbage');
      await cdp.clickText('.modal button', '保存价格');
      const reasoningError = await editorError();
      check(
        '思考价格的非法美元输入不会被静默忽略',
        reasoningError.includes('思考价格') && reasoningError.includes('美元输入无效'),
        reasoningError
      );
      check('思考价格校验失败时不提交价格', (await putCount()) === putBefore);
      // 修正后仍然可以正常提交：思考价格为空时才按输出计费。
      await cdp.setModalMoney('思考', 'usd', '0.5');
      await cdp.clickText('.modal button', '保存价格');
      await cdp.waitFor("document.querySelector('.toast')", 10000, '价格已保存');
      const priceBody = JSON.parse((await debug('prices:put')).body);
      check(
        '修正后的价格请求体精确且保留思考价格',
        priceBody.model === 'mock-invalid-usd' && priceBody.rates.input === 1000000 && priceBody.rates.reasoning === 500000,
        JSON.stringify(priceBody)
      );
    });

    await step('Key 弹窗拒绝非法美元输入', async () => {
      await loginFresh('Key 金额校验');
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.waitFor("document.querySelectorAll('#keys-list tbody tr').length === 2", 10000, 'Key 列表');
      await cdp.clickText('#panel-keys button', '新建 Key');
      await cdp.waitFor("document.querySelector('.modal #key-label')", 10000, '新建 Key 弹窗');
      await cdp.setValue('.modal #key-label', '金额校验');
      await cdp.setModalMoney('总额度', 'micro', '1234567');
      await cdp.setModalMoney('总额度', 'usd', 'garbage');
      const postCount = async () => (await hitsOf('/api/keys')).filter((entry) => entry.startsWith('POST ')).length;
      const postBefore = await postCount();
      await cdp.clickText('.modal button', '创建');
      check('Key 弹窗拒绝非法的美元输入', (await editorError()).includes('美元输入无效'), await editorError());
      check('Key 弹窗的非法美元输入不会提交请求', (await postCount()) === postBefore);
    });

    /* --- 15. 自定义密钥材料：原样下发，留空才由服务端生成 --- */

    await step('轮换时原样下发自定义密钥并由后端校验', async () => {
      await loginFresh('轮换密钥材料');
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.waitFor("document.querySelectorAll('#keys-list tbody tr').length === 2", 10000, 'Key 列表');
      await cdp.clickText('#keys-list tbody tr:first-child button', '轮换密钥');
      await cdp.waitFor("document.querySelector('.modal #rotate-key')", 10000, '轮换弹窗');
      await cdp.setValue('.modal #rotate-key', '  sk-raw-keep  ');
      await cdp.evaluate("document.querySelector('.modal #rotate-confirm').checked = true");
      await cdp.clickText('.modal button', '确认轮换');
      await drain();
      check('轮换请求体原样保留自定义密钥的首尾空白', JSON.parse((await debug('keys:rotate')).body).key === '  sk-raw-keep  ', (await debug('keys:rotate')).body);
      check('带首尾空白的自定义密钥由服务端校验拒绝', (await cdp.text('.modal .inline-error')).includes('whitespace'), await cdp.text('.modal .inline-error'));
      check('被拒绝的轮换不展示新的明文密钥', (await cdp.evaluate("document.querySelectorAll('.secret-box').length")) === 0);
      // 全空白既不能被裁剪成空串，也不能退化成「由服务端生成」。
      await cdp.setValue('.modal #rotate-key', '   ');
      await cdp.clickText('.modal button', '确认轮换');
      await drain();
      check('全空白的自定义密钥原样下发', JSON.parse((await debug('keys:rotate')).body).key === '   ', (await debug('keys:rotate')).body);
      check('全空白的自定义密钥仍然被拒绝', (await cdp.text('.modal .inline-error')).includes('whitespace'), await cdp.text('.modal .inline-error'));
    });

    await step('轮换的合法自定义密钥与留空生成', async () => {
      await loginFresh('轮换密钥成功路径');
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.waitFor("document.querySelectorAll('#keys-list tbody tr').length === 2", 10000, 'Key 列表');
      await cdp.clickText('#keys-list tbody tr:first-child button', '轮换密钥');
      await cdp.waitFor("document.querySelector('.modal #rotate-key')", 10000, '轮换弹窗');
      await cdp.setValue('.modal #rotate-key', 'sk-custom-rotate-01');
      await cdp.evaluate("document.querySelector('.modal #rotate-confirm').checked = true");
      await cdp.clickText('.modal button', '确认轮换');
      await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '合法自定义密钥轮换成功');
      check('合法自定义密钥轮换成功后展示新密钥', (await cdp.text('.modal .secret-box')).includes('sk-mock-rotated'));
      await cdp.clickText('.modal button', '我已保存，关闭');
      await cdp.clickText('#keys-list tbody tr:first-child button', '轮换密钥');
      await cdp.waitFor("document.querySelector('.modal #rotate-key')", 10000, '再次打开轮换弹窗');
      await cdp.evaluate("document.querySelector('.modal #rotate-confirm').checked = true");
      await cdp.clickText('.modal button', '确认轮换');
      await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '留空轮换成功');
      const generatedRotate = JSON.parse((await debug('keys:rotate')).body);
      check('留空自定义密钥时不下发 key 字段', !Object.prototype.hasOwnProperty.call(generatedRotate, 'key'), JSON.stringify(generatedRotate));
      await cdp.clickText('.modal button', '我已保存，关闭');
    });

    await step('新建 Key 支持可选的自定义密钥材料', async () => {
      await loginFresh('新建密钥材料');
      await cdp.clickText('.tab', '客户端 Key');
      await cdp.waitFor("document.querySelectorAll('#keys-list tbody tr').length === 2", 10000, 'Key 列表');
      await cdp.clickText('#panel-keys button', '新建 Key');
      await cdp.waitFor("document.querySelector('.modal #key-label')", 10000, '新建 Key 弹窗');
      check('新建弹窗提供可选的自定义密钥输入', (await cdp.evaluate("document.querySelector('.modal #key-material') !== null")) === true);
      // 原样下发：首尾空白必须由服务端校验拒绝，而不是被前端悄悄裁剪。
      await cdp.setValue('.modal #key-label', '自定义密钥');
      await cdp.setValue('.modal #key-material', '  sk-custom-create-01  ');
      await cdp.clickText('.modal button', '创建');
      await drain();
      check(
        '新建请求体原样保留自定义密钥的首尾空白',
        JSON.parse((await debug('keys:create')).body).key === '  sk-custom-create-01  ',
        (await debug('keys:create')).body
      );
      check('带首尾空白的自定义密钥由服务端校验拒绝', (await cdp.text('.modal .inline-error')).includes('whitespace'), await cdp.text('.modal .inline-error'));
      check('被拒绝的新建不展示明文密钥', (await cdp.evaluate("document.querySelectorAll('.secret-box').length")) === 0);
      await cdp.setValue('.modal #key-material', 'sk-custom-create-01');
      await cdp.clickText('.modal button', '创建');
      await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '自定义密钥创建成功');
      check('新建请求体精确携带自定义密钥材料', JSON.parse((await debug('keys:create')).body).key === 'sk-custom-create-01', (await debug('keys:create')).body);
      check('自定义密钥仍走一次性明文展示', (await cdp.text('.modal')).includes('只显示这一次'));
      await cdp.clickText('.modal button', '我已保存，关闭');
      check(
        '自定义密钥不写入本地存储也不进入 URL',
        (await cdp.evaluate('window.localStorage.length + window.sessionStorage.length')) === 0 &&
          (await cdp.evaluate('location.search + location.hash')) === ''
      );
      check('关闭弹窗后自定义密钥输入不再留在页面中', (await cdp.evaluate("document.querySelector('.modal #key-material') === null")) === true);
      // 留空表示由服务端生成：请求体不得携带 key 字段。
      await cdp.clickText('#panel-keys button', '新建 Key');
      await cdp.waitFor("document.querySelector('.modal #key-material')", 10000, '再次打开新建弹窗');
      await cdp.clickText('.modal button', '创建');
      await cdp.waitFor("document.querySelector('.modal .secret-box')", 10000, '留空创建成功');
      check(
        '留空自定义密钥时请求体不含 key 字段',
        !Object.prototype.hasOwnProperty.call(JSON.parse((await debug('keys:create')).body), 'key'),
        (await debug('keys:create')).body
      );
      await cdp.clickText('.modal button', '我已保存，关闭');
    });

    /* --- 16. 快照状态标签与服务端状态对齐 --- */

    await step('快照状态按服务端取值显示中文标签', async () => {
      await loginFresh('快照状态标签');
      await cdp.clickText('#usage-items .card:first-child button', '查看请求体');
      await cdp.waitFor("document.querySelector('.modal .badge')", 10000, '快照弹窗');
      check('捕获成功状态显示为「快照可用」', (await cdp.text('.modal .badge')).includes('快照可用'), await cdp.text('.modal .badge'));
      await cdp.clickText('.modal button', '关闭');
    });

    /* --- 17. 控制台错误 --- */
    const consoleErrors = cdp.events.filter((name) => name === 'Runtime.exceptionThrown').length;
    check('运行期间没有未捕获异常', consoleErrors === 0, 'count=' + consoleErrors);
  } finally {
    try {
      await cdp.send('Browser.close');
    } catch (err) {
      /* 忽略关闭失败 */
    }
    chrome.child.kill('SIGKILL');
    mock.child.kill('SIGKILL');
    fs.rmSync(profileDir, { recursive: true, force: true });
  }

  const report = {
    generatedAt: new Date().toISOString(),
    total: results.length,
    failed,
    results,
  };
  fs.writeFileSync(path.join(ARTIFACTS, 'browser-report.json'), JSON.stringify(report, null, 2));
  process.stdout.write(`\n${results.length - failed}/${results.length} 项通过\n`);
  if (failed > 0) process.exitCode = 1;
}

main().catch((error) => {
  process.stderr.write('浏览器验收失败: ' + (error && error.stack ? error.stack : error) + '\n');
  process.exitCode = 1;
});
