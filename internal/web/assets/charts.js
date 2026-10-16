/*
 * credit-manager 趋势图渲染器。
 *
 * 目标：把 Report.trend 渲染成原版风格的可交互 ECharts 图表，同时保持原有的精确
 * 财务边界。纯原生 JS，UMD（浏览器挂 window.CMCharts，Node 走 module.exports），
 * 不依赖 app.js，也不引入任何 CDN；ECharts 由 assets/vendor/echarts-5.6.0.min.js
 * 以本地脚本方式提供（window.echarts）。
 *
 * 对外契约
 *   CMCharts.render({requests, tokens, cost}, points, options)
 *     - 第一个参数是三个图表容器（DOM 元素）：请求/执行、Token 分解、成本。
 *     - points 是 Report.trend：每项形如
 *         {time, requests, executions, cost_micro_usd, adjustment_micro_usd,
 *          tokens:{input, output, reasoning, cache_read, cache_write, total}}
 *       超出 Number 精度的整数会被 app.js 表示成 {cmIntText:'...'}，本模块同样接受。
 *     - options = {bucket:'hour'|'day'|'month', tokenUnit:'raw'|'k'|'wan'|'m',
 *                  currency:'USD'|'CNY', rate:number}
 *     返回 {empty, missingEcharts, rendered:{requests,tokens,cost}, model}。
 *
 * 关键边界
 *   - 金额与整数一律走 BigInt/十进制字符串：几何坐标可以用浮点，展示/tooltip 必须是
 *     精确整数文本，绝不用浮点做财务换算。
 *   - Token 分解只展示互不重叠的桶（输入、输出、缓存读、缓存写、思考），合计是这五项
 *     之和；reasoning 是独立桶（canonical output 已排除 reasoning），不会重复计入。
 *   - 执行成本与人工补记是两条独立序列，绝不静默相加。
 *   - tooltip 使用 renderMode:'richText' 输出纯文本，不做任何 HTML 拼接。
 *   - 复用容器上的图表实例；空数据时 dispose；resize 观察器/监听器随实例释放，不泄漏。
 *   - 主题由 document/元素上的 data-palette 决定（white|light|dark|auto），并支持
 *     refreshTheme() 在切主题后重新上色，避免主题卡死。
 */
(function (root, factory) {
  'use strict';
  var api = factory(root);
  if (typeof module !== 'undefined' && module && module.exports) module.exports = api;
  if (root && (typeof root === 'object' || typeof root === 'function')) root.CMCharts = api;
})(typeof globalThis !== 'undefined' ? globalThis : (typeof self !== 'undefined' ? self : this), function (root) {
  'use strict';

  var VERSION = '1.0.0';
  var SLOTS = ['requests', 'tokens', 'cost'];
  var BUCKETS = ['hour', 'day', 'month'];
  var TOKEN_UNITS = ['raw', 'k', 'wan', 'm'];
  var CURRENCIES = ['USD', 'CNY'];
  var MAX_ZOOM_POINTS = 10; // 与视觉稿一致：超过 10 个点才显示 dataZoom
  var MAX_SYMBOL_POINTS = 24;

  var DONUT_COLORS = [
    '#1eb787', '#7968ee', '#f0a04b', '#3f9ad6', '#e56b8a',
    '#58be91', '#b5a5e9', '#efb860', '#6fb3e0', '#ef8779',
    '#4c8dff', '#e5a12d', '#36b37e', '#ffab00', '#ff5630',
  ];

  // Token 桶 = 互不重叠的规范计量。契约见 internal/plugin/accounting/accounting.go：
  // Tokens.Output = breakdown.Output.NonReasoningTokens（已剔除 reasoning），
  // Tokens.Reasoning = breakdown.Output.ReasoningTokens；即 output 与 reasoning 是
  // 两条独立列，五个桶可安全堆叠，reasoning 只计一次，绝不重复计入 output。
  var TOKEN_SERIES = [
    { key: 'input', label: '输入' },
    { key: 'output', label: '输出' },
    { key: 'cacheRead', label: '缓存读' },
    { key: 'cacheWrite', label: '缓存写' },
    { key: 'reasoning', label: '思考' },
  ];

  // 主题色取自原版 console.css 的 data-palette 覆盖（white/light/dark），
  // 系列色只用于图形；文本/网格/tooltip 随底色切换，保证对比度。
  var PALETTES = {
    white: {
      axis: '#6a766e', grid: '#edf1ee', axisLine: '#dce5de', panel: '#ffffff',
      tooltipBg: 'rgba(26,31,28,.94)', tooltipText: '#ffffff', areaAlpha: 0.13,
      colors: {
        requests: '#4c8dff', executions: '#1eb787',
        input: '#1eb787', output: '#7968ee', cacheRead: '#f0a04b', cacheWrite: '#3f9ad6', reasoning: '#e56b8a',
        cost: '#1eb787', adjustment: '#e5a12d',
      },
    },
    light: {
      axis: '#6d6760', grid: '#e3e1db', axisLine: '#d8d3ca', panel: '#fffdf9',
      tooltipBg: 'rgba(45,42,38,.94)', tooltipText: '#fffdf9', areaAlpha: 0.13,
      colors: {
        requests: '#3f7fd6', executions: '#168b63',
        input: '#168b63', output: '#8573bd', cacheRead: '#d97706', cacheWrite: '#3f7fd6', reasoning: '#c65746',
        cost: '#168b63', adjustment: '#d97706',
      },
    },
    dark: {
      axis: '#b6afa3', grid: '#3a3530', axisLine: '#49433c', panel: '#1d1b18',
      tooltipBg: 'rgba(18,16,14,.94)', tooltipText: '#f6f4f1', areaAlpha: 0.22,
      colors: {
        requests: '#6fb3e0', executions: '#58be91',
        input: '#58be91', output: '#b5a5e9', cacheRead: '#efb860', cacheWrite: '#6fb3e0', reasoning: '#ef8779',
        cost: '#58be91', adjustment: '#efb860',
      },
    },
  };

  /* ------------------------------------------------------------------ *
   * 精确整数 / 金额（BigInt + 十进制字符串，绝不经过浮点）
   * ------------------------------------------------------------------ */

  function intText(value) {
    if (value === null || value === undefined) return '';
    if (typeof value === 'bigint') return value.toString();
    if (typeof value === 'object' && typeof value.cmIntText === 'string') return value.cmIntText;
    if (typeof value === 'number') {
      if (!isFinite(value)) return String(value);
      if (Number.isInteger(value)) {
        if (Number.isSafeInteger(value)) return String(value);
        try {
          return BigInt(value).toString();
        } catch (err) {
          return String(value);
        }
      }
      return String(value);
    }
    if (typeof value === 'boolean') return String(value);
    return String(value).trim();
  }

  function isIntText(text) {
    return /^-?\d+$/.test(String(text == null ? '' : text).trim());
  }

  function groupDigits(text) {
    var raw = String(text == null ? '' : text);
    var neg = raw.charAt(0) === '-';
    var digits = neg ? raw.slice(1) : raw;
    if (!/^\d+$/.test(digits)) return raw;
    var out = '';
    for (var i = digits.length; i > 0; i -= 3) {
      var start = Math.max(0, i - 3);
      out = digits.slice(start, i) + (out ? ',' + out : '');
    }
    return (neg ? '-' : '') + out;
  }

  // 整数展示：千分位；非整数文本原样返回，绝不使用指数形式。
  function formatInt(value) {
    var text = intText(value);
    return isIntText(text) ? groupDigits(text) : text;
  }

  // 微单位(1e-6) -> 十进制文本，最多 6 位小数、去掉尾随 0；非整数返回 null。
  function microToUSDText(micro) {
    var raw = intText(micro);
    if (!isIntText(raw)) return null;
    var neg = raw.charAt(0) === '-';
    var digits = (neg ? raw.slice(1) : raw).replace(/^0+(?=\d)/, '');
    var big;
    try {
      big = BigInt(digits === '' ? '0' : digits);
    } catch (err) {
      return null;
    }
    var whole = big / 1000000n;
    var frac = big % 1000000n;
    var fracText = frac.toString().padStart(6, '0').replace(/0+$/, '');
    return (neg ? '-' : '') + whole.toString() + (fracText ? '.' + fracText : '');
  }

  // 拆出符号与数字部分；调用方分别处理符号，避免乘除时丢号。
  function decimalParts(text) {
    var raw = String(text == null ? '' : text).trim();
    var neg = false;
    if (raw.charAt(0) === '-') {
      neg = true;
      raw = raw.slice(1);
    } else if (raw.charAt(0) === '+') {
      raw = raw.slice(1);
    }
    var match = /^(\d*)(?:\.(\d*))?$/.exec(raw);
    if (!match || (match[1] === '' && (match[2] === undefined || match[2] === ''))) return null;
    return { neg: neg, int: match[1] || '0', frac: match[2] || '' };
  }

  function pow10Big(count) {
    return BigInt('1' + new Array(count + 1).join('0'));
  }

  function trimZeros(parts) {
    return { int: parts.int.replace(/^0+(?=\d)/, ''), frac: parts.frac.replace(/0+$/, '') };
  }

  // 两个十进制字符串相乘，四舍五入到 fracLimit 位小数，符号由两个操作数决定
  // （用于 CNY = USD * rate，也可用于负金额）。取绝对值四舍五入后补回符号。
  function multiplyDecimal(a, b, fracLimit) {
    var pa = decimalParts(a);
    var pb = decimalParts(b);
    if (!pa || !pb) return null;
    var limit = typeof fracLimit === 'number' && fracLimit >= 0 ? fracLimit : 6;
    var product = BigInt(pa.int + pa.frac) * BigInt(pb.int + pb.frac);
    var scale = pa.frac.length + pb.frac.length;
    if (scale > limit) {
      var divisor = pow10Big(scale - limit);
      var quotient = product / divisor;
      var remainder = product % divisor;
      if (remainder * 2n >= divisor) quotient += 1n; // 绝对值 half-away-from-zero
      product = quotient;
      scale = limit;
    }
    var neg = pa.neg !== pb.neg && product !== 0n;
    var digits = product.toString().padStart(scale + 1, '0');
    var whole = digits.slice(0, digits.length - scale);
    var frac = digits.slice(digits.length - scale).replace(/0+$/, '');
    return (neg ? '-' : '') + whole + (frac ? '.' + frac : '');
  }

  var RATE_MAX_EXPONENT = 18; // rate 必须落在 1e-18..1e18，超界视为非法

  // 科学计数法安全展开为普通十进制；非法/超界返回 ''。
  function expandExponent(text) {
    var raw = String(text == null ? '' : text).trim();
    var match = /^([+-]?)(\d*)(?:\.(\d*))?[eE]([+-]?\d+)$/.exec(raw);
    if (!match) return decimalParts(raw) ? raw : '';
    var sign = match[1] === '-' ? '-' : '';
    var int = match[2] || '0';
    var frac = match[3] || '';
    var exponent = parseInt(match[4], 10);
    if (!isFinite(exponent) || Math.abs(exponent) > RATE_MAX_EXPONENT) return '';
    var digits = (int + frac).replace(/^0+(?=\d)/, '');
    var point = int.length + exponent; // 小数点相对 digits 的位置
    if (point <= 0) return sign + '0.' + new Array(1 - point).join('0') + digits;
    if (point >= digits.length) return sign + digits + new Array(point - digits.length + 1).join('0');
    return sign + digits.slice(0, point) + '.' + digits.slice(point);
  }

  // rate 归一化为「正的普通十进制文本」；非法、非正、超界一律回退为 '1'（有界，绝不产出 '—'）。
  function rateTextOf(rate) {
    var text;
    if (typeof rate === 'number') text = isFinite(rate) ? String(rate) : '';
    else text = String(rate == null ? '' : rate).trim();
    if (text === '') return '1';
    var expanded = expandExponent(text);
    if (expanded === '') return '1';
    var parts = decimalParts(expanded);
    if (!parts || parts.neg) return '1';
    if ((parts.int + parts.frac).replace(/^0+/, '') === '') return '1';
    var trimmed = trimZeros(parts);
    return trimmed.int + (trimmed.frac ? '.' + trimmed.frac : '');
  }

  function normalizeCurrency(value) {
    var text = String(value == null ? '' : value).toUpperCase();
    return CURRENCIES.indexOf(text) >= 0 ? text : 'USD';
  }

  function normalizeTokenUnit(value) {
    var text = String(value == null ? '' : value).toLowerCase();
    return TOKEN_UNITS.indexOf(text) >= 0 ? text : 'raw';
  }

  function normalizeBucket(value) {
    var text = String(value == null ? '' : value).toLowerCase();
    return BUCKETS.indexOf(text) >= 0 ? text : 'day';
  }

  function normalizePalette(value) {
    var text = String(value == null ? '' : value).toLowerCase();
    return PALETTES[text] ? text : '';
  }

  // 精确金额文本：USD 直接用微美元；CNY 用 rate 做精确十进制换算。
  function formatMoneyMicro(micro, currency, rate) {
    var base = microToUSDText(micro);
    if (base === null) return '—';
    if (normalizeCurrency(currency) === 'CNY') {
      var converted = multiplyDecimal(base, rateTextOf(rate), 6);
      return converted === null ? '—' : converted + ' CNY';
    }
    return base + ' USD';
  }

  // 坐标轴/几何只接受浮点值，这里把它还原成微单位后按同一套规则展示。
  function formatMoneyDisplay(value, currency) {
    var num = Number(value);
    if (!isFinite(num)) return '';
    var base = microToUSDText(String(Math.round(num * 1e6)));
    if (base === null) return '';
    return base + ' ' + normalizeCurrency(currency);
  }

  // 浮点美元(或人民币)值 -> 几何用数值；仅用于绘图，不参与财务展示。
  function moneyDisplayNumber(microText, currency, rate) {
    var usd = microToUSDText(microText);
    if (usd === null) return 0;
    var num = Number(usd);
    if (!isFinite(num)) return 0;
    if (normalizeCurrency(currency) === 'CNY') {
      var rateNum = Number(rateTextOf(rate));
      if (isFinite(rateNum) && rateNum > 0) num = num * rateNum;
    }
    return isFinite(num) ? num : 0;
  }

  function tokenUnitDivisorBig(unit) {
    if (unit === 'k') return 1000n;
    if (unit === 'wan') return 10000n;
    if (unit === 'm') return 1000000n;
    return 1n;
  }

  function tokenUnitDivisor(unit) {
    return Number(tokenUnitDivisorBig(normalizeTokenUnit(unit)));
  }

  // 几何坐标必须与坐标轴 formatter 使用同一单位：按 tokenUnit 缩放（如 k -> /1000），
  // 否则 12500 会被标成 “12500K”。精确 tooltip 仍使用未缩放的整数文本。
  function tokenDataNumber(value, unit) {
    var num = toNumber(value);
    var divisor = tokenUnitDivisor(unit);
    return divisor === 1 ? num : num / divisor;
  }

  function tokenUnitSuffix(unit) {
    if (unit === 'k') return 'K';
    if (unit === 'wan') return '万';
    if (unit === 'm') return 'M';
    return '';
  }

  // Token 展示：raw 走千分位；k/wan/m 精确整除并保留 2 位小数（BigInt，不用浮点）。
  function formatTokenCount(value, unit) {
    var text = intText(value);
    if (!isIntText(text)) return text;
    var normalized = normalizeTokenUnit(unit);
    if (normalized === 'raw') return groupDigits(text);
    var divisor = tokenUnitDivisorBig(normalized);
    var neg = text.charAt(0) === '-';
    var digits = neg ? text.slice(1) : text;
    var big = BigInt(digits);
    var whole = big / divisor;
    var remainder = big % divisor;
    var scaled = remainder * 100n;
    var hundredths = scaled / divisor;
    if ((scaled % divisor) * 2n >= divisor) hundredths += 1n;
    if (hundredths >= 100n) {
      whole += 1n;
      hundredths -= 100n;
    }
    var frac = hundredths === 0n ? '' : hundredths.toString().padStart(2, '0').replace(/0+$/, '');
    return (neg ? '-' : '') + groupDigits(whole.toString()) + (frac ? '.' + frac : '');
  }

  // 与 app.js 的 formatTokens 一致：单位后缀紧贴数字（如 12.5K），坐标轴与 tooltip 同一写法。
  function tokenText(value, unit) {
    var normalized = normalizeTokenUnit(unit);
    return formatTokenCount(value, normalized) + tokenUnitSuffix(normalized);
  }

  // 契约字段（计数/token/微美元）缺省即 0；非整数输入保守回退为 0，避免 NaN 进入图形。
  function intOrDefault(value) {
    var text = intText(value);
    return isIntText(text) ? text : '0';
  }

  function toNumber(text) {
    var num = Number(text);
    return isFinite(num) ? num : 0;
  }

  function axisIntText(value) {
    var num = Number(value);
    if (!isFinite(num)) return '';
    return groupDigits(String(Math.round(num)));
  }

  function formatDisplayNumber(value, decimals) {
    var num = Number(value);
    if (!isFinite(num)) return '';
    var fixed = Math.abs(num).toFixed(decimals);
    var parts = fixed.split('.');
    var out = groupDigits(parts[0]);
    if (parts[1]) {
      var frac = parts[1].replace(/0+$/, '');
      if (frac) out += '.' + frac;
    }
    return (num < 0 ? '-' : '') + out;
  }

  function axisTokenText(value, unit) {
    var normalized = normalizeTokenUnit(unit);
    if (!isFinite(Number(value))) return '';
    if (normalized === 'raw') return axisIntText(value);
    return formatDisplayNumber(value, 2) + tokenUnitSuffix(normalized);
  }

  function sumIntTexts(list) {
    var total = 0n;
    var seen = false;
    for (var i = 0; i < list.length; i++) {
      var text = intText(list[i]);
      if (!isIntText(text)) continue;
      total += BigInt(text);
      seen = true;
    }
    return seen ? total.toString() : '0';
  }

  /* ------------------------------------------------------------------ *
   * 时间标签
   * ------------------------------------------------------------------ */

  function pad2(value) {
    return String(value).padStart(2, '0');
  }

  // 服务端 bucket 与 time 都是 UTC（基于 epoch 的 started_at）；这里一律用 UTC getter，
  // 否则 UTC 2026-03-01 在西部时区会被标成 02-28。
  function formatDateTime(value) {
    if (!value) return '';
    var date = new Date(value);
    if (isNaN(date.getTime())) return String(value);
    return (
      date.getUTCFullYear() + '-' + pad2(date.getUTCMonth() + 1) + '-' + pad2(date.getUTCDate()) +
      ' ' + pad2(date.getUTCHours()) + ':' + pad2(date.getUTCMinutes()) + ':' + pad2(date.getUTCSeconds())
    );
  }

  function bucketLabel(value, bucket) {
    if (!value) return '';
    var date = new Date(value);
    if (isNaN(date.getTime())) return String(value);
    if (bucket === 'month') return date.getUTCFullYear() + '-' + pad2(date.getUTCMonth() + 1);
    if (bucket === 'hour') {
      return pad2(date.getUTCMonth() + 1) + '-' + pad2(date.getUTCDate()) + ' ' + pad2(date.getUTCHours()) + ':00';
    }
    return pad2(date.getUTCMonth() + 1) + '-' + pad2(date.getUTCDate());
  }

  /* ------------------------------------------------------------------ *
   * 主题：基于 data-palette（white|light|dark|auto）
   * ------------------------------------------------------------------ */

  function environment(options) {
    return (options && options.env) || root || {};
  }

  function readAttribute(node, name) {
    var current = node;
    var guard = 0;
    while (current && typeof current.getAttribute === 'function' && guard < 32) {
      var value = current.getAttribute(name);
      if (value) return String(value);
      current = current.parentElement;
      guard++;
    }
    return '';
  }

  // 优先元素自身/祖先的 data-palette，其次 document.documentElement；缺省与 'auto' 等价。
  function resolvePalette(element, options) {
    var env = environment(options);
    var value = readAttribute(element, 'data-palette');
    if (!value) {
      var documentElement = env && env.document && env.document.documentElement;
      value = readAttribute(documentElement, 'data-palette');
    }
    if (!value && typeof document !== 'undefined' && document && document.documentElement) {
      value = readAttribute(document.documentElement, 'data-palette');
    }
    var text = String(value || '').toLowerCase();
    if (text === '' || text === 'auto') {
      var media = env && typeof env.matchMedia === 'function' ? env.matchMedia('(prefers-color-scheme: dark)') : null;
      text = media && media.matches ? 'dark' : 'white';
    }
    return PALETTES[text] ? text : 'white';
  }

  function withAlpha(color, alpha) {
    var match = /^#([0-9a-fA-F]{6})$/.exec(String(color));
    if (!match) return color;
    var value = parseInt(match[1], 16);
    return 'rgba(' + ((value >> 16) & 255) + ',' + ((value >> 8) & 255) + ',' + (value & 255) + ',' + alpha + ')';
  }

  /* ------------------------------------------------------------------ *
   * 数据规范化
   * ------------------------------------------------------------------ */

  function normalizePoints(points, bucket) {
    if (!Array.isArray(points)) return [];
    return points.map(function (point) {
      var source = point || {};
      var tokens = source.tokens || {};
      var input = intOrDefault(tokens.input);
      var output = intOrDefault(tokens.output);
      var cacheRead = intOrDefault(tokens.cache_read !== undefined ? tokens.cache_read : tokens.cacheRead);
      var cacheWrite = intOrDefault(tokens.cache_write !== undefined ? tokens.cache_write : tokens.cacheWrite);
      var reasoning = intOrDefault(tokens.reasoning);
      // 合计只取互不重叠的五个桶之和，避免把 reasoning 既当独立桶又计入 output 的重复计数。
      var sum = sumIntTexts([input, output, cacheRead, cacheWrite, reasoning]);
      var stamp = formatDateTime(source.time);
      return {
        time: source.time,
        label: bucketLabel(source.time, bucket),
        // tooltip 明确标注 UTC，避免读者把 UTC 聚合桶误当本地时间。
        timeText: stamp ? stamp + ' UTC' : '',
        requests: intOrDefault(source.requests),
        executions: intOrDefault(source.executions),
        failed: intOrDefault(source.failed),
        pending: intOrDefault(source.pending),
        costMicro: intOrDefault(source.cost_micro_usd),
        adjustmentMicro: intOrDefault(source.adjustment_micro_usd),
        tokens: {
          input: input,
          output: output,
          cacheRead: cacheRead,
          cacheWrite: cacheWrite,
          reasoning: reasoning,
          sum: sum, // 分解图与占比以此为准
          reported: intOrDefault(tokens.total), // 服务端上报的规范 total，仅存档不对账
        },
      };
    });
  }

  /* ------------------------------------------------------------------ *
   * ECharts option 构造（纯函数，可在无 DOM 的 Node 下断言）
   * ------------------------------------------------------------------ */

  function baseOption(theme) {
    return {
      animationDuration: 420,
      animationEasing: 'cubicOut',
      textStyle: { color: theme.axis },
      legend: {
        show: true,
        top: 0,
        left: 0,
        icon: 'roundRect',
        itemWidth: 10,
        itemHeight: 8,
        itemGap: 14,
        textStyle: { color: theme.axis, fontSize: 11 },
      },
      tooltip: {
        trigger: 'axis',
        // richText 渲染：tooltip 只承载纯文本，永远不会被当作 HTML 解析。
        renderMode: 'richText',
        confine: true,
        backgroundColor: theme.tooltipBg,
        borderWidth: 0,
        padding: [10, 12],
        textStyle: { color: theme.tooltipText, fontSize: 12 },
        axisPointer: { type: 'line', lineStyle: { color: theme.axis, type: 'dashed' } },
      },
      grid: { top: 34, right: 14, bottom: 10, left: 8, containLabel: true },
    };
  }

  function categoryAxis(theme) {
    return {
      axisLine: { lineStyle: { color: theme.axisLine } },
      axisTick: { show: false },
      axisLabel: { color: theme.axis, fontSize: 11, hideOverlap: true, margin: 10 },
    };
  }

  function valueAxis(theme, min, formatter) {
    return {
      type: 'value',
      min: min,
      splitNumber: 3,
      axisLabel: { color: theme.axis, fontSize: 11, formatter: formatter },
      splitLine: { lineStyle: { color: theme.grid, type: 'dashed' } },
      axisLine: { show: false },
      axisTick: { show: false },
    };
  }

  function dataZoomFor(count, theme) {
    if (count <= MAX_ZOOM_POINTS) return [];
    return [
      { type: 'inside', start: 0, end: 100, zoomOnMouseWheel: 'shift', moveOnMouseMove: true },
      {
        type: 'slider',
        height: 14,
        bottom: 2,
        start: 0,
        end: 100,
        borderColor: 'transparent',
        backgroundColor: withAlpha(theme.axisLine, 0.28),
        fillerColor: withAlpha(theme.colors.requests, 0.22),
        handleStyle: { color: theme.colors.requests, borderColor: theme.colors.requests },
        textStyle: { color: 'transparent' },
      },
    ];
  }

  function minOfData(values, fallback) {
    var min = Infinity;
    for (var i = 0; i < values.length; i++) {
      var num = Number(values[i]);
      if (isFinite(num) && num < min) min = num;
    }
    if (!isFinite(min)) return fallback;
    return min < 0 ? undefined : fallback;
  }

  function axisIndex(params) {
    var list = Array.isArray(params) ? params : (params ? [params] : []);
    for (var i = 0; i < list.length; i++) {
      if (list[i] && typeof list[i].dataIndex === 'number') return list[i].dataIndex;
    }
    return -1;
  }

  function pointAt(model, params) {
    var index = axisIndex(params);
    if (index < 0 || index >= model.points.length) return null;
    return model.points[index];
  }

  function requestsTooltip(model, params) {
    var point = pointAt(model, params);
    if (!point) return '';
    return [
      point.timeText,
      '请求次数：' + formatInt(point.requests),
      '执行次数：' + formatInt(point.executions),
      '失败请求：' + formatInt(point.failed),
      '未结算请求：' + formatInt(point.pending),
    ].join('\n');
  }

  function tokensTooltip(model, params) {
    var point = pointAt(model, params);
    if (!point) return '';
    var lines = [point.timeText];
    TOKEN_SERIES.forEach(function (item) {
      lines.push(item.label + '：' + tokenText(point.tokens[item.key], model.tokenUnit));
    });
    // 合计 = 五个互不重叠桶之和（reasoning 是独立桶，不会被重复计入）。
    lines.push('合计：' + tokenText(point.tokens.sum, model.tokenUnit));
    return lines.join('\n');
  }

  function costTooltip(model, params) {
    var point = pointAt(model, params);
    if (!point) return '';
    return [
      point.timeText,
      '执行成本：' + formatMoneyMicro(point.costMicro, model.currency, model.rate),
      // 人工补记保持独立序列，绝不静默并入执行成本。
      '人工补记：' + formatMoneyMicro(point.adjustmentMicro, model.currency, model.rate) + '（独立记账，不计入执行成本）',
    ].join('\n');
  }

  function barSeries(name, color, data) {
    return {
      name: name,
      type: 'bar',
      barMaxWidth: 18,
      itemStyle: { color: color, borderRadius: [3, 3, 0, 0] },
      emphasis: { focus: 'series' },
      data: data,
    };
  }

  function lineSeries(name, color, data, options) {
    var theme = options.theme;
    var series = {
      name: name,
      type: 'line',
      z: options.z === undefined ? 2 : options.z,
      symbol: 'circle',
      symbolSize: options.dashed ? 4 : 6,
      showSymbol: options.showSymbol,
      smooth: options.smooth,
      lineStyle: {
        width: options.dashed ? 1.6 : 2.4,
        type: options.dashed ? 'dashed' : 'solid',
        color: color,
      },
      itemStyle: { color: color, borderWidth: 1.5, borderColor: theme.panel },
      emphasis: { focus: 'series' },
      data: data,
    };
    if (options.area) series.areaStyle = { color: withAlpha(color, theme.areaAlpha) };
    return series;
  }

  function requestsOption(model) {
    var theme = model.theme;
    var points = model.points;
    var option = baseOption(theme);
    option.legend.data = ['请求次数', '执行次数'];
    option.grid.bottom = points.length > MAX_ZOOM_POINTS ? 40 : 10;
    option.xAxis = Object.assign(
      { type: 'category', boundaryGap: true, data: points.map(function (p) { return p.label; }) },
      categoryAxis(theme)
    );
    option.yAxis = [valueAxis(theme, 0, function (value) { return axisIntText(value); })];
    option.series = [
      barSeries('请求次数', theme.colors.requests, points.map(function (p) { return toNumber(p.requests); })),
      barSeries('执行次数', theme.colors.executions, points.map(function (p) { return toNumber(p.executions); })),
    ];
    option.dataZoom = dataZoomFor(points.length, theme);
    option.tooltip.formatter = function (params) { return requestsTooltip(model, params); };
    return option;
  }

  function tokensOption(model) {
    var theme = model.theme;
    var points = model.points;
    var option = baseOption(theme);
    option.legend.data = TOKEN_SERIES.map(function (item) { return item.label; });
    option.grid.bottom = points.length > MAX_ZOOM_POINTS ? 40 : 10;
    var tokenAxisFormatter = function (value) { return axisTokenText(value, model.tokenUnit); };
    option.xAxis = Object.assign(
      { type: 'category', boundaryGap: false, data: points.map(function (p) { return p.label; }) },
      categoryAxis(theme)
    );
    option.yAxis = [valueAxis(theme, 0, tokenAxisFormatter)];
    option.series = TOKEN_SERIES.map(function (item) {
      return {
        name: item.label,
        type: 'line',
        // 堆叠展示分解：每段颜色对应一个互不重叠的桶，堆叠高度即合计。
        stack: 'tokens',
        smooth: false,
        symbol: 'circle',
        symbolSize: 5,
        showSymbol: points.length <= MAX_SYMBOL_POINTS,
        lineStyle: { width: 1.6, color: theme.colors[item.key] },
        itemStyle: { color: theme.colors[item.key], borderWidth: 1, borderColor: theme.panel },
        areaStyle: { color: withAlpha(theme.colors[item.key], theme.areaAlpha) },
        emphasis: { focus: 'series' },
        data: points.map(function (p) { return tokenDataNumber(p.tokens[item.key], model.tokenUnit); }),
      };
    });
    option.dataZoom = dataZoomFor(points.length, theme);
    option.tooltip.formatter = function (params) { return tokensTooltip(model, params); };
    return option;
  }

  function costOption(model) {
    var theme = model.theme;
    var points = model.points;
    var option = baseOption(theme);
    option.legend.data = ['执行成本', '人工补记'];
    option.grid.bottom = points.length > MAX_ZOOM_POINTS ? 40 : 10;
    var costData = points.map(function (p) { return moneyDisplayNumber(p.costMicro, model.currency, model.rate); });
    var adjustmentData = points.map(function (p) { return moneyDisplayNumber(p.adjustmentMicro, model.currency, model.rate); });
    var moneyFormatter = function (value) { return formatMoneyDisplay(value, model.currency); };
    option.xAxis = Object.assign(
      { type: 'category', boundaryGap: false, data: points.map(function (p) { return p.label; }) },
      categoryAxis(theme)
    );
    option.yAxis = [valueAxis(theme, minOfData(costData.concat(adjustmentData), 0), moneyFormatter)];
    option.series = [
      lineSeries('执行成本', theme.colors.cost, costData, {
        theme: theme,
        z: 3,
        smooth: points.length > 6 ? 0.35 : false,
        showSymbol: points.length <= MAX_SYMBOL_POINTS,
        area: true,
      }),
      lineSeries('人工补记', theme.colors.adjustment, adjustmentData, {
        theme: theme,
        z: 2,
        dashed: true,
        smooth: false,
        showSymbol: points.length <= MAX_SYMBOL_POINTS,
      }),
    ];
    option.dataZoom = dataZoomFor(points.length, theme);
    option.tooltip.formatter = function (params) { return costTooltip(model, params); };
    return option;
  }

  function buildChartOptions(points, options) {
    var opts = options || {};
    var bucket = normalizeBucket(opts.bucket);
    var palette = normalizePalette(opts.palette) || resolvePalette(opts.reference || opts.element, opts);
    var theme = PALETTES[palette];
    var list = normalizePoints(points, bucket);
    var model = {
      empty: list.length === 0,
      bucket: bucket,
      tokenUnit: normalizeTokenUnit(opts.tokenUnit),
      currency: normalizeCurrency(opts.currency),
      rate: rateTextOf(opts.rate),
      palette: palette,
      theme: theme,
      points: list,
      count: list.length,
      requests: null,
      tokens: null,
      cost: null,
    };
    if (model.empty) return model;
    model.requests = requestsOption(model);
    model.tokens = tokensOption(model);
    model.cost = costOption(model);
    return model;
  }

  /* ------------------------------------------------------------------ *
   * DOM / ECharts 生命周期：复用实例、resize、dispose、主题刷新
   * ------------------------------------------------------------------ */

  // 以容器元素为键的实例注册表：value = {chart, slot, points, options}。
  var records = new Map();
  var resizeObserver = null;
  var windowHandler = null;
  var windowEnv = null;

  function resizeAll() {
    records.forEach(function (record) {
      var chart = record.chart;
      if (chart && typeof chart.resize === 'function') {
        try {
          chart.resize();
        } catch (err) {
          /* 单个图表 resize 失败不影响其它图表 */
        }
      }
    });
  }

  function teardownResizeIfIdle() {
    if (records.size > 0) return;
    if (resizeObserver) {
      if (typeof resizeObserver.disconnect === 'function') {
        try {
          resizeObserver.disconnect();
        } catch (err) {
          /* ignore */
        }
      }
      resizeObserver = null;
    }
    if (windowHandler && windowEnv && typeof windowEnv.removeEventListener === 'function') {
      try {
        windowEnv.removeEventListener('resize', windowHandler);
      } catch (err) {
        /* ignore */
      }
    }
    windowHandler = null;
    windowEnv = null;
  }

  function attachResize(el, options) {
    var env = environment(options);
    var Observer = options.ResizeObserver || (env && env.ResizeObserver);
    if (!resizeObserver && typeof Observer === 'function') {
      resizeObserver = new Observer(function () { resizeAll(); });
    }
    if (resizeObserver && typeof resizeObserver.observe === 'function') {
      resizeObserver.observe(el);
      return;
    }
    // 无 ResizeObserver 时回退到窗口 resize；只挂一个共享监听，dispose 后移除。
    if (env && typeof env.addEventListener === 'function' && typeof env.removeEventListener === 'function') {
      if (!windowHandler) {
        windowHandler = function () { resizeAll(); };
        windowEnv = env;
        env.addEventListener('resize', windowHandler);
      }
    }
  }

  function dispose(el) {
    var record = records.get(el);
    if (record) {
      if (record.chart && typeof record.chart.dispose === 'function') {
        try {
          record.chart.dispose();
        } catch (err) {
          /* ignore */
        }
      }
      records.delete(el);
    }
    if (resizeObserver && typeof resizeObserver.unobserve === 'function') {
      try {
        resizeObserver.unobserve(el);
      } catch (err) {
        /* ignore */
      }
    }
    teardownResizeIfIdle();
  }

  function disposeAll() {
    Array.from(records.keys()).forEach(dispose);
  }

  function showEmpty(el, text) {
    if (!el) return;
    try {
      // 只用 textContent，绝不拼接 HTML。
      el.textContent = typeof text === 'string' ? text : '';
    } catch (err) {
      /* 非 DOM 环境忽略 */
    }
  }

  function obtainChart(el, options) {
    var record = records.get(el);
    if (record) return { chart: record.chart, created: false };
    var env = environment(options);
    var echarts = options.echarts || env.echarts;
    if (!echarts || typeof echarts.init !== 'function') return null;
    var chart = echarts.init(el, options.themeName || null, { renderer: options.renderer || 'svg' });
    return { chart: chart, created: true };
  }

  function render(targets, points, options) {
    var opts = Object.assign({}, options);
    if (!opts.reference) {
      opts.reference = targets && (targets.tokens || targets.requests || targets.cost);
    }
    var model = buildChartOptions(points, opts);
    var result = {
      empty: model.empty,
      missingEcharts: false,
      rendered: { requests: false, tokens: false, cost: false },
      model: model,
    };
    for (var i = 0; i < SLOTS.length; i++) {
      var slot = SLOTS[i];
      var el = targets ? targets[slot] : null;
      if (!el) continue;
      if (model.empty || !model[slot]) {
        dispose(el);
        showEmpty(el, opts.emptyText);
        continue;
      }
      var obtained = obtainChart(el, opts);
      if (!obtained) {
        result.missingEcharts = true;
        dispose(el);
        if (typeof opts.missingText === 'string') showEmpty(el, opts.missingText);
        continue;
      }
      obtained.chart.setOption(model[slot], true); // notMerge：复用实例但不残留旧序列
      if (obtained.created) {
        records.set(el, { chart: obtained.chart, slot: slot, points: points, options: opts });
        attachResize(el, opts);
      } else {
        var record = records.get(el);
        record.slot = slot;
        record.points = points;
        record.options = opts;
      }
      result.rendered[slot] = true;
    }
    return result;
  }

  function buildDonutOption(items, options) {
    var palette = resolvePalette(options);
    var centerTitle = (options && options.centerTitle) || '';
    var centerTotal = (options && options.centerTotal) || '';
    var colors = (options && options.colors) || DONUT_COLORS;
    var isDark = palette.panel === '#1d1b18';
    var totalColor = isDark ? '#f6f4f1' : '#1f2922';

    if (!items || !items.length) {
      return { empty: true };
    }

    return {
      empty: false,
      color: colors,
      animationDuration: 300,
      legend: { show: false },
      tooltip: {
        trigger: 'item',
        renderMode: 'richText',
        backgroundColor: palette.tooltipBg,
        borderColor: palette.axisLine,
        textStyle: { color: palette.tooltipText, fontSize: 12 },
        formatter: function (param) {
          if (!param) return '';
          var name = param.name || '';
          var percent = typeof param.percent === 'number' ? param.percent.toFixed(1) + '%' : '';
          var rawVal = param.data && param.data.displayValue ? param.data.displayValue : (param.value !== undefined ? formatInt(param.value) : '');
          return name + '\n' + (centerTitle ? centerTitle + ': ' : '') + rawVal + (percent ? ' (' + percent + ')' : '');
        }
      },
      series: [{
        name: centerTitle,
        type: 'pie',
        radius: ['45%', '70%'],
        center: ['50%', '50%'],
        minAngle: 3,
        avoidLabelOverlap: true,
        itemStyle: {
          borderColor: palette.panel,
          borderWidth: 2,
          borderRadius: 4
        },
        label: {
          show: true,
          position: 'center',
          formatter: '{total|' + centerTotal + '}\n{caption|' + centerTitle + '}',
          rich: {
            total: { color: totalColor, fontSize: 16, fontWeight: 700, lineHeight: 22 },
            caption: { color: palette.axis, fontSize: 11, fontWeight: 600, lineHeight: 15 }
          }
        },
        labelLine: { show: false },
        emphasis: {
          scale: true,
          scaleSize: 6,
          itemStyle: {
            shadowBlur: 10,
            shadowColor: isDark ? 'rgba(0,0,0,.5)' : 'rgba(31,41,34,.16)'
          }
        },
        data: items
      }]
    };
  }

  function renderDonut(container, items, options) {
    var opts = Object.assign({}, options);
    if (!opts.reference) opts.reference = container;
    var result = {
      empty: false,
      missingEcharts: false,
      rendered: false,
      chart: null,
    };
    if (!container) return result;
    if (!items || !items.length) {
      result.empty = true;
      dispose(container);
      showEmpty(container, opts.emptyText || '当前暂无数据');
      return result;
    }
    var model = buildDonutOption(items, opts);
    if (model.empty) {
      result.empty = true;
      dispose(container);
      showEmpty(container, opts.emptyText || '当前暂无数据');
      return result;
    }
    var obtained = obtainChart(container, opts);
    if (!obtained) {
      result.missingEcharts = true;
      dispose(container);
      if (typeof opts.missingText === 'string') showEmpty(container, opts.missingText);
      return result;
    }
    obtained.chart.setOption(model, true);
    if (typeof opts.onItemClick === 'function') {
      try {
        obtained.chart.off('click');
        obtained.chart.on('click', function (params) {
          if (params && params.data && params.data.item) {
            opts.onItemClick(params.data.item);
          }
        });
      } catch (_) {}
    }
    if (obtained.created) {
      records.set(container, { chart: obtained.chart, slot: 'donut', items: items, options: opts });
      attachResize(container, opts);
    } else {
      var record = records.get(container);
      if (record) {
        record.slot = 'donut';
        record.items = items;
        record.options = opts;
      }
    }
    result.rendered = true;
    result.chart = obtained.chart;
    return result;
  }

  // 优先显式 palette，否则重新读取元素当前 data-palette，重上色避免主题切不动。
  function refreshTheme(palette) {
    Array.from(records.entries()).forEach(function (entry) {
      var el = entry[0];
      var record = entry[1];
      var opts = Object.assign({}, record.options, { reference: el });
      if (palette) opts.palette = palette;
      else delete opts.palette;
      if (record.slot === 'donut') {
        var donutOpt = buildDonutOption(record.items || [], opts);
        if (!donutOpt.empty) record.chart.setOption(donutOpt, true);
        record.options = opts;
        return;
      }
      var model = buildChartOptions(record.points, opts);
      if (model.empty || !model[record.slot]) return;
      record.chart.setOption(model[record.slot], true);
      record.model = model;
      record.options = opts;
    });
  }

  function resize() {
    resizeAll();
  }

  function chartCount() {
    return records.size;
  }

  // 公共 API：生命周期（app.js 只用 render/refreshTheme，其余供集成方按需调用）
  // 与精确数值工具（金额/token 文本，供测试与复用）。内部实现不在此暴露。
  return {
    version: VERSION,
    PALETTES: PALETTES,
    DONUT_COLORS: DONUT_COLORS,
    intText: intText,
    formatInt: formatInt,
    multiplyDecimal: multiplyDecimal,
    normalizeCurrency: normalizeCurrency,
    formatMoneyMicro: formatMoneyMicro,
    normalizeBucket: normalizeBucket,
    formatTokenCount: formatTokenCount,
    tokenText: tokenText,
    bucketLabel: bucketLabel,
    buildChartOptions: buildChartOptions,
    buildDonutOption: buildDonutOption,
    render: render,
    renderDonut: renderDonut,
    resize: resize,
    refreshTheme: refreshTheme,
    dispose: dispose,
    disposeAll: disposeAll,
    chartCount: chartCount,
  };
});
