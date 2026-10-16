'use strict';

// charts.js 单元测试：精确整数/金额、Token 分解与单位、tooltip 安全与精度、
// 空/零状态、实例复用、主题跟随 data-palette、resize/dispose 无监听泄漏。
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { ASSETS_DIR } = require('./helpers');

const CMCharts = require(path.join(ASSETS_DIR, 'charts.js'));

// --- 测试替身 ---------------------------------------------------------------

function fakeElement(id, palette) {
  return {
    id: id,
    _palette: palette || null,
    textContent: '',
    getAttribute(name) {
      return name === 'data-palette' ? this._palette : null;
    },
    parentElement: null,
  };
}

// 记录 init/setOption/resize/dispose 的假 echarts；init 只按元素记录实例，便于断言复用。
function fakeEcharts() {
  const log = [];
  const instances = [];
  const echarts = {
    init(el, theme, opts) {
      const instance = {
        el: el,
        theme: theme,
        initOptions: opts,
        options: [],
        resizes: 0,
        disposed: false,
      };
      instance.setOption = (option, notMerge) => {
        instance.options.push({ option: option, notMerge: notMerge });
        log.push({ type: 'setOption', id: el.id });
      };
      instance.resize = () => {
        instance.resizes++;
        log.push({ type: 'resize', id: el.id });
      };
      instance.dispose = () => {
        instance.disposed = true;
        log.push({ type: 'dispose', id: el.id });
      };
      log.push({ type: 'init', id: el.id, renderer: opts && opts.renderer });
      instances.push(instance);
      return instance;
    },
    // 暴露给断言
    __log: log,
    __instances: instances,
    __count(type) {
      return log.filter((entry) => entry.type === type).length;
    },
  };
  return echarts;
}

function fakeResizeObserver(bucket) {
  return function FakeResizeObserver(callback) {
    this.callback = callback;
    this.observe = (el) => bucket.push({ type: 'observe', id: el.id });
    this.unobserve = (el) => bucket.push({ type: 'unobserve', id: el.id });
    this.disconnect = () => bucket.push({ type: 'disconnect' });
  };
}

function deepFreeze(value) {
  if (value && typeof value === 'object' && !Object.isFrozen(value)) {
    Object.freeze(value);
    Object.keys(value).forEach((key) => deepFreeze(value[key]));
  }
  return value;
}

// 时间戳一律带 Z：服务端 bucket 是 UTC，断言也必须是 UTC，才能在任意 TZ 下成立
// （测试在 TZ=America/Los_Angeles 下运行以验证这一点）。
const POINTS = [
  {
    time: '2026-01-02T03:04:05Z',
    requests: 2,
    executions: 3,
    failed: 1,
    pending: 0,
    cost_micro_usd: '1250000',
    adjustment_micro_usd: '500000',
    tokens: { input: 1000, output: 2000, cache_read: 300, cache_write: 40, reasoning: 50, total: 3390 },
  },
  {
    time: '2026-01-02T04:04:05Z',
    requests: 0,
    executions: 1,
    failed: 0,
    pending: 2,
    cost_micro_usd: '9007199254740993',
    adjustment_micro_usd: '0',
    tokens: {
      input: { cmIntText: '9007199254740993' },
      output: 0,
      cache_read: 0,
      cache_write: 0,
      reasoning: 7,
      total: { cmIntText: '9007199254741000' },
    },
  },
];

function findNonFinite(value, pathStr) {
  if (typeof value === 'number') return isFinite(value) ? null : pathStr;
  if (Array.isArray(value)) {
    for (let i = 0; i < value.length; i++) {
      const found = findNonFinite(value[i], pathStr + '[' + i + ']');
      if (found) return found;
    }
    return null;
  }
  if (value && typeof value === 'object') {
    for (const key of Object.keys(value)) {
      const found = findNonFinite(value[key], pathStr + '.' + key);
      if (found) return found;
    }
  }
  return null;
}

// --- 精确整数与金额 ---------------------------------------------------------

test('intText/formatInt 保留超出 Number 精度的大整数', () => {
  const huge = { cmIntText: '9007199254740993' };
  assert.equal(CMCharts.intText(huge), '9007199254740993');
  assert.equal(CMCharts.intText(42), '42');
  assert.equal(CMCharts.intText(null), '');
  assert.equal(CMCharts.formatInt(huge), '9,007,199,254,740,993');
  // 非整数文本原样返回，绝不改写。
  assert.equal(CMCharts.formatInt('n/a'), 'n/a');
});

test('金额：USD 微美元精确文本、CNY 按 rate 精确换算', () => {
  assert.equal(CMCharts.formatMoneyMicro('1250000', 'USD'), '1.25 USD');
  assert.equal(CMCharts.formatMoneyMicro('1', 'USD'), '0.000001 USD');
  assert.equal(CMCharts.formatMoneyMicro('0', 'USD'), '0 USD');
  assert.equal(CMCharts.formatMoneyMicro('9007199254740993', 'USD'), '9007199254.740993 USD');
  // CNY：1_250_000 µUSD = 1.25 USD，1.25 * 7.1 = 8.875，全程十进制字符串。
  assert.equal(CMCharts.formatMoneyMicro('1250000', 'CNY', 7.1), '8.875 CNY');
  assert.equal(CMCharts.multiplyDecimal('1.25', '7.1', 6), '8.875');
  // 非法 rate 回退为 1，绝不产生 NaN/空串。
  assert.equal(CMCharts.formatMoneyMicro('1000000', 'CNY', 0), '1 CNY');
  assert.equal(CMCharts.formatMoneyMicro('1000000', 'CNY', NaN), '1 CNY');
});

test('带符号十进制乘法保留负数符号（half away from zero）', () => {
  assert.equal(CMCharts.multiplyDecimal('-1.25', '7.1', 6), '-8.875');
  assert.equal(CMCharts.multiplyDecimal('1.25', '-7.1', 6), '-8.875');
  assert.equal(CMCharts.multiplyDecimal('-1.25', '-7.1', 6), '8.875');
  assert.equal(CMCharts.multiplyDecimal('-0.000001', '7.1', 6), '-0.000007');
  assert.equal(CMCharts.multiplyDecimal('-1.25', '7.1', 2), '-8.88'); // 负值进位方向正确
  assert.equal(CMCharts.multiplyDecimal('0', '-7.1', 6), '0'); // 零不带负号
  // 负微美元金额：CNY 换算不得丢号。
  assert.equal(CMCharts.formatMoneyMicro('-1250000', 'CNY', 7.1), '-8.875 CNY');
  assert.equal(CMCharts.formatMoneyMicro('-1250000', 'USD'), '-1.25 USD');
});

test('rate 支持科学计数法展开，非法/超界/负数一律有界回退', () => {
  // '1e-7' 展开为 0.0000001，不再静默返回 '—'。
  assert.equal(CMCharts.formatMoneyMicro('1250000', 'CNY', '1e-7'), '0 CNY'); // 1.25e-7 四舍五入到 6 位 = 0
  assert.equal(CMCharts.formatMoneyMicro('1250000', 'CNY', 1e3), '1250 CNY'); // 1.25 * 1000（金额文本不带千分位）
  assert.equal(CMCharts.formatMoneyMicro('1250000', 'CNY', '1.5E+2'), '187.5 CNY');
  // 非法/超界/负数/零：回退为 1，输出始终是合法的 CNY 文本（绝不 '—'、绝不 NaN）。
  for (const bad of [-7, '0', '1e400', '1e-400', 'abc', '', null, undefined, NaN, Infinity]) {
    const text = CMCharts.formatMoneyMicro('1250000', 'CNY', bad);
    assert.equal(text, '1.25 CNY', 'rate=' + String(bad));
  }
});

test('Token 单位换算精确，raw 千分位，k/wan/m 保留 2 位小数', () => {
  assert.equal(CMCharts.formatTokenCount('12345678', 'raw'), '12,345,678');
  assert.equal(CMCharts.formatTokenCount('12345678', 'k'), '12,345.68');
  assert.equal(CMCharts.formatTokenCount('12345678', 'wan'), '1,234.57');
  assert.equal(CMCharts.formatTokenCount('12345678', 'm'), '12.35');
  assert.equal(CMCharts.formatTokenCount('0', 'k'), '0');
  assert.equal(CMCharts.tokenText('1500', 'k'), '1.5K');
  assert.equal(CMCharts.tokenText('1500', 'raw'), '1,500');
  // 非整数透传，不臆造数值。
  assert.equal(CMCharts.formatTokenCount('', 'k'), '');
});

test('bucket 标签按 UTC 生成 hour/day/month（跨月边界不受本地时区影响）', () => {
  assert.equal(CMCharts.bucketLabel('2026-01-02T03:04:05Z', 'hour'), '01-02 03:00');
  assert.equal(CMCharts.bucketLabel('2026-01-02T03:04:05Z', 'day'), '01-02');
  assert.equal(CMCharts.bucketLabel('2026-01-02T03:04:05Z', 'month'), '2026-01');
  // UTC 月初：在西部时区（TZ=America/Los_Angeles）本地时间仍是 02-28，
  // 必须按 UTC 标成 03-01 / 2026-03，否则月份分桶会错位。
  assert.equal(CMCharts.bucketLabel('2026-03-01T00:00:00Z', 'month'), '2026-03');
  assert.equal(CMCharts.bucketLabel('2026-03-01T00:00:00Z', 'day'), '03-01');
  assert.equal(CMCharts.bucketLabel('2026-03-01T00:30:00Z', 'hour'), '03-01 00:00');
  // 同一时刻不同写法：UTC 12:00 的本地日在西部时区会退到前一天，标签必须仍用 UTC 日。
  assert.equal(CMCharts.bucketLabel('2026-01-02T12:00:00Z', 'day'), '01-02');
  assert.equal(CMCharts.normalizeBucket('week'), 'day');
  assert.equal(CMCharts.normalizeBucket('month'), 'month');
});

// --- 数据规范化 -------------------------------------------------------------

test('token 合计是互不重叠桶之和，与上报 total 无关，reasoning 不重复计入', () => {
  const model = CMCharts.buildChartOptions(
    [
      {
        time: '2026-01-02T03:04:05Z',
        tokens: { input: 1000, output: 2000, cache_read: 300, cache_write: 40, reasoning: 50, total: 99999 },
      },
    ],
    { tokenUnit: 'raw' }
  );
  const tokens = model.points[0].tokens;
  // 1000 + 2000 + 300 + 40 + 50 = 3390；即使上报 total 不同也以五项之和为准。
  assert.equal(tokens.sum, '3390');
  assert.equal(tokens.reported, '99999');
  // 堆叠高度（各序列之和）等于合计，没有把 reasoning 重复叠加。
  const stacked = model.tokens.series.reduce((sum, series) => sum + series.data[0], 0);
  assert.equal(stacked, 3390);
});

test('Token 五桶互斥：规范化 output 已剔除 reasoning，堆叠不会重复计数', () => {
  // 契约来源：internal/plugin/accounting/accounting.go 中
  //   Tokens.Output = breakdown.Output.NonReasoningTokens
  //   Tokens.Reasoning = breakdown.Output.ReasoningTokens
  // 即服务端下发的 tokens.output 不含 reasoning，tokens_output 与 tokens_reasoning
  // 是两条独立列；因此五桶可安全堆叠，reasoning 只计一次。
  const model = CMCharts.buildChartOptions(
    [{ time: '2026-01-02T03:04:05Z', tokens: { input: 10, output: 20, cache_read: 30, cache_write: 40, reasoning: 50, total: 150 } }],
    { tokenUnit: 'raw' }
  );
  const tokens = model.points[0].tokens;
  assert.equal(tokens.output, '20'); // 规范 output，不含 reasoning
  assert.equal(tokens.reasoning, '50'); // reasoning 是独立桶
  assert.equal(tokens.sum, '150'); // 10+20+30+40+50，reasoning 只计一次
  const data = model.tokens.series.map((series) => series.data[0]);
  assert.deepEqual(data, [10, 20, 30, 40, 50]);
  assert.equal(data.reduce((sum, value) => sum + value, 0), 150);
  // 反向断言：绝不做 output+reasoning(=70) 这种把 reasoning 并回 output 的重复计数。
  assert.notEqual(data[1], 70);
});

// --- ECharts option 结构 ----------------------------------------------------

test('buildChartOptions 产出请求/Token/成本三张图', () => {
  const model = CMCharts.buildChartOptions(POINTS, { bucket: 'hour', tokenUnit: 'k', currency: 'USD' });
  assert.equal(model.empty, false);

  // 请求/执行：两根柱子，category 轴。
  assert.deepEqual(
    model.requests.series.map((s) => s.name),
    ['请求次数', '执行次数']
  );
  assert.ok(model.requests.series.every((s) => s.type === 'bar'));
  assert.equal(model.requests.xAxis.type, 'category');
  assert.equal(model.requests.xAxis.boundaryGap, true);
  assert.equal(model.requests.yAxis[0].min, 0);

  // Token：五个互不重叠桶，堆叠。
  assert.deepEqual(
    model.tokens.series.map((s) => s.name),
    ['输入', '输出', '缓存读', '缓存写', '思考']
  );
  assert.ok(model.tokens.series.every((s) => s.stack === 'tokens'));
  // tokenUnit='k' 时几何坐标按 /1000 缩放（与坐标轴单位一致）。
  assert.deepEqual(model.tokens.series.map((s) => s.data[0]), [1, 2, 0.3, 0.04, 0.05]);

  // 成本：执行成本与人工补记两条独立序列，没有合并序列。
  assert.deepEqual(
    model.cost.series.map((s) => s.name),
    ['执行成本', '人工补记']
  );
  assert.deepEqual(model.cost.series.map((s) => s.data[0]), [1.25, 0.5]);
  assert.equal(model.cost.series[1].lineStyle.type, 'dashed');
  assert.ok(!model.cost.series.some((s) => /合计|总计/.test(s.name)));
});

test('buildChartOptions 空数据返回空模型且不构造 option', () => {
  for (const empty of [[], null, undefined]) {
    const model = CMCharts.buildChartOptions(empty, { bucket: 'day' });
    assert.equal(model.empty, true);
    assert.equal(model.requests, null);
    assert.equal(model.tokens, null);
    assert.equal(model.cost, null);
    assert.deepEqual(model.points, []);
  }
});

test('零值不产生 NaN，坐标轴仍固定 min:0', () => {
  const zero = [{ time: '2026-01-02T03:04:05Z', requests: 0, executions: 0, cost_micro_usd: 0, adjustment_micro_usd: 0, tokens: {} }];
  const model = CMCharts.buildChartOptions(zero, { tokenUnit: 'k' });
  assert.equal(model.empty, false);
  for (const option of [model.requests, model.tokens, model.cost]) {
    assert.equal(findNonFinite(option, '$'), null, 'option 不应包含非有限数值');
  }
  assert.deepEqual(model.requests.series.map((s) => s.data[0]), [0, 0]);
  assert.ok(model.tokens.series.every((s) => s.data[0] === 0));
  assert.deepEqual(model.cost.series.map((s) => s.data[0]), [0, 0]);
  assert.equal(model.tokens.yAxis[0].min, 0);
});

test('超过 10 个点启用 dataZoom，少量点关闭', () => {
  const many = [];
  for (let i = 0; i < 11; i++) {
    many.push({ time: '2026-01-02T0' + (i % 10) + ':04:05Z', tokens: {} });
  }
  assert.equal(CMCharts.buildChartOptions(many, {}).tokens.dataZoom.length, 2);
  assert.equal(CMCharts.buildChartOptions(POINTS, {}).tokens.dataZoom.length, 0);
});

test('tokenUnit 缩放几何坐标，与坐标轴 formatter 单位一致（不是 12500K）', () => {
  const point = { time: '2026-01-02T00:00:00Z', tokens: { input: 12500, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 12500 } };

  const scaled = CMCharts.buildChartOptions([point], { tokenUnit: 'k' });
  const inputSeries = scaled.tokens.series[0];
  const fmt = scaled.tokens.yAxis[0].axisLabel.formatter;
  assert.equal(inputSeries.data[0], 12.5); // 12500 个 token -> 12.5（以 k 为单位）
  assert.equal(fmt(12.5), '12.5K'); // 轴与几何同单位
  assert.equal(fmt(12500), '12,500K'); // 若几何未缩放，就正好会得到这个错误标注
  // 精确 tooltip 仍是同一单位（12.5K == 12500 tokens）。
  assert.match(scaled.tokens.tooltip.formatter([{ dataIndex: 0 }]), /输入：12.5K/);

  // raw 单位：几何即为原始整数。
  const raw = CMCharts.buildChartOptions([point], { tokenUnit: 'raw' });
  assert.equal(raw.tokens.series[0].data[0], 12500);
  assert.equal(raw.tokens.yAxis[0].axisLabel.formatter(12500), '12,500');
  assert.match(raw.tokens.tooltip.formatter([{ dataIndex: 0 }]), /输入：12,500/);

  // wan / m 同样按 1e4 / 1e6 缩放，轴与数据一致。
  const wan = CMCharts.buildChartOptions([point], { tokenUnit: 'wan' });
  assert.equal(wan.tokens.series[0].data[0], 1.25);
  assert.equal(wan.tokens.yAxis[0].axisLabel.formatter(1.25), '1.25万');
  const million = CMCharts.buildChartOptions([point], { tokenUnit: 'm' });
  assert.equal(million.tokens.series[0].data[0], 0.0125);
  assert.equal(million.tokens.yAxis[0].axisLabel.formatter(0.0125), '0.01M');
});

// --- tooltip：纯文本、精确、无注入 ------------------------------------------

test('tooltip 使用 richText 且输出纯文本，无 HTML 注入面', () => {
  const model = CMCharts.buildChartOptions(POINTS, { tokenUnit: 'raw', currency: 'USD' });
  for (const option of [model.requests, model.tokens, model.cost]) {
    assert.equal(option.tooltip.renderMode, 'richText'); // 不做 HTML 渲染
    assert.equal(typeof option.tooltip.formatter, 'function');
  }
  // 逐条精确比对：纯文本、`\n` 换行、精确整数/金额，且不含任何 HTML 元字符。
  // tooltip 首行必须显式标注 UTC，说明聚合口径。
  assert.equal(
    model.requests.tooltip.formatter([{ dataIndex: 0 }]),
    '2026-01-02 03:04:05 UTC\n请求次数：2\n执行次数：3\n失败请求：1\n未结算请求：0'
  );
  assert.equal(
    model.tokens.tooltip.formatter([{ dataIndex: 0 }]),
    '2026-01-02 03:04:05 UTC\n输入：1,000\n输出：2,000\n缓存读：300\n缓存写：40\n思考：50\n合计：3,390'
  );
  assert.equal(
    model.cost.tooltip.formatter([{ dataIndex: 0 }]),
    '2026-01-02 03:04:05 UTC\n执行成本：1.25 USD\n人工补记：0.5 USD（独立记账，不计入执行成本）'
  );
  const text = [
    model.requests.tooltip.formatter([{ dataIndex: 0 }]),
    model.tokens.tooltip.formatter([{ dataIndex: 0 }]),
    model.cost.tooltip.formatter([{ dataIndex: 0 }]),
  ].join('\n');
  assert.doesNotMatch(text, /[<>&]/);
});

test('tooltip 精确展示大整数与金额，人工补记独立不合并', () => {
  const model = CMCharts.buildChartOptions(POINTS, { tokenUnit: 'raw', currency: 'USD' });
  // 第二个点：input 是超大 cmIntText，合计必须精确。
  const tokens = model.tokens.tooltip.formatter([{ dataIndex: 1 }]);
  assert.match(tokens, /输入：9,007,199,254,740,993/);
  assert.match(tokens, /思考：7/);
  // 9007199254740993 + 7 = 9007199254741000，精确到个位。
  assert.match(tokens, /合计：9,007,199,254,741,000/);
  // 成本：两条独立金额，既不相加也不丢精度（金额文本与 app.js 一致，不带千分位）。
  const cost = model.cost.tooltip.formatter([{ dataIndex: 1 }]);
  assert.equal(
    cost,
    '2026-01-02 04:04:05 UTC\n执行成本：9007199254.740993 USD\n人工补记：0 USD（独立记账，不计入执行成本）'
  );
  // 请求 tooltip 使用精确整数（失败/未结算按 0 兜底）。
  const requests = model.requests.tooltip.formatter([{ dataIndex: 0 }]);
  assert.match(requests, /请求次数：2/);
  assert.match(requests, /失败请求：1/);
  assert.match(requests, /未结算请求：0/);
});

test('tooltip 对越界 dataIndex 安全返回空串', () => {
  const model = CMCharts.buildChartOptions(POINTS, {});
  assert.equal(model.tokens.tooltip.formatter([{ dataIndex: 99 }]), '');
  assert.equal(model.cost.tooltip.formatter(undefined), '');
  assert.equal(model.requests.tooltip.formatter(null), '');
});

// --- render 生命周期 --------------------------------------------------------

test('render 初始化三个图表并使用 svg 渲染器', () => {
  const echarts = fakeEcharts();
  const targets = { requests: fakeElement('requests'), tokens: fakeElement('tokens'), cost: fakeElement('cost') };
  const result = CMCharts.render(targets, POINTS, { bucket: 'hour', tokenUnit: 'k', env: { echarts } });
  assert.deepEqual(result.rendered, { requests: true, tokens: true, cost: true });
  assert.equal(result.missingEcharts, false);
  assert.equal(echarts.__count('init'), 3);
  assert.equal(echarts.__count('setOption'), 3);
  assert.ok(echarts.__instances.every((i) => i.initOptions.renderer === 'svg'));
  assert.ok(echarts.__instances.every((i) => i.options[0].notMerge === true));
  CMCharts.disposeAll();
});

test('render 复用容器实例：第二次不重复 init，但重新 setOption', () => {
  const echarts = fakeEcharts();
  const targets = { requests: fakeElement('requests'), tokens: fakeElement('tokens'), cost: fakeElement('cost') };
  CMCharts.render(targets, POINTS, { bucket: 'hour', env: { echarts } });
  CMCharts.render(targets, POINTS, { bucket: 'day', env: { echarts } });
  assert.equal(echarts.__count('init'), 3);
  assert.equal(echarts.__count('setOption'), 6);
  assert.equal(CMCharts.chartCount(), 3);
  CMCharts.disposeAll();
});

test('render 空数据 dispose 旧实例并清空容器', () => {
  const echarts = fakeEcharts();
  const targets = { requests: fakeElement('requests'), tokens: fakeElement('tokens'), cost: fakeElement('cost') };
  CMCharts.render(targets, POINTS, { env: { echarts } });
  const result = CMCharts.render(targets, [], { env: { echarts }, emptyText: '没有趋势数据' });
  assert.equal(result.empty, true);
  assert.equal(echarts.__count('dispose'), 3);
  assert.equal(CMCharts.chartCount(), 0);
  assert.equal(targets.tokens.textContent, '没有趋势数据');
  // 再次渲染会重新 init（旧实例已释放）。
  CMCharts.render(targets, POINTS, { env: { echarts } });
  assert.equal(echarts.__count('init'), 6);
  CMCharts.disposeAll();
});

test('render 缺少 echarts 时安全降级，不抛异常', () => {
  const targets = { requests: fakeElement('requests') };
  const result = CMCharts.render(targets, POINTS, { missingText: '图表组件加载失败' });
  assert.equal(result.missingEcharts, true);
  assert.equal(result.rendered.requests, false);
  assert.equal(targets.requests.textContent, '图表组件加载失败');
  assert.equal(CMCharts.chartCount(), 0);
});

// --- 主题 -------------------------------------------------------------------

test('主题跟随元素 data-palette，refreshTheme 重新上色不会卡住', () => {
  const echarts = fakeEcharts();
  const tokens = fakeElement('tokens', 'dark');
  const targets = { tokens };
  CMCharts.render(targets, POINTS, { env: { echarts } });
  const chart = echarts.__instances[0];
  assert.equal(chart.options[0].option.yAxis[0].axisLabel.color, CMCharts.PALETTES.dark.axis);

  // 切换元素 palette 后刷新，必须用新主题重画。
  tokens._palette = 'white';
  CMCharts.refreshTheme();
  const last = chart.options[chart.options.length - 1].option;
  assert.equal(last.yAxis[0].axisLabel.color, CMCharts.PALETTES.white.axis);
  CMCharts.disposeAll();
});

test('主题回退到 document.documentElement 并支持 auto', () => {
  const echarts = fakeEcharts();
  const envDark = { echarts, document: { documentElement: fakeElement('html', 'dark') } };
  const dark = CMCharts.render({ tokens: fakeElement('tokens') }, POINTS, { env: envDark });
  assert.equal(dark.model.palette, 'dark');

  const envAuto = { echarts, document: { documentElement: fakeElement('html', 'auto') }, matchMedia: () => ({ matches: true }) };
  const auto = CMCharts.render({ tokens: fakeElement('tokens') }, POINTS, { env: envAuto });
  assert.equal(auto.model.palette, 'dark');

  const envAutoLight = { echarts, document: { documentElement: fakeElement('html', 'auto') }, matchMedia: () => ({ matches: false }) };
  const autoLight = CMCharts.render({ tokens: fakeElement('tokens') }, POINTS, { env: envAutoLight });
  assert.equal(autoLight.model.palette, 'white');
  CMCharts.disposeAll();
});

// --- resize / dispose 不泄漏 -------------------------------------------------

test('使用 ResizeObserver 时 observe/unobserve/disconnect 成对，无泄漏', () => {
  const echarts = fakeEcharts();
  const bucket = [];
  const ResizeObserver = fakeResizeObserver(bucket);
  const targets = { requests: fakeElement('requests'), tokens: fakeElement('tokens'), cost: fakeElement('cost') };
  CMCharts.render(targets, POINTS, { env: { echarts, ResizeObserver } });
  assert.equal(bucket.filter((e) => e.type === 'observe').length, 3);
  assert.equal(bucket.filter((e) => e.type === 'disconnect').length, 0);
  CMCharts.disposeAll();
  assert.equal(bucket.filter((e) => e.type === 'unobserve').length, 3);
  assert.equal(bucket.filter((e) => e.type === 'disconnect').length, 1);
});

test('无 ResizeObserver 时回退窗口监听，并在 dispose 后移除', () => {
  const echarts = fakeEcharts();
  const listeners = [];
  const env = {
    echarts,
    addEventListener: (type, handler) => listeners.push({ type, handler }),
    removeEventListener: (type) => {
      for (let i = listeners.length - 1; i >= 0; i--) {
        if (listeners[i].type === type) listeners.splice(i, 1);
      }
    },
  };
  const targets = { requests: fakeElement('requests'), tokens: fakeElement('tokens'), cost: fakeElement('cost') };
  CMCharts.render(targets, POINTS, { env });
  assert.equal(listeners.filter((l) => l.type === 'resize').length, 1);

  // 触发共享 resize 处理器，三个图表都应 resize。
  listeners[0].handler();
  assert.equal(echarts.__count('resize'), 3);

  CMCharts.disposeAll();
  assert.equal(listeners.filter((l) => l.type === 'resize').length, 0);
});

test('render 对冻结的 options 不报错（无副作用式入参）', () => {
  const echarts = fakeEcharts();
  const targets = { tokens: fakeElement('tokens') };
  const frozen = deepFreeze({ bucket: 'day', tokenUnit: 'm', currency: 'CNY', rate: 7.1, env: Object.freeze({ echarts }) });
  const result = CMCharts.render(targets, POINTS, frozen);
  assert.equal(result.rendered.tokens, true);
  CMCharts.disposeAll();
});

test('renderDonut 初始化环形图并使用 svg 渲染器，支持空数据清空与复用', () => {
  const echarts = fakeEcharts();
  const container = fakeElement('model-share');
  const items = [
    { name: 'gpt-5', value: 10, displayValue: '10' },
    { name: 'glm-5', value: 20, displayValue: '20' }
  ];
  const env = { echarts };
  const res = CMCharts.renderDonut(container, items, { env, centerTitle: '调用次数', centerTotal: '30' });
  assert.equal(res.rendered, true);
  assert.equal(echarts.__count('init'), 1);
  assert.equal(echarts.__count('setOption'), 1);

  // 空数据 dispose
  const resEmpty = CMCharts.renderDonut(container, [], { env });
  assert.equal(resEmpty.empty, true);
  assert.equal(echarts.__count('dispose'), 1);
  CMCharts.disposeAll();
});
