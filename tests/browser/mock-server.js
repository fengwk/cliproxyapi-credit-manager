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
      json(res, 201, { item: { id: 'key-new', spec: JSON.parse(body), spent_micro_usd: 0, held_micro_usd: 0 }, key: 'sk-mock-onetime-5f3a9c' });
      return;
    }
    if (endpoint === '/api/keys' && req.method === 'PUT') {
      const body = await readBody(req);
      recorded.last['keys:update'] = { body };
      json(res, 200, { item: { id: 'key-1', spec: JSON.parse(body).spec } });
      return;
    }
    if (endpoint === '/api/rotate') {
      const body = await readBody(req);
      recorded.last['keys:rotate'] = { body };
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
          status: 'ok',
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
