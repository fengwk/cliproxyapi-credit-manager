'use strict';

// app.js 纯逻辑单元测试：查询参数一致性、金额/整数精度、无损 JSON、命名空间、请求体构造。
const test = require('node:test');
const assert = require('node:assert/strict');
const { loadCM } = require('./helpers');

const CM = loadCM();

test('buildUsageQuery 覆盖过滤器并省略空值', () => {
  const now = new Date('2026-01-02T03:04:05Z');
  const filters = CM.defaultUsageFilters(now);
  const query = CM.buildUsageQuery(filters);
  const params = new URLSearchParams(query);
  assert.equal(params.get('bucket'), 'day');
  assert.equal(params.get('page'), '1');
  assert.equal(params.get('page_size'), '20');
  assert.equal(params.get('key_id'), null);
  assert.equal(params.get('model'), null);
  assert.equal(params.get('result'), null);
  assert.equal(params.get('search'), null);
  // from/to 由本地 datetime-local 转 RFC3339，跨度必须是 24 小时。
  const from = new Date(params.get('from'));
  const to = new Date(params.get('to'));
  assert.equal(to.getTime() - from.getTime(), 24 * 60 * 60 * 1000);
  assert.match(params.get('from'), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
});

test('buildUsageQuery 映射每个过滤器字段名', () => {
  const query = CM.buildUsageQuery({
    from: '2026-01-01T00:00',
    to: '2026-01-02T00:00',
    key: 'key-1',
    model: 'opencode-go/glm-5',
    provider: 'opencode-go',
    auth: 'auth-9',
    result: 'failed',
    search: 'trace 123',
    bucket: 'hour',
    page: 3,
    pageSize: 50,
  });
  const params = new URLSearchParams(query);
  assert.equal(params.get('key_id'), 'key-1');
  assert.equal(params.get('model'), 'opencode-go/glm-5');
  assert.equal(params.get('provider'), 'opencode-go');
  assert.equal(params.get('auth_id'), 'auth-9');
  assert.equal(params.get('result'), 'failed');
  assert.equal(params.get('search'), 'trace 123');
  assert.equal(params.get('bucket'), 'hour');
  assert.equal(params.get('page'), '3');
  assert.equal(params.get('page_size'), '50');
  // 字段顺序固定，便于比对后端契约。
  assert.deepEqual([...params.keys()], [
    'from', 'to', 'key_id', 'model', 'provider', 'auth_id', 'result', 'search', 'bucket', 'page', 'page_size',
  ]);
});

test('buildUsageQuery 缺省与非法分页/粒度回退', () => {
  const query = CM.buildUsageQuery({ page: 'abc', pageSize: 0, bucket: 'week' });
  const params = new URLSearchParams(query);
  assert.equal(params.get('page'), '1');
  assert.equal(params.get('page_size'), '20');
  assert.equal(params.get('bucket'), 'day');
});

test('分页切换保留其余过滤器', () => {
  const filters = {
    from: '2026-01-01T00:00',
    to: '2026-01-02T00:00',
    model: 'gpt-5',
    result: 'success',
    bucket: 'day',
    page: 2,
    pageSize: 50,
  };
  const first = new URLSearchParams(CM.buildUsageQuery(filters));
  const second = new URLSearchParams(CM.buildUsageQuery({ ...filters, page: 3 }));
  for (const key of ['from', 'to', 'model', 'result', 'bucket', 'page_size']) {
    assert.equal(second.get(key), first.get(key), `分页后 ${key} 应保持不变`);
  }
  assert.equal(second.get('page'), '3');
});

test('快捷范围按当前时间回推并同步输入框', () => {
  // datetime-local 精确到分钟，期望值同样截断到分钟。
  const now = new Date('2026-03-04T05:06:07Z');
  const minute = new Date(Math.floor(now.getTime() / 60000) * 60000);
  const range = CM.quickRangeFilters('7d', now);
  assert.equal(range.quick, '7d');
  assert.equal(range.to, CM.localDatetimeValue(minute));
  assert.equal(range.from, CM.localDatetimeValue(new Date(minute.getTime() - 7 * 24 * 60 * 60 * 1000)));
  assert.equal(CM.quickRangeFilters('nope', now), null);
});

test('微美元与美元之间精确互转，无浮点误差', () => {
  assert.equal(CM.microToUSDText(1000000), '1');
  assert.equal(CM.microToUSDText(1), '0.000001');
  assert.equal(CM.microToUSDText(1234567), '1.234567');
  assert.equal(CM.microToUSDText(500000), '0.5');
  assert.equal(CM.microToUSDText(0), '0');
  assert.equal(CM.microToUSDText(-1500000), '-1.5');
  assert.equal(CM.microToUSDText('9007199254740993'), '9007199254.740993');
  assert.equal(CM.microToUSDText('123456789012345678901'), '123456789012345.678901');
  assert.equal(CM.microToUSDText('abc'), null);

  // 0.1 USD 这类无法用二进制浮点精确表示的金额必须得到精确微美元整数。
  assert.deepEqual(CM.usdToMicroUSD('0.1'), { ok: true, micro: '100000' });
  assert.deepEqual(CM.usdToMicroUSD('1.25'), { ok: true, micro: '1250000' });
  assert.deepEqual(CM.usdToMicroUSD('1'), { ok: true, micro: '1000000' });
  assert.deepEqual(CM.usdToMicroUSD('  .5 '), { ok: true, micro: '500000' });
  assert.deepEqual(CM.usdToMicroUSD('-2.5'), { ok: true, micro: '-2500000' });
  assert.equal(CM.usdToMicroUSD('1.2345678').ok, false, '超过微美元精度的美元输入必须报错');
  assert.equal(CM.usdToMicroUSD('abc').ok, false);
  assert.equal(CM.usdToMicroUSD('').ok, false);

  for (const micro of ['0', '1', '999999', '1000000', '123456789', '4503599627370497']) {
    const usd = CM.microToUSDText(micro);
    assert.equal(CM.usdToMicroUSD(usd).micro, micro, `往返转换必须精确: ${micro}`);
  }
});

test('formatInt 处理超出 Number 精度的整数且不使用科学计数法', () => {
  assert.equal(CM.formatInt(1234567), '1,234,567');
  assert.equal(CM.formatInt(0), '0');
  assert.equal(CM.formatInt(-9876543), '-9,876,543');
  assert.equal(CM.formatInt('9007199254740993'), '9,007,199,254,740,993');
  assert.equal(CM.formatInt(CM.intVal('12345678901234567890')), '12,345,678,901,234,567,890');
  assert.equal(CM.formatInt(1e21), '1,000,000,000,000,000,000,000');
  assert.ok(!CM.formatInt(1e21).includes('e'));
});

test('parseJSON 保留超精度整数，exactStringify 不破坏数字', () => {
  const parsed = CM.parseJSON('{"cost_micro_usd":12345678901234567890,"tokens":{"total":9007199254740993}}');
  assert.ok(CM.isIntVal(parsed.cost_micro_usd));
  assert.equal(CM.intString(parsed.cost_micro_usd), '12345678901234567890');
  assert.equal(CM.intString(parsed.tokens.total), '9007199254740993');
  const roundTrip = CM.exactStringify(parsed);
  assert.equal(roundTrip, '{"cost_micro_usd":12345678901234567890,"tokens":{"total":9007199254740993}}');
  assert.ok(!roundTrip.includes('e+'));

  // 同形字符串字段不能被误认为整数。
  const text = CM.parseJSON('{"id":"12345678901234567890"}');
  assert.equal(typeof text.id, 'string');
  assert.equal(text.id, '12345678901234567890');

  // 普通小数与布尔、null 保持原语义。
  const mixed = CM.parseJSON('{"ratio":0.5,"ok":true,"none":null}');
  assert.equal(mixed.ratio, 0.5);
  assert.equal(mixed.ok, true);
  assert.equal(mixed.none, null);
});

test('prettyJSONLossless 词法美化，逐字保留数字与字符串', () => {
  const body = '{"model":"gpt-5","n":12345678901234567890,"s":"a}b\\"c","arr":[1,{"x":2}],"empty":{}}';
  const pretty = CM.prettyJSONLossless(body);
  assert.ok(pretty.includes('12345678901234567890'), '大整数必须逐字保留');
  assert.ok(pretty.includes('"a}b\\"c"'), '字符串内的括号与转义必须逐字保留');
  assert.ok(pretty.includes('\n'), '应包含换行');
  assert.equal(pretty.replace(/\s+/g, ''), body.replace(/\s+/g, ''), '去除空白后应与原文一致');

  const formatted = CM.formatBodyLossless(body);
  assert.equal(formatted.ok, true);
  assert.ok(formatted.text.includes('12345678901234567890'));

  const invalid = CM.formatBodyLossless('{oops');
  assert.equal(invalid.ok, false);
  assert.ok(invalid.reason.includes('JSON'));
  assert.equal(CM.formatBodyLossless('   ').ok, false);
  assert.equal(CM.formatBodyLossless('[1, 2]').text, '[\n  1,\n  2\n]');
});

test('命名空间仅做显示归一，未知前缀不删除', () => {
  assert.deepEqual(CM.splitNamespace('opencode-go/glm-5'), { prefix: 'opencode-go/', bare: 'glm-5' });
  assert.deepEqual(CM.splitNamespace('commandcode/claude-sonnet-5'), {
    prefix: 'commandcode/',
    bare: 'claude-sonnet-5',
  });
  assert.deepEqual(CM.splitNamespace('vendor-x/thing'), { prefix: '', bare: 'vendor-x/thing' });
  assert.deepEqual(CM.splitNamespace(''), { prefix: '', bare: '' });

  const normalized = CM.modelRelation('opencode-go/gpt-5', 'gpt-5', '');
  assert.equal(normalized.namespaceOnly, true);
  assert.ok(normalized.note.includes('gpt-5'));
  assert.deepEqual(normalized.namespaces, ['opencode-go/']);

  const different = CM.modelRelation('commandcode/claude-sonnet-5', 'commandcode/native/claude-sonnet-5', '');
  assert.equal(different.namespaceOnly, false, 'native 子命名空间属于不同模型，不应视为仅命名空间差异');
  assert.equal(different.note, '');

  const unknown = CM.modelRelation('vendor-x/a', 'vendor-y/a', '');
  assert.equal(unknown.namespaceOnly, false);

  const single = CM.modelRelation('gpt-5', '', '');
  assert.equal(single.namespaceOnly, false);
  assert.equal(single.note, '');
});

test('KeySpec 请求体使用精确整数并保留既有字段', () => {
  const spec = CM.keySpecPayload({
    label: '测试',
    enabled: true,
    quotaMicroUSD: '1234567',
    dailyMicroUSD: '0',
    weeklyMicroUSD: '0',
    monthlyMicroUSD: '9007199254740993',
    maxConcurrent: '2',
    allowedModels: ['gpt-5'],
    modelTokenLimits: [{ model: 'gpt-5', total: '1000', daily: '0', weekly: '0', monthly: '0' }],
    expiresAt: '2026-06-01T00:00:00Z',
  });
  const body = CM.exactStringify(spec);
  assert.ok(body.includes('"quota_micro_usd":1234567'));
  assert.ok(body.includes('"monthly_micro_usd":9007199254740993'));
  assert.ok(body.includes('"expires_at":"2026-06-01T00:00:00Z"'));
  assert.ok(body.includes('"max_concurrent":2'));
  assert.ok(!body.includes('e+'));

  // 清空 token 限额 / 过期时间时必须显式下发 null，否则合并会保留旧限制。
  const cleared = CM.exactStringify(
    CM.keySpecPayload({
      label: 'x',
      enabled: true,
      quotaMicroUSD: '0',
      dailyMicroUSD: '0',
      weeklyMicroUSD: '0',
      monthlyMicroUSD: '0',
      maxConcurrent: '0',
      allowedModels: [],
      modelTokenLimits: null,
      expiresAt: '',
    })
  );
  assert.ok(cleared.includes('"model_token_limits":null'));
  assert.ok(cleared.includes('"expires_at":null'));

  const raw = CM.parseJSON(
    '{"label":"旧","enabled":false,"quota_micro_usd":10,"allowed_models":["a"],' +
      '"model_token_limits":[{"model":"a","total":5}],"future_field":{"keep":true},"key":"sk-secret"}'
  );
  const merged = CM.mergeKeySpec(raw, { label: '新', enabled: true });
  assert.equal(merged.label, '新');
  assert.equal(merged.enabled, true);
  assert.deepEqual(merged.allowed_models, ['a'], '未编辑字段必须原样保留');
  assert.deepEqual(merged.model_token_limits, [{ model: 'a', total: 5 }]);
  assert.deepEqual(merged.future_field, { keep: true }, '未知字段必须保留，避免 PUT 丢失策略');
  assert.equal('key' in merged, false, 'PUT 不得回传 key 材料');
  assert.equal(CM.intString(merged.quota_micro_usd), '10');
});

test('model_token_limits 解析与回显', () => {
  assert.deepEqual(CM.parseModelTokenLimits(''), { ok: true, value: null });
  const ok = CM.parseModelTokenLimits('[{"model":"gpt-5","total":100,"daily":10}]');
  assert.equal(ok.ok, true);
  assert.equal(ok.value.length, 1);
  assert.equal(CM.parseModelTokenLimits('{"model":"x"}').ok, false);
  assert.equal(CM.parseModelTokenLimits('[{oops}').ok, false);
  assert.equal(CM.parseModelTokenLimits('[{"total":1}]').ok, false);
  assert.equal(CM.parseModelTokenLimits('[{"model":"x","total":"many"}]').ok, false);
  assert.equal(
    CM.modelTokenLimitsToText([{ model: 'x', total: '9007199254740993' }]).includes('9007199254740993'),
    true
  );
  assert.deepEqual(CM.splitAllowedModels('a\nb, c\n\n'), ['a', 'b', 'c']);
});

test('价格请求体：思考留空则不下发该字段', () => {
  const withReasoning = CM.exactStringify(
    CM.pricePayload({ model: 'gpt-5', input: '1000000', output: '2000000', reasoning: '3000000', cacheRead: '0', cacheWrite: '0', version: '3' })
  );
  assert.ok(withReasoning.includes('"reasoning":3000000'));
  assert.ok(withReasoning.includes('"version":3'));
  const without = CM.exactStringify(
    CM.pricePayload({ model: 'gpt-5', input: '1', output: '2', reasoning: '', cacheRead: '3', cacheWrite: '4' })
  );
  assert.ok(!without.includes('reasoning'));
  assert.ok(without.includes('"version":0'));
});

test('价格缺省与免费状态不混淆', () => {
  assert.equal(CM.priceMissing(null), true);
  assert.equal(CM.priceMissing({}), true);
  assert.equal(CM.priceMissing({ model: 'gpt-5', rates: {} }), false);
  assert.equal(CM.ratesSummaryText({ input: 0, output: 0, cache_read: 0, cache_write: 0 }), '显式全零：免费');
  assert.equal(CM.ratesSummaryText({ input: 0, output: 1, cache_read: 0, cache_write: 0 }), null);
  assert.equal(CM.ratesSummaryText(null), '无价格记录（拒绝计费）');
  assert.equal(CM.reasoningRateText({ output: 5 }), '缺失（按输出计费）');
  assert.equal(CM.reasoningRateText({ output: 5, reasoning: '1234567' }), '1,234,567 µUSD / 1M');
});

test('额度指标与桶渲染', () => {
  assert.match(CM.metricValueText({ key: 'balance', value: 12.5, format: 'currency', currency: 'USD' }), /12\.5/);
  assert.equal(CM.metricValueText({ key: 'requests', value: 1234, format: 'number', unit: '次' }), '1,234 次');
  assert.equal(CM.metricValueText({ key: 'x', value: 'nope' }), '—');
  assert.equal(CM.metricValueText({ key: 'x', value: Infinity }), '—');
  assert.match(CM.metricValueText({ key: 'x', value: 1, format: 'currency', currency: 'NOTACODE' }), /NOTACODE/);
  assert.equal(CM.bucketPercent(0.5), '50%');
  assert.equal(CM.bucketPercent(1), '100%');
  assert.equal(CM.bucketPercent(0.1234), '12.3%');
  assert.equal(CM.bucketPercent(undefined), '—');
  assert.equal(CM.bucketTone(0.02), 'danger');
  assert.equal(CM.bucketTone(0.2), 'warn');
  assert.equal(CM.bucketTone(0.9), '');
});

test('结果/状态/质量标签与语气与契约枚举一致', () => {
  // outcome / state / quality 的取值集合必须与 credit 契约完全一致，且不再兼容别名。
  assert.equal(CM.outcomeLabel('succeeded'), '成功');
  assert.equal(CM.outcomeLabel('failed'), '失败');
  assert.equal(CM.outcomeLabel('rejected'), '拒绝');
  assert.equal(CM.outcomeLabel('canceled'), '取消');
  for (const alias of ['success', 'ok', 'failure', 'error', 'pending', 'cancelled', 'charge']) {
    assert.equal(CM.outcomeLabel(alias), alias, `别名 ${alias} 不应再被翻译`);
  }
  assert.equal(CM.outcomeLabel('mystery'), 'mystery');
  assert.equal(CM.outcomeLabel(''), '未知');
  assert.equal(CM.outcomeLabel(undefined), '未知');

  assert.equal(CM.stateLabel('active'), '进行中');
  assert.equal(CM.stateLabel('pending'), '待定');
  assert.equal(CM.stateLabel('settled'), '已结算');
  assert.equal(CM.stateLabel('complete'), 'complete', '旧状态值不应再被翻译');
  assert.equal(CM.stateLabel(''), '—');

  assert.equal(CM.qualityLabel('complete'), '完整');
  assert.equal(CM.qualityLabel('partial'), '部分');
  assert.equal(CM.qualityLabel('unknown'), '未知');
  assert.equal(CM.qualityLabel(undefined), '未知');
  assert.equal(CM.qualityLabel('inconsistent'), 'inconsistent', '旧质量值不应再被翻译');
  assert.equal(CM.qualityLabel('unclassified'), 'unclassified');

  assert.equal(CM.outcomeTone('succeeded'), 'ok');
  assert.equal(CM.outcomeTone('failed'), 'danger');
  assert.equal(CM.outcomeTone('rejected'), 'warn');
  assert.equal(CM.outcomeTone('canceled'), '');
  assert.equal(CM.outcomeTone('success'), '', '别名不再有语气映射');
  assert.equal(CM.qualityTone('complete'), 'ok');
  assert.equal(CM.qualityTone('partial'), 'warn');
  assert.equal(CM.qualityTone('unknown'), 'warn');

  // CPA 凭据状态（sdk/cliproxy/auth 的 Status 常量）。
  assert.deepEqual(CM.credentialStatus('active'), { label: '正常', tone: 'ok' });
  assert.deepEqual(CM.credentialStatus('error'), { label: '异常', tone: 'danger' });
  assert.deepEqual(CM.credentialStatus('disabled'), { label: '已禁用', tone: 'warn' });
  assert.equal(CM.credentialStatus('refreshing').tone, 'warn');
  assert.deepEqual(CM.credentialStatus(''), { label: '未知', tone: '' });
  assert.deepEqual(CM.credentialStatus('weird'), { label: 'weird', tone: '' });
});

test('只有 pending 状态的请求才提示管理员处理', () => {
  // state=pending 必须可处理；active（进行中）与 settled（已结算）不应出现处理入口。
  assert.equal(CM.isPendingRequest({ state: 'pending', executions: [] }), true);
  assert.equal(CM.isPendingRequest({ state: 'active', executions: [] }), false);
  assert.equal(CM.isPendingRequest({ state: 'settled', executions: [] }), false);
  assert.equal(CM.isPendingRequest({ state: 'PENDING' }), true);
  // 状态缺失时按保守规则判断：没有任何执行记录且未完成才视作待定。
  assert.equal(CM.isPendingRequest({ executions: [] }), true);
  assert.equal(CM.isPendingRequest({ executions: [], completed_at: '2026-01-01T00:00:00Z' }), false);
  assert.equal(CM.isPendingRequest({ executions: [{ id: 'e1' }] }), false);
  assert.equal(CM.isPendingRequest(null), false);
  assert.equal(CM.isPendingRequest(undefined), false);
});

test('字节数、时长与时间格式化', () => {
  assert.equal(CM.formatBytes(0), '0 B');
  assert.equal(CM.formatBytes(2048), '2.00 KiB');
  assert.equal(CM.formatBytes(5 * 1024 * 1024), '5.00 MiB');
  assert.equal(CM.formatDurationMS(0), '0 ms');
  assert.equal(CM.formatDurationMS(1500), '1.50 s');
  assert.equal(CM.formatDurationMS(null), '—');
  assert.equal(CM.formatDateTime(''), '—');
  assert.equal(CM.formatDateTime('not-a-date'), 'not-a-date');
  assert.match(CM.formatDateTime('2026-01-02T03:04:05Z'), /^2026-01-02 \d{2}:\d{2}:\d{2}$/);
});

test('请求体解析失败时截断错误文本', () => {
  const long = 'x'.repeat(1000);
  assert.equal(CM.truncateText(long).length, 301);
  assert.equal(CM.truncateText('ok'), 'ok');
});
