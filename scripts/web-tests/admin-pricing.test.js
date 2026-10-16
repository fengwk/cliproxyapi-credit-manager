'use strict';

/*
 * admin-pricing.js 单元测试。
 *
 * 目标：把「真实后端 JSON 契约」固定下来，任何回退到扁平费率别名
 * （price.input_rate / price.output_rate / ...）或把 reasoning 从「继承」改写成
 * 「显式免费」的改动都必须让测试失败。
 *
 * - 纯函数部分直接断言请求体形状、64 位边界、目录映射与分页；
 * - DOM 部分用一个最小 DOM 桩驱动真实的编辑弹窗（新建/编辑/阶梯/确认门禁），
 *   桩在写入 innerHTML 时直接抛错，因此任何 XSS 式拼接都会让测试失败；
 * - 会话部分验证登出（dispose）后到达的响应/错误一律作废，不会写回界面、
 *   也不会穿过重新登录（init）后的新会话。
 */
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ASSETS_DIR = process.env.CM_ASSETS_DIR
  ? path.resolve(process.env.CM_ASSETS_DIR)
  : path.resolve(__dirname, '..', '..', 'internal', 'web', 'assets');

/* ------------------------------------------------------------------ *
 * 最小 DOM 桩
 * ------------------------------------------------------------------ */

function createDocument() {
  const nodes = [];
  const byId = new Map();

  function textOf(node) {
    if (!node) return '';
    if (node.nodeType === 3) return node.textContent;
    let out = node._text || '';
    for (const child of node.childNodes) out += textOf(child);
    return out;
  }

  function markDetached(node) {
    if (!node || node.nodeType !== 1) return;
    node._detached = true;
    for (const child of node.childNodes) markDetached(child);
  }

  function markAttached(node) {
    if (!node || node.nodeType !== 1) return;
    node._detached = false;
    for (const child of node.childNodes) markAttached(child);
  }

  function classesOf(node) {
    return String(node.className || '').split(/\s+/).filter(Boolean);
  }

  function matchesSimple(node, selector) {
    if (node.nodeType !== 1) return false;
    let rest = selector;
    const idMatch = rest.match(/#[A-Za-z0-9_-]+/);
    if (idMatch) {
      if (node.id !== idMatch[0].slice(1)) return false;
      rest = rest.replace(idMatch[0], '');
    }
    const attrMatch = rest.match(/\[([A-Za-z0-9_-]+)(?:="([^"]*)")?\]/);
    if (attrMatch) {
      const actual = node._attributes[attrMatch[1]];
      if (actual === undefined) return false;
      if (attrMatch[2] !== undefined && actual !== attrMatch[2]) return false;
      rest = rest.replace(attrMatch[0], '');
    }
    for (const cls of rest.match(/\.[A-Za-z0-9_-]+/g) || []) {
      if (classesOf(node).indexOf(cls.slice(1)) < 0) return false;
    }
    rest = rest.replace(/\.[A-Za-z0-9_-]+/g, '');
    if (rest && node.tagName.toLowerCase() !== rest.toLowerCase()) return false;
    return true;
  }

  function matchesChain(node, parts) {
    let index = parts.length - 1;
    if (!matchesSimple(node, parts[index])) return false;
    index--;
    let current = node.parentNode;
    while (index >= 0 && current) {
      if (current.nodeType === 1 && matchesSimple(current, parts[index])) index--;
      current = current.parentNode;
    }
    return index < 0;
  }

  function queryAll(list, selector) {
    const parts = String(selector).trim().split(/\s+/);
    return list.filter((node) => node.nodeType === 1 && matchesChain(node, parts));
  }

  function descendants(node, visit) {
    for (const child of node.childNodes) {
      if (child.nodeType === 1) {
        visit(child);
        descendants(child, visit);
      }
    }
  }

  function makeElement(tag) {
    const node = {
      nodeType: 1,
      tagName: String(tag).toUpperCase(),
      childNodes: [],
      parentNode: null,
      // 新建/被移出文档树的节点视为已脱离，避免 doc.querySelectorAll 命中
      // 上一次渲染遗留的孤儿节点（真实 DOM 里它们已经不在文档中）。
      _detached: true,
      _text: '',
      _attributes: {},
      className: '',
      value: '',
      type: '',
      placeholder: '',
      spellcheck: false,
      disabled: false,
      checked: false,
      required: false,
      hidden: false,
      selected: false,
      style: {},
      dataset: {},
      onclick: null,
      onchange: null,
      oninput: null,
      appendChild(child) {
        if (child.parentNode) {
          const at = child.parentNode.childNodes.indexOf(child);
          if (at >= 0) child.parentNode.childNodes.splice(at, 1);
        }
        child.parentNode = node;
        node.childNodes.push(child);
        markAttached(child);
        return child;
      },
      setAttribute(name, value) {
        node._attributes[name] = String(value);
        if (name === 'id') node.id = String(value);
      },
      getAttribute(name) {
        return node._attributes[name];
      },
      querySelectorAll(selector) {
        const found = [];
        descendants(node, (child) => {
          if (matchesChain(child, String(selector).trim().split(/\s+/))) found.push(child);
        });
        return found;
      },
      querySelector(selector) {
        return node.querySelectorAll(selector)[0] || null;
      },
    };
    Object.defineProperty(node, 'id', {
      get() {
        return node._attributes.id || '';
      },
      set(value) {
        node._attributes.id = String(value);
        byId.set(String(value), node);
      },
    });
    Object.defineProperty(node, 'textContent', {
      get() {
        return node._text === '' && node.childNodes.length > 0 ? textOf(node) : node._text;
      },
      set(value) {
        for (const child of node.childNodes) markDetached(child);
        node.childNodes.length = 0;
        node._text = value === null || value === undefined ? '' : String(value);
      },
    });
    // 服务端数据只允许 textContent/createTextNode；直接写 innerHTML 立即失败。
    Object.defineProperty(node, 'innerHTML', {
      get() {
        throw new Error('测试桩禁止读取 innerHTML');
      },
      set() {
        throw new Error('admin-pricing.js 不得写入 innerHTML');
      },
    });
    return node;
  }

  const doc = {
    nodeType: 9,
    createElement(tag) {
      const node = makeElement(tag);
      nodes.push(node);
      return node;
    },
    createTextNode(text) {
      return { nodeType: 3, textContent: text === null || text === undefined ? '' : String(text), parentNode: null };
    },
    getElementById(id) {
      return byId.get(String(id)) || null;
    },
    querySelectorAll(selector) {
      return queryAll(nodes.filter((node) => !node._detached), selector);
    },
    querySelector(selector) {
      return doc.querySelectorAll(selector)[0] || null;
    },
    register(id, tag) {
      const node = makeElement(tag || 'div');
      node.id = id;
      nodes.push(node);
      return node;
    },
  };
  return doc;
}

/* ------------------------------------------------------------------ *
 * 载入被测模块（同 realm，便于严格断言）
 * ------------------------------------------------------------------ */

function loadAdminPricing(win, doc) {
  const source = fs.readFileSync(path.join(ASSETS_DIR, 'admin-pricing.js'), 'utf8');
  const factory = new Function('window', 'document', 'require', source + '\n//# sourceURL=admin-pricing.js');
  factory(win, doc, require);
  if (!win.CMAdminPricing) throw new Error('admin-pricing.js 未导出 window.CMAdminPricing');
  return win.CMAdminPricing;
}

// 与 app.js exactStringify 相同的语义：整数以精确十进制字面量下发。
// 断言真实的 HTTP 请求体文本，避免 JSON.parse 的先丢失精度。
function exactStringify(value) {
  if (value === null || value === undefined) return 'null';
  const type = typeof value;
  if (type === 'string') return JSON.stringify(value);
  if (type === 'boolean') return value ? 'true' : 'false';
  if (type === 'number') return String(value);
  if (type === 'bigint') return value.toString();
  if (type === 'object' && typeof value.cmIntText === 'string') return value.cmIntText;
  if (Array.isArray(value)) {
    return '[' + value.filter((item) => item !== undefined).map(exactStringify).join(',') + ']';
  }
  return '{' + Object.keys(value)
    .filter((key) => value[key] !== undefined)
    .map((key) => JSON.stringify(key) + ':' + exactStringify(value[key]))
    .join(',') + '}';
}

// 每个 ctx 独立记录调用/提示/弹窗，便于断言「旧会话的回调绝不能写到新会话」。
function makeStore() {
  return { calls: [], toasts: [], modals: [] };
}

function makeContext(store, responder) {
  return {
    api(requestPath, opts) {
      const call = { path: requestPath, opts: opts || {} };
      store.calls.push(call);
      const response = responder ? responder(call) : undefined;
      if (response instanceof Error) return Promise.reject(response);
      // 返回 Promise 时按 thenable 采纳，用于模拟「登出后才到达的响应」。
      return Promise.resolve(response === undefined ? { items: [] } : response);
    },
    toast(message) {
      store.toasts.push(message);
    },
    showModal(options) {
      const handle = {
        title: options.title,
        body: [],
        footer: [],
        error: '',
        closed: false,
        setBody(children) {
          handle.body = children;
        },
        setFooter(children) {
          handle.footer = children;
        },
        setError(message) {
          handle.error = message || '';
        },
        close() {
          handle.closed = true;
        },
      };
      store.modals.push(handle);
      return handle;
    },
  };
}

function harness(responder) {
  const doc = createDocument();
  // 与页面一致：charts.js 已加载，提供统一的 formatInt 口径。
  const win = { document: doc, CMCharts: require(path.join(ASSETS_DIR, 'charts.js')) };
  const store = makeStore();
  const api = loadAdminPricing(win, doc);
  const ctx = makeContext(store, responder);
  api.init(ctx);
  return {
    api,
    doc,
    ctx,
    win,
    calls: store.calls,
    modals: store.modals,
    toasts: store.toasts,
    // 模拟「登出后重新登录」：新会话的调用与提示单独记录。
    reattach(nextResponder) {
      const nextStore = makeStore();
      const nextCtx = makeContext(nextStore, nextResponder);
      api.init(nextCtx);
      return { ctx: nextCtx, calls: nextStore.calls, toasts: nextStore.toasts, modals: nextStore.modals };
    },
  };
}

const flush = () => new Promise((resolve) => setImmediate(resolve));

function footerButton(handle, label) {
  return handle.footer.find((node) => textOfNode(node).trim() === label) || null;
}

function textOfNode(node) {
  if (!node) return '';
  if (node.nodeType === 3) return node.textContent;
  let out = node._text || '';
  for (const child of node.childNodes) out += textOfNode(child);
  return out;
}

function putBodyText(calls) {
  const puts = calls.filter((call) => call.path === '/api/pricing-rules' && call.opts.method === 'PUT');
  const last = puts[puts.length - 1];
  return last ? exactStringify(last.opts.body) : null;
}

function jsonBody(calls) {
  const raw = putBodyText(calls);
  return raw === null ? null : JSON.parse(raw);
}

/* ------------------------------------------------------------------ *
 * 纯函数：64 位整数解析
 * ------------------------------------------------------------------ */

test('parseSigned64 接受 int64 边界并拒绝越界/空/非数字', () => {
  const { parseSigned64 } = harness().api.__test;
  const bounds = [];
  assert.equal(parseSigned64('9223372036854775807', '值', bounds), 9223372036854775807n);
  assert.equal(parseSigned64('-9223372036854775808', '值', bounds), -9223372036854775808n);
  assert.deepEqual(bounds, []);

  const tooBig = [];
  assert.equal(parseSigned64('9223372036854775808', '值', tooBig), undefined);
  assert.match(tooBig.join(''), /超出 64 位/);

  const tooSmall = [];
  assert.equal(parseSigned64('-9223372036854775809', '值', tooSmall), undefined);
  assert.match(tooSmall.join(''), /超出 64 位/);

  // 空值不再被静默当成 0：必填字段必须显式填写。
  const empty = [];
  assert.equal(parseSigned64('', '优先级', empty), undefined);
  assert.match(empty.join(''), /不能为空/);

  const bad = [];
  assert.equal(parseSigned64('12abc', '优先级', bad), undefined);
  assert.match(bad.join(''), /合法的 64 位整数/);
});

test('parseMoney 拒绝负数，parseOptionalMoney 空值返回 null（表示未提供）', () => {
  const { parseMoney, parseOptionalMoney } = harness().api.__test;
  const negative = [];
  assert.equal(parseMoney('-1', '费率', negative), undefined);
  assert.match(negative.join(''), /不能为负数/);

  assert.equal(parseMoney('0', '费率', []), 0n);
  assert.equal(parseOptionalMoney('', '费率', []), null);
  assert.equal(parseOptionalMoney('0', '费率', []), 0n);
});

/* ------------------------------------------------------------------ *
 * 纯函数：请求体契约
 * ------------------------------------------------------------------ */

function editorState(overrides) {
  return Object.assign({
    id: 'rule-1',
    matchKind: 'glob',
    pattern: 'opencode-go/*',
    priority: '10',
    enabled: true,
    billingMode: 'token',
    perImage: '',
    baseRates: { input: '1000000', output: '3000000', cacheRead: '100000', cacheWrite: '2000000', reasoning: '' },
    tiers: [],
  }, overrides || {});
}

test('buildRulePayload 输出嵌套 price.rates 且绝不出现扁平别名', () => {
  const { buildRulePayload } = harness().api.__test;
  const errors = [];
  const payload = buildRulePayload(editorState(), errors);
  assert.deepEqual(errors, []);
  assert.deepEqual(Object.keys(payload.price).sort(), ['billing_mode', 'model', 'per_image_micro_usd', 'rates']);
  assert.deepEqual(Object.keys(payload.price.rates).sort(), ['cache_read', 'cache_write', 'input', 'output']);
  assert.equal(payload.price.rates.input, 1000000n);
  assert.equal(payload.price.rates.cache_write, 2000000n);
  assert.equal(payload.price.model, 'opencode-go/*');
  assert.equal(payload.match_kind, 'glob');
  assert.equal(payload.priority, 10n);
  // 扁平别名一个都不许出现。
  const raw = exactStringify(payload);
  for (const flat of ['input_rate', 'output_rate', 'cache_read_rate', 'cache_write_rate', 'reasoning_rate']) {
    assert.equal(raw.includes(flat), false, '请求体不得包含扁平字段 ' + flat);
  }
});

test('buildRulePayload 保留 reasoning：留空省略字段，显式 0 表示免费', () => {
  const { buildRulePayload } = harness().api.__test;

  const inherit = buildRulePayload(editorState(), []);
  assert.equal('reasoning' in inherit.price.rates, false, '留空必须省略 reasoning（按输出单价）');

  const free = buildRulePayload(editorState({
    baseRates: { input: '1', output: '2', cacheRead: '0', cacheWrite: '0', reasoning: '0' },
  }), []);
  assert.equal(free.price.rates.reasoning, 0n, '显式 0 必须原样下发（思考免费）');
});

test('buildRulePayload 基础费率必填：留空登记错误且不静默补 0', () => {
  const { buildRulePayload } = harness().api.__test;
  const errors = [];
  const payload = buildRulePayload(editorState({
    baseRates: { input: '', output: '', cacheRead: '', cacheWrite: '', reasoning: '' },
  }), errors);
  assert.match(errors.join('；'), /基础输入费率 不能为空/);
  assert.match(errors.join('；'), /缓存写入费率 不能为空/);
  // 出错时不得伪造 0 作为有效费率。
  assert.equal(payload.price.rates.input, undefined);
});

test('buildRulePayload 阶梯：嵌套 rates、包含 cache_write、context 门槛必须为正', () => {
  const { buildRulePayload } = harness().api.__test;
  const errors = [];
  const payload = buildRulePayload(editorState({
    tiers: [
      {
        kind: 'context',
        threshold: '128000',
        service: '',
        label: '长上下文',
        rates: { input: '2000000', output: '', cacheRead: '', cacheWrite: '2500000', reasoning: '' },
      },
      { kind: 'service', threshold: '', service: 'priority,fast', label: '', rates: { input: '', output: '', cacheRead: '', cacheWrite: '', reasoning: '0' } },
    ],
  }), errors);
  assert.deepEqual(errors, []);
  assert.equal(payload.tiers.length, 2);

  const context = payload.tiers[0];
  assert.equal(context.kind, 'context');
  assert.equal(context.threshold, 128000n);
  assert.equal(context.rates.input, 2000000n);
  // 留空/0 表示继承基础费率。
  assert.equal(context.rates.output, 0n);
  assert.equal(context.rates.cache_write, 2500000n);
  assert.equal('reasoning' in context.rates, false, '阶梯思考留空 = 继承');

  const service = payload.tiers[1];
  assert.equal(service.kind, 'service');
  assert.equal(service.service, 'priority,fast');
  assert.equal('threshold' in service, false);
  assert.equal(service.rates.reasoning, 0n, '阶梯显式 0 = 该阶梯思考免费');

  const badErrors = [];
  const bad = buildRulePayload(editorState({
    tiers: [{ kind: 'context', threshold: '0', service: '', label: '', rates: {} }],
  }), badErrors);
  assert.match(badErrors.join('；'), /必须为正整数/);
  assert.equal(bad.tiers.length, 1);

  const serviceErrors = [];
  buildRulePayload(editorState({ tiers: [{ kind: 'service', threshold: '', service: '', label: '', rates: {} }] }), serviceErrors);
  assert.match(serviceErrors.join('；'), /服务阶梯必须填写服务标识/);
});

test('buildRulePayload 按图计费：per_image 费率必填，token 费率不强制', () => {
  const { buildRulePayload } = harness().api.__test;
  const errors = [];
  const payload = buildRulePayload(editorState({
    billingMode: 'per_image',
    perImage: '50000',
    baseRates: { input: '', output: '', cacheRead: '', cacheWrite: '', reasoning: '' },
  }), errors);
  assert.deepEqual(errors, []);
  assert.equal(payload.price.billing_mode, 'per_image');
  assert.equal(payload.price.per_image_micro_usd, 50000n);
  assert.equal(payload.price.rates.input, 0n);

  const missing = [];
  buildRulePayload(editorState({ billingMode: 'per_image', perImage: '', baseRates: {} }), missing);
  assert.match(missing.join('；'), /按图费率 不能为空/);
});

test('ruleToEditorState 只读取嵌套 rates，扁平别名被忽略', () => {
  const { ruleToEditorState } = harness().api.__test;
  const state = ruleToEditorState({
    id: 'rule-9',
    match_kind: 'exact',
    pattern: 'gpt-5',
    priority: { cmIntText: '9007199254740993' },
    enabled: false,
    // 真实后端返回的是嵌套 rates；这里故意混入扁平别名，验证它们不被读取。
    input_rate: 999,
    price: {
      model: 'gpt-5',
      billing_mode: 'token',
      input_rate: 888,
      rates: { input: 1000, output: 2000, cache_read: 0, cache_write: 3000 },
    },
    tiers: [{ kind: 'context', threshold: 128000, label: 'x', service: '', rates: { input: 4000 } }],
  });
  assert.equal(state.baseRates.input, '1000');
  assert.equal(state.baseRates.output, '2000');
  assert.equal(state.baseRates.cacheWrite, '3000');
  assert.equal(state.baseRates.reasoning, '', 'reasoning 缺失必须保留为「继承」而不是 0');
  assert.equal(state.priority, '9007199254740993');
  assert.equal(state.enabled, false);
  assert.equal(state.tiers.length, 1);
  assert.equal(state.tiers[0].rates.input, '4000');
  assert.equal(state.tiers[0].rates.reasoning, '');

  const explicitFree = ruleToEditorState({ price: { rates: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: 0 } } });
  assert.equal(explicitFree.baseRates.reasoning, '0', '显式 0 必须与「继承」区分');
});

/* ------------------------------------------------------------------ *
 * 纯函数：目录
 * ------------------------------------------------------------------ */

test('catalogStatusText 如实标注来源/缓存/过期，绝不把过期数据说成最新', () => {
  const { catalogStatusText } = harness().api.__test;
  const fresh = catalogStatusText({ source: 'models.dev', fetched_at: '2026-10-16T00:00:00Z', cached: false, stale: false });
  assert.match(fresh, /实时获取/);
  assert.match(fresh, /models\.dev/);

  const cached = catalogStatusText({ source: 'models.dev', cached: true });
  assert.match(cached, /缓存/);
  assert.equal(cached.includes('最新'), false);

  const stale = catalogStatusText({ source: 'models.dev', cached: true, stale: true, error: 'dial tcp timeout' });
  assert.match(stale, /上游刷新失败/);
  assert.match(stale, /可能已过期/);
  assert.match(stale, /dial tcp timeout/);
  assert.equal(stale.includes('最新'), false, '过期数据绝不能标注为「最新」');
});

test('paginate 每页最多 50 行并夹紧页码', () => {
  const { paginate, CATALOG_PAGE_SIZE } = harness().api.__test;
  assert.equal(CATALOG_PAGE_SIZE, 50);
  const items = Array.from({ length: 8902 }, (unused, index) => ({ id: 'm-' + index }));
  const first = paginate(items, 1, CATALOG_PAGE_SIZE);
  assert.equal(first.items.length, 50);
  assert.equal(first.total, 8902);
  assert.equal(first.pages, Math.ceil(8902 / 50));
  const last = paginate(items, 9999, CATALOG_PAGE_SIZE);
  assert.equal(last.page, first.pages);
  assert.ok(last.items.length <= 50);
  const clamped = paginate(items, 0, CATALOG_PAGE_SIZE);
  assert.equal(clamped.page, 1);
});

test('filterCatalog 按模型 ID/Provider/名称过滤', () => {
  const { filterCatalog } = harness().api.__test;
  const items = [
    { id: 'opencode-go/glm-5', provider: 'opencode-go', name: 'GLM-5' },
    { id: 'openai/gpt-5', provider: 'openai', name: 'GPT-5' },
  ];
  assert.equal(filterCatalog(items, 'opencode-go').length, 1);
  assert.equal(filterCatalog(items, 'GPT').length, 1);
  assert.equal(filterCatalog(items, '').length, 2);
  assert.equal(filterCatalog(items, 'nothing').length, 0);
});

test('catalogEntryToRule 保留 provider 限定模型名，仅导入费率完整的阶梯', () => {
  const { catalogEntryToRule, catalogRuleID } = harness().api.__test;
  const mapping = catalogEntryToRule({
    id: 'opencode-go/glm-5',
    provider: 'opencode-go',
    context_tiers: [
      { threshold: 128000, label: '128K', rates: { input: 2000000, output: 4000000, cache_read: 200000, cache_write: 2500000 } },
      { threshold: 200000, label: '200K', rates: { input: 3000000, output: 5000000 } },
    ],
    rates: { input: 1000000, output: 3000000, cache_read: 100000, cache_write: 2000000, reasoning: 4000000 },
  }, { source: 'models.dev', stale: true });
  assert.equal(mapping.rule.pattern, 'opencode-go/glm-5', '不得自动剥掉 provider 前缀');
  assert.equal(mapping.rule.price.model, 'opencode-go/glm-5');
  assert.equal(mapping.rule.match_kind, 'exact');
  assert.equal(mapping.rule.price.rates.cache_write, 2000000);
  assert.equal(mapping.rule.price.rates.reasoning, 4000000);
  assert.equal(mapping.totalTiers, 2);
  assert.equal(mapping.skippedTiers, 1, '费率不完整的阶梯必须被跳过');
  assert.equal(mapping.rule.tiers.length, 1);
  assert.equal(mapping.rule.tiers[0].kind, 'context');
  assert.equal(mapping.rule.tiers[0].rates.cache_write, 2500000);
  assert.equal(catalogRuleID('opencode-go/glm-5'), 'catalog-opencode-go-glm-5');

  // 目录缺失的费率保持为「未提供」，交给编辑器要求手工填写。
  const missing = catalogEntryToRule({ id: 'anthropic/claude-x', rates: { input: 3000000, output: 15000000 } }, {});
  assert.equal(missing.rule.price.rates.cache_read, undefined);
  assert.equal(missing.rule.price.rates.cache_write, undefined);

  const note = harness().api.__test.catalogImportNote(mapping);
  assert.match(note, /可能已过期/);
  assert.match(note, /手工显式填写/);
});

/* ------------------------------------------------------------------ *
 * DOM：规则编辑器（新建 / 编辑 / 阶梯 / 确认）
 * ------------------------------------------------------------------ */

function fillBaseRates(doc, values) {
  doc.getElementById('rule-input-rate').value = values.input;
  doc.getElementById('rule-output-rate').value = values.output;
  doc.getElementById('rule-cache-read-rate').value = values.cacheRead;
  doc.getElementById('rule-cache-write-rate').value = values.cacheWrite;
  doc.getElementById('rule-reasoning-rate').value = values.reasoning;
}

test('编辑器新建规则：提交嵌套 rates，reasoning 留空保持继承', async () => {
  const { api, doc, calls, modals } = harness();
  const handle = api.__test.openRuleModal(null);
  assert.ok(handle, '必须打开编辑弹窗');

  doc.getElementById('rule-pattern').value = 'gpt-5-*';
  doc.getElementById('rule-priority').value = '20';
  fillBaseRates(doc, { input: '1111111111111111', output: '2000000', cacheRead: '0', cacheWrite: '3000000', reasoning: '' });

  footerButton(handle, '保存规则').onclick();
  await flush();

  const raw = putBodyText(calls);
  assert.ok(raw, '保存必须发起 PUT /api/pricing-rules');
  // 64 位整数必须逐字精确下发（JSON.stringify 会四舍五入）。
  assert.ok(raw.includes('"input":1111111111111111'), raw);
  for (const flat of ['input_rate', 'output_rate', 'cache_read_rate', 'cache_write_rate', 'reasoning_rate']) {
    assert.equal(raw.includes(flat), false, '请求体不得包含扁平字段 ' + flat);
  }
  const body = JSON.parse(raw);
  assert.equal(body.price.rates.output, 2000000);
  assert.equal(body.price.rates.cache_read, 0);
  assert.equal(body.price.rates.cache_write, 3000000);
  assert.equal('reasoning' in body.price.rates, false);
  assert.equal(body.match_kind, 'glob');
  assert.equal(body.enabled, true);
  assert.equal(modals[modals.length - 1].closed, true, '保存成功后关闭弹窗');
});

test('编辑器编辑规则：reasoning=null 保持继承，reasoning=0 保持显式免费', async () => {
  const inheritRun = harness();
  const inheritHandle = inheritRun.api.__test.openRuleModal({
    id: 'rule-inherit',
    match_kind: 'glob',
    pattern: 'a/*',
    priority: 5,
    enabled: true,
    price: { model: 'a/*', billing_mode: 'token', rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 } },
    tiers: [],
  });
  assert.equal(inheritRun.doc.getElementById('rule-reasoning-rate').value, '', '缺失 reasoning 必须显示为空（继承）');
  footerButton(inheritHandle, '保存规则').onclick();
  await flush();
  const inheritBody = jsonBody(inheritRun.calls);
  assert.equal('reasoning' in inheritBody.price.rates, false, '原样保存不得把继承改写成显式免费');
  assert.equal(inheritBody.price.rates.input, 1);

  const freeRun = harness();
  const freeHandle = freeRun.api.__test.openRuleModal({
    id: 'rule-free',
    match_kind: 'exact',
    pattern: 'b',
    priority: 1,
    enabled: true,
    price: { model: 'b', billing_mode: 'token', rates: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: 0 } },
    tiers: [],
  });
  assert.equal(freeRun.doc.getElementById('rule-reasoning-rate').value, '0', '显式 0 必须预填为 0');
  footerButton(freeHandle, '保存规则').onclick();
  await flush();
  const freeBody = jsonBody(freeRun.calls);
  assert.equal(freeBody.price.rates.reasoning, 0, '显式免费必须保留');
});

test('编辑大整数费率不抛异常（禁用 JSON.parse(JSON.stringify) 克隆）', async () => {
  // cmIntText 是真实响应里大整数的表示；BigInt 则保证一旦有人改回
  // JSON.parse(JSON.stringify(...)) 克隆就会立刻抛错。
  const run = harness();
  const handle = run.api.__test.openRuleModal({
    id: 'rule-big',
    match_kind: 'glob',
    pattern: 'big/*',
    priority: 1,
    enabled: true,
    price: { model: 'big/*', billing_mode: 'token', rates: { input: 9007199254740993n, output: { cmIntText: '123456789012345678' }, cache_read: 0, cache_write: 0 } },
    tiers: [],
  });
  assert.ok(handle, '大整数规则也必须能打开编辑器');
  assert.equal(run.doc.getElementById('rule-input-rate').value, '9007199254740993');
  assert.equal(run.doc.getElementById('rule-output-rate').value, '123456789012345678');
  footerButton(handle, '保存规则').onclick();
  await flush();
  const raw = putBodyText(run.calls);
  assert.ok(raw.includes('"input":9007199254740993'));
  assert.ok(raw.includes('"output":123456789012345678'));
});

test('阶梯编辑：添加/移除不丢已输入内容，cache_write 一并提交', async () => {
  const run = harness();
  const handle = run.api.__test.openRuleModal(null);
  const doc = run.doc;
  doc.getElementById('rule-pattern').value = 'tiered/*';
  doc.getElementById('rule-priority').value = '1';
  fillBaseRates(doc, { input: '1', output: '2', cacheRead: '0', cacheWrite: '0', reasoning: '' });

  const addTier = doc.querySelectorAll('.tiers-editor-head button')[0];
  addTier.onclick();
  let cards = doc.querySelectorAll('.tier-item-card');
  assert.equal(cards.length, 1);
  // 在第一张卡片里输入内容（尚未保存）。
  cards[0].querySelector('.tier-threshold').value = '64000';
  cards[0].querySelector('.tier-label').value = '已输入的标签';
  cards[0].querySelector('.tier-rate-cache-write').value = '7777777';

  // 再添加一张阶梯：重渲染绝不能丢掉第一张卡片里已输入的内容。
  addTier.onclick();
  cards = doc.querySelectorAll('.tier-item-card');
  assert.equal(cards.length, 2);
  assert.equal(cards[0].querySelector('.tier-threshold').value, '64000', '添加阶梯不得丢掉未保存输入');
  assert.equal(cards[0].querySelector('.tier-label').value, '已输入的标签');
  assert.equal(cards[0].querySelector('.tier-rate-cache-write').value, '7777777');

  // 移除第二张阶梯，第一张的数据必须保留。
  cards[1].querySelector('button').onclick();
  cards = doc.querySelectorAll('.tier-item-card');
  assert.equal(cards.length, 1);
  assert.equal(cards[0].querySelector('.tier-threshold').value, '64000');

  footerButton(handle, '保存规则').onclick();
  await flush();
  const body = jsonBody(run.calls);
  assert.equal(body.tiers.length, 1);
  assert.equal(body.tiers[0].threshold, 64000);
  assert.equal(body.tiers[0].label, '已输入的标签');
  assert.equal(body.tiers[0].rates.cache_write, 7777777, '阶梯必须提交 cache_write');
  assert.equal('reasoning' in body.tiers[0].rates, false, '阶梯思考留空 = 继承');
});

test('目录导入必须显式勾选确认后才会写入', async () => {
  const run = harness();
  const handle = run.api.__test.openRuleModal({ id: 'catalog-x', match_kind: 'exact', pattern: 'm', priority: 10, enabled: true, price: { model: 'm', rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 } }, tiers: [] }, {
    asNew: true,
    requireConfirm: true,
    note: '来自模型价格目录',
  });
  assert.ok(run.doc.getElementById('rule-confirm'), '必须渲染确认勾选框');
  assert.equal(run.doc.getElementById('rule-id').disabled, false, '导入按新建处理，规则 ID 可调');

  footerButton(handle, '保存规则').onclick();
  await flush();
  assert.equal(putBodyText(run.calls), null, '未确认时不得发起写入');
  assert.match(handle.error, /勾选确认/);

  run.doc.getElementById('rule-confirm').checked = true;
  footerButton(handle, '保存规则').onclick();
  await flush();
  const body = jsonBody(run.calls);
  assert.ok(body, '确认后必须写入');
  assert.deepEqual(Object.keys(body.price).sort(), ['billing_mode', 'model', 'per_image_micro_usd', 'rates']);
});

test('规则列表以文本节点渲染服务端数据（桩禁止 innerHTML）', async () => {
  const run = harness(() => ({
    items: [{
      id: 'rule-xss',
      match_kind: 'glob',
      pattern: '<img src=x onerror="window.__xss=1">',
      priority: 1,
      enabled: true,
      price: { model: 'x', billing_mode: 'token', rates: { input: { cmIntText: '1234567890123456' }, output: 0, cache_read: 0, cache_write: 0 } },
      tiers: [{ kind: 'context', threshold: 128000, label: '<b>t</b>', service: '', rates: { input: 0, output: 0, cache_read: 0, cache_write: 0 } }],
    }],
  }));
  const listEl = run.doc.register('pricing-rules-list', 'div');
  run.doc.register('rules-count', 'span');

  await run.api.load();
  assert.equal(listEl.querySelectorAll('table').length, 1);
  const text = textOfNode(listEl);
  assert.ok(text.includes('<img src=x'), '服务端文本必须原样显示');
  assert.ok(text.includes('1,234,567,890,123,456'), '大整数费率必须精确格式化：' + text);
  assert.ok(text.includes('上下文: >128,000'), '阶梯摘要必须展示：' + text);
  assert.ok(text.includes('继承输出'), '缺失 reasoning 必须显示为继承输出：' + text);
});

test('删除规则走结构化确认弹窗（无浏览器原生 confirm）', async () => {
  const run = harness(() => ({
    items: [{
      id: 'rule-del',
      match_kind: 'exact',
      pattern: 'del',
      priority: 1,
      enabled: true,
      price: { model: 'del', billing_mode: 'token', per_image_micro_usd: 0, rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 } },
      tiers: [],
    }],
  }));
  run.doc.register('pricing-rules-list', 'div');
  run.doc.register('rules-count', 'span');
  await run.api.load();

  const deleteBtn = run.doc.querySelectorAll('#pricing-rules-list button').find((node) => textOfNode(node).trim() === '删除');
  deleteBtn.onclick();
  await flush();

  const confirmModal = run.modals[run.modals.length - 1];
  assert.ok(run.doc.getElementById('confirm-checkbox'), '必须使用结构化确认弹窗');
  const posts = () => run.calls.filter((call) => call.path === '/api/delete-price-rule').length;

  footerButton(confirmModal, '删除').onclick();
  await flush();
  assert.equal(posts(), 0, '未勾选确认时不得删除');
  assert.match(confirmModal.error, /勾选确认/);

  run.doc.getElementById('confirm-checkbox').checked = true;
  footerButton(confirmModal, '删除').onclick();
  await flush();
  assert.equal(posts(), 1, '确认后必须发出删除请求');
  // 删除成功后 load() 会追加一次列表 GET，因此按路径取删除请求而不是取最后一条。
  const deleteCall = run.calls.filter((call) => call.path === '/api/delete-price-rule')[0];
  assert.equal(deleteCall.opts.method, 'POST');
  assert.equal(deleteCall.opts.body.id, 'rule-del');
});

/* ------------------------------------------------------------------ *
 * DOM：目录弹窗（分页 / 过期标注 / 导入）
 * ------------------------------------------------------------------ */

function catalogSnapshot(count) {
  const items = [];
  for (let index = 0; index < count; index++) {
    items.push({
      id: 'vendor' + (index % 3) + '/model-' + String(index).padStart(4, '0'),
      provider: 'vendor' + (index % 3),
      name: 'Model ' + index,
      rates: { input: 1000000 + index, output: 2000000 + index, cache_read: 100000, cache_write: 200000 },
    });
  }
  items.push({
    id: 'opencode-go/glm-5',
    provider: 'opencode-go',
    name: 'GLM-5',
    rates: { input: 1000000, output: 3000000, cache_read: 100000, cache_write: 2000000, reasoning: 4000000 },
    context_tiers: [
      { threshold: 128000, label: '128K', rates: { input: 2000000, output: 4000000, cache_read: 200000, cache_write: 2500000 } },
      { threshold: 200000, label: '200K', rates: { input: 3000000, output: 5000000 } },
    ],
  });
  items.push({ id: 'anthropic/claude-missing', provider: 'anthropic', name: 'Claude Missing', rates: { input: 3000000, output: 15000000 } });
  return {
    items,
    source: 'models.dev',
    fetched_at: '2026-10-16T00:00:00Z',
    cached: true,
    stale: true,
    error: 'models.dev: dial tcp 1.2.3.4:443: i/o timeout',
  };
}

test('目录弹窗最多渲染 50 行并如实标注过期与来源', async () => {
  const run = harness((call) => (call.path.indexOf('/api/model-catalog') === 0 ? catalogSnapshot(130) : undefined));
  const handle = run.api.__test.openCatalogModal();
  await flush();

  const rows = run.doc.querySelectorAll('.catalog-row');
  assert.equal(rows.length, 50, '132 个模型只渲染当前页 50 行');
  const status = textOfNode(run.doc.querySelector('.catalog-status'));
  assert.match(status, /上游刷新失败/);
  assert.match(status, /models\.dev/);
  assert.match(status, /i\/o timeout/);
  assert.equal(status.includes('最新'), false, '过期数据不得标注为最新');
  const pager = textOfNode(run.doc.querySelector('.catalog-pager'));
  assert.match(pager, /共 132 个模型/);
  assert.match(pager, /每页最多 50 行/);
  assert.match(pager, /第 1 \/ 3 页/);

  // 翻页不增加单页行数。
  const next = run.doc.querySelectorAll('.catalog-pager button').find((node) => textOfNode(node).trim() === '下一页');
  next.onclick();
  assert.equal(run.doc.querySelectorAll('.catalog-row').length, 50);
  assert.match(textOfNode(run.doc.querySelector('.catalog-pager')), /第 2 \/ 3 页/);

  // 搜索把候选收敛到精确模型。
  const search = run.doc.getElementById('catalog-search');
  search.value = 'opencode-go/glm-5';
  search.oninput();
  const filtered = run.doc.querySelectorAll('.catalog-row');
  assert.equal(filtered.length, 1);
  assert.ok(textOfNode(filtered[0]).includes('opencode-go/glm-5'));
  assert.ok(handle === run.modals[0]);
});

test('目录导入：精确规则带阶梯，缺失费率必须手工填写', async () => {
  const run = harness((call) => (call.path.indexOf('/api/model-catalog') === 0 ? catalogSnapshot(130) : undefined));
  run.api.__test.openCatalogModal();
  await flush();

  const selectRow = (modelID) => {
    const search = run.doc.getElementById('catalog-search');
    search.value = modelID;
    search.oninput();
    const row = run.doc.querySelectorAll('.catalog-row')[0];
    row.querySelector('button').onclick();
    return run.modals[run.modals.length - 1];
  };

  // 1) 完整费率 + 阶梯：预填后可确认导入为精确 PricingRule。
  const editor = selectRow('opencode-go/glm-5');
  assert.equal(run.doc.getElementById('rule-pattern').value, 'opencode-go/glm-5', '模型名保持 provider 限定且可编辑');
  assert.equal(run.doc.getElementById('rule-input-rate').value, '1000000');
  assert.equal(run.doc.getElementById('rule-cache-write-rate').value, '2000000');
  assert.equal(run.doc.getElementById('rule-reasoning-rate').value, '4000000');
  assert.equal(run.doc.getElementById('rule-id').value, 'catalog-opencode-go-glm-5');
  const note = textOfNode(run.doc.querySelector('.rule-source-note'));
  assert.match(note, /可能已过期/);
  assert.match(note, /1\/2 条上下文阶梯/);

  run.doc.getElementById('rule-confirm').checked = true;
  footerButton(editor, '保存规则').onclick();
  await flush();

  const body = jsonBody(run.calls);
  assert.deepEqual(body, {
    id: 'catalog-opencode-go-glm-5',
    match_kind: 'exact',
    pattern: 'opencode-go/glm-5',
    priority: 10,
    enabled: true,
    price: {
      model: 'opencode-go/glm-5',
      rates: { input: 1000000, output: 3000000, cache_read: 100000, cache_write: 2000000, reasoning: 4000000 },
      billing_mode: 'token',
      per_image_micro_usd: 0,
    },
    tiers: [{ kind: 'context', threshold: 128000, label: '128K', rates: { input: 2000000, output: 4000000, cache_read: 200000, cache_write: 2500000 } }],
  });

  // 2) 缺失费率的模型：不得静默按 0 导入，必须先显式填写。
  const putsBefore = run.calls.filter((call) => call.opts.method === 'PUT').length;
  const missingEditor = selectRow('anthropic/claude-missing');
  assert.equal(run.doc.getElementById('rule-input-rate').value, '3000000');
  assert.equal(run.doc.getElementById('rule-cache-read-rate').value, '', '缺失的缓存读费率必须留空');
  assert.equal(run.doc.getElementById('rule-cache-write-rate').value, '', '缺失的缓存写费率必须留空');
  run.doc.getElementById('rule-confirm').checked = true;
  footerButton(missingEditor, '保存规则').onclick();
  await flush();
  assert.match(missingEditor.error, /缓存读取费率 不能为空/);
  assert.equal(run.calls.filter((call) => call.opts.method === 'PUT').length, putsBefore, '缺失费率时不得写入');
});

/* ------------------------------------------------------------------ *
 * 会话生命周期：登出后在途回调一律作废（含登出后重新登录）
 * ------------------------------------------------------------------ */

function sampleRule(overrides) {
  return Object.assign({
    id: 'rule-live',
    match_kind: 'glob',
    pattern: 'live/*',
    priority: 1,
    enabled: true,
    price: {
      model: 'live/*',
      billing_mode: 'token',
      per_image_micro_usd: 0,
      rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 },
    },
    tiers: [],
  }, overrides || {});
}

// 可控的未决响应：用于模拟「请求已发出，登出后才返回」。
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function registerRuleList(doc) {
  return {
    listEl: doc.register('pricing-rules-list', 'div'),
    countEl: doc.register('rules-count', 'span'),
  };
}

test('登出后 dispose 清空模块界面并解绑按钮，重新 init 再绑定', async () => {
  const run = harness(() => ({ items: [sampleRule()] }));
  const { listEl, countEl } = registerRuleList(run.doc);
  const createBtn = run.doc.register('rules-create', 'button');
  const catalogBtn = run.doc.register('btn-load-model-catalog', 'button');
  // 页面在 DOM 就绪后才 init：这里补一次 init 模拟真实绑定顺序。
  run.api.init(run.ctx);
  assert.equal(typeof createBtn.onclick, 'function', 'init 必须绑定新增规则按钮');
  assert.equal(typeof catalogBtn.onclick, 'function', 'init 必须绑定目录按钮');

  await run.api.load();
  assert.ok(textOfNode(listEl).includes('live/*'), '加载后必须渲染列表');
  assert.equal(countEl.textContent, '共 1 条高级规则');

  run.api.dispose();
  assert.equal(listEl.textContent, '', '登出必须清空规则列表');
  assert.equal(countEl.textContent, '', '登出必须清空计数');
  assert.equal(createBtn.onclick, null, '登出必须解绑按钮');
  assert.equal(catalogBtn.onclick, null, '登出必须解绑按钮');

  const next = run.reattach();
  assert.equal(typeof createBtn.onclick, 'function', '重新登录必须重新绑定按钮');
  assert.equal(typeof catalogBtn.onclick, 'function', '重新登录必须重新绑定按钮');
  createBtn.onclick();
  assert.ok(next.modals.length >= 1, '重新绑定后按钮必须仍能打开编辑器');
});

test('登出后在途的规则列表响应不得写回界面，也不得触达新会话', async () => {
  const pending = deferred();
  const run = harness((call) => (call.path === '/api/pricing-rules' ? pending.promise : undefined));
  const { listEl, countEl } = registerRuleList(run.doc);

  const loading = run.api.load();
  assert.equal(countEl.textContent, '加载中...');

  run.api.dispose();
  const next = run.reattach();
  pending.resolve({ items: [sampleRule()] });
  await loading;
  await flush();

  assert.equal(listEl.textContent, '', '登出后到达的响应不得渲染');
  assert.equal(listEl.childNodes.length, 0);
  assert.equal(countEl.textContent, '', '登出后计数不得被改写');
  assert.equal(next.calls.length, 0, '旧回调不得为新会话发起请求');
  assert.deepEqual(next.toasts, [], '旧回调不得提示新会话');
});

test('登出后在途的规则列表错误不得渲染错误框', async () => {
  const pending = deferred();
  const run = harness((call) => (call.path === '/api/pricing-rules' ? pending.promise : undefined));
  const { listEl, countEl } = registerRuleList(run.doc);

  const loading = run.api.load();
  run.api.dispose();
  const next = run.reattach();
  pending.reject(new Error('session gone'));
  await loading;
  await flush();

  assert.equal(listEl.textContent, '', '登出后的错误不得渲染错误框');
  assert.equal(listEl.querySelectorAll('.error-box').length, 0);
  assert.equal(countEl.textContent, '', '登出后的错误不得写「加载失败」');
  assert.deepEqual(next.toasts, []);
});

test('登出后在途的目录响应不得渲染，也不得请求新会话', async () => {
  const pending = deferred();
  const run = harness((call) => (call.path.indexOf('/api/model-catalog') === 0 ? pending.promise : undefined));
  const handle = run.api.__test.openCatalogModal();
  assert.equal(run.doc.querySelectorAll('.catalog-row').length, 0);

  run.api.dispose();
  const next = run.reattach();
  pending.resolve(catalogSnapshot(3));
  await flush();

  assert.equal(run.doc.querySelectorAll('.catalog-row').length, 0, '登出后到达的目录不得渲染');
  assert.equal(handle.error, '', '登出后到达的目录不得改写已作废弹窗');
  assert.equal(textOfNode(run.doc.querySelector('.catalog-status')), '正在获取模型价格目录…');
  assert.equal(next.calls.length, 0);
});

test('登出后在途的目录错误不得改写弹窗，也不得提示新会话', async () => {
  const pending = deferred();
  const run = harness((call) => (call.path.indexOf('/api/model-catalog') === 0 ? pending.promise : undefined));
  const handle = run.api.__test.openCatalogModal();

  run.api.dispose();
  const next = run.reattach();
  pending.reject(new Error('catalog offline'));
  await flush();

  assert.equal(handle.error, '', '登出后的错误不得写入已作废弹窗');
  assert.equal(textOfNode(run.doc.querySelector('.catalog-status')), '正在获取模型价格目录…');
  assert.deepEqual(next.toasts, []);
});

test('登出后在途的保存响应不得提示/关闭弹窗/刷新新会话', async () => {
  const pending = deferred();
  const run = harness((call) => (call.opts.method === 'PUT' ? pending.promise : undefined));
  registerRuleList(run.doc);

  const handle = run.api.__test.openRuleModal(sampleRule({ id: 'rule-save', pattern: 'save/*' }));
  run.doc.getElementById('rule-pattern').value = 'save/*';
  run.doc.getElementById('rule-priority').value = '1';
  fillBaseRates(run.doc, { input: '1', output: '2', cacheRead: '0', cacheWrite: '0', reasoning: '' });
  footerButton(handle, '保存规则').onclick();
  await flush();

  run.api.dispose();
  const next = run.reattach();
  pending.resolve({});
  await flush();

  assert.equal(handle.closed, false, '登出后到达的成功响应不得关闭弹窗');
  assert.deepEqual(next.toasts, [], '登出后不得提示新会话');
  assert.equal(next.calls.length, 0, '登出后不得为新会话刷新列表');
});

test('登出后在途的保存失败不得改写已作废弹窗的报错', async () => {
  const pending = deferred();
  const run = harness((call) => (call.opts.method === 'PUT' ? pending.promise : undefined));
  registerRuleList(run.doc);

  const handle = run.api.__test.openRuleModal(sampleRule({ id: 'rule-save-2', pattern: 'save2/*' }));
  run.doc.getElementById('rule-priority').value = '1';
  fillBaseRates(run.doc, { input: '1', output: '2', cacheRead: '0', cacheWrite: '0', reasoning: '' });
  footerButton(handle, '保存规则').onclick();
  await flush();

  run.api.dispose();
  const next = run.reattach();
  pending.reject(new Error('write rejected'));
  await flush();

  assert.equal(handle.error, '', '登出后到达的失败响应不得写入已作废弹窗');
  assert.deepEqual(next.toasts, [], '登出后不得向新会话提示失败');
});

test('登出后在途的删除响应不得提示，也不得刷新新会话', async () => {
  const pending = deferred();
  const run = harness((call) => {
    if (call.path === '/api/delete-price-rule') return pending.promise;
    if (call.path === '/api/pricing-rules') return { items: [sampleRule({ id: 'rule-del-live' })] };
    return undefined;
  });
  registerRuleList(run.doc);
  await run.api.load();

  const deleteBtn = run.doc.querySelectorAll('#pricing-rules-list button').find((node) => textOfNode(node).trim() === '删除');
  deleteBtn.onclick();
  await flush();
  const confirmModal = run.modals[run.modals.length - 1];
  run.doc.getElementById('confirm-checkbox').checked = true;
  footerButton(confirmModal, '删除').onclick();
  await flush();
  assert.equal(run.calls.filter((call) => call.path === '/api/delete-price-rule').length, 1);

  run.api.dispose();
  const next = run.reattach();
  pending.resolve({});
  await flush();

  assert.deepEqual(next.toasts, [], '登出后不得提示「规则已删除」');
  assert.equal(next.calls.length, 0, '登出后不得为新会话刷新列表');
});

/* ------------------------------------------------------------------ *
 * 目录导入：同 ID upsert 覆盖确认与阶梯显式 0 的语义限制
 * ------------------------------------------------------------------ */

test('catalogEntryToRule 检测同 ID 冲突（slug 不是单射）', () => {
  const { catalogEntryToRule } = harness().api.__test;
  const entry = { id: 'a/b', rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 } };
  const mapping = catalogEntryToRule(entry, {}, [sampleRule({ id: 'other' }), sampleRule({ id: 'catalog-a-b', pattern: 'a-b' })]);
  assert.equal(mapping.rule.id, 'catalog-a-b');
  assert.ok(mapping.conflict, '必须检出同 ID 已存在的规则');
  assert.equal(mapping.conflict.pattern, 'a-b');
  assert.equal(catalogEntryToRule(entry, {}, []).conflict, null);
  assert.equal(catalogEntryToRule(entry, {}).conflict, null, '未提供既有规则时不得误报冲突');
});

test('目录阶梯的显式 0 会被标注为「继承」语义限制（缺失仍跳过不补零）', () => {
  const { catalogEntryToRule, catalogImportNote } = harness().api.__test;
  const mapping = catalogEntryToRule({
    id: 'vendor/zero-tier',
    rates: { input: 1, output: 2, cache_read: 0, cache_write: 0 },
    context_tiers: [
      { threshold: 1000, rates: { input: 0, output: 2, cache_read: 0, cache_write: 0 } },
      { threshold: 2000, rates: { input: 3, output: 4, cache_read: 1, cache_write: 1 } },
      { threshold: 3000, rates: { input: 3, output: 4 } },
    ],
  }, { source: 'models.dev' });

  assert.equal(mapping.rule.tiers.length, 2, '费率不完整的阶梯必须跳过');
  assert.equal(mapping.skippedTiers, 1);
  assert.equal(mapping.zeroTiers, 1, '显式 0 的阶梯必须被单独统计');
  assert.equal(mapping.rule.tiers[0].rates.input, 0, '显式 0 不得被静默丢弃');

  const note = catalogImportNote(mapping);
  assert.match(note, /upsert/);
  assert.match(note, /相同 ID 已存在的规则会被覆盖/);
  assert.match(note, /显式 0/);
  assert.match(note, /继承基础费率/);
});

test('目录导入命中同 ID 冲突时必须额外确认覆盖，未冲突则无需确认', async () => {
  const existing = sampleRule({ id: 'catalog-opencode-go-glm-5', pattern: 'opencode-go/glm-5' });
  const run = harness((call) => {
    if (call.path.indexOf('/api/model-catalog') === 0) return catalogSnapshot(3);
    if (call.path === '/api/pricing-rules') return { items: [existing] };
    return undefined;
  });
  registerRuleList(run.doc);
  await run.api.load();
  run.api.__test.openCatalogModal();
  await flush();

  const pick = (modelID) => {
    const search = run.doc.getElementById('catalog-search');
    search.value = modelID;
    search.oninput();
    run.doc.querySelectorAll('.catalog-row')[0].querySelector('button').onclick();
    return run.modals[run.modals.length - 1];
  };

  // 1) 无冲突：不渲染覆盖确认，导入提示仍需说明按 ID upsert 会覆盖同 ID。
  const clean = pick('vendor0/model-0000');
  assert.equal(run.doc.getElementById('rule-overwrite'), null, '无冲突不得要求覆盖确认');
  assert.match(textOfNode(clean.body[0].querySelector('.rule-source-note')), /upsert/);
  assert.equal(clean.body[0].querySelector('.rule-overwrite'), null);

  // 2) 同 ID 冲突：必须额外勾选覆盖确认才允许写入。
  const editor = pick('opencode-go/glm-5');
  const overwriteBox = run.doc.getElementById('rule-overwrite');
  assert.ok(overwriteBox, '同 ID 冲突必须渲染覆盖确认');
  assert.match(textOfNode(editor.body[0].querySelector('.rule-source-note')), /已存在同 ID 的规则/);
  assert.equal(run.doc.getElementById('rule-id').value, 'catalog-opencode-go-glm-5');

  run.doc.getElementById('rule-confirm').checked = true;
  footerButton(editor, '保存规则').onclick();
  await flush();
  assert.equal(putBodyText(run.calls), null, '未确认覆盖时不得写入');
  assert.match(editor.error, /覆盖/);

  overwriteBox.checked = true;
  footerButton(editor, '保存规则').onclick();
  await flush();
  const body = jsonBody(run.calls);
  assert.ok(body, '确认覆盖后必须写入');
  assert.equal(body.id, 'catalog-opencode-go-glm-5', '覆盖写入沿用同一 ID');
  assert.equal(body.pattern, 'opencode-go/glm-5');
});
