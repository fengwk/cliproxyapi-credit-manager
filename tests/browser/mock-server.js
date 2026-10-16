'use strict';

/*
 * credit-manager 浏览器验收用的本地 mock 服务（无第三方依赖）。
 * - 只读取仓库内的 assets 目录，不接触任何真实 profile、数据库或 CPA 实例。
 * - 单进程、仅监听 loopback，便于隔离与清理。
 * 用法：node mock-server.js [--port 0] [--assets <dir>] [--key cm-mock-key]
 */

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const args = process.argv.slice(2);
function argValue(name, fallback) {
  const index = args.indexOf(name);
  return index >= 0 && args[index + 1] ? args[index + 1] : fallback;
}

const ASSETS_DIR = path.resolve(
  argValue('--assets', path.resolve(__dirname, '..', '..', 'internal', 'web', 'assets'))
);
const MANAGEMENT_KEY = argValue('--key', 'cm-mock-key');
const RESOURCE_PREFIX = '/v0/resource/plugins/credit-manager';
const MANAGER_BASE = '/v0/management/plugins/credit-manager';

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'application/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
};

// 超出 Number 精度的 64 位整数在 JS 里无法用字面量精确表达，
// 因此 fixtures 用占位符字符串承载，序列化时再还原为裸数字（等价于 Go 端直接输出 int64）。
const BIG_COST_TEXT = '123456789012345678'; // 18 位，用于验证无损显示
const BIG_TOKENS_TEXT = '9007199254740993'; // 2^53 + 1
const big = (digits) => '\u00abBIG:' + digits + '\u00bb';
const BIG_COST = big(BIG_COST_TEXT);
const BIG_TOKENS = big(BIG_TOKENS_TEXT);

// 用于验证界面把服务端数据当作文本渲染（不会被解析成 HTML）。
const XSS_MODEL = '<img src=x onerror="window.__xss=1">';

const PRICES = [
  {
    model: 'gpt-5',
    rates: { input: 1234567, output: 2000000, cache_read: 200000, cache_write: 2500000 },
    version: 3,
  },
  {
    model: 'opencode-go/glm-5',
    rates: { input: 1000000, output: 3000000, reasoning: 4000000, cache_read: 100000, cache_write: 2000000 },
    version: 1,
  },
  { model: 'free-model', rates: { input: 0, output: 0, cache_read: 0, cache_write: 0 }, version: 1 },
];

const KEYS = [
  {
    id: 'key-1',
    principal: 'cmk:11111111-1111-1111-1111-111111111111',
    scope: 'scope-1',
    fingerprint: 'fp-abcd1234',
    spec: {
      label: '生产主力',
      enabled: true,
      quota_micro_usd: 50000000,
      daily_micro_usd: 0,
      weekly_micro_usd: 0,
      monthly_micro_usd: 10000000,
      max_concurrent: 4,
      allowed_models: ['opencode-go/glm-5', 'gpt-5'],
      model_token_limits: [{ model: 'gpt-5', total: 1000000, daily: 0, weekly: 0, monthly: 0 }],
      expires_at: '2026-12-31T00:00:00Z',
      future_policy: 'keep-me',
    },
    spent_micro_usd: 12345678,
    held_micro_usd: 6000000,
    created_at: '2026-01-01T00:00:00Z',
  },
  {
    id: 'key-2',
    principal: 'cmk:22222222-2222-2222-2222-222222222222',
    scope: 'scope-2',
    fingerprint: 'fp-efgh5678',
    spec: {
      label: '内部试用',
      enabled: false,
      quota_micro_usd: 0,
      daily_micro_usd: 500000,
      weekly_micro_usd: 0,
      monthly_micro_usd: 0,
      max_concurrent: 0,
      allowed_models: [],
    },
    spent_micro_usd: 0,
    held_micro_usd: 0,
    created_at: '2026-02-02T00:00:00Z',
  },
];

const CALLERS = [
  {
    id: 'caller-1',
    display_name: '研发一部',
    enabled: true,
    created_at: '2026-01-01T00:00:00Z',
  },
  {
    id: 'caller-2',
    display_name: '数据分析团队',
    enabled: true,
    created_at: '2026-02-01T00:00:00Z',
  },
];

const AUDIT_EVENTS = [
  {
    seq: 1,
    at: '2026-10-15T10:00:00Z',
    action: 'create_key',
    key_id: 'key-1',
    caller_id: 'caller-1',
    reason: '初始化系统',
    detail: '创建生产主力 Key',
  },
  {
    seq: 2,
    at: '2026-10-15T11:00:00Z',
    action: 'reset_spend',
    key_id: 'key-1',
    caller_id: 'caller-1',
    reason: '月度周期刷新',
    detail: '重置月度额度基线',
  },
];

// 高级定价规则 fixture：与 internal/credit.PricingRule 的真实 JSON 完全一致——
// 费率嵌套在 price.rates 下，阶梯挂在规则顶层且自带嵌套 rates。
// 扁平别名（input_rate/...）在这里会被 PUT 校验直接拒绝，因此不能出现在 fixture 中。
const PRICING_RULES = [
  {
    id: 'rule-1',
    match_kind: 'glob',
    pattern: 'opencode-go/*',
    priority: 10,
    enabled: true,
    price: {
      model: 'opencode-go/*',
      billing_mode: 'token',
      per_image_micro_usd: 0,
      // reasoning 缺失 = 按输出单价（继承）；显式 0 才是「思考免费」。
      rates: { input: 1000000, output: 3000000, cache_read: 100000, cache_write: 2000000 },
    },
    tiers: [
      {
        kind: 'context',
        threshold: 128000,
        label: '长上下文',
        service: '',
        rates: { input: 2000000, output: 4000000, cache_read: 0, cache_write: 0 },
      },
    ],
  },
  {
    id: 'rule-image',
    match_kind: 'exact',
    pattern: 'gpt-image-1',
    priority: 20,
    enabled: true,
    price: {
      model: 'gpt-image-1',
      billing_mode: 'per_image',
      per_image_micro_usd: 50000,
      rates: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
    },
    tiers: [],
  },
  {
    id: 'rule-free-reasoning',
    match_kind: 'regexp',
    pattern: '^free-.*$',
    priority: 1,
    enabled: false,
    price: {
      model: '^free-.*$',
      billing_mode: 'token',
      per_image_micro_usd: 0,
      rates: { input: 0, output: 0, reasoning: 0, cache_read: 0, cache_write: 0 },
    },
    tiers: [],
  },
];

// 模型目录 fixture：真实后端返回 catalog.Snapshot{items,source,fetched_at,cached,stale,error}。
// 条目数刻意超过一页（每页最多 50 行），用于验证界面不会一次性渲染近万个模型；
// stale=true 用于验证界面如实标注「缓存数据可能已过期」而不是谎称「最新」。
const CATALOG_MODELS = (() => {
  const items = [];
  for (let index = 0; index < 130; index++) {
    items.push({
      id: 'vendor' + (index % 3) + '/model-' + String(index).padStart(4, '0'),
      provider: 'vendor' + (index % 3),
      name: 'Model ' + index,
      rates: { input: 1000000 + index, output: 2000000 + index, cache_read: 100000, cache_write: 200000 },
    });
  }
  items.push({
    // provider 限定名必须原样保留（不得自动剥离 provider 前缀）。
    id: 'opencode-go/glm-5',
    provider: 'opencode-go',
    name: 'GLM-5',
    rates: { input: 1000000, output: 3000000, cache_read: 100000, cache_write: 2000000, reasoning: 4000000 },
    context_tiers: [
      { threshold: 128000, label: '128K', rates: { input: 2000000, output: 4000000, cache_read: 200000, cache_write: 2500000 } },
      // 费率不完整的阶梯不得被导入（缺失 != 0）。
      { threshold: 200000, label: '200K', rates: { input: 3000000, output: 5000000 } },
    ],
  });
  items.push({
    // 缺失 cache_read/cache_write：导入时必须要求手工显式填写，绝不静默补 0。
    id: 'anthropic/claude-missing-rates',
    provider: 'anthropic',
    name: 'Claude Missing Rate',
    rates: { input: 3000000, output: 15000000 },
  });
  return items;
})();

const CATALOG_SNAPSHOT = {
  items: CATALOG_MODELS,
  source: 'models.dev',
  fetched_at: '2026-10-16T00:00:00Z',
  cached: true,
  stale: true,
  error: 'models.dev: dial tcp 1.2.3.4:443: i/o timeout',
};

const REQUESTS_PAGE_1 = [
  {
    id: 'req-001',
    trace_id: 'trace001',
    key_id: 'key-1',
    model: 'opencode-go/glm-5',
    outcome: 'succeeded',
    state: 'settled',
    started_at: '2026-10-15T10:00:00Z',
    completed_at: '2026-10-15T10:00:06Z',
    held_micro_usd: 6000000,
    price: PRICES[1],
    executions: [
      {
        id: 'exec-001',
        request_id: 'req-001',
        trace_id: 'trace001',
        key_id: 'key-1',
        provider: 'opencode-go',
        auth_id: 'auth-open-primary',
        auth_index: '0',
        executor: 'opencode-go',
        model: 'opencode-go/glm-5',
        response_model: 'glm-5',
        tokens: {
          input: 12000,
          cache_read: 4000,
          cache_write: 0,
          output: 8000,
          reasoning: 3000,
          total: BIG_TOKENS,
        },
        quality: 'complete',
        cost_micro_usd: BIG_COST,
        failed: false,
        status_code: 200,
        at: '2026-10-15T10:00:06Z',
        latency_ms: 4321,
        ttft_ms: 812,
      },
    ],
  },
  {
    id: 'req-002',
    trace_id: 'trace002',
    key_id: 'key-2',
    model: 'commandcode/claude-sonnet-5',
    outcome: 'failed',
    state: 'settled',
    started_at: '2026-10-15T11:00:00Z',
    completed_at: '2026-10-15T11:00:02Z',
    held_micro_usd: 0,
    price: PRICES[2],
    executions: [
      {
        id: 'exec-002',
        request_id: 'req-002',
        trace_id: 'trace002',
        key_id: 'key-2',
        provider: 'commandcode',
        auth_id: 'auth-cmd',
        auth_index: '1',
        executor: 'commandcode',
        model: 'commandcode/claude-sonnet-5',
        response_model: 'commandcode/native/claude-sonnet-5',
        tokens: { input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0, total: 0 },
        quality: 'unknown',
        failed: true,
        status_code: 429,
        at: '2026-10-15T11:00:02Z',
        latency_ms: 1200,
        ttft_ms: 0,
      },
    ],
  },
  {
    id: 'req-003',
    trace_id: 'trace003',
    key_id: 'key-1',
    model: 'gpt-5',
    state: 'pending',
    started_at: '2026-10-15T12:00:00Z',
    held_micro_usd: 6000000,
    price: PRICES[0],
    executions: [],
  },
  {
    id: 'req-005',
    trace_id: 'trace005',
    key_id: 'key-1',
    model: 'gpt-5',
    outcome: 'succeeded',
    state: 'settled',
    started_at: '2026-10-13T08:00:00Z',
    completed_at: '2026-10-13T08:00:02Z',
    held_micro_usd: 0,
    // 管理员 Resolve 的人工记账必须与执行成本分开显示（契约字段：adjustment_micro_usd）。
    adjustment_micro_usd: 2500000,
    price: PRICES[0],
    executions: [
      {
        id: 'exec-005',
        request_id: 'req-005',
        trace_id: 'trace005',
        key_id: 'key-1',
        provider: 'openai',
        auth_id: 'auth-openai',
        auth_index: '2',
        executor: 'openai',
        model: 'gpt-5',
        tokens: { input: 100, cache_read: 0, cache_write: 0, output: 200, reasoning: 0, total: 300 },
        quality: 'partial',
        cost_micro_usd: 1233500,
        failed: false,
        status_code: 200,
        at: '2026-10-13T08:00:02Z',
        latency_ms: 1500,
        ttft_ms: 210,
      },
    ],
  },
  {
    id: 'req-006',
    trace_id: 'trace006',
    key_id: 'key-1',
    // 模型名与标签都是服务端数据，必须原样以文本呈现，绝不能被当作 HTML 解析。
    model: XSS_MODEL,
    outcome: 'rejected',
    state: 'settled',
    started_at: '2026-10-15T13:00:00Z',
    completed_at: '2026-10-15T13:00:00Z',
    held_micro_usd: 0,
    price: PRICES[0],
    executions: [],
  },
  {
    // 进行中的请求：仍占用并发与持有金额，后端 Resolve 只接受 pending，因此不得给出人工处理入口。
    id: 'req-007',
    trace_id: 'trace007',
    key_id: 'key-1',
    model: 'gpt-5',
    state: 'active',
    started_at: '2026-10-15T14:00:00Z',
    held_micro_usd: 1500000,
    adjustment_micro_usd: 0,
    price: PRICES[0],
    executions: [],
  },
];

const REQUESTS_PAGE_2 = [
  {
    id: 'req-004',
    trace_id: 'trace004',
    key_id: 'key-1',
    model: 'gpt-5',
    outcome: 'succeeded',
    state: 'settled',
    started_at: '2026-10-14T09:00:00Z',
    completed_at: '2026-10-14T09:00:03Z',
    held_micro_usd: 0,
    price: PRICES[0],
    executions: [
      {
        id: 'exec-004',
        request_id: 'req-004',
        trace_id: 'trace004',
        key_id: 'key-1',
        provider: 'openai',
        auth_id: 'auth-openai',
        auth_index: '2',
        executor: 'openai',
        model: 'gpt-5',
        tokens: { input: 500, cache_read: 0, cache_write: 0, output: 700, reasoning: 0, total: 1200 },
        quality: 'complete',
        cost_micro_usd: 1234500,
        failed: false,
        status_code: 200,
        at: '2026-10-14T09:00:03Z',
        latency_ms: 2100,
        ttft_ms: 320,
      },
    ],
  },
];

// 直接手写文本，确保 64 位整数逐字精确（JSON.stringify 会先丢精度）。
const CAPTURE_BODY =
  '{"model":"opencode-go/glm-5","stream":true,"max_tokens":' + BIG_TOKENS_TEXT +
  ',"metadata":{"request_cost_micro_usd":' + BIG_COST_TEXT +
  ',"note":"大整数必须逐字保留，含 } 与 \\" 字符"}}';

const HEALTH = {
  status: 'degraded',
  version: 'v0.1.0-mock',
  capture: {
    database_bytes: 52428800,
    wal_bytes: 1048576,
    queue_bytes: 0,
    dropped: 3,
    last_error: 'mock 捕获队列曾达到上限',
  },
  retention_months: 3,
  capture_enabled: true,
  last_error: 'mock 财务维护失败：cleanup 未完成',
  // 真实 handler 会在同一响应里附带插件配置；界面必须把它当作未知字段原样列出。
  config: {
    data_dir: '/tmp/cm-mock-data',
    retention_months: 3,
    max_pending_requests: 128,
    capture: { enabled: true, retention: '24h', max_body_bytes: 1048576, max_storage_bytes: 1073741824 },
  },
};

const CREDENTIALS = {
  files: [
    {
      id: 'cred-open',
      auth_index: '0',
      name: 'opencode-go-primary.json',
      provider: 'opencode-go',
      type: 'opencode-go',
      status: 'active',
      disabled: false,
      unavailable: false,
      account: 'team-pro',
      email: 'ops@example.com',
      supports_quota: true,
      quota_provider: 'opencode-go',
      quota_plugin_id: 'z-quota',
      quota: { observed_at: '2026-10-15T09:00:00Z', signals: { exceeded: 'false' } },
    },
    {
      id: 'cred-cmd',
      auth_index: '1',
      name: 'commandcode.json',
      provider: 'commandcode',
      type: 'commandcode',
      status: 'error',
      status_message: '上游 429 冷却中',
      disabled: false,
      unavailable: true,
      next_retry_after: '2026-10-16T02:00:00Z',
      supports_quota: true,
      quota_provider: 'commandcode',
      quota_plugin_id: 'z-quota-broken',
      quota: { observed_at: '2026-10-15T09:00:00Z', signals: {} },
    },
    {
      id: 'cred-plain',
      auth_index: '2',
      name: 'plain-custom.json',
      provider: 'custom',
      type: 'custom',
      status: 'active',
      disabled: false,
      unavailable: false,
      quota: {},
    },
  ],
};

const QUOTA_GOOD = {
  subscription: { plan: 'max-20x', tierName: 'Max', tierId: 'tier-9' },
  summary: [
    { key: 'balance', label: '余额', value: 150.25, format: 'currency', currency: 'USD' },
    { key: 'requests_today', label: '今日请求', value: 1234, format: 'number', unit: '次' },
  ],
  groups: [
    {
      displayName: '月度额度',
      buckets: [
        { window: 'monthly', remainingFraction: 0.7342, resetTime: '2026-11-01T00:00:00Z', description: '自然月额度' },
        { window: 'weekly', remainingFraction: 0.02, resetTime: '2026-10-19T00:00:00Z', description: '周额度' },
      ],
    },
  ],
  serverTimeOffsetMs: 1200,
};

const recorded = { usage: [], last: {}, hits: [] };

/* ------------------------------------------------------------------ *
 * 严格 schema 校验
 *
 * 真实后端 internal/web/handler.go 的 decode 使用 DisallowUnknownFields，
 * 且 credit.normalizePricingRule 要求嵌套费率、非负整数与合法的阶梯。
 * mock 在这里用同一套约束拒绝请求，任何扁平费率别名或多余字段都无法
 * 蒙混过关——未被测试覆盖的契约不匹配会直接表现为 4xx。
 * ------------------------------------------------------------------ */

const RULE_KEYS = ['id', 'match_kind', 'pattern', 'priority', 'enabled', 'price', 'tiers'];
const PRICE_KEYS = ['model', 'rates', 'billing_mode', 'per_image_micro_usd', 'version'];
const RATE_KEYS = ['input', 'output', 'cache_read', 'cache_write', 'reasoning'];
const TIER_KEYS = ['kind', 'threshold', 'service', 'label', 'rates'];
const REQUIRED_RATE_KEYS = ['input', 'output', 'cache_read', 'cache_write'];

function isInt(value) {
  return typeof value === 'number' && Number.isInteger(value);
}

function firstUnknownKey(obj, allowed) {
  for (const key of Object.keys(obj)) {
    if (allowed.indexOf(key) < 0) return key;
  }
  return '';
}

function validateTier(tier) {
  if (!tier || typeof tier !== 'object' || Array.isArray(tier)) {
    return 'invalid: price tier must be a JSON object';
  }
  const unknown = firstUnknownKey(tier, TIER_KEYS);
  if (unknown) return 'invalid: json: unknown field "' + unknown + '"';
  if (tier.kind !== 'context' && tier.kind !== 'service') {
    return 'invalid: unknown price tier kind ' + JSON.stringify(tier.kind);
  }
  if (!tier.rates || typeof tier.rates !== 'object' || Array.isArray(tier.rates)) {
    return 'invalid: price tier rates are required';
  }
  const unknownRate = firstUnknownKey(tier.rates, RATE_KEYS);
  if (unknownRate) return 'invalid: json: unknown field "' + unknownRate + '"';
  for (const key of RATE_KEYS) {
    const value = tier.rates[key];
    if (value === undefined || value === null) continue;
    if (!isInt(value) || value < 0) {
      return 'invalid: price tier rate ' + key + ' must be a non-negative integer';
    }
  }
  if (tier.kind === 'context') {
    if (!isInt(tier.threshold) || tier.threshold <= 0) {
      return 'invalid: context price tier threshold must be positive';
    }
  } else if (typeof tier.service !== 'string' || tier.service.trim() === '') {
    return 'invalid: service price tier must name a service';
  }
  return '';
}

// 返回错误字符串；空串表示请求体合法。
function validatePricingRule(rule) {
  if (!rule || typeof rule !== 'object' || Array.isArray(rule)) {
    return 'invalid: pricing rule must be a JSON object';
  }
  const unknown = firstUnknownKey(rule, RULE_KEYS);
  if (unknown) return 'invalid: json: unknown field "' + unknown + '"';
  if (typeof rule.id !== 'string' || rule.id.trim() === '') return 'invalid: pricing rule id is required';
  if (typeof rule.pattern !== 'string' || rule.pattern.trim() === '') return 'invalid: pricing rule pattern is required';
  if (rule.match_kind !== undefined && ['exact', 'glob', 'regexp'].indexOf(rule.match_kind) < 0) {
    return 'invalid: unknown match kind ' + JSON.stringify(rule.match_kind);
  }
  if (rule.priority !== undefined && !isInt(rule.priority)) return 'invalid: priority must be an integer';
  if (rule.enabled !== undefined && typeof rule.enabled !== 'boolean') return 'invalid: enabled must be a boolean';

  const price = rule.price;
  if (!price || typeof price !== 'object' || Array.isArray(price)) return 'invalid: price is required';
  const unknownPrice = firstUnknownKey(price, PRICE_KEYS);
  if (unknownPrice) return 'invalid: json: unknown field "' + unknownPrice + '"';

  // 费率必须嵌套在 price.rates 下：扁平别名会在这里被明确拒绝。
  if (!price.rates || typeof price.rates !== 'object' || Array.isArray(price.rates)) {
    return 'invalid: price.rates is required (flat rate fields are not accepted)';
  }
  const unknownRate = firstUnknownKey(price.rates, RATE_KEYS);
  if (unknownRate) return 'invalid: json: unknown field "' + unknownRate + '"';
  for (const key of REQUIRED_RATE_KEYS) {
    if (!isInt(price.rates[key]) || price.rates[key] < 0) {
      return 'invalid: price.rates.' + key + ' must be a non-negative integer';
    }
  }
  if (price.rates.reasoning !== undefined && price.rates.reasoning !== null) {
    if (!isInt(price.rates.reasoning) || price.rates.reasoning < 0) {
      return 'invalid: price.rates.reasoning must be a non-negative integer or null';
    }
  }
  if (price.billing_mode !== undefined && ['', 'token', 'per_image'].indexOf(price.billing_mode) < 0) {
    return 'invalid: unknown billing mode ' + JSON.stringify(price.billing_mode);
  }
  if (price.per_image_micro_usd !== undefined && (!isInt(price.per_image_micro_usd) || price.per_image_micro_usd < 0)) {
    return 'invalid: price.per_image_micro_usd must be a non-negative integer';
  }
  if (price.billing_mode === 'per_image' && !price.per_image_micro_usd) {
    return 'invalid: a per-image price requires per_image_micro_usd';
  }

  const tiers = rule.tiers === undefined ? [] : rule.tiers;
  if (!Array.isArray(tiers)) return 'invalid: tiers must be an array';
  if (tiers.length > 16) return 'invalid: at most 16 price tiers';
  for (const tier of tiers) {
    const tierError = validateTier(tier);
    if (tierError) return tierError;
  }
  return '';
}

// 模拟 scanPricingRule/normalizePricingRule 的规范化结果：price.model 由 pattern 决定。
function canonicalPricingRule(rule) {
  const stored = JSON.parse(JSON.stringify(rule));
  stored.match_kind = stored.match_kind || 'exact';
  stored.enabled = stored.enabled !== false;
  stored.tiers = stored.tiers || [];
  stored.price.model = stored.pattern;
  stored.price.billing_mode = stored.price.billing_mode === 'per_image' ? 'per_image' : 'token';
  return stored;
}

function serialize(payload) {
  return JSON.stringify(payload).replace(/"\u00abBIG:(\d+)\u00bb"/g, '$1');
}

function json(res, status, payload) {
  const body = serialize(payload);
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' });
  res.end(body);
}

function requireAuth(req, res) {
  const header = String(req.headers.authorization || '');
  if (header === 'Bearer ' + MANAGEMENT_KEY) return true;
  json(res, header ? 401 : 401, { error: header ? 'invalid management key' : 'missing management key' });
  return false;
}

function readBody(req) {
  return new Promise((resolve) => {
    let data = '';
    req.on('data', (chunk) => {
      data += chunk;
    });
    req.on('end', () => resolve(data));
  });
}

// 与 credit.validateMaterial 保持一致：只有空值才生成新的密钥材料；带首尾空白或
// 控制字符的自定义材料一律拒绝，绝不静默裁剪，否则调用方会拿到一个与输入不同的密钥。
function validateCustomMaterial(value) {
  if (typeof value !== 'string' || value === '') return null;
  if (value.trim() !== value) return 'invalid: key material must not have leading or trailing whitespace';
  if (value.length < 8) return 'invalid: key material is too short';
  if (/[\u0000-\u001f\u007f]/.test(value)) return 'invalid: key material must not contain control characters';
  return null;
}

function usageReport(query) {
  const page = Number(query.get('page') || '1');
  const pageSize = Number(query.get('page_size') || '20');
  const search = String(query.get('search') || '');
  const model = String(query.get('model') || '');
  const items = page <= 1 ? REQUESTS_PAGE_1 : page === 2 ? REQUESTS_PAGE_2 : [];
  const empty = model.includes('__none__');
  return {
    summary: empty
      ? {
          requests: 0,
          executions: 0,
          failed: 0,
          pending: 0,
          cost_micro_usd: 0,
          tokens: { input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0, total: 0 },
          complete_executions: 0,
        }
      : {
          requests: 6,
          executions: 4,
          // 契约：failed 是失败请求数（failed/canceled/rejected），pending 是 active+pending 的未结算请求数。
          failed: 2,
          pending: 2,
          cost_micro_usd: BIG_COST,
          adjustment_micro_usd: 2500000,
          tokens: { input: 12500, cache_read: 4000, cache_write: 0, output: 8700, reasoning: 3000, total: BIG_TOKENS },
          complete_executions: 2,
          cache_read_ratio: 0.2424,
        },
    trend: empty
      ? []
      : [
          { time: '2026-10-12T00:00:00Z', requests: 3, executions: 5, failed: 0, pending: 0, cost_micro_usd: 1200000, adjustment_micro_usd: 0, tokens: { total: 1000 } },
          { time: '2026-10-13T00:00:00Z', requests: 7, executions: 9, failed: 2, pending: 1, cost_micro_usd: 5600000, adjustment_micro_usd: 2500000, tokens: { total: 2000 } },
          { time: '2026-10-14T00:00:00Z', requests: 4, executions: 4, failed: 0, pending: 0, cost_micro_usd: BIG_COST, adjustment_micro_usd: 0, tokens: { total: 3000 } },
          { time: '2026-10-15T00:00:00Z', requests: 2, executions: 1, failed: 1, pending: 1, cost_micro_usd: 0, adjustment_micro_usd: 0, tokens: { total: 4000 } },
        ],
    items: empty ? [] : items,
    total: empty ? 0 : 25,
    page,
    page_size: pageSize,
    retention_from: '2026-07-01T00:00:00Z',
    mock_search: search,
  };
}

async function handle(req, res) {
  const url = new URL(req.url, 'http://127.0.0.1');
  const pathname = url.pathname;

  if (!pathname.startsWith('/__debug/')) {
    recorded.hits.push(req.method + ' ' + pathname + url.search);
  }

  if (pathname === '/__debug/last') {
    json(res, 200, recorded);
    return;
  }
  if (pathname === '/__debug/hits') {
    json(res, 200, { hits: recorded.hits.slice() });
    return;
  }
  if (pathname === '/__debug/reset') {
    recorded.usage = [];
    recorded.last = {};
    recorded.hits = [];
    json(res, 200, { ok: true });
    return;
  }

  if (pathname.startsWith(RESOURCE_PREFIX + '/')) {
    const name = pathname.slice(RESOURCE_PREFIX.length + 1);
    const file = path.join(ASSETS_DIR, name);
    if (!file.startsWith(ASSETS_DIR) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
      json(res, 404, { error: 'resource not found' });
      return;
    }
    res.writeHead(200, {
      'Content-Type': CONTENT_TYPES[path.extname(file)] || 'application/octet-stream',
      'Cache-Control': 'no-store',
    });
    res.end(fs.readFileSync(file));
    return;
  }

  if (pathname === '/v8/management/credentials') {
    if (!requireAuth(req, res)) return;
    json(res, 200, CREDENTIALS);
    return;
  }

  if (pathname.startsWith('/v8/management/plugins/') && pathname.endsWith('/quota')) {
    if (!requireAuth(req, res)) return;
    const pluginID = pathname.slice('/v8/management/plugins/'.length, -'/quota'.length);
    recorded.last['quota:' + pluginID] = { body: await readBody(req) };
    if (pluginID === 'z-quota') {
      json(res, 200, QUOTA_GOOD);
      return;
    }
    json(res, 502, { error: 'failed to fetch quota: mock upstream timeout' });
    return;
  }

  if (pathname.startsWith(MANAGER_BASE)) {
    if (!requireAuth(req, res)) return;
    const endpoint = pathname.slice(MANAGER_BASE.length);
    const query = url.searchParams;

    if (endpoint === '/api/health') {
      json(res, 200, HEALTH);
      return;
    }
    if (endpoint === '/api/keys' && req.method === 'GET') {
      json(res, 200, { items: KEYS });
      return;
    }
    if (endpoint === '/api/keys' && req.method === 'POST') {
      const body = await readBody(req);
      recorded.last['keys:create'] = { body };
      let parsed = null;
      try {
        parsed = JSON.parse(body);
      } catch (err) {
        json(res, 400, { error: 'invalid: request body must be JSON' });
        return;
      }
      const materialError = validateCustomMaterial(parsed && parsed.key);
      if (materialError) {
        json(res, 400, { error: materialError });
        return;
      }
      json(res, 201, { item: { id: 'key-new', spec: parsed, spent_micro_usd: 0, held_micro_usd: 0 }, key: 'sk-mock-onetime-5f3a9c' });
      return;
    }
    if (endpoint === '/api/keys' && req.method === 'PUT') {
      const body = await readBody(req);
      recorded.last['keys:update'] = { body };
      const updated = JSON.parse(body);
      // 更新接口不得回传密钥材料（handler.go 对 credit.ErrInvalid 的处理）。
      if (updated && updated.spec && updated.spec.key) {
        json(res, 400, { error: 'invalid: key material must not be updated' });
        return;
      }
      json(res, 200, { item: { id: 'key-1', spec: updated.spec } });
      return;
    }
    if (endpoint === '/api/rotate') {
      const body = await readBody(req);
      recorded.last['keys:rotate'] = { body };
      let parsed = null;
      try {
        parsed = JSON.parse(body);
      } catch (err) {
        json(res, 400, { error: 'invalid: request body must be JSON' });
        return;
      }
      const materialError = validateCustomMaterial(parsed && parsed.key);
      if (materialError) {
        json(res, 400, { error: materialError });
        return;
      }
      json(res, 200, { item: { id: 'key-1' }, key: 'sk-mock-rotated-9c2b41' });
      return;
    }
    if (endpoint === '/api/prices' && req.method === 'GET') {
      json(res, 200, { items: PRICES });
      return;
    }
    if (endpoint === '/api/prices' && req.method === 'PUT') {
      const body = await readBody(req);
      recorded.last['prices:put'] = { body };
      json(res, 200, { ok: true });
      return;
    }
    if (endpoint === '/api/usage') {
      recorded.usage.push(url.search);
      recorded.last['usage'] = url.search;
      if (String(query.get('search') || '').includes('__boom__')) {
        json(res, 500, { error: 'mock 故障：使用统计暂不可用' });
        return;
      }
      json(res, 200, usageReport(query));
      return;
    }
    if (endpoint === '/api/capture') {
      const id = String(query.get('id') || '');
      if (id === 'req-001') {
        json(res, 200, {
          request_id: id,
          // 服务端真实状态取值来自 internal/capture：captured/disabled/oversize/dropped/expired/unavailable。
          status: 'captured',
          headers: {
            'Content-Type': 'application/json',
            'X-Api-Key': 'REDACTED',
            'X-Goog-Api-Key': 'REDACTED',
            'Content-Length': '1234',
          },
          body: CAPTURE_BODY,
          at: '2026-10-15T10:00:00Z',
          expires_at: '2026-10-16T10:00:00Z',
        });
        return;
      }
      if (id === 'req-002') {
        json(res, 200, { request_id: id, status: 'unavailable', at: '2026-10-15T11:00:00Z', expires_at: '2026-10-16T11:00:00Z' });
        return;
      }
      if (id === 'req-003') {
        json(res, 200, { request_id: id, status: 'disabled', at: '2026-10-15T12:00:00Z', expires_at: '2026-10-16T12:00:00Z' });
        return;
      }
      json(res, 404, { error: 'capture snapshot not found' });
      return;
    }
    if (endpoint === '/api/resolve') {
      const body = await readBody(req);
      recorded.last['resolve'] = { body };
      json(res, 200, { ok: true });
      return;
    }
    if (endpoint === '/api/overview') {
      recorded.last['overview'] = url.search;
      json(res, 200, {
        summary: {
          requests: 6,
          executions: 7,
          failed: 1,
          pending: 1,
          cost_micro_usd: 1250000000000000,
          adjustment_micro_usd: 500000,
          tokens: 2800000000000000,
          complete_executions: 6,
          cache_read_ratio: 0.25,
        },
        trend: [
          { bucket: '2026-10-14T00:00:00Z', requests: 2, executions: 2, failed: 0, pending: 0, cost_micro_usd: 400000, adjustment_micro_usd: 0, tokens: 1000000 },
          { bucket: '2026-10-15T00:00:00Z', requests: 4, executions: 5, failed: 1, pending: 1, cost_micro_usd: 850000, adjustment_micro_usd: 500000, tokens: 1800000 },
        ],
        models: [
          {
            model: 'opencode-go/glm-5',
            requests: 4,
            executions: 5,
            failed: 0,
            pending: 0,
            cost_micro_usd: 800000000000000,
            adjustment_micro_usd: 0,
            tokens: 1800000000000000,
            complete_executions: 5,
            cache_read_ratio: 0.35,
            latency_ms: 6000,
            latency_samples: 4,
            ttft_ms: 1200,
            ttft_samples: 4,
            output_tokens: 500000000000000,
            measured_output_tokens: 500000000000000,
          },
          {
            model: 'gpt-5',
            requests: 2,
            executions: 2,
            failed: 1,
            pending: 1,
            cost_micro_usd: 450000000000000,
            adjustment_micro_usd: 500000,
            tokens: 1000000000000000,
            complete_executions: 1,
            cache_read_ratio: 0.15,
            latency_ms: 3500,
            latency_samples: 1,
            ttft_ms: 800,
            ttft_samples: 1,
            output_tokens: 300000000000000,
            measured_output_tokens: 300000000000000,
          },
        ],
        retention_from: '2026-07-01T00:00:00Z',
      });
      return;
    }
    if (endpoint === '/api/reveal' && req.method === 'POST') {
      const body = await readBody(req);
      const parsed = JSON.parse(body || '{}');
      if (parsed.id === 'key-legacy') {
        json(res, 409, { error: 'legacy key cannot be revealed, rotate to encrypted' });
        return;
      }
      json(res, 200, { id: parsed.id, key: 'sk-revealed-' + (parsed.id || 'secret') });
      return;
    }
    if (endpoint === '/api/delete-key' && req.method === 'POST') {
      const body = await readBody(req);
      const parsed = JSON.parse(body || '{}');
      recorded.last['delete-key'] = parsed;
      const k = KEYS.find((item) => item.id === parsed.id);
      if (k) k.deleted_at = new Date().toISOString();
      json(res, 200, { ok: true });
      return;
    }
    if (endpoint === '/api/reset-spend' && req.method === 'POST') {
      const body = await readBody(req);
      recorded.last['reset-spend'] = JSON.parse(body || '{}');
      json(res, 200, { keys: 1, windows: ['daily', 'weekly', 'monthly', 'total'] });
      return;
    }
    if (endpoint === '/api/callers' && req.method === 'GET') {
      json(res, 200, { items: CALLERS });
      return;
    }
    if (endpoint === '/api/callers' && req.method === 'POST') {
      const body = await readBody(req);
      const parsed = JSON.parse(body || '{}');
      const caller = {
        id: parsed.id || 'caller-' + (CALLERS.length + 1),
        display_name: parsed.display_name || '',
        enabled: parsed.enabled !== false,
        created_at: new Date().toISOString(),
      };
      CALLERS.push(caller);
      json(res, 201, { item: caller });
      return;
    }
    if (endpoint === '/api/callers' && req.method === 'PUT') {
      const body = await readBody(req);
      const parsed = JSON.parse(body || '{}');
      const caller = CALLERS.find((c) => c.id === parsed.id);
      if (caller) {
        if (parsed.display_name !== undefined) caller.display_name = parsed.display_name;
        if (parsed.enabled !== undefined) caller.enabled = parsed.enabled;
      }
      json(res, 200, { item: caller || parsed });
      return;
    }
    if (endpoint === '/api/audit' && req.method === 'GET') {
      json(res, 200, { items: AUDIT_EVENTS, total: AUDIT_EVENTS.length, page: 1, page_size: 20 });
      return;
    }
    if (endpoint === '/api/pricing-rules' && req.method === 'GET') {
      json(res, 200, { items: PRICING_RULES });
      return;
    }
    if (endpoint === '/api/pricing-rules' && req.method === 'PUT') {
      const body = await readBody(req);
      recorded.last['rules:put'] = { body };
      let parsed = null;
      try {
        parsed = JSON.parse(body);
      } catch (err) {
        json(res, 400, { error: 'invalid: request body must be JSON' });
        return;
      }
      const invalid = validatePricingRule(parsed);
      if (invalid) {
        json(res, 400, { error: invalid });
        return;
      }
      const stored = canonicalPricingRule(parsed);
      const at = PRICING_RULES.findIndex((rule) => rule.id === stored.id);
      if (at >= 0) PRICING_RULES[at] = stored;
      else PRICING_RULES.push(stored);
      json(res, 200, { item: stored });
      return;
    }
    if (endpoint === '/api/delete-price-rule' && req.method === 'POST') {
      const body = await readBody(req);
      const parsed = JSON.parse(body || '{}');
      const idx = PRICING_RULES.findIndex((r) => r.id === parsed.id);
      if (idx >= 0) PRICING_RULES.splice(idx, 1);
      json(res, 200, { ok: true });
      return;
    }
    if (endpoint === '/api/model-catalog') {
      // 只读目录：真实后端返回 catalog.Snapshot，且绝不会因为一次拉取就改写价格。
      recorded.last['model-catalog'] = { refresh: query.get('refresh') };
      json(res, 200, CATALOG_SNAPSHOT);
      return;
    }
    if (endpoint === '/api/exchange-rate') {
      json(res, 200, { rate: 7.25, currency: 'CNY', updated_at: '2026-10-16T12:00:00Z' });
      return;
    }
    json(res, 404, { error: 'unknown endpoint' });
    return;
  }

  json(res, 404, { error: 'not found' });
}

function start() {
  const port = Number(argValue('--port', '0'));
  const server = http.createServer((req, res) => {
    handle(req, res).catch((error) => {
      json(res, 500, { error: 'mock server error: ' + error.message });
    });
  });
  server.listen(port, '127.0.0.1', () => {
    const address = server.address();
    process.stdout.write(JSON.stringify({ port: address.port, assets: ASSETS_DIR }) + '\n');
  });
  return server;
}

if (require.main === module) {
  start();
}

module.exports = { start, MANAGEMENT_KEY, RESOURCE_PREFIX, BIG_COST_TEXT, BIG_TOKENS_TEXT, CAPTURE_BODY, XSS_MODEL, PRICES, KEYS };
