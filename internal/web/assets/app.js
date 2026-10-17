/*
 * credit-manager 管理界面脚本。
 * 纯原生 JS：无框架、无 bundler、无 CDN。文件顶部为可独立测试的纯逻辑，
 * DOM 相关代码集中在 boot() 之后，仅在存在 document 时才执行。
 */
(function (global) {
  'use strict';

  /* ------------------------------------------------------------------ *
   * 常量
   * ------------------------------------------------------------------ */

  var MANAGER_BASE = '/v0/management/plugins/credit-manager';
  var CPA_BASE = '';

  // 仅用于显示的已知命名空间前缀（不做计费/鉴权依据，不删除其它前缀）。
  var DISPLAY_NAMESPACES = ['opencode-go/', 'commandcode/'];

  // 取值与 credit 契约一致（quality complete|partial|unknown、state active|pending|settled、
  // outcome succeeded|failed|rejected|canceled）；未收录的值原样显示，不做别名猜测。
  var QUALITY_LABELS = {
    complete: '完整',
    partial: '部分',
    unknown: '未知',
  };

  var OUTCOME_LABELS = {
    succeeded: '成功',
    failed: '失败',
    rejected: '拒绝',
    canceled: '取消',
  };

  var STATE_LABELS = {
    active: '执行中',
    pending: '用量待完整',
    settled: '已完成',
  };

  // CPA 凭据状态（sdk/cliproxy/auth 的 Status 常量）的中文显示与语气。
  var CREDENTIAL_STATUS = {
    active: { label: '正常', tone: 'ok' },
    error: { label: '异常', tone: 'danger' },
    disabled: { label: '已禁用', tone: 'warn' },
    pending: { label: '待处理', tone: 'warn' },
    refreshing: { label: '刷新中', tone: 'warn' },
    unknown: { label: '未知', tone: 'warn' },
  };

  /* ------------------------------------------------------------------ *
   * 精确整数与金额（全部走字符串/BigInt，不经过浮点）
   * ------------------------------------------------------------------ */

  function intVal(value) {
    return { cmIntText: intString(value) };
  }

  function isIntVal(value) {
    return !!value && typeof value === 'object' && typeof value.cmIntText === 'string';
  }

  // 把任意整数来源（number/bigint/字符串/IntVal）转成十进制字符串，绝不使用指数形式。
  function intString(value) {
    if (value === null || value === undefined) return '';
    if (typeof value === 'bigint') return value.toString();
    if (isIntVal(value)) return value.cmIntText;
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

  function isIntegerText(text) {
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

  // 整数展示：带千分位，不使用科学计数法，支持超出 Number 精度的十进制字符串。
  function formatInt(value) {
    var text = intString(value);
    if (!isIntegerText(text)) return text;
    return groupDigits(text);
  }

  function formatRawInt(value) {
    return intString(value);
  }

  // 微美元 -> 精确美元文本（最多 6 位小数，去掉末尾多余 0）。返回 null 表示输入不是整数。
  function microToUSDText(micro) {
    var raw = intString(micro);
    if (!isIntegerText(raw)) return null;
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

  // 美元文本 -> 精确微美元整数字符串。字符串运算，避免 0.1*1e6 这类浮点误差。
  function usdToMicroUSD(text) {
    var raw = String(text == null ? '' : text).trim();
    if (raw === '') return { ok: false, error: '请输入金额' };
    var match = /^([+-]?)(\d*)(?:\.(\d*))?$/.exec(raw);
    if (!match || (match[2] === '' && (match[3] === undefined || match[3] === ''))) {
      return { ok: false, error: '金额格式不正确，示例：1.250000' };
    }
    var frac = match[3] === undefined ? '' : match[3];
    if (frac.length > 6) {
      return { ok: false, error: '美元金额最多 6 位小数（微美元是最小货币单位）' };
    }
    var whole = (match[2] || '0').replace(/^0+(?=\d)/, '');
    var fracText = (frac + '000000').slice(0, 6);
    var micro;
    try {
      micro = BigInt(whole === '' ? '0' : whole) * 1000000n + BigInt(fracText === '' ? '0' : fracText);
    } catch (err) {
      return { ok: false, error: '金额超出可表示范围' };
    }
    var negative = match[1] === '-';
    var digits = micro.toString();
    return { ok: true, micro: (negative && digits !== '0' ? '-' : '') + digits };
  }

  function getChartsApi() {
    if (typeof window !== 'undefined' && window && window.CMCharts) return window.CMCharts;
    if (typeof globalThis !== 'undefined' && globalThis && globalThis.CMCharts) return globalThis.CMCharts;
    if (typeof global !== 'undefined' && global && global.CMCharts) return global.CMCharts;
    if (typeof require === 'function') {
      try {
        return require('./charts.js');
      } catch (_) {}
    }
    return null;
  }

  function formatMicroUSD(micro) {
    var charts = getChartsApi();
    if (charts && typeof charts.formatMoneyMicro === 'function') {
      return charts.formatMoneyMicro(micro, state.currency, state.currencyRate);
    }
    var usdText = microToUSDText(micro);
    if (usdText === null) return '—';
    return usdText + ' USD';
  }

  function formatMoneyMicro(micro, currency, rate) {
    var charts = getChartsApi();
    if (charts && typeof charts.formatMoneyMicro === 'function') {
      return charts.formatMoneyMicro(micro, currency || state.currency, rate || state.currencyRate);
    }
    return formatMicroUSD(micro);
  }

  function formatMicroUSDPair(micro) {
    var moneyText = formatMicroUSD(micro);
    if (moneyText === '—') return '—';
    return moneyText + '（' + formatInt(micro) + ' µUSD）';
  }

  // 查询派生金额：null/undefined 表示未定价，绝不能显示成 0 或“免费”。
  function hasCostValue(value) {
    return value !== undefined && value !== null;
  }

  function formatCostValue(value) {
    return hasCostValue(value) ? formatMicroUSD(value) : '未定价';
  }

  // 真实契约的 cost_micro_usd 是 int64：全未定价时也是 0。必须用
  // executions>0 && unpriced_executions>=executions 判断“没有已定价执行”，
  // 混合小计 0（免费 + 未定价）仍是已定价小计，显式免费 0 也保留为 0。
  function costBreakdown(cost, executions, unpriced) {
    var execText = intString(executions || 0);
    var unpricedText = intString(unpriced || 0);
    var hasExecutions = execText !== '0';
    var allUnpriced = false;
    try {
      allUnpriced = hasExecutions && BigInt(unpricedText) >= BigInt(execText);
    } catch (err) {
      allUnpriced = false;
    }
    var hasUnpriced = unpricedText !== '0';
    var known = hasCostValue(cost) && !allUnpriced;
    return {
      known: known,
      allUnpriced: allUnpriced,
      hasUnpriced: hasUnpriced,
      unpricedText: unpricedText,
      hasExecutions: hasExecutions,
      costText: known ? formatMicroUSD(cost) : '未定价',
    };
  }

  function formatTokens(value) {
    var charts = getChartsApi();
    if (charts && typeof charts.tokenText === 'function') {
      return charts.tokenText(value, state.tokenUnit) || '—';
    }
    var text = intString(value);
    if (!isIntegerText(text)) return '—';
    return formatInt(text);
  }

  function formatBytes(value) {
    var num = Number(intString(value));
    if (!isFinite(num) || num < 0) return '—';
    if (num < 1024) return num + ' B';
    var units = ['KiB', 'MiB', 'GiB', 'TiB'];
    var index = -1;
    do {
      num = num / 1024;
      index++;
    } while (num >= 1024 && index < units.length - 1);
    return num.toFixed(num < 10 ? 2 : 1) + ' ' + units[index];
  }

  function formatDurationMS(value) {
    if (value === null || value === undefined || value === '') return '—';
    var ms = Number(intString(value));
    if (!isFinite(ms)) return '—';
    if (ms < 1000) return ms + ' ms';
    return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + ' s';
  }

  function pad2(value) {
    return String(value).padStart(2, '0');
  }

  function formatDateTime(value) {
    if (!value) return '—';
    var date = new Date(value);
    if (isNaN(date.getTime())) return String(value);
    return (
      date.getFullYear() + '-' + pad2(date.getMonth() + 1) + '-' + pad2(date.getDate()) +
      ' ' + pad2(date.getHours()) + ':' + pad2(date.getMinutes()) + ':' + pad2(date.getSeconds())
    );
  }

  function formatDayLabel(value) {
    if (!value) return '';
    var date = new Date(value);
    if (isNaN(date.getTime())) return String(value);
    return pad2(date.getMonth() + 1) + '-' + pad2(date.getDate());
  }

  function formatHourLabel(value) {
    if (!value) return '';
    var date = new Date(value);
    if (isNaN(date.getTime())) return String(value);
    return pad2(date.getDate()) + ' ' + pad2(date.getHours()) + ':00';
  }

  function localDatetimeValue(date) {
    return (
      date.getFullYear() + '-' + pad2(date.getMonth() + 1) + '-' + pad2(date.getDate()) +
      'T' + pad2(date.getHours()) + ':' + pad2(date.getMinutes()) + ':' +
      pad2(date.getSeconds()) + '.' + ('00' + date.getMilliseconds()).slice(-3)
    );
  }

  // datetime-local 的本地时间 -> RFC3339；空值或非法值返回 ''。
  function toISOFromLocal(value) {
    if (!value) return '';
    var date = new Date(value);
    if (isNaN(date.getTime())) return '';
    return date.toISOString();
  }

  /* ------------------------------------------------------------------ *
   * 无损 JSON：解析与格式化都不改写整数文本
   * ------------------------------------------------------------------ */

  // 扫描 JSON 文本，字符串原样跳过，仅对裸数字调用 replaceNumber。
  function rewriteNumbers(text, replaceNumber) {
    return global.CMNumbers.rewriteNumbers(text, replaceNumber);
  }

  function isBigIntegerLiteral(literal) {
    return global.CMNumbers.isBigIntegerLiteral(literal);
  }

  // 与 JSON.parse 行为一致，但超出 Number 精度的整数会保留为精确文本（CMInt）。
  function parseJSON(text) {
    return global.CMNumbers.parse(text);
  }

  function numberLiteral(value) {
    if (typeof value === 'number') {
      if (!isFinite(value)) return 'null';
      if (Number.isInteger(value)) return intString(value);
      return String(value);
    }
    return String(value);
  }

  // 生成请求体时使用：CMInt / BigInt / number 都输出精确的十进制字面量。
  function exactStringify(value) {
    if (value === null || value === undefined) return 'null';
    var type = typeof value;
    if (type === 'string') return JSON.stringify(value);
    if (type === 'boolean') return value ? 'true' : 'false';
    if (type === 'number') return numberLiteral(value);
    if (type === 'bigint') return value.toString();
    if (isIntVal(value)) return value.cmIntText;
    if (Array.isArray(value)) {
      var items = [];
      for (var i = 0; i < value.length; i++) {
        if (value[i] === undefined) continue;
        items.push(exactStringify(value[i]));
      }
      return '[' + items.join(',') + ']';
    }
    if (type === 'object') {
      var parts = [];
      var keys = Object.keys(value);
      for (var k = 0; k < keys.length; k++) {
        if (value[keys[k]] === undefined) continue;
        parts.push(JSON.stringify(keys[k]) + ':' + exactStringify(value[keys[k]]));
      }
      return '{' + parts.join(',') + '}';
    }
    return JSON.stringify(String(value));
  }

  // 词法级美化：逐字符扫描 JSON 文本，数字与字符串内容按原样保留。
  function prettyJSONLossless(text) {
    var source = String(text == null ? '' : text);
    var out = '';
    var indent = 0;
    var index = 0;
    var length = source.length;
    var pad = function (level) {
      var spaces = '';
      for (var i = 0; i < level; i++) spaces += '  ';
      return spaces;
    };
    while (index < length) {
      var ch = source.charAt(index);
      if (ch === '"') {
        var start = index;
        index++;
        while (index < length) {
          var cur = source.charAt(index);
          if (cur === '\\') {
            index += 2;
            continue;
          }
          index++;
          if (cur === '"') break;
        }
        out += source.slice(start, index);
        continue;
      }
      if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
        index++;
        continue;
      }
      if (ch === '{' || ch === '[') {
        var lookahead = index + 1;
        while (lookahead < length && /\s/.test(source.charAt(lookahead))) lookahead++;
        var closer = ch === '{' ? '}' : ']';
        if (lookahead < length && source.charAt(lookahead) === closer) {
          out += ch + closer;
          index = lookahead + 1;
          continue;
        }
        indent++;
        out += ch + '\n' + pad(indent);
        index++;
        continue;
      }
      if (ch === '}' || ch === ']') {
        indent--;
        out += '\n' + pad(indent) + ch;
        index++;
        continue;
      }
      if (ch === ',') {
        out += ',\n' + pad(indent);
        index++;
        continue;
      }
      if (ch === ':') {
        out += ': ';
        index++;
        continue;
      }
      var valueStart = index;
      while (index < length && !/[\s{}\[\],:]/.test(source.charAt(index))) index++;
      out += source.slice(valueStart, index);
    }
    return out;
  }

  // 请求体格式化入口：仅在确认是合法 JSON 时才美化，否则给出原因并回退到原文展示。
  function formatBodyLossless(text) {
    var source = String(text == null ? '' : text);
    if (source.trim() === '') return { ok: false, reason: '正文为空' };
    try {
      parseJSON(source);
    } catch (err) {
      return { ok: false, reason: '正文不是合法 JSON，已按原文显示' };
    }
    try {
      return { ok: true, text: prettyJSONLossless(source) };
    } catch (err) {
      return { ok: false, reason: '格式化失败，已按原文显示' };
    }
  }

  /* ------------------------------------------------------------------ *
   * 使用统计查询参数
   * ------------------------------------------------------------------ */

  var USAGE_BUCKETS = ['hour', 'day', 'month'];

  function defaultUsageFilters(now) {
    var current = now ? new Date(now) : new Date();
    var from = new Date(current.getTime() - 24 * 60 * 60 * 1000);
    return {
      from: localDatetimeValue(from),
      to: localDatetimeValue(current),
      quick: '24h',
      key: '',
      model: '',
      provider: '',
      auth: '',
      result: '',
      search: '',
      bucket: 'day',
      pageSize: 20,
      page: 1,
    };
  }

  function defaultOverviewFilters(now) {
    var current = now ? new Date(now) : new Date();
    var startOfDay = new Date(current.getFullYear(), current.getMonth(), current.getDate(), 0, 0, 0, 0);
    return {
      quick: 'today',
      from: localDatetimeValue(startOfDay),
      to: localDatetimeValue(current),
      key: '',
      model: '',
      bucket: 'day',
    };
  }

  function resolveQuickRange(quick, now) {
    var current = now ? new Date(now) : new Date();
    if (quick === 'today') {
      var start = new Date(current.getFullYear(), current.getMonth(), current.getDate(), 0, 0, 0, 0);
      return { from: localDatetimeValue(start), to: localDatetimeValue(current), quick: 'today' };
    }
    if (quick === '24h') {
      return { from: localDatetimeValue(new Date(current.getTime() - 24 * 3600 * 1000)), to: localDatetimeValue(current), quick: '24h' };
    }
    if (quick === '7d') {
      return { from: localDatetimeValue(new Date(current.getTime() - 7 * 24 * 3600 * 1000)), to: localDatetimeValue(current), quick: '7d' };
    }
    if (quick === '30d') {
      return { from: localDatetimeValue(new Date(current.getTime() - 30 * 24 * 3600 * 1000)), to: localDatetimeValue(current), quick: '30d' };
    }
    if (quick === '90d') {
      return { from: localDatetimeValue(new Date(current.getTime() - 90 * 24 * 3600 * 1000)), to: localDatetimeValue(current), quick: '90d' };
    }
    if (quick === 'all') {
      return { from: '', to: '', quick: 'all' };
    }
    return null;
  }

  function buildOverviewQuery(filters) {
    var input = filters || {};
    var pairs = [];
    var push = function (name, value) {
      if (value === undefined || value === null) return;
      var text = String(value).trim();
      if (text === '') return;
      pairs.push([name, text]);
    };
    var fromVal = input.from;
    var toVal = input.to;
    if (input.quick && input.quick !== 'custom' && input.quick !== 'all') {
      var qRange = resolveQuickRange(input.quick);
      if (qRange) {
        fromVal = qRange.from;
        toVal = qRange.to;
      }
    } else if (input.quick === 'all') {
      fromVal = '';
      toVal = '';
    }
    push('from', toISOFromLocal(fromVal));
    push('to', toISOFromLocal(toVal));
    push('key_id', input.key);
    push('model', input.model);
    push('bucket', USAGE_BUCKETS.indexOf(input.bucket) >= 0 ? input.bucket : 'day');
    return pairs
      .map(function (pair) {
        return encodeURIComponent(pair[0]) + '=' + encodeURIComponent(pair[1]);
      })
      .join('&');
  }

  function normalizePageSize(value) {
    var size = Number(intString(value));
    if (!isFinite(size) || size <= 0) return 20;
    if (size > 200) return 200;
    return Math.floor(size);
  }

  function normalizePage(value) {
    var page = Number(intString(value));
    if (!isFinite(page) || page <= 0) return 1;
    return Math.floor(page);
  }

  // filters -> 查询字符串：字段固定顺序，空值省略，便于对照后端契约与测试。
  function buildUsageQuery(filters) {
    var input = filters || {};
    var pairs = [];
    var push = function (name, value) {
      if (value === undefined || value === null) return;
      var text = String(value).trim();
      if (text === '') return;
      pairs.push([name, text]);
    };
    push('from', toISOFromLocal(input.from));
    push('to', toISOFromLocal(input.to));
    push('key_id', input.key);
    push('model', input.model);
    push('provider', input.provider);
    push('auth_id', input.auth);
    push('result', input.result);
    push('search', input.search);
    push('bucket', USAGE_BUCKETS.indexOf(input.bucket) >= 0 ? input.bucket : 'day');
    push('page', String(normalizePage(input.page)));
    push('page_size', String(normalizePageSize(input.pageSize)));
    return pairs
      .map(function (pair) {
        return encodeURIComponent(pair[0]) + '=' + encodeURIComponent(pair[1]);
      })
      .join('&');
  }

  function usageFiltersFromForm(form) {
    return {
      from: form.from,
      to: form.to,
      quick: form.quick,
      key: form.key,
      model: form.model,
      provider: form.provider,
      auth: form.auth,
      result: form.result,
      search: form.search,
      bucket: form.bucket,
      pageSize: form.pageSize,
      page: form.page,
    };
  }

  var QUICK_RANGES = {
    '1h': 60 * 60 * 1000,
    '24h': 24 * 60 * 60 * 1000,
    '7d': 7 * 24 * 60 * 60 * 1000,
    '30d': 30 * 24 * 60 * 60 * 1000,
  };

  function quickRangeFilters(quick, now) {
    var span = QUICK_RANGES[quick];
    if (!span) return null;
    var current = now ? new Date(now) : new Date();
    return {
      from: localDatetimeValue(new Date(current.getTime() - span)),
      to: localDatetimeValue(current),
      quick: quick,
    };
  }

  /* ------------------------------------------------------------------ *
   * 模型命名空间（仅显示用）
   * ------------------------------------------------------------------ */

  function splitNamespace(model) {
    var text = model === null || model === undefined ? '' : String(model);
    for (var i = 0; i < DISPLAY_NAMESPACES.length; i++) {
      var prefix = DISPLAY_NAMESPACES[i];
      if (text.length > prefix.length && text.indexOf(prefix) === 0) {
        return { prefix: prefix, bare: text.slice(prefix.length) };
      }
    }
    return { prefix: '', bare: text };
  }

  // 请求模型 / 执行模型 / 响应模型的显示关系；命名空间差异只做提示，不作为异常。
  function modelRelation(requested, execution, response) {
    var values = [];
    var push = function (value) {
      if (value === null || value === undefined) return;
      var text = String(value).trim();
      if (text !== '') values.push(text);
    };
    push(requested);
    push(execution);
    push(response);

    var result = {
      requested: requested ? String(requested) : '',
      execution: execution ? String(execution) : '',
      response: response ? String(response) : '',
      namespaceOnly: false,
      namespaces: [],
      note: '',
    };
    if (values.length < 2) return result;

    var bare = splitNamespace(values[0]).bare;
    var namespaceOnly = true;
    var namespaces = [];
    for (var i = 0; i < values.length; i++) {
      var parts = splitNamespace(values[i]);
      if (parts.bare !== bare) {
        namespaceOnly = false;
      }
      if (parts.prefix && namespaces.indexOf(parts.prefix) < 0) namespaces.push(parts.prefix);
    }
    if (namespaceOnly && namespaces.length > 0) {
      result.namespaceOnly = true;
      result.namespaces = namespaces;
      result.note = '与请求模型 ' + bare + ' 仅命名空间前缀不同（显示归一，非异常）';
    }
    return result;
  }

  /* ------------------------------------------------------------------ *
   * 标签映射
   * ------------------------------------------------------------------ */

  function labelFromMap(map, value, fallback) {
    if (value === null || value === undefined) return fallback || '—';
    var text = String(value).trim();
    if (text === '') return fallback || '—';
    var key = text.toLowerCase();
    if (Object.prototype.hasOwnProperty.call(map, key)) return map[key];
    return text;
  }

  function qualityLabel(value) {
    return labelFromMap(QUALITY_LABELS, value, '未知');
  }

  function outcomeLabel(value) {
    return labelFromMap(OUTCOME_LABELS, value, '未知');
  }

  function stateLabel(value) {
    return labelFromMap(STATE_LABELS, value, '—');
  }

  function outcomeTone(value) {
    var key = String(value == null ? '' : value).trim().toLowerCase();
    if (key === 'succeeded') return 'ok';
    if (key === 'failed') return 'danger';
    if (key === 'rejected') return 'warn';
    return '';
  }

  function qualityTone(value) {
    var key = String(value == null ? '' : value).trim().toLowerCase();
    if (key === 'complete') return 'ok';
    if (key === 'partial' || key === 'unknown') return 'warn';
    return '';
  }

  function credentialStatus(value) {
    var key = String(value == null ? '' : value).trim().toLowerCase();
    if (Object.prototype.hasOwnProperty.call(CREDENTIAL_STATUS, key)) return CREDENTIAL_STATUS[key];
    return { label: key || '未知', tone: '' };
  }

  // 只有服务端标记为 pending 的请求才可人工 Resolve：active 仍在执行并占用并发槽位，
  // 后端会拒绝（ErrConflict），因此界面不得为 active 提供入口。
  function isPendingRequest(request) {
    if (!request || typeof request !== 'object') return false;
    return String(request.state == null ? '' : request.state).trim().toLowerCase() === 'pending';
  }

  /* ------------------------------------------------------------------ *
   * 额度指标与价格
   * ------------------------------------------------------------------ */

  function metricValueText(metric) {
    if (!metric || typeof metric !== 'object') return '—';
    var value = metric.value;
    if (value === null || value === undefined || typeof value !== 'number' || !isFinite(value)) return '—';
    var format = String(metric.format || '').trim().toLowerCase();
    var unit = metric.unit ? String(metric.unit) : '';
    if (format === 'currency') {
      var code = metric.currency ? String(metric.currency).toUpperCase() : '';
      if (!code) return value + ' ' + (unit || '（未标注币种）');
      try {
        return new Intl.NumberFormat('zh-CN', { style: 'currency', currency: code }).format(value);
      } catch (err) {
        return value + ' ' + code;
      }
    }
    var text = String(value);
    try {
      text = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(value);
    } catch (err) {
      text = String(value);
    }
    return unit ? text + ' ' + unit : text;
  }

  function bucketPercent(fraction) {
    if (typeof fraction !== 'number' || !isFinite(fraction)) return '—';
    var clamped = Math.max(0, Math.min(1, fraction));
    return (clamped * 100).toFixed(clamped * 100 % 1 === 0 ? 0 : 1) + '%';
  }

  function bucketTone(fraction) {
    if (typeof fraction !== 'number' || !isFinite(fraction)) return '';
    if (fraction <= 0.05) return 'danger';
    if (fraction <= 0.25) return 'warn';
    return '';
  }

  function reasoningRateText(rates) {
    if (!rates || typeof rates !== 'object') return '—';
    if (rates.reasoning === null || rates.reasoning === undefined) return '缺失（按输出计费）';
    var text = intString(rates.reasoning);
    return isIntegerText(text) ? formatInt(text) + ' µUSD / 1M' : String(text);
  }

  function ratesSummaryText(rates) {
    if (!rates || typeof rates !== 'object') return '无当前价格（历史执行显示为未定价，不是免费）';
    var keys = ['input', 'output', 'reasoning', 'cache_read', 'cache_write'];
    var allZero = true;
    for (var i = 0; i < keys.length; i++) {
      var value = rates[keys[i]];
      if (value === null || value === undefined) continue;
      if (intString(value) !== '0') {
        allZero = false;
        break;
      }
    }
    if (allZero) return '显式全零：免费';
    return null;
  }

  function priceMissing(price) {
    if (!price || typeof price !== 'object') return true;
    return !price.model || String(price.model).trim() === '';
  }

  // 价格请求体：金额用精确十进制字面量，思考留空时不下发该字段（后端回退按输出计费）。
  function pricePayload(input) {
    var payload = { model: String(input.model == null ? '' : input.model).trim() };
    var rates = {};
    rates.input = intVal(input.input);
    rates.output = intVal(input.output);
    if (input.reasoning !== null && input.reasoning !== undefined && String(input.reasoning).trim() !== '') {
      rates.reasoning = intVal(input.reasoning);
    }
    rates.cache_read = intVal(input.cacheRead);
    rates.cache_write = intVal(input.cacheWrite);
    payload.rates = rates;
    payload.version = intVal(input.version === undefined || input.version === null ? '0' : input.version);
    return payload;
  }

  function keySpecPayload(input) {
    var spec = {
      label: String(input.label == null ? '' : input.label).trim(),
      enabled: input.enabled === true,
      quota_micro_usd: intVal(input.quotaMicroUSD),
      daily_micro_usd: intVal(input.dailyMicroUSD),
      weekly_micro_usd: intVal(input.weeklyMicroUSD),
      monthly_micro_usd: intVal(input.monthlyMicroUSD),
      max_concurrent: intVal(input.maxConcurrent),
      allowed_models: (input.allowedModels || []).slice(),
    };
    if (input.callerId !== undefined && input.callerId !== null) {
      spec.caller_id = String(input.callerId).trim();
    }
    // 自定义密钥材料只在显式提供非空值时下发，且原样（不裁剪）：留空表示由服务端生成，
    // 首尾空白由服务端校验拒绝，避免界面悄悄把调用方的密钥改写成另一个值。
    if (input.key !== undefined && input.key !== null && input.key !== '') {
      spec.key = String(input.key);
    }
    // 显式 null 表示清空：界面清空 token 限额或过期时间时必须下发 null，
    // 否则合并既有 spec 时会保留旧值，用户无法取消限制。
    spec.model_token_limits = input.modelTokenLimits === null || input.modelTokenLimits === undefined
      ? null
      : input.modelTokenLimits;
    spec.expires_at = input.expiresAt ? String(input.expiresAt) : null;
    return spec;
  }

  // PUT /api/keys 需要携带完整 KeySpec：以服务端返回的 spec 为基底合并编辑结果，
  // 保留界面未涉及的字段，并始终剔除 key 材料。
  function mergeKeySpec(rawSpec, edits) {
    var spec = {};
    if (rawSpec && typeof rawSpec === 'object') {
      Object.keys(rawSpec).forEach(function (key) {
        spec[key] = rawSpec[key];
      });
    }
    if (edits && typeof edits === 'object') {
      Object.keys(edits).forEach(function (key) {
        spec[key] = edits[key];
      });
    }
    delete spec.key;
    if (spec.allowed_models === null || spec.allowed_models === undefined) spec.allowed_models = [];
    return spec;
  }

  // 模型 token 限额 JSON textarea -> 数组；空文本表示不下发该字段。
  function parseModelTokenLimits(text) {
    var raw = String(text == null ? '' : text).trim();
    if (raw === '') return { ok: true, value: null };
    var parsed;
    try {
      parsed = parseJSON(raw);
    } catch (err) {
      return { ok: false, error: '模型 token 限额不是合法 JSON' };
    }
    if (!Array.isArray(parsed)) return { ok: false, error: '模型 token 限额需要是 JSON 数组' };
    for (var i = 0; i < parsed.length; i++) {
      var item = parsed[i];
      if (!item || typeof item !== 'object' || typeof item.model !== 'string' || item.model.trim() === '') {
        return { ok: false, error: '第 ' + (i + 1) + ' 项缺少 model 字段' };
      }
      var fields = ['total', 'daily', 'weekly', 'monthly'];
      for (var f = 0; f < fields.length; f++) {
        var value = item[fields[f]];
        if (value === undefined || value === null) continue;
        if (!isIntegerText(intString(value))) {
          return { ok: false, error: '第 ' + (i + 1) + ' 项的 ' + fields[f] + ' 必须是整数' };
        }
      }
      item.model = item.model.trim();
    }
    return { ok: true, value: parsed };
  }

  function modelTokenLimitsToText(limits) {
    if (!limits || !limits.length) return '';
    try {
      return prettyJSONLossless(exactStringify(limits));
    } catch (err) {
      return '';
    }
  }

  function splitAllowedModels(text) {
    return String(text == null ? '' : text)
      .split(/[\n,]/)
      .map(function (item) {
        return item.trim();
      })
      .filter(function (item) {
        return item !== '';
      });
  }

  /* ------------------------------------------------------------------ *
   * 状态
   * ------------------------------------------------------------------ */

  var TOKEN_KEY = 'credit_manager_mgmt_token';
  var TOKEN_ORIGIN_KEY = 'credit_manager_mgmt_token_origin';
  var TOKEN_UNIT_KEY = 'credit_manager_token_unit';
  var CURRENCY_KEY = 'credit_manager_currency';
  var USD_CNY_RATE_KEY = 'credit_manager_usd_cny_rate';
  var CPA_THEME_KEY = 'cli-proxy-theme';
  var THEMES = ['auto', 'light', 'white', 'dark'];
  var TOKEN_UNITS = ['raw', 'k', 'wan', 'm'];
  var CURRENCIES = ['USD', 'CNY'];
  var DEFAULT_USD_CNY_RATE = 7.2;

  var state = {
    key: '',
    // 会话纪元：登录尝试、登出与会话失效时递增。请求在发起时捕获纪元，
    // 纪元变化后到达的任何响应（含失败）都被判定为过期，不得写回状态、渲染或触发登出。
    sessionEpoch: 0,
    tab: 'overview',
    tokenUnit: 'raw',
    currency: 'USD',
    currencyRate: DEFAULT_USD_CNY_RATE,
    theme: 'auto',
    palette: 'white',
    overview: {
      filters: defaultOverviewFilters(),
      data: null,
      loading: false,
      error: '',
      requestedQuery: '',
      loadedAt: null,
      loadSeq: 0,
      modelShareMetric: 'requests',
      modelRankMetric: 'value',
    },
    usage: {
      filters: defaultUsageFilters(),
      report: null,
      loading: false,
      error: '',
      requestedQuery: '',
      loadedAt: null,
      loadSeq: 0,
    },
    keys: { items: null, loading: false, error: '', loadSeq: 0 },
    callers: { items: null, loading: false, error: '', loadSeq: 0 },
    prices: { items: null, loading: false, error: '', loadSeq: 0 },
    audit: {
      items: null,
      total: 0,
      page: 1,
      pageSize: 20,
      filters: { keyId: '', callerId: '' },
      loading: false,
      error: '',
      loadSeq: 0,
    },
    credentials: { items: null, loading: false, error: '', quota: {}, loadSeq: 0 },
    health: { data: null, loading: false, error: '', loadSeq: 0 },
  };

  function parseStoredPreference(value) {
    if (!value) return '';
    try {
      var parsed = JSON.parse(value);
      return parsed && typeof parsed === 'object'
        ? (parsed.theme || (parsed.state && parsed.state.theme) || parsed.language || '')
        : parsed;
    } catch (_) {
      return value;
    }
  }

  function readStoredTheme() {
    try {
      var raw = parseStoredPreference(localStorage.getItem(CPA_THEME_KEY));
      var candidate = String(raw || 'auto').toLowerCase();
      return THEMES.indexOf(candidate) >= 0 ? candidate : 'auto';
    } catch (_) {
      return 'auto';
    }
  }

  function applyTheme(theme) {
    state.theme = THEMES.indexOf(theme) >= 0 ? theme : 'auto';
    var isDark = false;
    try {
      isDark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (_) {}
    var palette = state.theme === 'auto' ? (isDark ? 'dark' : 'white') : state.theme;
    state.palette = palette;
    if (typeof document !== 'undefined' && document.documentElement) {
      document.documentElement.dataset.theme = state.theme;
      document.documentElement.dataset.palette = palette;
    }
    if (global.CMCharts && typeof global.CMCharts.refreshTheme === 'function') {
      try { global.CMCharts.refreshTheme(); } catch (_) {}
    }
  }

  function initThemeSync() {
    applyTheme(readStoredTheme());
    if (typeof window !== 'undefined') {
      try {
        if (window.matchMedia) {
          window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function () {
            if (state.theme === 'auto') applyTheme('auto');
          });
        }
      } catch (_) {}
      window.addEventListener('storage', function (event) {
        if (event && event.key === CPA_THEME_KEY) {
          applyTheme(readStoredTheme());
        }
      });
    }
  }

  function normalizeTokenUnit(unit) {
    return TOKEN_UNITS.indexOf(unit) >= 0 ? unit : 'raw';
  }

  function normalizeCurrency(currency) {
    return currency === 'CNY' ? 'CNY' : 'USD';
  }

  function normalizeRate(rate) {
    var n = Number(rate);
    return isFinite(n) && n > 0 ? n : DEFAULT_USD_CNY_RATE;
  }

  function readStoredCredential() {
    var currentOrigin = typeof location !== 'undefined' && location ? location.origin : '';
    var sessToken = '';
    var sessOriginOk = false;
    try {
      var sessOrigin = sessionStorage.getItem(TOKEN_ORIGIN_KEY);
      if (!sessOrigin || !currentOrigin || sessOrigin === currentOrigin) {
        sessToken = (sessionStorage.getItem(TOKEN_KEY) || '').trim();
        sessOriginOk = true;
      } else {
        try {
          sessionStorage.removeItem(TOKEN_KEY);
          sessionStorage.removeItem(TOKEN_ORIGIN_KEY);
        } catch (_) {}
      }
    } catch (_) {}

    var locToken = '';
    var locOriginOk = false;
    try {
      var locOrigin = localStorage.getItem(TOKEN_ORIGIN_KEY);
      if (!locOrigin || !currentOrigin || locOrigin === currentOrigin) {
        locToken = (localStorage.getItem(TOKEN_KEY) || '').trim();
        locOriginOk = true;
      } else {
        try {
          localStorage.removeItem(TOKEN_KEY);
          localStorage.removeItem(TOKEN_ORIGIN_KEY);
        } catch (_) {}
      }
    } catch (_) {}

    if (sessToken && sessOriginOk) {
      var isRemembered = Boolean(locToken && locOriginOk && locToken === sessToken);
      return { token: sessToken, remembered: isRemembered };
    }
    if (locToken && locOriginOk) {
      return { token: locToken, remembered: true };
    }
    return { token: '', remembered: false };
  }

  function persistCredential(key, remember) {
    var trimmed = String(key || '').trim();
    var currentOrigin = typeof location !== 'undefined' ? location.origin : '';
    if (!trimmed) {
      clearStoredCredential();
      return;
    }
    try {
      sessionStorage.setItem(TOKEN_KEY, trimmed);
      sessionStorage.setItem(TOKEN_ORIGIN_KEY, currentOrigin);
    } catch (_) {}
    try {
      if (remember) {
        localStorage.setItem(TOKEN_KEY, trimmed);
        localStorage.setItem(TOKEN_ORIGIN_KEY, currentOrigin);
      } else {
        localStorage.removeItem(TOKEN_KEY);
        localStorage.removeItem(TOKEN_ORIGIN_KEY);
      }
    } catch (_) {}
  }

  function clearStoredCredential() {
    try {
      sessionStorage.removeItem(TOKEN_KEY);
      sessionStorage.removeItem(TOKEN_ORIGIN_KEY);
    } catch (_) {}
    try {
      localStorage.removeItem(TOKEN_KEY);
      localStorage.removeItem(TOKEN_ORIGIN_KEY);
    } catch (_) {}
    var box = $('remember-management-key');
    if (box) box.checked = false;
  }

  function readUnitPreferences() {
    var tokenUnit = 'raw';
    var currency = 'USD';
    var rate = DEFAULT_USD_CNY_RATE;
    try {
      var u = localStorage.getItem(TOKEN_UNIT_KEY);
      if (u) tokenUnit = normalizeTokenUnit(u);
      var c = localStorage.getItem(CURRENCY_KEY);
      if (c) currency = normalizeCurrency(c);
      var r = localStorage.getItem(USD_CNY_RATE_KEY);
      if (r) rate = normalizeRate(r);
    } catch (_) {}
    state.tokenUnit = tokenUnit;
    state.currency = currency;
    state.currencyRate = rate;
  }

  function syncUnitControls() {
    var tu = $('token-unit');
    if (tu) tu.value = state.tokenUnit || 'raw';
    var cu = $('currency-unit');
    if (cu) cu.value = state.currency || 'USD';
    var cr = $('currency-rate');
    if (cr) cr.value = String(state.currencyRate || DEFAULT_USD_CNY_RATE);
  }

  function wireUnitControls() {
    readUnitPreferences();
    syncUnitControls();

    var tu = $('token-unit');
    if (tu) {
      tu.addEventListener('change', function () {
        var val = normalizeTokenUnit(tu.value);
        state.tokenUnit = val;
        try { localStorage.setItem(TOKEN_UNIT_KEY, val); } catch (_) {}
        rerenderAfterUnitChange();
      });
    }

    var cu = $('currency-unit');
    if (cu) {
      cu.addEventListener('change', function () {
        var val = normalizeCurrency(cu.value);
        state.currency = val;
        try { localStorage.setItem(CURRENCY_KEY, val); } catch (_) {}
        rerenderAfterUnitChange();
      });
    }

    var cr = $('currency-rate');
    if (cr) {
      var handleRate = function () {
        var val = normalizeRate(cr.value);
        state.currencyRate = val;
        try { localStorage.setItem(USD_CNY_RATE_KEY, String(val)); } catch (_) {}
        rerenderAfterUnitChange();
      };
      cr.addEventListener('change', handleRate);
      cr.addEventListener('input', handleRate);
    }
  }

  function rerenderAfterUnitChange() {
    if (state.usage && state.usage.report) {
      if (state.usage.report.summary) renderUsageSummary(state.usage.report.summary);
      if (state.usage.report.trend) renderUsageTrend(state.usage.report.trend);
      if (state.usage.report.items) renderUsageItems(state.usage.report.items);
    }
  }

  function persistUnitPreferences(prefs) {
    if (!prefs) return;
    try {
      if (prefs.tokenUnit) localStorage.setItem(TOKEN_UNIT_KEY, normalizeTokenUnit(prefs.tokenUnit));
      if (prefs.currency) localStorage.setItem(CURRENCY_KEY, normalizeCurrency(prefs.currency));
      if (prefs.currencyRate !== undefined) localStorage.setItem(USD_CNY_RATE_KEY, String(normalizeRate(prefs.currencyRate)));
    } catch (_) {}
  }

  function authHeaders() {
    return { Authorization: 'Bearer ' + state.key };
  }

  function truncateText(text, limit) {
    var raw = String(text == null ? '' : text);
    var max = limit || 300;
    return raw.length > max ? raw.slice(0, max) + '…' : raw;
  }

  // 过期请求的专用错误：调用方通过 failRequest 静默丢弃，绝不提示、绝不登出。
  function staleError() {
    var error = new Error('会话已变更，忽略过期响应');
    error.kind = 'stale';
    return error;
  }

  // 统一请求入口：始终携带管理密钥；401/403 视为密钥失效，由调用方回到登录界面。
  // 会话纪元在开始解析响应之前校验：登出或重新登录之后到达的旧响应一律作废，
  // 否则旧会话的迟到成功会回填缓存甚至弹出明文密钥，迟到的 401 会把新会话踢下线。
  function request(path, options) {
    var opts = options || {};
    var epoch = state.sessionEpoch;
    var stale = function () {
      return epoch !== state.sessionEpoch;
    };
    var init = {
      method: opts.method || 'GET',
      headers: authHeaders(),
      credentials: 'same-origin',
      cache: 'no-store',
    };
    if (opts.body !== undefined && opts.body !== null) {
      init.headers['Content-Type'] = 'application/json';
      init.body = typeof opts.body === 'string' ? opts.body : exactStringify(opts.body);
    }
    return fetch(path, init).then(
      function (response) {
        return response.text().then(
          function (text) {
            if (stale()) throw staleError();
            var data = null;
            if (text.trim() !== '') {
              try {
                data = parseJSON(text);
              } catch (err) {
                data = null;
              }
            }
            if (!response.ok) {
              var message = data && typeof data.error === 'string' && data.error ? data.error : truncateText(text);
              var error = new Error(message || ('HTTP ' + response.status));
              error.status = response.status;
              error.kind = response.status === 401 || response.status === 403 ? 'auth' : 'http';
              error.detail = text;
              throw error;
            }
            return data;
          },
          function (cause) {
            // 读取响应体失败同样是一种请求结果：会话变更后到达的失败必须作废，
            // 否则旧会话的普通错误会被当成新会话的错误写进界面状态。
            if (stale()) throw staleError();
            throw cause;
          }
        );
      },
      function (cause) {
        if (stale()) throw staleError();
        var error = new Error('无法连接管理 API：' + (cause && cause.message ? cause.message : '网络错误'));
        error.kind = 'network';
        throw error;
      }
    );
  }

  function managerPath(path) {
    return MANAGER_BASE + path;
  }

  var api = {
    overview: function (query) {
      return request(managerPath('/api/overview') + (query ? '?' + query : ''));
    },
    listKeys: function () {
      return request(managerPath('/api/keys'));
    },
    createKey: function (spec) {
      return request(managerPath('/api/keys'), { method: 'POST', body: spec });
    },
    updateKey: function (id, spec) {
      return request(managerPath('/api/keys'), { method: 'PUT', body: { id: id, spec: spec } });
    },
    rotateKey: function (id, customKey) {
      var body = { id: id };
      if (customKey) body.key = customKey;
      return request(managerPath('/api/rotate'), { method: 'POST', body: body });
    },
    revealKey: function (id) {
      return request(managerPath('/api/reveal'), { method: 'POST', body: { id: id } });
    },
    deleteKey: function (id, reason) {
      return request(managerPath('/api/delete-key'), { method: 'POST', body: { id: id, reason: reason } });
    },
    resetTokens: function (spec) {
      return request(managerPath('/api/reset-tokens'), { method: 'POST', body: spec });
    },
    listCallers: function () {
      return request(managerPath('/api/callers'));
    },
    createCaller: function (spec) {
      return request(managerPath('/api/callers'), { method: 'POST', body: spec });
    },
    updateCaller: function (spec) {
      return request(managerPath('/api/callers'), { method: 'PUT', body: spec });
    },
    audit: function (query) {
      return request(managerPath('/api/audit') + (query ? '?' + query : ''));
    },
    listPrices: function () {
      return request(managerPath('/api/prices'));
    },
    putPrice: function (price) {
      return request(managerPath('/api/prices'), { method: 'PUT', body: price });
    },
    usage: function (query) {
      return request(managerPath('/api/usage') + (query ? '?' + query : ''));
    },
    capture: function (id) {
      return request(managerPath('/api/capture') + '?id=' + encodeURIComponent(id));
    },
    health: function () {
      return request(managerPath('/api/health'));
    },
    credentials: function () {
      return request(CPA_BASE + '/v8/management/credentials');
    },
    credentialQuota: function (pluginID, authIndex) {
      return request(CPA_BASE + '/v8/management/plugins/' + encodeURIComponent(pluginID) + '/quota', {
        method: 'POST',
        body: { auth_index: authIndex },
      });
    },
    exchangeRate: function () {
      return request(managerPath('/api/exchange-rate'));
    },
  };

  global.CM = {
    constants: {
      MANAGER_BASE: MANAGER_BASE,
      DISPLAY_NAMESPACES: DISPLAY_NAMESPACES,
      QUICK_RANGES: QUICK_RANGES,
    },
    state: state,
    api: api,
    request: request,
    intVal: intVal,
    isIntVal: isIntVal,
    intString: intString,
    isIntegerText: isIntegerText,
    groupDigits: groupDigits,
    formatInt: formatInt,
    formatRawInt: formatRawInt,
    microToUSDText: microToUSDText,
    usdToMicroUSD: usdToMicroUSD,
    formatMicroUSD: formatMicroUSD,
    formatMicroUSDPair: formatMicroUSDPair,
    formatBytes: formatBytes,
    formatDurationMS: formatDurationMS,
    formatDateTime: formatDateTime,
    formatDayLabel: formatDayLabel,
    formatHourLabel: formatHourLabel,
    localDatetimeValue: localDatetimeValue,
    toISOFromLocal: toISOFromLocal,
    parseJSON: parseJSON,
    exactStringify: exactStringify,
    prettyJSONLossless: prettyJSONLossless,
    formatBodyLossless: formatBodyLossless,
    isBigIntegerLiteral: isBigIntegerLiteral,
    rewriteNumbers: rewriteNumbers,
    buildUsageQuery: buildUsageQuery,
    buildOverviewQuery: buildOverviewQuery,
    usageFiltersFromForm: usageFiltersFromForm,
    defaultUsageFilters: defaultUsageFilters,
    defaultOverviewFilters: defaultOverviewFilters,
    quickRangeFilters: quickRangeFilters,
    normalizePage: normalizePage,
    normalizePageSize: normalizePageSize,
    splitNamespace: splitNamespace,
    modelRelation: modelRelation,
    qualityLabel: qualityLabel,
    outcomeLabel: outcomeLabel,
    stateLabel: stateLabel,
    outcomeTone: outcomeTone,
    qualityTone: qualityTone,
    credentialStatus: credentialStatus,
    isPendingRequest: isPendingRequest,
    hasExecutionScope: hasExecutionScope,
    usageSummaryCards: usageSummaryCards,
    usageScopeNote: usageScopeNote,
    metricValueText: metricValueText,
    bucketPercent: bucketPercent,
    bucketTone: bucketTone,
    reasoningRateText: reasoningRateText,
    ratesSummaryText: ratesSummaryText,
    priceMissing: priceMissing,
    pricePayload: pricePayload,
    keySpecPayload: keySpecPayload,
    mergeKeySpec: mergeKeySpec,
    moneyInputMicro: moneyInputMicro,
    captureStatusText: captureStatusText,
    parseModelTokenLimits: parseModelTokenLimits,
    modelTokenLimitsToText: modelTokenLimitsToText,
    splitAllowedModels: splitAllowedModels,
    truncateText: truncateText,
    formatTokens: formatTokens,
    readStoredCredential: readStoredCredential,
    persistCredential: persistCredential,
    clearStoredCredential: clearStoredCredential,
    readStoredTheme: readStoredTheme,
    applyTheme: applyTheme,
    readUnitPreferences: readUnitPreferences,
    persistUnitPreferences: persistUnitPreferences,
    normalizeTokenUnit: normalizeTokenUnit,
    normalizeCurrency: normalizeCurrency,
    normalizeRate: normalizeRate,
  };

  /* ================================================================== *
   * 以下为界面层：所有动态内容都通过 createElement/textContent 写入，
   * 不使用 innerHTML，避免把服务端数据当 HTML 解析。
   * ================================================================== */

  function $(id) {
    return document.getElementById(id);
  }

  function applyAttrs(node, attrs) {
    if (!attrs) return;
    Object.keys(attrs).forEach(function (key) {
      var value = attrs[key];
      if (value === null || value === undefined || value === false) return;
      if (key === 'text') {
        node.textContent = String(value);
        return;
      }
      if (key === 'class') {
        // SVGElement.className 是只读的 SVGAnimatedString，必须用 setAttribute。
        if (typeof node.className === 'string') {
          node.className = String(value);
        } else {
          node.setAttribute('class', String(value));
        }
        return;
      }
      if (key === 'dataset') {
        Object.keys(value).forEach(function (name) {
          node.dataset[name] = String(value[name]);
        });
        return;
      }
      if (key.indexOf('on') === 0 && typeof value === 'function') {
        node.addEventListener(key.slice(2), value);
        return;
      }
      if (value === true) {
        node.setAttribute(key, '');
        return;
      }
      node.setAttribute(key, String(value));
    });
  }

  function appendChildren(node, children) {
    if (children === null || children === undefined) return;
    var list = Array.isArray(children) ? children : [children];
    for (var i = 0; i < list.length; i++) {
      var child = list[i];
      if (child === null || child === undefined || child === false) continue;
      if (typeof child === 'object' && typeof child.nodeType === 'number') {
        node.appendChild(child);
      } else {
        node.appendChild(document.createTextNode(String(child)));
      }
    }
  }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    applyAttrs(node, attrs);
    appendChildren(node, children);
    return node;
  }

  function replace(node, children) {
    if (!node) return;
    node.textContent = '';
    appendChildren(node, children);
  }

  function setStatusLine(node, text) {
    if (node) node.textContent = text || '';
  }

  function renderLoading(container, message) {
    replace(container, el('div', { class: 'loading-state', text: message || '加载中…' }));
  }

  function renderEmpty(container, message) {
    replace(container, el('div', { class: 'empty-state', text: message }));
  }

  function renderErrorBox(box, message, retry) {
    if (!box) return;
    if (!message) {
      box.hidden = true;
      box.textContent = '';
      return;
    }
    box.hidden = false;
    replace(box, [
      el('p', { text: message }),
      el('div', { class: 'btn-row' }, [
        el('button', { class: 'btn btn-small', type: 'button', text: '重试', onclick: retry }),
      ]),
    ]);
  }

  function notify(message, level) {
    var root = $('toast-root');
    if (!root) return;
    var node = el('div', { class: 'toast' + (level === 'error' ? ' is-error' : level === 'ok' ? ' is-ok' : ''), text: message });
    root.appendChild(node);
    while (root.childElementCount > 3) {
      root.removeChild(root.firstElementChild);
    }
    setTimeout(function () {
      if (node.parentNode) node.parentNode.removeChild(node);
    }, 7000);
  }

  function describeError(error) {
    if (!error) return '未知错误';
    var status = error.status ? '（HTTP ' + error.status + '）' : '';
    return (error.message || '请求失败') + status;
  }

  function shortId(id) {
    var text = String(id == null ? '' : id);
    if (text.length <= 12) return text || '—';
    return text.slice(0, 8) + '…' + text.slice(-3);
  }

  function badge(text, tone) {
    return el('span', { class: 'badge' + (tone ? ' badge-' + tone : ''), text: String(text) });
  }

  function metricCard(label, value, sub, tone) {
    var text = String(value);
    var compact = text.length > 18;
    return el('div', { class: 'metric' + (tone ? ' is-' + tone : '') + (compact ? ' is-compact' : '') }, [
      el('div', { class: 'metric-label', text: label }),
      el('div', { class: 'metric-value', text: text }),
      sub ? el('div', { class: 'metric-sub', text: sub }) : null,
    ]);
  }

  function tableScroll(table) {
    return el('div', { class: 'table-scroll' }, [table]);
  }

  function kvRow(label, value, mono) {
    return el('div', { class: 'kv' }, [
      el('span', { class: 'kv-k', text: label }),
      el('span', { class: 'kv-v' + (mono === false ? '' : ''), text: value === null || value === undefined || value === '' ? '—' : String(value) }),
    ]);
  }

  function detailsBlock(summary, content) {
    var node = el('details', {}, [el('summary', { text: summary }), content]);
    return node;
  }

  function codeBlock(text) {
    return el('pre', { class: 'code', text: String(text == null ? '' : text) });
  }

  function copyText(text, okMessage) {
    var value = String(text == null ? '' : text);
    if (!value) {
      notify('没有可复制的内容', 'error');
      return;
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(value).then(
        function () {
          notify(okMessage || '已复制到剪贴板', 'ok');
        },
        function () {
          notify('复制失败：浏览器拒绝了剪贴板访问，请手动选择文本复制', 'error');
        }
      );
      return;
    }
    notify('当前浏览器不支持剪贴板接口，请手动选择文本复制', 'error');
  }

  function copyButton(getText, label, okMessage) {
    return el('button', {
      class: 'btn btn-small',
      type: 'button',
      text: label || '复制',
      onclick: function () {
        copyText(getText(), okMessage);
      },
    });
  }

  /* ---------------- 弹窗 ---------------- */

  var dialogHandle = null;

  function closeDialog() {
    var root = $('modal-root');
    if (root) {
      root.textContent = '';
      root.hidden = true;
    }
    if (dialogHandle && dialogHandle.keyHandler) {
      document.removeEventListener('keydown', dialogHandle.keyHandler);
    }
    dialogHandle = null;
  }

  // 打开弹窗：立即渲染（含加载态），后续通过 handle 更新内容。
  function openDialog(options) {
    var opts = options || {};
    closeDialog();
    var root = $('modal-root');
    var errorNode = el('p', { class: 'inline-error', hidden: true, role: 'alert' });
    var bodyWrap = el('div', { class: 'modal-body' });
    var footWrap = el('div', { class: 'modal-foot' });
    var dialog = el(
      'div',
      {
        class: 'modal' + (opts.wide ? ' modal-wide' : ''),
        role: 'dialog',
        'aria-modal': 'true',
        'aria-label': opts.title || '对话框',
        tabindex: '-1',
      },
      [
        el('div', { class: 'modal-head' }, [
          el('h2', { text: opts.title || '' }),
          el('button', { class: 'btn btn-small', type: 'button', text: '关闭', onclick: closeDialog }),
        ]),
        errorNode,
        bodyWrap,
        footWrap,
      ]
    );
    var keyHandler = function (event) {
      if (event.key === 'Escape') {
        closeDialog();
      }
    };
    document.addEventListener('keydown', keyHandler);
    root.textContent = '';
    root.hidden = false;
    root.appendChild(dialog);

    var handle = {
      root: root,
      dialog: dialog,
      errorNode: errorNode,
      bodyWrap: bodyWrap,
      footWrap: footWrap,
      keyHandler: keyHandler,
      close: closeDialog,
      setBody: function (children) {
        replace(bodyWrap, children);
      },
      setFooter: function (children) {
        replace(footWrap, children);
      },
      setError: function (message) {
        if (!message) {
          errorNode.hidden = true;
          errorNode.textContent = '';
          return;
        }
        errorNode.hidden = false;
        errorNode.textContent = message;
      },
    };
    dialogHandle = handle;
    if (opts.body) handle.setBody(opts.body);
    if (opts.footer) handle.setFooter(opts.footer);
    dialog.focus();
    return handle;
  }

  function confirmDialog(options) {
    var opts = options || {};
    return new Promise(function (resolve) {
      var confirmInput = el('input', { type: 'checkbox', id: 'confirm-checkbox' });
      var handle = openDialog({
        title: opts.title || '请确认',
        body: [
          el('p', { text: opts.message || '' }),
          opts.warning ? el('p', { class: 'hint hint-warn', text: opts.warning }) : null,
          opts.requireCheckbox === false
            ? null
            : el('div', { class: 'field' }, [
                el('label', { for: 'confirm-checkbox', text: opts.checkboxLabel || '我确认执行该操作' }),
                confirmInput,
              ]),
        ],
        footer: [
          el('button', {
            class: 'btn',
            type: 'button',
            text: '取消',
            onclick: function () {
              handle.close();
              resolve(false);
            },
          }),
          el('button', {
            class: 'btn ' + (opts.danger ? 'btn-danger' : 'btn-primary'),
            type: 'button',
            text: opts.confirmText || '确认',
            onclick: function () {
              if (opts.requireCheckbox !== false && !confirmInput.checked) {
                handle.setError('请先勾选确认项');
                return;
              }
              handle.close();
              resolve(true);
            },
          }),
        ],
      });
    });
  }

  /* ---------------- 金额输入对（微美元 + 美元精确换算） ---------------- */

  function moneyPairInput(options) {
    var opts = options || {};
    var fieldLabel = opts.label || '金额';
    var microInput = el('input', { type: 'text', inputmode: 'numeric', class: 'money-micro', value: opts.micro === undefined ? '0' : String(opts.micro), placeholder: '整数微美元', 'aria-label': fieldLabel + '（微美元 µUSD）' });
    var usdInput = el('input', { type: 'text', inputmode: 'decimal', class: 'money-usd', placeholder: '美元（可留空）', 'aria-label': fieldLabel + '（美元 USD）' });
    var hint = el('div', { class: 'metric-sub', text: '' });

    var syncFromMicro = function () {
      var text = microInput.value.trim();
      if (text === '') {
        usdInput.value = '';
        hint.textContent = '留空表示 0';
        return;
      }
      var usd = microToUSDText(text);
      if (usd === null) {
        usdInput.value = '';
        hint.textContent = '需要是整数微美元';
        return;
      }
      usdInput.value = usd;
      hint.textContent = '= ' + usd + ' USD';
    };
    var syncFromUSD = function () {
      var text = usdInput.value.trim();
      if (text === '') {
        microInput.value = '';
        hint.textContent = '留空表示 0';
        return;
      }
      var result = usdToMicroUSD(text);
      if (!result.ok) {
        hint.textContent = result.error;
        return;
      }
      microInput.value = result.micro;
      hint.textContent = '= ' + microToUSDText(result.micro) + ' USD';
    };

    microInput.addEventListener('input', syncFromMicro);
    usdInput.addEventListener('input', syncFromUSD);
    if (opts.micro !== undefined && opts.micro !== null && String(opts.micro) !== '') {
      syncFromMicro();
    } else {
      hint.textContent = '留空表示 0';
    }

    var row = el('div', { class: 'field' }, [
      el('label', { text: fieldLabel }),
      el('div', { class: 'input-pair' }, [
        el('label', { class: 'money-unit-group' }, [
          el('span', { class: 'money-unit-tag', text: 'µUSD' }), microInput,
        ]),
        el('label', { class: 'money-unit-group' }, [
          el('span', { class: 'money-unit-tag', text: 'USD' }), usdInput,
        ]),
      ]),
      hint,
    ]);
    return { node: row, microInput: microInput, usdInput: usdInput, hint: hint };
  }

  // 纯逻辑：金额输入对的取值。美元字段非空时必须以美元字段为准重新校验——
  // 否则非法的美元输入会让「上一次换算留下的微美元值」被当作有效金额提交，
  // 用户以为填了无效金额而界面却按旧值扣费/建 Key/写价格。
  function moneyInputMicro(input, label, errors) {
    var raw = input || {};
    var usdText = String(raw.usd == null ? '' : raw.usd).trim();
    if (usdText !== '') {
      var converted = usdToMicroUSD(usdText);
      if (!converted.ok) {
        errors.push(label + '：美元输入无效（' + converted.error + '），请改成合法金额或清空后再提交');
        return '0';
      }
      return microAmountValue(converted.micro, label, errors);
    }
    return microAmountValue(String(raw.micro == null ? '' : raw.micro).trim(), label, errors);
  }

  function microAmountValue(text, label, errors) {
    if (text === '') return '0';
    if (!isIntegerText(text)) {
      errors.push(label + ' 必须是整数微美元（1 USD = 1,000,000 微美元）');
      return '0';
    }
    if (text.charAt(0) === '-') {
      errors.push(label + ' 不能为负数');
      return '0';
    }
    return text;
  }

  function readMoneyInput(pair, label, errors) {
    return moneyInputMicro({ micro: pair.microInput.value, usd: pair.usdInput.value }, label, errors);
  }

  // 金额输入对是否完全留空：留空表示 0（可选金额字段还表示「不下发该字段」）。
  function moneyPairEmpty(pair) {
    return pair.microInput.value.trim() === '' && pair.usdInput.value.trim() === '';
  }

  /* ---------------- 登录与会话 ---------------- */

  function showLoginView() {
    var loginView = $('login-view');
    var mainView = $('main-view');
    if (loginView) loginView.hidden = false;
    if (mainView) mainView.hidden = true;
    var status = $('session-status');
    if (status) {
      status.textContent = state.key ? '未验证' : '未登录';
      status.className = 'session-status';
    }
    var logout = $('logout-button');
    if (logout) logout.hidden = true;
  }

  function showMainView() {
    var loginView = $('login-view');
    var mainView = $('main-view');
    if (loginView) loginView.hidden = true;
    if (mainView) mainView.hidden = false;
    var status = $('session-status');
    if (status) {
      status.textContent = '已登录';
      status.className = 'session-status is-active';
    }
    var logout = $('logout-button');
    if (logout) logout.hidden = false;
  }

  // 清空会话私有状态：登出或会话失效后，内存里不得再保留上一个会话的缓存与查询串。
  // 每个资源的读取序号一并重置，配合会话纪元保证在途响应不会被当成有效结果。
  function clearSessionState() {
    state.usage.report = null;
    state.usage.loading = false;
    state.usage.error = '';
    state.usage.requestedQuery = '';
    state.usage.loadedAt = null;
    state.usage.loadSeq = 0;
    state.overview = {
      data: null,
      loading: false,
      error: '',
      loadSeq: 0,
      requestedQuery: '',
      loadedAt: null,
      filters: defaultOverviewFilters(),
      modelShareMetric: 'requests',
      modelRankMetric: 'value',
    };
    state.callers = { items: null, loading: false, error: '', loadSeq: 0 };
    state.audit = { items: null, total: 0, page: 1, pageSize: 20, filters: { keyId: '', callerId: '' }, loading: false, error: '', loadSeq: 0 };
    state.keys = { items: null, loading: false, error: '', loadSeq: 0 };
    state.prices = { items: null, loading: false, error: '', loadSeq: 0 };
    state.credentials = { items: null, loading: false, error: '', quota: {}, loadSeq: 0 };
    state.health = { data: null, loading: false, error: '', loadSeq: 0 };
  }

  // 已渲染的私有内容：登出/会话失效时必须从 DOM 中抹掉，不能只隐藏容器。
  var PRIVATE_RENDER_TARGETS = [
    'overview-status',
    'overview-summary',
    'overview-retention',
    'overview-token-chart',
    'overview-cost-chart',
    'overview-model-share',
    'overview-model-share-legend',
    'overview-model-count',
    'overview-model-rank',
    'overview-rank-count',
    'overview-rank-hint',
    'usage-status',
    'usage-summary',
    'usage-quality-note',
    'usage-chart',
    'usage-token-chart',
    'usage-cost-chart',
    'usage-legend',
    'usage-token-legend',
    'usage-cost-legend',
    'usage-total',
    'usage-retention',
    'usage-items',
    'usage-pagination',
    'keys-status',
    'keys-list',
    'callers-status',
    'callers-list',
    'prices-status',
    'prices-list',
    'audit-status',
    'audit-list',
    'audit-pagination',
    'credentials-status',
    'credentials-list',
    'health-status',
    'health-cards',
    'health-extra',
  ];

  var PRIVATE_ERROR_TARGETS = [
    'overview-error',
    'usage-error',
    'keys-error',
    'callers-error',
    'prices-error',
    'audit-error',
    'credentials-error',
    'health-error',
  ];

  function clearRenderedPrivateContent() {
    var charts = getChartsApi();
    if (charts && typeof charts.disposeAll === 'function') {
      try { charts.disposeAll(); } catch (_) {}
    }
    for (var i = 0; i < PRIVATE_RENDER_TARGETS.length; i++) {
      var node = $(PRIVATE_RENDER_TARGETS[i]);
      if (node) node.textContent = '';
    }
    for (var e = 0; e < PRIVATE_ERROR_TARGETS.length; e++) {
      renderErrorBox($(PRIVATE_ERROR_TARGETS[e]), '');
    }
    var keySelect = $('usage-key');
    if (keySelect) {
      replace(keySelect, [el('option', { value: '', text: '全部' })]);
      keySelect.value = '';
    }
    var overviewKeySelect = $('overview-key');
    if (overviewKeySelect) {
      replace(overviewKeySelect, [el('option', { value: '', text: '全部' })]);
      overviewKeySelect.value = '';
    }
  }

  function handleSessionExpired(message) {
    state.sessionEpoch++;
    state.key = '';
    clearStoredCredential();
    clearSessionState();
    clearRenderedPrivateContent();
    closeDialog();
    if (global.CMAdminPricing && typeof global.CMAdminPricing.dispose === 'function') {
      try { global.CMAdminPricing.dispose(); } catch (_) {}
    }
    showLoginView();
    var errorNode = $('login-error');
    if (errorNode) {
      errorNode.hidden = false;
      errorNode.textContent = message || '管理密钥无效或已失效，请重新登录。';
    }
  }

  // 统一处理请求失败：会话纪元变化后的过期请求静默丢弃（既不提示也不登出），
  // 鉴权失败回到登录界面，其余错误交给调用方展示。
  function failRequest(error) {
    if (error && error.kind === 'stale') return true;
    if (error && error.kind === 'auth') {
      handleSessionExpired(describeError(error));
      return true;
    }
    return false;
  }

  function initAdminPricing() {
    if (global.CMAdminPricing && typeof global.CMAdminPricing.init === 'function') {
      try {
        global.CMAdminPricing.init({
          api: function (path, opts) {
            var options = opts || {};
            var url = path.indexOf('/') === 0 ? managerPath(path) : managerPath('/' + path);
            return request(url, options);
          },
          el: el,
          toast: notify,
          showModal: openDialog,
          closeModal: closeDialog,
          onExactApplied: function () {
            loadPrices(false);
          },
        });
      } catch (err) {
        console.warn('CMAdminPricing.init 失败', err);
      }
    }
  }

  function autoConnectSavedCredential(savedToken, wasRemembered) {
    state.key = savedToken;
    var epoch = ++state.sessionEpoch;
    var button = $('login-button');
    if (button) button.disabled = true;
    var errorNode = $('login-error');
    if (errorNode) {
      errorNode.hidden = true;
      errorNode.textContent = '';
    }
    api.health().then(
      function () {
        if (epoch !== state.sessionEpoch) return;
        if (button) button.disabled = false;
        var input = $('management-key');
        if (input) input.value = '';
        var box = $('remember-management-key');
        if (box) box.checked = Boolean(wasRemembered);
        persistCredential(savedToken, Boolean(wasRemembered));
        initAdminPricing();
        showMainView();
        activateTab('overview', false);
        state.overview.filters = defaultOverviewFilters();
        applyOverviewFiltersToForm(state.overview.filters);
        loadOverview(true);
        loadKeys(true);
      },
      function (error) {
        if (epoch !== state.sessionEpoch) return;
        if (button) button.disabled = false;
        state.key = '';
        if (error && (error.kind === 'auth' || error.status === 401 || error.status === 403)) {
          clearStoredCredential();
        }
        showLoginView();
        var input = $('management-key');
        if (input) input.value = '';
        if (errorNode) {
          errorNode.hidden = false;
          var message = describeError(error);
          if (error && (error.kind === 'auth' || error.status === 403)) {
            message += '　（为避免触发管理接口的失败次数封禁，界面不会自动重试）';
          }
          errorNode.textContent = message;
        }
      }
    );
  }

  function submitLogin(event) {
    if (event) event.preventDefault();
    var input = $('management-key');
    var rememberBox = $('remember-management-key');
    var button = $('login-button');
    var errorNode = $('login-error');
    var key = input ? input.value : '';
    var remember = rememberBox ? !!rememberBox.checked : false;
    if (!key) {
      if (errorNode) {
        errorNode.hidden = false;
        errorNode.textContent = '请输入管理密钥';
      }
      return;
    }
    state.key = key;
    // 每次登录尝试都开启新的会话纪元：旧会话（含同一密钥的旧会话）的在途请求立即作废。
    var epoch = ++state.sessionEpoch;
    if (errorNode) {
      errorNode.hidden = true;
      errorNode.textContent = '';
    }
    if (button) button.disabled = true;
    api.health().then(
      function () {
        if (epoch !== state.sessionEpoch) return;
        if (button) button.disabled = false;
        if (input) input.value = '';
        persistCredential(key, remember);
        initAdminPricing();
        showMainView();
        activateTab('overview', false);
        state.overview.filters = defaultOverviewFilters();
        applyOverviewFiltersToForm(state.overview.filters);
        loadOverview(true);
        loadKeys(true);
      },
      function (error) {
        if (epoch !== state.sessionEpoch) return;
        if (button) button.disabled = false;
        state.key = '';
        showLoginView();
        if (input) input.value = '';
        if (errorNode) {
          errorNode.hidden = false;
          var message = describeError(error);
          if (error.kind === 'auth' || (error && error.status === 403)) {
            message += '　（为避免触发管理接口的失败次数封禁，界面不会自动重试）';
          }
          errorNode.textContent = message;
        }
      }
    );
  }

  function logout() {
    // 先递增会话纪元：所有在途请求立即失效，其响应不得再写回状态或弹窗。
    state.sessionEpoch++;
    state.key = '';
    clearStoredCredential();
    clearSessionState();
    clearRenderedPrivateContent();
    closeDialog();
    if (global.CMAdminPricing && typeof global.CMAdminPricing.dispose === 'function') {
      global.CMAdminPricing.dispose();
    }
    showLoginView();
    var keyInput = $('management-key');
    if (keyInput) keyInput.value = '';
    notify('已清除管理密钥');
  }

  /* ---------------- 标签页 ---------------- */

  var TAB_LOADERS = {
    overview: function () {
      loadOverview(false);
    },
    usage: function () {
      loadUsage(false);
    },
    keys: function () {
      loadKeys(false);
    },
    callers: function () {
      loadCallers(false);
    },
    prices: function () {
      loadPrices(false);
    },
    audit: function () {
      loadAudit(false);
    },
    credentials: function () {
      loadCredentials(false);
    },
    health: function () {
      loadHealth(false);
    },
  };

  function activateTab(name, focus) {
    if (!TAB_LOADERS[name]) return;
    state.tab = name;
    var tabs = document.querySelectorAll('.tab');
    for (var i = 0; i < tabs.length; i++) {
      var tab = tabs[i];
      var selected = tab.dataset.tab === name;
      tab.setAttribute('aria-selected', selected ? 'true' : 'false');
      tab.tabIndex = selected ? 0 : -1;
      if (selected && focus) tab.focus();
    }
    var panels = document.querySelectorAll('.panel');
    for (var p = 0; p < panels.length; p++) {
      panels[p].hidden = panels[p].id !== 'panel-' + name;
    }
    TAB_LOADERS[name]();
  }

  function wireTabs() {
    var tabs = document.querySelectorAll('.tab');
    for (var i = 0; i < tabs.length; i++) {
      (function (tab) {
        tab.addEventListener('click', function () {
          activateTab(tab.dataset.tab, false);
        });
        tab.addEventListener('keydown', function (event) {
          if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return;
          event.preventDefault();
          var list = Array.prototype.slice.call(document.querySelectorAll('.tab'));
          var index = list.indexOf(tab);
          var next = event.key === 'ArrowRight' ? index + 1 : index - 1;
          if (next < 0) next = list.length - 1;
          if (next >= list.length) next = 0;
          activateTab(list[next].dataset.tab, true);
        });
      })(tabs[i]);
    }
  }

  /* ================================================================== *
   * 概览 (Overview)
   * ================================================================== */

  function applyOverviewFiltersToForm(filters) {
    var f = filters || {};
    var set = function (id, value) {
      var node = $(id);
      if (node) node.value = value === undefined || value === null ? '' : String(value);
    };
    set('overview-quick', f.quick || 'today');
    set('overview-from', f.from || '');
    set('overview-to', f.to || '');
    set('overview-key', f.key || '');
    set('overview-model', f.model || '');
    set('overview-bucket', f.bucket || 'day');

    var fromWrap = $('overview-from-wrap');
    var toWrap = $('overview-to-wrap');
    var showCustom = f.quick === 'custom';
    if (fromWrap) fromWrap.hidden = !showCustom;
    if (toWrap) toWrap.hidden = !showCustom;
  }

  function overviewFiltersFromForm() {
    var quick = $('overview-quick') ? $('overview-quick').value : 'today';
    return {
      quick: quick,
      from: $('overview-from') ? $('overview-from').value : '',
      to: $('overview-to') ? $('overview-to').value : '',
      key: $('overview-key') ? $('overview-key').value : '',
      model: $('overview-model') ? $('overview-model').value : '',
      bucket: $('overview-bucket') ? $('overview-bucket').value : 'day',
    };
  }

  function wireOverviewForm() {
    var form = $('overview-filters');
    if (form) {
      form.addEventListener('submit', function (event) {
        event.preventDefault();
        state.overview.filters = overviewFiltersFromForm();
        loadOverview(true);
      });
    }
    var resetBtn = $('overview-reset');
    if (resetBtn) {
      resetBtn.addEventListener('click', function () {
        state.overview.filters = defaultOverviewFilters();
        applyOverviewFiltersToForm(state.overview.filters);
        loadOverview(true);
      });
    }
    var quickSelect = $('overview-quick');
    if (quickSelect) {
      quickSelect.addEventListener('change', function () {
        var val = quickSelect.value;
        var fromWrap = $('overview-from-wrap');
        var toWrap = $('overview-to-wrap');
        if (val === 'custom') {
          if (fromWrap) fromWrap.hidden = false;
          if (toWrap) toWrap.hidden = false;
        } else {
          if (fromWrap) fromWrap.hidden = true;
          if (toWrap) toWrap.hidden = true;
          var range = resolveQuickRange(val);
          if (range) {
            if ($('overview-from')) $('overview-from').value = range.from;
            if ($('overview-to')) $('overview-to').value = range.to;
          }
        }
      });
    }

    var shareMetricGroup = $('model-share-metric');
    if (shareMetricGroup) {
      shareMetricGroup.addEventListener('click', function (event) {
        var btn = event.target.closest('button[data-metric]');
        if (!btn) return;
        var metric = btn.getAttribute('data-metric');
        state.overview.modelShareMetric = metric;
        var buttons = shareMetricGroup.querySelectorAll('button');
        for (var i = 0; i < buttons.length; i++) {
          buttons[i].classList.toggle('active', buttons[i] === btn);
        }
        if (state.overview.data && state.overview.data.models) {
          renderOverviewModelShare(state.overview.data.models, state.overview.data.summary);
        }
      });
    }

    var rankMetricGroup = $('model-rank-metric');
    if (rankMetricGroup) {
      rankMetricGroup.addEventListener('click', function (event) {
        var btn = event.target.closest('button[data-metric]');
        if (!btn) return;
        var metric = btn.getAttribute('data-metric');
        state.overview.modelRankMetric = metric;
        var buttons = rankMetricGroup.querySelectorAll('button');
        for (var j = 0; j < buttons.length; j++) {
          buttons[j].classList.toggle('active', buttons[j] === btn);
        }
        if (state.overview.data && state.overview.data.models) {
          renderOverviewModelRank(state.overview.data.models);
        }
      });
    }
  }

  function loadOverview(force) {
    if (!state.key) return;
    var filters = state.overview.filters;
    var query = buildOverviewQuery(filters);
    var seq = ++state.overview.loadSeq;
    if (!force && state.overview.requestedQuery === query && state.overview.data) {
      state.overview.loading = false;
      state.overview.error = '';
      renderOverview();
      return;
    }
    state.overview.loading = true;
    state.overview.error = '';
    renderOverviewStatus();
    renderOverviewError();
    api.overview(query).then(
      function (data) {
        if (seq !== state.overview.loadSeq) return;
        state.overview.loading = false;
        state.overview.data = data || {};
        state.overview.requestedQuery = query;
        state.overview.loadedAt = new Date();
        renderOverview();
      },
      function (error) {
        if (seq !== state.overview.loadSeq) return;
        if (failRequest(error)) return;
        state.overview.loading = false;
        state.overview.error = describeError(error);
        renderOverviewStatus();
        renderOverviewError();
      }
    );
  }

  function renderOverviewStatus() {
    var node = $('overview-status');
    if (!node) return;
    if (state.overview.loading) {
      node.textContent = '正在读取概览指标…';
      return;
    }
    var parts = [];
    if (state.overview.requestedQuery !== undefined && state.overview.data) {
      parts.push('接口：GET ' + MANAGER_BASE + '/api/overview');
    }
    if (state.overview.loadedAt) parts.push('数据时间：' + formatDateTime(state.overview.loadedAt.toISOString()));
    if (state.overview.data && state.overview.requestedQuery) parts.push('查询：' + state.overview.requestedQuery);
    node.textContent = parts.join('　·　');
  }

  function renderOverviewError() {
    renderErrorBox($('overview-error'), state.overview.error, function () {
      loadOverview(true);
    });
  }

  function renderOverviewSummary(summary) {
    var container = $('overview-summary');
    if (!container) return;
    if (!summary) {
      container.textContent = '';
      return;
    }
    var cost = summary.cost_micro_usd;
    var unpriced = summary.unpriced_executions || 0;
    var costState = costBreakdown(cost, summary.executions, unpriced);
    var costKnown = costState.known;
    var hasUnpriced = costState.hasUnpriced;

    var costSub = costState.allUnpriced
      ? '所选范围内 ' + formatInt(summary.executions || 0) + ' 条执行均未定价（不是免费）'
      : costKnown
        ? (hasUnpriced
            ? '已定价小计 ' + formatMicroUSD(cost) + '；另有 ' + formatInt(unpriced) + ' 条未定价（不是免费）'
            : '已定价小计 · 当前价格 × 所选范围内实际日志')
        : '所选范围内无可计算金额的执行（未定价 ≠ 免费）';

    var cards = [
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '总请求数' }),
        el('div', { class: 'stat-value mono', text: formatInt(summary.requests || 0) }),
        el('div', { class: 'stat-sub muted', text: '执行 ' + formatInt(summary.executions || 0) + ' 次' }),
      ]),
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '总消耗 Token' }),
        el('div', { class: 'stat-value mono', text: formatTokens(summary.tokens ? summary.tokens.total : 0, state.tokenUnit) }),
        el('div', { class: 'stat-sub muted', text: '输入 ' + formatTokens(summary.tokens ? summary.tokens.input : 0, state.tokenUnit) + ' · 输出 ' + formatTokens(summary.tokens ? summary.tokens.output : 0, state.tokenUnit) }),
      ]),
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '已定价费用小计' }),
        el('div', { class: 'stat-value mono' + (costKnown ? '' : ' text-warn'), text: costState.costText }),
        el('div', { class: 'stat-sub muted', text: costSub }),
      ]),
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '未定价执行' }),
        el('div', { class: 'stat-value mono' + (hasUnpriced ? ' text-warn' : ''), text: formatInt(unpriced) + ' 条' }),
        el('div', { class: 'stat-sub muted', text: '缺价、用量不完整或无法证明计费数量；不是免费' }),
      ]),
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '缓存读取率' }),
        el('div', { class: 'stat-value mono', text: (summary.cache_read_ratio !== null && summary.cache_read_ratio !== undefined) ? (Number(summary.cache_read_ratio) * 100).toFixed(1) + '%' : '—' }),
        el('div', { class: 'stat-sub muted', text: '完整质量执行 ' + formatInt(summary.complete_executions || 0) + ' 次（仅用量质量，不代表价格完整）' }),
      ]),
      el('div', { class: 'card stat-card metric-card' }, [
        el('div', { class: 'stat-label metric-label', text: '异常请求' }),
        el('div', { class: 'stat-value mono' + ((Number(intString(summary.failed || 0)) > 0 || Number(intString(summary.pending || 0)) > 0) ? ' text-warn' : ''), text: formatInt(summary.failed || 0) + ' 失败 · ' + formatInt(summary.pending || 0) + ' 待完整' }),
        el('div', { class: 'stat-sub muted', text: '两类状态存在重叠，独立计数不累加' }),
      ]),
    ];
    replace(container, cards);
  }

  function renderOverviewCharts(trend) {
    var tokenChart = $('overview-token-chart');
    var costChart = $('overview-cost-chart');
    var points = trend || [];
    if (!tokenChart || !costChart) return;

    if (global.CMCharts && typeof global.CMCharts.render === 'function') {
      global.CMCharts.render(
        { tokens: tokenChart, cost: costChart },
        points,
        {
          bucket: state.overview.filters.bucket || 'day',
          tokenUnit: state.tokenUnit,
          currency: state.currency,
          rate: state.currencyRate,
          theme: state.theme,
          palette: state.palette,
        }
      );
    }
  }

  function renderOverviewModelShare(models, summary) {
    var chartContainer = $('overview-model-share');
    var legendContainer = $('overview-model-share-legend');
    var countNode = $('overview-model-count');
    if (countNode) countNode.textContent = '共 ' + (models ? models.length : 0) + ' 个模型';
    if (!chartContainer) return;
    if (!models || models.length === 0) {
      if (global.CMCharts && typeof global.CMCharts.renderDonut === 'function') {
        global.CMCharts.renderDonut(chartContainer, [], {});
      }
      if (legendContainer) legendContainer.textContent = '暂无模型调用数据';
      return;
    }

    var metric = state.overview.modelShareMetric || 'requests';
    var items = [];
    var unpricedModels = [];
    var centerTitle = '总调用';
    var centerTotal = formatInt(summary && summary.requests ? summary.requests : 0);

    if (metric === 'tokens') {
      centerTitle = '总 Token';
      var sumTokens = summary && summary.tokens ? summary.tokens.total : 0;
      centerTotal = formatTokens(sumTokens, state.tokenUnit);
    } else if (metric === 'cost') {
      centerTitle = '已定价小计';
      centerTotal = summary
        ? costBreakdown(summary.cost_micro_usd, summary.executions, summary.unpriced_executions).costText
        : '未定价';
    }

    models.forEach(function (m) {
      var val = 0;
      var disp = '';
      if (metric === 'requests') {
        val = Number(intString(m.requests || 0));
        disp = formatInt(m.requests || 0) + ' 次';
      } else if (metric === 'tokens') {
        var tVal = m.tokens ? m.tokens.total : 0;
        val = Number(intString(tVal || 0));
        disp = formatTokens(tVal || 0, state.tokenUnit);
      } else if (metric === 'cost') {
        var modelCost = costBreakdown(m.cost_micro_usd, m.executions, m.unpriced_executions);
        if (!modelCost.known) {
          // 全未定价模型不能以数值 0 参与占比，否则会被误读为免费。
          unpricedModels.push(m.model || 'unknown');
          return;
        }
        val = Number(intString(m.cost_micro_usd || 0));
        disp = modelCost.hasUnpriced
          ? modelCost.costText + '（另有 ' + formatInt(m.unpriced_executions) + ' 条未定价）'
          : modelCost.costText;
      }
      items.push({
        name: m.model || 'unknown',
        value: val,
        displayValue: disp,
      });
    });

    if (global.CMCharts && typeof global.CMCharts.renderDonut === 'function') {
      global.CMCharts.renderDonut(chartContainer, items, {
        centerTitle: centerTitle,
        centerTotal: centerTotal,
        metric: metric,
        theme: state.theme,
        palette: state.palette,
      });
    }

    if (legendContainer) {
      legendContainer.textContent = '';
      var colors = (global.CMCharts && global.CMCharts.DONUT_COLORS) || ['#2563eb', '#10b981', '#f59e0b', '#ec4899', '#8b5cf6', '#06b6d4', '#64748b'];
      var totalSum = items.reduce(function (acc, it) { return acc + (it.value > 0 ? it.value : 0); }, 0);
      items.forEach(function (it, idx) {
        var color = colors[idx % colors.length];
        var pct = totalSum > 0 ? ((it.value / totalSum) * 100).toFixed(1) + '%' : '0%';
        var itemEl = el('div', { class: 'share-legend-item' }, [
          el('span', { class: 'share-swatch', style: 'background-color:' + color }),
          el('span', { class: 'share-name', text: it.name }),
          el('span', { class: 'share-pct', text: pct }),
          el('span', { class: 'share-val muted', text: it.displayValue }),
        ]);
        legendContainer.appendChild(itemEl);
      });
      if (unpricedModels.length) {
        legendContainer.appendChild(el('div', {
          class: 'share-legend-note muted',
          text: unpricedModels.length + ' 个模型未定价，未计入费用分布（不是免费）：' + unpricedModels.join('、'),
        }));
      }
    }
  }

  function renderOverviewModelRank(models) {
    var container = $('overview-model-rank');
    var hint = $('overview-rank-hint');
    var countNode = $('overview-rank-count');
    if (countNode) countNode.textContent = '共 ' + (models ? models.length : 0) + ' 个模型';
    if (!container) return;
    if (!models || models.length === 0) {
      container.textContent = '暂无模型排行数据';
      return;
    }

    var metric = state.overview.modelRankMetric || 'value';
    var hintMap = {
      value: '按每美元产出 Token 排序，越高越省',
      unit: '按单次调用平均成本排序，越低越轻量（仅完整计量）',
      cache: '按缓存读取占比排序，越高缓存效率越好',
      tps: '按累计测得吞吐率（Token/s）排序，非瞬时速率',
      latency: '按平均响应延迟排序，越低响应越快',
    };
    if (hint) hint.textContent = hintMap[metric] || '';

    var charts = getChartsApi();
    var list = [];
    if (charts && typeof charts.computeModelRankings === 'function') {
      list = charts.computeModelRankings(models, metric, {
        tokenUnit: state.tokenUnit,
        currency: state.currency,
        rate: state.currencyRate,
      });
    } else {
      list = (models || []).map(function (m) {
        return {
          model: m.model || 'unknown',
          score: null,
          display: '—',
          requestsText: intString(m.requests || 0),
          tokensText: intString(m.tokens ? m.tokens.total : 0),
          costMicroText: hasCostValue(m.cost_micro_usd) ? intString(m.cost_micro_usd) : '',
          unpricedText: intString(m.unpriced_executions || 0),
        };
      });
    }

    container.textContent = '';
    list.forEach(function (item, idx) {
      var row = el('div', { class: 'model-rank-item' }, [
        el('div', { class: 'rank-index' + (idx < 3 ? ' top-rank' : ''), text: String(idx + 1) }),
        el('div', { class: 'rank-info' }, [
          el('div', { class: 'rank-header' }, [
            el('span', { class: 'rank-model mono', text: item.model }),
            el('span', { class: 'rank-value mono', text: item.display }),
          ]),
          el('div', { class: 'rank-meta muted' }, [
            el('span', { text: formatInt(item.requestsText) + ' 次调用' }),
            el('span', { text: formatTokens(item.tokensText, state.tokenUnit) + ' tokens' }),
            el('span', { text: item.costMicroText ? formatMicroUSD(item.costMicroText) : '未定价' }),
            item.unpricedText && intString(item.unpricedText) !== '0'
              ? el('span', { text: formatInt(item.unpricedText) + ' 条未定价（不是免费）' })
              : null,
          ]),
        ]),
      ]);
      container.appendChild(row);
    });
  }

  function renderOverview() {
    var data = state.overview.data || {};
    renderOverviewStatus();
    renderOverviewError();
    renderOverviewSummary(data.summary);
    var retNode = $('overview-retention');
    if (retNode) {
      retNode.textContent = data.retention_from ? '数据保留自 ' + formatDateTime(data.retention_from) : '';
    }
    renderOverviewCharts(data.trend);
    renderOverviewModelShare(data.models || [], data.summary);
    renderOverviewModelRank(data.models || []);
  }

  /* ================================================================== *
   * 使用统计
   * ================================================================== */

  function applyUsageFiltersToForm(filters) {
    var set = function (id, value) {
      var node = $(id);
      if (node) node.value = value === undefined || value === null ? '' : String(value);
    };
    set('usage-from', filters.from);
    set('usage-to', filters.to);
    set('usage-quick', filters.quick || '');
    set('usage-model', filters.model);
    set('usage-provider', filters.provider);
    set('usage-auth', filters.auth);
    set('usage-result', filters.result);
    set('usage-search', filters.search);
    set('usage-bucket', filters.bucket || 'day');
    set('usage-page-size', filters.pageSize || 20);
    var keySelect = $('usage-key');
    if (keySelect) keySelect.value = filters.key || '';
  }

  function collectUsageFilters() {
    var value = function (id) {
      var node = $(id);
      return node ? node.value : '';
    };
    return {
      from: value('usage-from'),
      to: value('usage-to'),
      quick: value('usage-quick'),
      key: value('usage-key'),
      model: value('usage-model').trim(),
      provider: value('usage-provider').trim(),
      auth: value('usage-auth').trim(),
      result: value('usage-result'),
      search: value('usage-search').trim(),
      bucket: value('usage-bucket'),
      pageSize: normalizePageSize(value('usage-page-size')),
      page: state.usage.filters.page || 1,
    };
  }

  function submitUsageFilters(event) {
    if (event) event.preventDefault();
    var filters = collectUsageFilters();
    var range = quickRangeFilters(filters.quick);
    if (range) {
      filters.from = range.from;
      filters.to = range.to;
      applyUsageFiltersToForm(filters);
    }
    filters.page = 1;
    state.usage.filters = filters;
    loadUsage(true);
  }

  function resetUsageFilters() {
    state.usage.filters = defaultUsageFilters();
    applyUsageFiltersToForm(state.usage.filters);
    loadUsage(true);
  }

  function loadUsage(force) {
    if (!state.key) return;
    var filters = state.usage.filters;
    var query = buildUsageQuery(filters);
    // 读取序号：只有最后一次发出的查询可以写回结果，先发的响应迟到一律丢弃。
    // 序号必须先自增（含命中缓存的分支）：即使本次命中缓存、不再发起请求，
    // 也必须作废在途的旧查询，否则更早发出的查询迟到时会覆盖当前筛选条件下的缓存结果。
    var seq = ++state.usage.loadSeq;
    if (!force && state.usage.requestedQuery === query && state.usage.report) {
      // 命中缓存：清掉在途查询留下的加载/错误状态，并用缓存结果重新渲染，保持界面自洽。
      state.usage.loading = false;
      state.usage.error = '';
      renderUsage();
      return;
    }
    state.usage.loading = true;
    state.usage.error = '';
    renderUsageStatus();
    renderUsageError();
    api.usage(query).then(
      function (data) {
        if (seq !== state.usage.loadSeq) return;
        state.usage.loading = false;
        state.usage.report = data || {};
        state.usage.requestedQuery = query;
        state.usage.loadedAt = new Date();
        renderUsage();
      },
      function (error) {
        if (seq !== state.usage.loadSeq) return;
        if (failRequest(error)) return;
        state.usage.loading = false;
        state.usage.error = describeError(error);
        renderUsageStatus();
        renderUsageError();
      }
    );
  }

  function renderUsageStatus() {
    var node = $('usage-status');
    if (!node) return;
    if (state.usage.loading) {
      node.textContent = '正在读取使用统计…';
      return;
    }
    var parts = [];
    if (state.usage.requestedQuery !== undefined && state.usage.report) {
      parts.push('接口：GET ' + MANAGER_BASE + '/api/usage');
    }
    if (state.usage.loadedAt) parts.push('数据时间：' + formatDateTime(state.usage.loadedAt.toISOString()));
    if (state.usage.report && state.usage.requestedQuery) parts.push('查询：' + state.usage.requestedQuery);
    node.textContent = parts.join('　·　');
  }

  function renderUsageError() {
    renderErrorBox($('usage-error'), state.usage.error, function () {
      loadUsage(true);
    });
  }

  function summaryValue(summary, key) {
    if (!summary) return 0;
    var value = summary[key];
    return value === undefined || value === null ? 0 : value;
  }

  function renderUsage() {
    renderUsageStatus();
    renderUsageError();
    var report = state.usage.report || {};
    renderUsageSummary(report.summary);
    renderUsageTrend(report.trend || []);
    renderUsageRetention(report);
    renderUsageItems(report);
    renderUsagePagination(report);
  }

  // 只有按执行维度过滤（provider/auth_id）时，明细与成本才只覆盖匹配的执行。
  function hasExecutionScope(filters) {
    var input = filters || {};
    return String(input.provider || '').trim() !== '' || String(input.auth || '').trim() !== '';
  }

  // 汇总卡片的纯描述：金额是所选日志范围按当前价格即时计算的结果，绝不代表永久账本。
  // 契约：summary.failed 是失败请求数（failed/canceled/rejected），summary.pending 是
  // 用量未完整的请求数（active+pending）；cost_micro_usd 是已定价执行小计，可为 null；
  // unpriced_executions 是未定价执行数，绝不能并入金额或显示为免费。
  function usageSummaryCards(summary, filters) {
    var scoped = hasExecutionScope(filters);
    var requests = summaryValue(summary, 'requests');
    var executions = summaryValue(summary, 'executions');
    var failed = summaryValue(summary, 'failed');
    var pending = summaryValue(summary, 'pending');
    var cost = summary.cost_micro_usd;
    var unpriced = summaryValue(summary, 'unpriced_executions');
    var costState = costBreakdown(cost, executions, unpriced);
    var costKnown = costState.known;
    var hasUnpriced = costState.hasUnpriced;
    var completeExecutions = summaryValue(summary, 'complete_executions');
    var tokens = summary.tokens || {};
    var ratio = summary.cache_read_ratio;

    var ratioText = '—';
    if (ratio !== null && ratio !== undefined && typeof ratio === 'number' && isFinite(ratio)) {
      ratioText = (ratio * 100).toFixed(2) + '%';
    }

    var costSub;
    if (costState.allUnpriced) {
      costSub = '所选范围内 ' + formatInt(executions) + ' 条执行均未定价（不是免费）' +
        (scoped ? '，且已按 Provider/auth_id 过滤' : '');
    } else if (!costKnown) {
      costSub = '所选范围内没有可计算金额的执行（未定价不是免费）' +
        (scoped ? '，且已按 Provider/auth_id 过滤' : '');
    } else if (hasUnpriced) {
      costSub = '已定价小计 ' + formatMicroUSD(cost) + '；另有 ' + formatInt(unpriced) +
        ' 条未定价执行不计入小计（不是免费）' + (scoped ? '，已按 Provider/auth_id 过滤' : '');
    } else {
      costSub = formatInt(cost) + ' µUSD　当前价格 × 实际日志；仅匹配范围内的执行' +
        (scoped ? '，已按 Provider/auth_id 过滤' : '');
    }

    return [
      { label: '请求数（Request 行）', value: formatInt(requests), sub: '每个请求可包含多次执行' },
      { label: '执行数（实际执行）', value: formatInt(executions), sub: '含重试与多执行' },
      {
        label: '失败请求',
        value: formatInt(failed),
        sub: '按请求结果统计（failed / canceled / rejected），不是失败执行数',
        tone: Number(intString(failed)) ? 'danger' : '',
      },
      {
        label: '用量待完整请求',
        value: formatInt(pending),
        sub: pending
          ? '执行中或真实用量缺失：完整用量与缺价执行仍会正常完成'
          : '没有执行中或用量待完整的请求',
        tone: Number(intString(pending)) ? 'warn' : '',
      },
      {
        label: '已定价费用小计',
        value: costState.costText,
        sub: costSub,
        tone: costKnown ? '' : 'warn',
      },
      {
        label: '未定价执行',
        value: formatInt(unpriced),
        sub: '缺价、用量不完整或无法从日志证明计费数量；未定价 ≠ 免费',
        tone: hasUnpriced ? 'warn' : '',
      },
      { label: '完整质量执行', value: formatInt(completeExecutions), sub: 'quality = complete；仅用量质量，不代表价格完整' },
      {
        label: '缓存读占比',
        value: ratioText,
        sub: ratio === null || ratio === undefined ? '服务端未提供该指标' : '只覆盖完整质量执行，说明见下方',
      },
      { label: 'tokens 合计', value: formatTokens(tokens.total), sub: '规范 tokens' },
      { label: '输入 tokens', value: formatTokens(tokens.input), sub: 'canonical uncached input' },
      { label: '输出 tokens', value: formatTokens(tokens.output), sub: '不含思考 tokens' },
      { label: '缓存读 tokens', value: formatTokens(tokens.cache_read) },
      { label: '缓存写 tokens', value: formatTokens(tokens.cache_write) },
      { label: '思考 tokens', value: formatTokens(tokens.reasoning) },
    ];
  }

  // 汇总下方的口径说明：明确金额是查询派生、随价格与日志范围变化，不存在永久财务余额。
  function usageScopeNote(summary, filters) {
    var executions = summary ? summaryValue(summary, 'executions') : 0;
    var completeExecutions = summary ? summaryValue(summary, 'complete_executions') : 0;
    var unpriced = summary ? summaryValue(summary, 'unpriced_executions') : 0;
    var coverage = '—';
    var execCount = Number(intString(executions));
    var completeCount = Number(intString(completeExecutions));
    if (isFinite(execCount) && execCount > 0) {
      coverage = ((completeCount / execCount) * 100).toFixed(1) + '%';
    }
    var text =
      '质量覆盖说明：完整执行占比 = ' + formatInt(completeExecutions) + ' / ' + formatInt(executions) +
      ' = ' + coverage + '；它只反映实际用量质量，不代表价格完整率。' +
      '「部分（partial）」或「未知（unknown）」执行不计入占比，也不会被当作 0。';
    text +=
      '费用口径：所有金额都是查询时按当前价格 × 实际日志即时计算，只覆盖当前选中的时间范围和保留边界；' +
      '修改、添加或删除价格后历史金额立即重算，不存在冻结价或永久累计支出。' +
      '已定价费用小计只汇总可计算执行，未定价执行 ' + formatInt(unpriced) + ' 条不计入小计，也不是免费。';
    if (hasExecutionScope(filters)) {
      text += ' 当前已按 Provider/auth_id 过滤：明细与金额只包含匹配的执行，不代表整个 Key 的全部日志。';
    }
    return text;
  }

  function renderUsageSummary(summary) {
    var container = $('usage-summary');
    var noteNode = $('usage-quality-note');
    if (!container) return;
    if (!summary) {
      replace(container, []);
      if (noteNode) noteNode.textContent = '';
      return;
    }
    replace(
      container,
      usageSummaryCards(summary, state.usage.filters).map(function (card) {
        return metricCard(card.label, card.value, card.sub, card.tone);
      })
    );
    if (noteNode) noteNode.textContent = usageScopeNote(summary, state.usage.filters);
  }

  function renderUsageTrend(trend) {
    var reqContainer = $('usage-chart');
    var tokenContainer = $('usage-token-chart');
    var costContainer = $('usage-cost-chart');
    var legend = $('usage-legend');
    if (!reqContainer && !tokenContainer && !costContainer) return;

    var charts = getChartsApi();
    if (charts && typeof charts.render === 'function') {
      charts.render(
        {
          requests: reqContainer,
          tokens: tokenContainer,
          cost: costContainer,
        },
        trend || [],
        {
          bucket: (state.usage.filters && state.usage.filters.bucket) || 'day',
          tokenUnit: state.tokenUnit || 'raw',
          currency: state.currency || 'USD',
          rate: Number(state.currencyRate) || DEFAULT_USD_CNY_RATE,
          emptyText: '当前筛选范围内没有可绘制的趋势数据。',
        }
      );
      return;
    }

    if (!trend || !trend.length) {
      if (reqContainer) renderEmpty(reqContainer, '当前筛选范围内没有可绘制的趋势数据。');
      if (tokenContainer) renderEmpty(tokenContainer, '当前筛选范围内没有可绘制的趋势数据。');
      if (costContainer) renderEmpty(costContainer, '当前筛选范围内没有可绘制的趋势数据。');
      if (legend) replace(legend, []);
      return;
    }
  }

  function renderUsageRetention(report) {
    var node = $('usage-retention');
    if (!node) return;
    var from = report ? report.retention_from : null;
    if (!from) {
      node.textContent = '保留边界：服务端未返回 retention_from，无法确认历史清理范围。';
      return;
    }
    node.textContent =
      '保留边界：早于 ' + formatDateTime(from) + ' 的历史记录已按保留策略清理；更早的请求不会出现在本列表中。';
  }

  // 单列展示规范 tokens，避免 16 列把模型标识挤到逐字断行。
  function tokenLines(tokens) {
    var data = tokens || {};
    var rows = [
      ['输入', data.input],
      ['缓存读', data.cache_read],
      ['缓存写', data.cache_write],
      ['输出', data.output],
      ['思考', data.reasoning],
      ['合计', data.total],
    ];
    return el(
      'div',
      { class: 'token-lines' },
      rows.map(function (row) {
        return el('div', { class: 'token-line' }, [
          el('span', { class: 'token-key', text: row[0] }),
          el('span', { class: 'token-value', text: formatInt(row[1] === undefined || row[1] === null ? 0 : row[1]) }),
        ]);
      })
    );
  }

  function renderUsageExecutionRow(execution, request) {
    var relation = modelRelation(request.model, execution.model, execution.response_model);
    var statusTone = execution.failed ? 'danger' : 'ok';
    var priced = hasCostValue(execution.cost_micro_usd);
    var costText = priced ? formatMicroUSD(execution.cost_micro_usd) : '未定价';
    var costSub = priced
      ? formatInt(execution.cost_micro_usd) + ' µUSD（当前价格 × 实际用量）'
      : '未定价：缺失价格、用量不完整或无法证明计费数量（不是 0）';

    var responseCell = el('td', {}, [
      el('div', { class: 'nowrap mono', text: execution.response_model ? String(execution.response_model) : '未上报' }),
      relation.namespaceOnly ? el('div', { class: 'pill-note', text: relation.note }) : null,
    ]);

    return el('tr', {}, [
      el('td', {}, [el('div', { class: 'nowrap mono', text: execution.model ? String(execution.model) : '—' })]),
      responseCell,
      el('td', {}, [
        el('div', { text: execution.provider || '—' }),
        el('div', { class: 'nowrap mono', text: execution.auth_id || '—' }),
        execution.auth_index ? el('div', { class: 'pill-note', text: 'auth_index ' + execution.auth_index }) : null,
      ]),
      el('td', {}, [
        badge(execution.failed ? '失败' : '成功', statusTone),
        execution.status_code ? el('div', { class: 'pill-note', text: 'HTTP ' + execution.status_code }) : null,
      ]),
      el('td', { class: 'tokens' }, [tokenLines(execution.tokens)]),
      el('td', {}, [badge(qualityLabel(execution.quality), qualityTone(execution.quality))]),
      el('td', { class: 'num' }, [
        el('div', { class: 'nowrap', text: costText }),
        el('div', { class: 'pill-note', text: costSub }),
      ]),
      el('td', { class: 'num', text: formatDurationMS(execution.ttft_ms) }),
      el('td', { class: 'num', text: formatDurationMS(execution.latency_ms) }),
      el('td', {}, [el('div', { class: 'mono-break', text: execution.executor || '—' })]),
    ]);
  }

  function executionTable(request) {
    var head = el('tr', {}, [
      el('th', { text: '执行模型' }),
      el('th', { text: '响应模型（上游声明）' }),
      el('th', { text: 'Provider / 上游凭据' }),
      el('th', { text: '状态' }),
      el('th', { title: '输入 / 缓存读 / 缓存写 / 输出 / 思考 / 合计（规范 tokens）', text: '规范 tokens' }),
      el('th', { text: '质量' }),
      el('th', { class: 'num', text: '成本' }),
      el('th', { class: 'num', text: 'TTFT' }),
      el('th', { class: 'num', text: '延迟' }),
      el('th', { text: '执行器' }),
    ]);
    var body = el('tbody', {}, (request.executions || []).map(function (execution) {
      return renderUsageExecutionRow(execution, request);
    }));
    return tableScroll(el('table', { class: 'data data-wide' }, [el('thead', {}, [head]), body]));
  }

  function renderUsageRequestCard(request) {
    var executions = request.executions || [];
    var keyLabel = (function () {
      if (!state.keys.items) return request.key_id || '—';
      for (var i = 0; i < state.keys.items.length; i++) {
        if (state.keys.items[i].id === request.key_id) {
          var label = state.keys.items[i].spec && state.keys.items[i].spec.label;
          return (label ? label + ' ' : '') + '（' + shortId(request.key_id) + '）';
        }
      }
      return request.key_id || '—';
    })();

    var head = el('div', { class: 'row-title' }, [
      el('span', { class: 'request-model mono', text: request.model || '—' }),
      el('span', { class: 'key-label mono', text: '请求 ' + shortId(request.id), title: String(request.id || '') }),
      badge(outcomeLabel(request.outcome), outcomeTone(request.outcome)),
      badge(stateLabel(request.state), isPendingRequest(request) ? 'warn' : ''),
      el('span', { class: 'pill-note', text: 'Key ' + keyLabel }),
      el('span', { class: 'pill-note', text: '开始 ' + formatDateTime(request.started_at) }),
    ]);

    var knownCosts = executions.filter(function (e) {
      return hasCostValue(e.cost_micro_usd);
    });
    var unpricedCount = executions.length - knownCosts.length;
    var executionCost = global.CMNumbers.sumIntTexts(knownCosts.map(function (e) { return e.cost_micro_usd; }));
    var costText;
    if (!executions.length) {
      costText = '费用按实际执行日志计算（当前无执行记录）';
    } else if (!knownCosts.length) {
      costText = '已定价小计 未定价（' + formatInt(unpricedCount) + ' 条执行，不是免费）';
    } else {
      costText = '已定价小计 ' + formatMicroUSD(executionCost) +
        (unpricedCount ? '；另有 ' + formatInt(unpricedCount) + ' 条未定价（不是免费）' : '');
    }
    var costLine = el('div', { class: 'request-totals' }, [
      el('span', { text: '执行 ' + formatInt(executions.length) + ' 次' }),
      el('span', {
        class: 'mono' + (knownCosts.length ? '' : ' is-warn'),
        text: costText,
      }),
    ]);

    var actions = el('div', { class: 'btn-row' }, [
      el('button', {
        class: 'btn btn-small',
        type: 'button',
        text: '查看请求体',
        onclick: function () {
          openCaptureDialog(request);
        },
      }),
    ]);

    var relation = modelRelation(
      request.model,
      executions.length ? executions[0].model : '',
      executions.length ? executions[0].response_model : ''
    );

    var meta = el('div', { class: 'request-meta' }, [
      el('div', { class: 'kv' }, [
        el('span', { class: 'kv-k', text: '请求模型' }),
        el('span', { class: 'kv-v', text: request.model ? String(request.model) : '—' }),
      ]),
      el('div', { class: 'kv' }, [
        el('span', { class: 'kv-k', text: '执行模型' }),
        el('span', { class: 'kv-v', text: executions.length && executions[0].model ? String(executions[0].model) : '—' }),
      ]),
      el('div', { class: 'kv' }, [
        el('span', { class: 'kv-k', text: '响应模型' }),
        el('span', {
          class: 'kv-v',
          text: executions.length && executions[0].response_model ? String(executions[0].response_model) : '未上报',
        }),
      ]),
      el('div', { class: 'kv' }, [
        el('span', { class: 'kv-k', text: '服务等级' }),
        el('span', { class: 'kv-v', text: request.service_tier ? String(request.service_tier) : '未上报' }),
      ]),
      el('div', { class: 'kv kv-wide' }, [
        el('span', { class: 'kv-k', text: '费用口径' }),
        el('span', {
          class: 'kv-v',
          text: '按实际执行日志与当前价格查询时计算；请求本身不保存冻结价格或人工金额。修改价格后该请求的历史金额会随之重算。',
        }),
      ]),
    ]);

    var scoped = hasExecutionScope(state.usage.filters);

    var detail = el('details', { class: 'request-detail' }, [
      el('summary', { text: '查看执行明细与计费依据' }),
      el('p', { class: 'note mono', text: 'trace ' + (request.trace_id || '—') +
        ' · ' + (request.completed_at ? '完成 ' + formatDateTime(request.completed_at) : '尚未完成') }),
      relation.namespaceOnly ? el('p', { class: 'note', text: relation.note }) : null,
      meta,
      executions.length ? executionTable(request) : el('p', {
        class: 'note',
        text: isPendingRequest(request)
          ? '该请求尚无执行记录，状态为用量待完整：费用只按实际日志与当前价格查询时计算，不需要人工结算。'
          : '该请求尚无执行记录；执行事件到达后，金额会按实际日志与当前价格查询时计算。',
      }),
    ]);
    return el('div', { class: 'card request-card' }, [
      head,
      el('div', { class: 'request-toolbar' }, [costLine, actions]),
      scoped
        ? el('p', {
            class: 'note',
            text:
              '当前按 Provider/auth_id 过滤：下方明细与该卡片的金额只包含匹配的执行，' +
              '不代表该 Key 或全历史的全部支出。',
          })
        : null,
      detail,
    ]);
  }

  function renderUsageItems(report) {
    var container = $('usage-items');
    var totalNode = $('usage-total');
    if (!container) return;
    var items = (report && report.items) || [];
    if (totalNode) {
      totalNode.textContent = '匹配总数：' + formatInt(report && report.total !== undefined ? report.total : items.length) + ' 条';
    }
    if (!items.length) {
      renderEmpty(container, state.usage.loading ? '加载中…' : '当前筛选条件下没有请求记录，可放宽时间范围或清空过滤条件。');
      return;
    }
    replace(container, items.map(renderUsageRequestCard));
  }

  function renderUsagePagination(report) {
    var container = $('usage-pagination');
    if (!container) return;
    var page = report && report.page ? report.page : state.usage.filters.page;
    var pageSize = report && report.page_size ? report.page_size : state.usage.filters.pageSize;
    var total = report && report.total !== undefined ? report.total : 0;
    var totalNum = Number(intString(total));
    var pages = pageSize > 0 && isFinite(totalNum) ? Math.max(1, Math.ceil(totalNum / pageSize)) : 1;
    var prevDisabled = Number(intString(page)) <= 1;
    var nextDisabled = Number(intString(page)) >= pages;

    var go = function (target) {
      state.usage.filters.page = target;
      loadUsage(true);
    };

    replace(container, [
      el('button', {
        class: 'btn btn-small',
        type: 'button',
        text: '上一页',
        disabled: prevDisabled || undefined,
        onclick: function () {
          go(Math.max(1, Number(intString(page)) - 1));
        },
      }),
      el('button', {
        class: 'btn btn-small',
        type: 'button',
        text: '下一页',
        disabled: nextDisabled || undefined,
        onclick: function () {
          go(Number(intString(page)) + 1);
        },
      }),
      el('span', { class: 'muted', text: '第 ' + formatInt(page) + ' / ' + formatInt(pages) + ' 页　每页 ' + formatInt(pageSize) + ' 条　共 ' + formatInt(total) + ' 条' }),
      el('span', { class: 'muted', text: '翻页会保留当前全部过滤条件' }),
    ]);
  }

  /* ---------------- 请求体快照 ---------------- */

  // 快照状态取值来自 internal/capture（captured/disabled/oversize/dropped/expired/unavailable）。
  // 未收录的状态原样显示，既不猜测含义也不隐藏。
  var CAPTURE_STATUS_LABELS = {
    captured: { label: '快照可用', tone: 'ok' },
    disabled: { label: '捕获功能未启用', tone: 'warn' },
    oversize: { label: '快照超出体积上限', tone: 'warn' },
    dropped: { label: '快照已丢弃', tone: 'warn' },
    expired: { label: '快照已过期', tone: 'warn' },
    unavailable: { label: '快照不可用', tone: 'warn' },
  };

  function captureStatusText(status) {
    var text = String(status == null ? '' : status);
    if (Object.prototype.hasOwnProperty.call(CAPTURE_STATUS_LABELS, text)) return CAPTURE_STATUS_LABELS[text];
    return { label: text || '未知状态', tone: '' };
  }

  function openCaptureDialog(request) {
    var handle = openDialog({
      title: '请求体快照 · ' + shortId(request.id),
      wide: true,
      body: [el('div', { class: 'loading-state', text: '正在读取快照…' })],
    });
    api.capture(request.id).then(
      function (detail) {
        renderCaptureDetail(handle, detail);
      },
      function (error) {
        if (failRequest(error)) return;
        handle.setError(describeError(error));
        handle.setBody([
          el('p', {
            class: 'note',
            text: '快照可能在保留期内已被清理，或捕获功能未启用；详情见状态页的捕获统计。',
          }),
        ]);
      }
    );
  }

  function renderCaptureDetail(handle, detail) {
    var data = detail || {};
    var status = captureStatusText(data.status);
    var rawBody = typeof data.body === 'string' ? data.body : '';
    var formatted = formatBodyLossless(rawBody);
    var activeView = { mode: 'pretty', tab: 'body' };

    var headersTable = (function () {
      var headers = data.headers || {};
      var rows = Object.keys(headers)
        .sort()
        .map(function (key) {
          var value = headers[key];
          var text = Array.isArray(value) ? value.join(', ') : String(value);
          return el('tr', {}, [el('td', { class: 'mono-break', text: key }), el('td', { class: 'mono-break', text: text })]);
        });
      if (!rows.length) return el('p', { class: 'note', text: '该快照没有记录请求头。' });
      return tableScroll(
        el('table', { class: 'data' }, [
          el('thead', {}, [el('tr', {}, [el('th', { text: '键' }), el('th', { text: '值' })])]),
          el('tbody', {}, rows),
        ])
      );
    })();

    var viewContainer = el('div', {});
    var tabsBar = el('div', { class: 'subtabs', role: 'tablist' });
    var bodyTabButton = el('button', { class: 'subtab', type: 'button', role: 'tab', text: '正文', 'aria-selected': 'true' });
    var headerTabButton = el('button', { class: 'subtab', type: 'button', role: 'tab', text: '请求头', 'aria-selected': 'false' });
    tabsBar.appendChild(bodyTabButton);
    tabsBar.appendChild(headerTabButton);

    var renderView = function () {
      if (activeView.tab === 'headers') {
        var headerBar = el('div', { class: 'btn-row' }, [
          copyButton(function () {
            var lines = [];
            var headers = data.headers || {};
            Object.keys(headers).sort().forEach(function (key) {
              var val = headers[key];
              lines.push(key + ': ' + (Array.isArray(val) ? val.join(', ') : String(val)));
            });
            return lines.join('\n');
          }, '复制请求头'),
        ]);
        replace(viewContainer, [headerBar, headersTable]);
        return;
      }
      var modeBar = el('div', { class: 'btn-row' }, [
        el('button', {
          class: 'btn btn-small',
          type: 'button',
          text: '原文',
          disabled: activeView.mode === 'raw' ? true : undefined,
          onclick: function () {
            activeView.mode = 'raw';
            renderView();
          },
        }),
        el('button', {
          class: 'btn btn-small',
          type: 'button',
          text: '格式化',
          disabled: activeView.mode === 'pretty' || !formatted.ok ? true : undefined,
          onclick: function () {
            activeView.mode = 'pretty';
            renderView();
          },
        }),
        copyButton(function () {
          return activeView.mode === 'pretty' && formatted.ok ? formatted.text : rawBody;
        }, '复制当前视图'),
      ]);
      var text = activeView.mode === 'pretty' && formatted.ok ? formatted.text : rawBody;
      replace(viewContainer, [
        modeBar,
        el('p', {
          class: 'note',
          text:
            '格式化使用词法级美化，不经过 JSON.parse/stringify 重写，超出 Number 精度的整数会逐字保留。' +
            (formatted.ok ? '' : '　' + formatted.reason),
        }),
        codeBlock(text || '（正文为空）'),
        el('p', { class: 'note', text: '正文字符数：' + formatInt(rawBody.length) }),
      ]);
    };

    bodyTabButton.addEventListener('click', function () {
      activeView.tab = 'body';
      bodyTabButton.setAttribute('aria-selected', 'true');
      headerTabButton.setAttribute('aria-selected', 'false');
      renderView();
    });
    headerTabButton.addEventListener('click', function () {
      activeView.tab = 'headers';
      bodyTabButton.setAttribute('aria-selected', 'false');
      headerTabButton.setAttribute('aria-selected', 'true');
      renderView();
    });

    var rowTitleNodes = [
      badge(status.label, status.tone),
      el('span', { class: 'pill-note', text: '请求 ID ' + (data.request_id || '—') }),
      el('span', { class: 'pill-note', text: '记录时间 ' + formatDateTime(data.at) }),
      el('span', { class: 'pill-note', text: '过期时间 ' + formatDateTime(data.expires_at) }),
    ];
    var bodyChildren = [el('div', { class: 'row-title' }, rowTitleNodes)];
    if (data.status === 'expired') {
      bodyChildren.push(el('p', { class: 'note hint-warn', text: '快照已过期：该请求已超出快照保留周期，详细数据已被清理。' }));
    } else if (data.status === 'unavailable') {
      bodyChildren.push(el('p', { class: 'note hint-warn', text: '快照不可用：该请求未捕获快照或在入库前已被丢弃。' }));
    }
    bodyChildren.push(
      el('p', {
        class: 'note',
        text: '该快照是执行入口处的请求快照（不是上游网络的原始字节），敏感头与明显的密钥字段已在写入前被替换。',
      }),
      tabsBar,
      viewContainer
    );
    handle.setBody(bodyChildren);
    renderView();
  }

  /* ================================================================== *
   * 客户端 Key
   * ================================================================== */

  function loadKeys(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.keys.items) return Promise.resolve();
    var seq = ++state.keys.loadSeq;
    state.keys.loading = true;
    state.keys.error = '';
    renderKeys();
    return api.listKeys().then(
      function (data) {
        if (seq !== state.keys.loadSeq) return;
        state.keys.loading = false;
        state.keys.items = (data && data.items) || [];
        renderKeys();
        renderUsageKeyOptions();
      },
      function (error) {
        if (seq !== state.keys.loadSeq) return;
        if (failRequest(error)) return;
        state.keys.loading = false;
        state.keys.error = describeError(error);
        renderKeys();
      }
    );
  }

  function renderUsageKeyOptions() {
    var select = $('usage-key');
    var overviewSelect = $('overview-key');
    var items = state.keys.items || [];
    var buildOptions = function () {
      var opts = [el('option', { value: '', text: '全部' })];
      items.forEach(function (key) {
        var label = (key.spec && key.spec.label) || '';
        opts.push(
          el('option', {
            value: key.id,
            text: (label ? label + ' · ' : '') + (key.fingerprint || shortId(key.id)),
          })
        );
      });
      return opts;
    };
    if (select) {
      var current = select.value;
      replace(select, buildOptions());
      select.value = current || '';
      if (!select.value) select.value = '';
    }
    if (overviewSelect) {
      var currentOv = overviewSelect.value;
      replace(overviewSelect, buildOptions());
      overviewSelect.value = currentOv || '';
      if (!overviewSelect.value) overviewSelect.value = '';
    }
  }

  function limitLine(label, value) {
    var text = intString(value);
    if (text === '0' || text === '') {
      return el('div', { class: 'nowrap', text: label + '：未设置' });
    }
    return el('div', { class: 'nowrap', text: label + '：' + microToUSDText(text) + ' USD · ' + formatInt(text) + ' µUSD' });
  }

  function renderKeys() {
    var container = $('keys-list');
    if (!container) return;
    var status = $('keys-status');
    renderErrorBox($('keys-error'), state.keys.error, function () {
      loadKeys(true);
    });
    if (state.keys.loading && !state.keys.items) {
      setStatusLine(status, '正在读取客户端 Key…');
      renderLoading(container, '加载中…');
      return;
    }
    if (!state.keys.items) {
      setStatusLine(status, '');
      renderEmpty(container, state.key ? '暂无数据，点击刷新重新读取。' : '请先登录。');
      return;
    }
    setStatusLine(status, '共 ' + formatInt(state.keys.items.length) + ' 个 Key（接口：GET ' + MANAGER_BASE + '/api/keys）');
    if (!state.keys.items.length) {
      renderEmpty(container, '还没有客户端 Key，点击「新建 Key」创建第一个。');
      return;
    }

    var rows = state.keys.items.map(function (key) {
      var spec = key.spec || {};
      var allowed = spec.allowed_models || [];
      var limits = spec.model_token_limits || [];
      var isDeleted = Boolean(key.deleted_at);
      var callerName = (function () {
        if (!spec.caller_id) return '—';
        var callers = state.callers.items || [];
        for (var c = 0; c < callers.length; c++) {
          if (callers[c].id === spec.caller_id) {
            return (callers[c].display_name ? callers[c].display_name + ' ' : '') + '（' + callers[c].id + '）';
          }
        }
        return spec.caller_id;
      })();

      var statusBadges = [];
      if (isDeleted) {
        statusBadges.push(badge('已软删除', 'warn'));
        statusBadges.push(el('div', { class: 'pill-note', text: '删除时间 ' + formatDateTime(key.deleted_at) }));
      } else {
        statusBadges.push(badge(spec.enabled ? '已启用' : '已禁用', spec.enabled ? 'ok' : 'warn'));
        if (spec.expires_at) {
          statusBadges.push(el('div', { class: 'pill-note', text: '过期 ' + formatDateTime(spec.expires_at) }));
        }
      }

      return el('tr', { class: 'row-main' + (isDeleted ? ' is-deleted' : '') }, [
        el('td', {}, [
          el('div', { text: spec.label || '（未命名）' }),
          el('div', { class: 'pill-note mono-break', text: key.id || '—' }),
        ]),
        el('td', {}, [
          el('div', { text: callerName }),
        ]),
        el('td', {}, statusBadges),
        el('td', {}, [el('div', { class: 'key-fingerprint', text: key.fingerprint || '—' })]),
        el('td', {}, [el('div', { class: 'key-principal', text: key.principal || '—' })]),
        el('td', { class: 'limits' }, [
          el('div', { class: 'pill-note', text: '仅展示，不限制调用' }),
          limitLine('总额预算', spec.quota_micro_usd),
          limitLine('日预算', spec.daily_micro_usd),
          limitLine('周预算', spec.weekly_micro_usd),
          limitLine('月预算', spec.monthly_micro_usd),
        ]),
        el('td', { class: 'num', text: intString(spec.max_concurrent) === '0' ? '不限' : formatInt(spec.max_concurrent) }),
        el('td', {}, [
          allowed.length
            ? el('div', { class: 'mono-break', text: allowed.join(', ') })
            : el('span', { class: 'pill-note', text: '全部模型' }),
          limits.length
            ? el('div', { class: 'pill-note', text: '模型 token 限额：' + formatInt(limits.length) + ' 条' })
            : null,
        ]),
        el('td', {}, [
          el('div', { class: 'btn-row' }, [
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '查看密钥',
              onclick: function () {
                openRevealKeyDialog(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '编辑',
              disabled: isDeleted ? true : undefined,
              onclick: function () {
                openKeyEditor(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: spec.enabled ? '禁用' : '启用',
              disabled: isDeleted ? true : undefined,
              onclick: function () {
                toggleKeyEnabled(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '轮换密钥',
              disabled: isDeleted ? true : undefined,
              onclick: function () {
                openRotateDialog(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '重置 Token',
              disabled: isDeleted ? true : undefined,
              onclick: function () {
                openResetTokensDialog(key);
              },
            }),
            el('button', {
              class: 'btn btn-small btn-danger',
              type: 'button',
              text: '删除',
              disabled: isDeleted ? true : undefined,
              onclick: function () {
                openDeleteKeyDialog(key);
              },
            }),
          ]),
        ]),
      ]);
    });

    var head = el('tr', {}, [
      el('th', { text: '标签 / Key ID' }),
      el('th', { text: '调用方' }),
      el('th', { text: '状态' }),
      el('th', { text: '指纹' }),
      el('th', { text: 'Principal' }),
      el('th', { class: 'num', text: '预算（仅展示，不限制调用）' }),
      el('th', { class: 'num', text: '并发' }),
      el('th', { text: '允许模型 / token 限额' }),
      el('th', { text: '操作' }),
    ]);
    replace(container, [tableScroll(el('table', { class: 'data keys-table' }, [el('thead', {}, [head]), el('tbody', {}, rows)]))]);
  }

  function openKeyEditor(existing) {
    var spec = (existing && existing.spec) || {};
    var creating = !existing;

    var labelInput = el('input', { type: 'text', id: 'key-label', value: spec.label || '' });
    var enabledInput = el('input', { type: 'checkbox', id: 'key-enabled', class: 'switch-input', role: 'switch' });
    enabledInput.checked = creating ? true : spec.enabled === true;
    var quotaField = moneyPairInput({ label: '总额预算（仅展示，不限制调用）', micro: intString(spec.quota_micro_usd || 0) });
    var dailyField = moneyPairInput({ label: '每日预算（仅展示）', micro: intString(spec.daily_micro_usd || 0) });
    var weeklyField = moneyPairInput({ label: '每周预算（仅展示）', micro: intString(spec.weekly_micro_usd || 0) });
    var monthlyField = moneyPairInput({ label: '每月预算（仅展示）', micro: intString(spec.monthly_micro_usd || 0) });
    var concurrentInput = el('input', { type: 'text', inputmode: 'numeric', id: 'key-concurrent', value: intString(spec.max_concurrent || 0) });
    var allowedInput = el('textarea', { id: 'key-allowed', rows: 4, placeholder: '每行一个精确模型 ID；留空表示允许全部模型' });
    allowedInput.value = (spec.allowed_models || []).join('\n');
    var limitsInput = el('textarea', { id: 'key-limits', rows: 6, placeholder: '[{"model":"gpt-5","total":1000000,"daily":0,"weekly":0,"monthly":0}]' });
    limitsInput.value = modelTokenLimitsToText(spec.model_token_limits);
    var expiresInput = el('input', { type: 'datetime-local', step: 'any', id: 'key-expires' });
    if (spec.expires_at) {
      var expiresDate = new Date(spec.expires_at);
      if (!isNaN(expiresDate.getTime())) expiresInput.value = localDatetimeValue(expiresDate);
    }
    var errorNode = el('p', { class: 'inline-error', hidden: true, role: 'alert' });
    // 自定义密钥材料只在新建时提供；编辑永远不下发该字段（服务端会拒绝回传材料）。
    var customKeyInput = creating
      ? el('input', {
          type: 'text',
          id: 'key-material',
          placeholder: '留空则由服务端生成',
          spellcheck: 'false',
          autocomplete: 'off',
        })
      : null;

    var handle = openDialog({
      title: creating ? '新建客户端 Key' : '编辑客户端 Key · ' + (spec.label || shortId(existing.id)),
      wide: true,
      body: [
        errorNode,
        creating ? el('p', {
          class: 'hint',
          text: '预算只用于展示，不限制请求；0 表示未设置预算。费用在查询日志时按当前价格与实际用量计算，缺价不会阻止调用。',
        }) : null,
        creating
          ? el('p', {
              class: 'note',
              text: '新建后可选择由服务端生成密钥，或填写自定义密钥；明文密钥只在创建响应中出现一次。',
            })
          : el('p', {
              class: 'note',
              text: '编辑会提交完整 KeySpec：界面会以服务端返回的现有 spec 为基础合并本次修改，未涉及的字段（含未知字段）会被原样保留。',
            }),
        el('div', { class: 'form-grid' }, [
          el('div', { class: 'field' }, [el('label', { for: 'key-label', text: '标签' }), labelInput]),
          el('div', { class: 'field' }, [
            el('label', { text: '状态' }),
            el('label', { for: 'key-enabled', class: 'switch-control' }, [
              enabledInput,
              el('span', { class: 'switch-track', 'aria-hidden': 'true' }, [
                el('span', { class: 'switch-thumb' }),
              ]),
              el('span', { class: 'switch-label', text: '启用（新建默认启用）' }),
            ]),
          ]),
          el('div', { class: 'field' }, [
            el('label', { for: 'key-concurrent', text: '最大并发（0 表示不限）' }),
            concurrentInput,
          ]),
          el('div', { class: 'field' }, [
            el('label', { for: 'key-expires', text: '过期时间（留空表示不过期）' }),
            expiresInput,
          ]),
          quotaField.node,
          dailyField.node,
          weeklyField.node,
          monthlyField.node,
          creating
            ? el('div', { class: 'field span-all' }, [
                el('label', { for: 'key-material', text: '自定义密钥（可选，留空由服务端生成）' }),
                el('p', {
                  class: 'note',
                  text: '原样提交、不做裁剪：首尾空白会由服务端校验拒绝，绝不会被悄悄改写。明文密钥只在创建响应中出现一次。',
                }),
                customKeyInput,
              ])
            : null,
          el('div', { class: 'field span-all' }, [
            el('label', { for: 'key-allowed', text: '允许模型（每行一个精确 ID；留空 = 全部）' }),
            allowedInput,
          ]),
          el('div', { class: 'field span-all' }, [
            el('label', { for: 'key-limits', text: '模型 token 限额（JSON 数组，可留空）' }),
            el('p', {
              class: 'note',
              text: '每项格式：{"model":"精确模型ID","total":整数,"daily":整数,"weekly":整数,"monthly":整数}，0 表示不限。',
            }),
            limitsInput,
          ]),
        ]),
      ],
      footer: [
        el('button', { class: 'btn', type: 'button', text: '取消', onclick: closeDialog }),
        el('button', {
          class: 'btn btn-primary',
          type: 'button',
          text: creating ? '创建' : '保存',
          onclick: function () {
            // 提交进行中忽略重复点击：避免快速双击创建出两个 Key，或把已失效的新密钥当成有效结果展示。
            if (this.disabled) return;
            var errors = [];
            var fields = {
              label: labelInput.value,
              enabled: enabledInput.checked,
              quotaMicroUSD: readMoneyInput(quotaField, '总额预算', errors),
              dailyMicroUSD: readMoneyInput(dailyField, '每日预算', errors),
              weeklyMicroUSD: readMoneyInput(weeklyField, '每周预算', errors),
              monthlyMicroUSD: readMoneyInput(monthlyField, '每月预算', errors),
              maxConcurrent: '0',
              allowedModels: splitAllowedModels(allowedInput.value),
            };
            if (creating) fields.key = customKeyInput.value;
            var concurrent = concurrentInput.value.trim();
            if (concurrent === '') concurrent = '0';
            if (!isIntegerText(concurrent) || concurrent.charAt(0) === '-') {
              errors.push('最大并发必须是 ≥ 0 的整数');
            } else {
              fields.maxConcurrent = concurrent;
            }
            var limits = parseModelTokenLimits(limitsInput.value);
            if (!limits.ok) {
              errors.push(limits.error);
            } else {
              fields.modelTokenLimits = limits.value;
            }
            if (expiresInput.value) {
              var iso = toISOFromLocal(expiresInput.value);
              if (!iso) {
                errors.push('过期时间格式不正确');
              } else {
                fields.expiresAt = iso;
              }
            }
            if (errors.length) {
              errorNode.hidden = false;
              errorNode.textContent = errors.join('；');
              return;
            }
            errorNode.hidden = true;
            var submitButton = this;
            submitButton.disabled = true;
            saveKey(handle, existing, fields, function () {
              // 只恢复发起本次提交的按钮，不触碰后来打开的弹窗。
              submitButton.disabled = false;
            });
          },
        }),
      ],
    });
    return handle;
  }

  function saveKey(handle, existing, fields, onFailure) {
    var editedSpec = keySpecPayload(fields);
    var request;
    if (existing) {
      var merged = mergeKeySpec(existing.spec, editedSpec);
      request = api.updateKey(existing.id, merged);
    } else {
      request = api.createKey(editedSpec);
    }
    request.then(
      function (data) {
        handle.close();
        var item = data && data.item;
        loadKeys(true);
        if (!existing && data && data.key) {
          openSecretDialog('新建 Key 成功', data.key, '请立即保存该明文密钥。');
        } else {
          notify('已保存 Key：' + ((item && item.spec && item.spec.label) || (item && item.id) || ''), 'ok');
        }
      },
      function (error) {
        if (failRequest(error)) return;
        if (onFailure) onFailure();
        handle.setError(describeError(error));
      }
    );
  }

  function openSecretDialog(title, keyText, note) {
    openDialog({
      title: title,
      body: [
        el('p', { class: 'hint hint-warn', text: '本弹窗只显示这一次，关闭后不在浏览器留存。新 Key 可经管理鉴权从「查看密钥」再次获取，每次查看均记审计。' }),
        el('div', { class: 'secret-box', text: keyText }),
        el('p', { class: 'note', text: note || '' }),
      ],
      footer: [
        copyButton(function () {
          return keyText;
        }, '复制密钥', '密钥已复制'),
        el('button', { class: 'btn btn-primary', type: 'button', text: '我已保存，关闭', onclick: closeDialog }),
      ],
    });
  }

  function toggleKeyEnabled(key) {
    var spec = key.spec || {};
    var next = !spec.enabled;
    confirmDialog({
      title: next ? '启用该 Key' : '禁用该 Key',
      message: next
        ? '启用后该 Key 可以再次发起请求。'
        : '禁用后使用该 Key 发起的请求会被拒绝（不影响已产生的账务）。',
      warning: next ? '' : '禁用会影响线上调用方，请确认。',
      confirmText: next ? '启用' : '禁用',
      danger: !next,
    }).then(function (confirmed) {
      if (!confirmed) return;
      var merged = mergeKeySpec(spec, { enabled: next });
      api.updateKey(key.id, merged).then(
        function () {
          notify(next ? '已启用该 Key' : '已禁用该 Key', 'ok');
          loadKeys(true);
        },
        function (error) {
          if (failRequest(error)) return;
          notify('更新失败：' + describeError(error), 'error');
        }
      );
    });
  }

  function openRotateDialog(key) {
    var customInput = el('input', { type: 'text', id: 'rotate-key', placeholder: '留空则由服务端生成', spellcheck: 'false' });
    var confirmInput = el('input', { type: 'checkbox', id: 'rotate-confirm' });
    var handle = openDialog({
      title: '轮换密钥 · ' + ((key.spec && key.spec.label) || shortId(key.id)),
      body: [
        el('p', { class: 'hint hint-warn', text: '轮换后旧密钥立即失效，使用旧密钥的调用方会开始失败，请先确认调用方已准备好更新。' }),
        el('div', { class: 'field' }, [
          el('label', { for: 'rotate-key', text: '自定义密钥（可选）' }),
          customInput,
        ]),
        el('div', { class: 'field' }, [
          el('label', { for: 'rotate-confirm', text: '我确认旧密钥立即失效' }),
          confirmInput,
        ]),
      ],
      footer: [
        el('button', { class: 'btn', type: 'button', text: '取消', onclick: closeDialog }),
        el('button', {
          class: 'btn btn-primary',
          type: 'button',
          text: '确认轮换',
          onclick: function () {
            // 提交进行中忽略重复点击：轮换不是幂等操作，重复提交会连续换掉两次密钥。
            if (this.disabled) return;
            if (!confirmInput.checked) {
              handle.setError('请先勾选确认项');
              return;
            }
            handle.setError('');
            this.disabled = true;
            var submitButton = this;
            // 自定义密钥原样提交（不裁剪）：留空才由服务端生成，
            // 首尾空白交给服务端校验拒绝，避免界面悄悄换成一个用户没输入的密钥。
            api.rotateKey(key.id, customInput.value).then(
              function (data) {
                closeDialog();
                loadKeys(true);
                if (data && data.key) {
                  openSecretDialog('轮换完成', data.key, '旧密钥已失效，请立即更新调用方配置。');
                } else {
                  notify('已轮换密钥，但服务端未返回明文密钥', 'error');
                }
              },
              function (error) {
                submitButton.disabled = false;
                if (failRequest(error)) return;
                handle.setError(describeError(error));
              }
            );
          },
        }),
      ],
    });
  }

  function openRevealKeyDialog(key) {
    if (!key || !key.id) return;
    var dialog = openDialog({
      title: '查看密钥明文 · ' + shortId(key.id),
      wide: false,
    });
    dialog.setBody([
      el('p', { class: 'muted', text: '正在向后端安全检索密文封存材料…' }),
    ]);

    api.revealKey(key.id).then(
      function (res) {
        var material = (res && res.key) || '';
        dialog.setBody([
          el('p', { class: 'hint hint-warn', text: '请妥善保管该明文密钥，该明文仅在弹窗内一次性显示，关闭后不在浏览器留存。' }),
          el('div', { class: 'field' }, [
            el('label', { text: 'Key 明文' }),
            el('div', { class: 'input-row' }, [
              el('input', { type: 'text', class: 'mono', value: material, readonly: true }),
              copyButton(function () { return material; }, '复制密钥'),
            ]),
          ]),
        ]);
        dialog.setFooter([
          el('button', { class: 'btn btn-primary', type: 'button', text: '已复制并关闭', onclick: dialog.close }),
        ]);
      },
      function (err) {
        dialog.setError('读取密钥失败：' + describeError(err));
        dialog.setFooter([
          el('button', { class: 'btn btn-ghost', type: 'button', text: '关闭', onclick: dialog.close }),
        ]);
      }
    );
  }

  function openDeleteKeyDialog(key) {
    if (!key || !key.id) return;
    var reasonInput = el('input', { type: 'text', id: 'delete-key-reason', placeholder: '请输入软删除原因（必填）' });
    var confirmCheckbox = el('input', { type: 'checkbox', id: 'delete-key-confirm' });
    var dialog = openDialog({
      title: '删除客户端 Key · ' + shortId(key.id),
      wide: false,
    });

    var submitBtn = el('button', {
      class: 'btn btn-danger',
      type: 'button',
      text: '确认软删除',
      onclick: function () {
        var reason = reasonInput.value.trim();
        if (!reason) {
          dialog.setError('请输入删除原因');
          return;
        }
        if (!confirmCheckbox.checked) {
          dialog.setError('请勾选确认复选框');
          return;
        }
        submitBtn.disabled = true;
        dialog.setError('');
        api.deleteKey(key.id, reason).then(
          function () {
            notify('客户端 Key 已成功软删除');
            dialog.close();
            loadKeys(true);
          },
          function (err) {
            submitBtn.disabled = false;
            dialog.setError(describeError(err));
          }
        );
      },
    });

    dialog.setBody([
      el('p', { class: 'hint hint-danger', text: '此操作将软删除并永久禁用该 Key。禁用后无法再用于发起任何 API 调用，但在该 Key 下产生的所有历史账单与使用记录均完整保留。' }),
      el('div', { class: 'form-grid' }, [
        el('div', { class: 'field' }, [
          el('label', { for: 'delete-key-reason', text: '删除原因（必填）' }),
          reasonInput,
        ]),
        el('div', { class: 'field' }, [
          el('label', { class: 'checkbox-label' }, [
            confirmCheckbox,
            document.createTextNode(' 我已知晓删除后无法撤销并确认软删除该 Key'),
          ]),
        ]),
      ]),
    ]);
    dialog.setFooter([
      submitBtn,
      el('button', { class: 'btn btn-ghost', type: 'button', text: '取消', onclick: dialog.close }),
    ]);
  }

  function openResetTokensDialog(key) {
    var isBatch = !key;
    var reasonInput = el('input', { type: 'text', id: 'reset-reason', placeholder: '例如: 月度账期刷新' });
    var chkTotal = el('input', { type: 'checkbox', checked: true });
    var chkDaily = el('input', { type: 'checkbox', checked: true });
    var chkWeekly = el('input', { type: 'checkbox', checked: true });
    var chkMonthly = el('input', { type: 'checkbox', checked: true });
    var confirmCheckbox = el('input', { type: 'checkbox', id: 'reset-tokens-confirm' });

    var dialog = openDialog({
      title: isBatch ? '批量重置 Token 用量基线' : ('重置 Key Token 用量基线 · ' + shortId(key.id)),
      wide: false,
    });

    var submitBtn = el('button', {
      class: 'btn btn-primary',
      type: 'button',
      text: '执行 Token 重置',
      onclick: function () {
        var reason = reasonInput.value.trim();
        if (!confirmCheckbox.checked) {
          dialog.setError('请勾选确认重置复选框');
          return;
        }
        submitBtn.disabled = true;
        dialog.setError('');
        var spec = {
          reason: reason || 'Token 用量基线重置',
          total: chkTotal.checked,
          daily: chkDaily.checked,
          weekly: chkWeekly.checked,
          monthly: chkMonthly.checked,
        };
        if (key && key.id) {
          spec.id = key.id;
        } else {
          spec.all = true;
        }

        api.resetTokens(spec).then(
          function (res) {
            notify('Token 用量基线已重置（影响 ' + (res && res.keys ? res.keys : 1) + ' 个 Key；不影响日志、费用或预算）');
            dialog.close();
            loadKeys(true);
          },
          function (err) {
            submitBtn.disabled = false;
            dialog.setError(describeError(err));
          }
        );
      },
    });

    dialog.setBody([
      el('p', { class: 'hint', text: '此操作只重置 Token 有效用量基线，不会删除或修改日志、查询派生费用与美元预算，也不会制造任何手工金额。进行中的请求与并发状态不受影响。' }),
      el('div', { class: 'form-grid' }, [
        el('div', { class: 'field' }, [
          el('label', { text: 'Token 计数周期选择' }),
          el('div', { class: 'checkbox-group' }, [
            el('label', { class: 'checkbox-label' }, [chkTotal, document.createTextNode(' 总计 Token (total)')]),
            el('label', { class: 'checkbox-label' }, [chkDaily, document.createTextNode(' 日度 Token (daily)')]),
            el('label', { class: 'checkbox-label' }, [chkWeekly, document.createTextNode(' 周度 Token (weekly)')]),
            el('label', { class: 'checkbox-label' }, [chkMonthly, document.createTextNode(' 月度 Token (monthly)')]),
          ]),
        ]),
        el('div', { class: 'field' }, [
          el('label', { for: 'reset-reason', text: '重置原因备注' }),
          reasonInput,
        ]),
        el('div', { class: 'field' }, [
          el('label', { class: 'checkbox-label' }, [
            confirmCheckbox,
            document.createTextNode(' 确认执行 Token 用量基线重置'),
          ]),
        ]),
      ]),
    ]);
    dialog.setFooter([
      submitBtn,
      el('button', { class: 'btn btn-ghost', type: 'button', text: '取消', onclick: dialog.close }),
    ]);
  }

  /* ================================================================== *
   * 调用方 (Callers)
   * ================================================================== */

  function loadCallers(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.callers.items) return Promise.resolve();
    var seq = ++state.callers.loadSeq;
    state.callers.loading = true;
    state.callers.error = '';
    renderCallers();
    return api.listCallers().then(
      function (data) {
        if (seq !== state.callers.loadSeq) return;
        state.callers.loading = false;
        state.callers.items = (data && data.items) || [];
        renderCallers();
        if (state.keys && state.keys.items) renderKeys();
      },
      function (error) {
        if (seq !== state.callers.loadSeq) return;
        if (failRequest(error)) return;
        state.callers.loading = false;
        state.callers.error = describeError(error);
        renderCallers();
      }
    );
  }

  function renderCallers() {
    var container = $('callers-list');
    if (!container) return;
    var status = $('callers-status');
    if (status) {
      status.textContent = state.callers.loading ? '正在读取调用方列表…' : ('共 ' + (state.callers.items ? state.callers.items.length : 0) + ' 个调用方');
    }
    renderErrorBox($('callers-error'), state.callers.error, function () {
      loadCallers(true);
    });
    if (state.callers.loading && !state.callers.items) {
      renderEmpty(container, '正在读取调用方…');
      return;
    }
    if (!state.callers.items || state.callers.items.length === 0) {
      renderEmpty(container, '暂无调用方数据。点击上方“新建调用方”添加团队或使用者。');
      return;
    }

    var table = el('table', { class: 'table data' }, [
      el('thead', {}, [
        el('tr', {}, [
          el('th', { text: '调用方 ID' }),
          el('th', { text: '显示名称 / 部门' }),
          el('th', { text: '状态' }),
          el('th', { text: '创建时间' }),
          el('th', { text: '操作' }),
        ]),
      ]),
      el('tbody', {}, state.callers.items.map(function (caller) {
        return el('tr', { class: 'row-main' }, [
          el('td', { class: 'mono', text: caller.id || '—' }),
          el('td', { text: caller.display_name || '（未命名）' }),
          el('td', {}, [
            badge(caller.enabled ? '已启用' : '已禁用', caller.enabled ? 'ok' : 'warn')
          ]),
          el('td', { class: 'muted', text: formatDateTime(caller.created_at) }),
          el('td', {}, [
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '编辑',
              onclick: function () {
                openCallerEditor(caller);
              },
            }),
          ]),
        ]);
      })),
    ]);
    replace(container, [tableScroll(table)]);
  }

  function openCallerEditor(caller) {
    var isEdit = Boolean(caller);
    var idInput = el('input', {
      type: 'text',
      id: 'caller-id',
      value: (caller && caller.id) || '',
      disabled: isEdit ? true : undefined,
      required: true,
      placeholder: '例如: team-alpha',
    });
    var nameInput = el('input', {
      type: 'text',
      id: 'caller-name',
      value: (caller && caller.display_name) || '',
      required: true,
      placeholder: '例如: 核心算法部',
    });
    var enabledInput = el('input', {
      type: 'checkbox',
      id: 'caller-enabled',
      checked: caller ? Boolean(caller.enabled) : true,
    });

    var dialog = openDialog({
      title: isEdit ? '编辑调用方 · ' + caller.id : '新建调用方',
      wide: false,
    });

    var form = el('form', { class: 'form-grid' }, [
      el('div', { class: 'field' }, [
        el('label', { for: 'caller-id', text: '调用方唯一 ID（必填，创建后不可更改）' }),
        idInput,
      ]),
      el('div', { class: 'field' }, [
        el('label', { for: 'caller-name', text: '显示名称 / 团队部门（必填）' }),
        nameInput,
      ]),
      el('div', { class: 'field' }, [
        el('label', { class: 'checkbox-label' }, [
          enabledInput,
          document.createTextNode(' 启用该调用方'),
        ]),
      ]),
    ]);

    var saveBtn = el('button', {
      class: 'btn btn-primary',
      type: 'button',
      text: isEdit ? '保存修改' : '创建调用方',
      onclick: function () {
        if (saveBtn.disabled) return;
        var id = idInput.value.trim();
        var displayName = nameInput.value.trim();
        var enabled = enabledInput.checked;
        idInput.removeAttribute('aria-invalid');
        nameInput.removeAttribute('aria-invalid');
        if (!id) {
          idInput.setAttribute('aria-invalid', 'true');
          dialog.setError('调用方 ID 不能为空');
          idInput.focus();
          return;
        }
        if (!displayName) {
          nameInput.setAttribute('aria-invalid', 'true');
          dialog.setError('显示名称不能为空，请填写调用方名称或团队部门');
          nameInput.focus();
          return;
        }
        saveBtn.disabled = true;
        dialog.setError('');
        var promise = isEdit
          ? api.updateCaller({ id: id, display_name: displayName, enabled: enabled })
          : api.createCaller({ id: id, display_name: displayName, enabled: enabled });
        promise.then(
          function () {
            notify(isEdit ? '调用方已更新' : '调用方创建成功');
            dialog.close();
            loadCallers(true);
          },
          function (err) {
            saveBtn.disabled = false;
            dialog.setError(describeError(err));
          }
        );
      },
    });

    dialog.setBody([form]);
    dialog.setFooter([
      saveBtn,
      el('button', { class: 'btn btn-ghost', type: 'button', text: '取消', onclick: dialog.close }),
    ]);
  }

  /* ================================================================== *
   * 审计日志 (Audit)
   * ================================================================== */

  function loadAudit(force) {
    if (!state.key) return Promise.resolve();
    var f = state.audit.filters;
    var query = 'page=' + state.audit.page + '&page_size=' + state.audit.pageSize;
    if (f.keyId) query += '&key_id=' + encodeURIComponent(f.keyId);
    if (f.callerId) query += '&caller_id=' + encodeURIComponent(f.callerId);

    var seq = ++state.audit.loadSeq;
    state.audit.loading = true;
    state.audit.error = '';
    renderAudit();

    return api.audit(query).then(
      function (data) {
        if (seq !== state.audit.loadSeq) return;
        state.audit.loading = false;
        state.audit.items = (data && data.items) || [];
        state.audit.total = (data && data.total !== undefined) ? data.total : 0;
        renderAudit();
      },
      function (error) {
        if (seq !== state.audit.loadSeq) return;
        if (failRequest(error)) return;
        state.audit.loading = false;
        state.audit.error = describeError(error);
        renderAudit();
      }
    );
  }

  function renderAudit() {
    var container = $('audit-list');
    var pagination = $('audit-pagination');
    var status = $('audit-status');
    if (!container) return;
    if (status) {
      status.textContent = state.audit.loading ? '正在查询审计日志…' : ('共 ' + formatInt(state.audit.total) + ' 条审计记录');
    }
    renderErrorBox($('audit-error'), state.audit.error, function () {
      loadAudit(true);
    });

    if (state.audit.loading && !state.audit.items) {
      renderEmpty(container, '正在查询日志…');
      return;
    }
    if (!state.audit.items || state.audit.items.length === 0) {
      renderEmpty(container, '当前筛选条件下暂无审计记录。');
      if (pagination) pagination.textContent = '';
      return;
    }

    var table = el('table', { class: 'table data' }, [
      el('thead', {}, [
        el('tr', {}, [
          el('th', { text: '序号' }),
          el('th', { text: '时间' }),
          el('th', { text: '操作动作' }),
          el('th', { text: 'Key ID' }),
          el('th', { text: '调用方 ID' }),
          el('th', { text: '原因' }),
          el('th', { text: '操作详情' }),
        ]),
      ]),
      el('tbody', {}, state.audit.items.map(function (ev) {
        return el('tr', { class: 'row-main' }, [
          el('td', { class: 'mono', text: String(ev.seq || '—') }),
          el('td', { class: 'muted', text: formatDateTime(ev.at) }),
          el('td', {}, [badge(ev.action || 'unknown', 'info')]),
          el('td', { class: 'mono', text: ev.key_id || '—' }),
          el('td', { class: 'mono', text: ev.caller_id || '—' }),
          el('td', { text: ev.reason || '—' }),
          el('td', { class: 'muted', text: ev.detail || '—' }),
        ]);
      })),
    ]);
    replace(container, [tableScroll(table)]);

    if (pagination) {
      var total = state.audit.total || 0;
      var pageSize = state.audit.pageSize || 20;
      var pages = Math.max(1, Math.ceil(Number(intString(total)) / pageSize));
      var page = state.audit.page;

      replace(pagination, [
        el('button', {
          class: 'btn btn-small',
          type: 'button',
          text: '上一页',
          disabled: page <= 1 ? true : undefined,
          onclick: function () {
            if (state.audit.page > 1) {
              state.audit.page -= 1;
              loadAudit(true);
            }
          },
        }),
        el('button', {
          class: 'btn btn-small',
          type: 'button',
          text: '下一页',
          disabled: page >= pages ? true : undefined,
          onclick: function () {
            if (state.audit.page < pages) {
              state.audit.page += 1;
              loadAudit(true);
            }
          },
        }),
        el('span', { class: 'muted', text: '第 ' + page + ' / ' + pages + ' 页 · 共 ' + formatInt(total) + ' 条' }),
      ]);
    }
  }

  function wireAuditForm() {
    var form = $('audit-filters');
    if (form) {
      form.addEventListener('submit', function (event) {
        event.preventDefault();
        state.audit.page = 1;
        state.audit.filters.keyId = $('audit-key-id') ? $('audit-key-id').value.trim() : '';
        state.audit.filters.callerId = $('audit-caller-id') ? $('audit-caller-id').value.trim() : '';
        state.audit.pageSize = $('audit-page-size') ? parseInt($('audit-page-size').value, 10) || 20 : 20;
        loadAudit(true);
      });
    }
    var resetBtn = $('audit-reset');
    if (resetBtn) {
      resetBtn.addEventListener('click', function () {
        if ($('audit-key-id')) $('audit-key-id').value = '';
        if ($('audit-caller-id')) $('audit-caller-id').value = '';
        state.audit.page = 1;
        state.audit.filters.keyId = '';
        state.audit.filters.callerId = '';
        loadAudit(true);
      });
    }
  }

  /* ================================================================== *
   * 价格
   * ================================================================== */

  function loadPrices(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.prices.items) {
      if (global.CMAdminPricing && typeof global.CMAdminPricing.load === 'function') {
        global.CMAdminPricing.load();
      }
      return Promise.resolve();
    }
    var seq = ++state.prices.loadSeq;
    state.prices.loading = true;
    state.prices.error = '';
    renderPrices();
    return api.listPrices().then(
      function (data) {
        if (seq !== state.prices.loadSeq) return;
        state.prices.loading = false;
        state.prices.items = (data && data.items) || [];
        renderPrices();
        if (global.CMAdminPricing && typeof global.CMAdminPricing.load === 'function') {
          global.CMAdminPricing.load();
        }
      },
      function (error) {
        if (seq !== state.prices.loadSeq) return;
        if (failRequest(error)) return;
        state.prices.loading = false;
        state.prices.error = describeError(error);
        renderPrices();
      }
    );
  }

  function rateCell(value, missingText) {
    if (value === null || value === undefined) return el('td', { class: 'num', text: missingText || '—' });
    var text = intString(value);
    if (!isIntegerText(text)) return el('td', { class: 'num', text: text });
    return el('td', { class: 'num' }, [
      el('div', { text: formatInt(text) + ' µUSD / 1M' }),
      el('div', { class: 'pill-note', text: formatMicroUSD(text) + ' / 1M' }),
    ]);
  }

  function renderPrices() {
    var container = $('prices-list');
    if (!container) return;
    var status = $('prices-status');
    var countNode = $('prices-count');
    renderErrorBox($('prices-error'), state.prices.error, function () {
      loadPrices(true);
    });
    if (state.prices.loading && !state.prices.items) {
      setStatusLine(status, '正在读取价格…');
      renderLoading(container, '加载中…');
      return;
    }
    if (!state.prices.items) {
      setStatusLine(status, '');
      renderEmpty(container, state.key ? '暂无数据，点击刷新重新读取。' : '请先登录。');
      return;
    }
    setStatusLine(status, '共 ' + formatInt(state.prices.items.length) + ' 条价格（接口：GET ' + MANAGER_BASE + '/api/prices）');
    if (countNode) countNode.textContent = '共 ' + formatInt(state.prices.items.length) + ' 条精确价格';
    if (!state.prices.items.length) {
      renderEmpty(container, '还没有价格记录。缺失当前价格的模型在日志中显示为未定价，不会被当作免费；补价后历史金额会立即重算。');
      return;
    }
    var rows = state.prices.items.map(function (price) {
      var rates = price.rates || {};
      var free = ratesSummaryText(rates);
      return el('tr', { class: 'row-main' }, [
        el('td', {}, [
          el('div', { class: 'mono-break', text: price.model || '—' }),
          free ? el('div', { class: 'pill-note', text: free }) : null,
        ]),
        rateCell(rates.input, '—'),
        rateCell(rates.output, '—'),
        rateCell(rates.reasoning, '缺失（按输出计费）'),
        rateCell(rates.cache_read, '—'),
        rateCell(rates.cache_write, '—'),
        el('td', { class: 'num', text: formatInt(price.version) }),
        el('td', {}, [
          el('button', {
            class: 'btn btn-small',
            type: 'button',
            text: '编辑',
            onclick: function () {
              openPriceEditor(price);
            },
          }),
        ]),
      ]);
    });
    var head = el('tr', {}, [
      el('th', { text: '精确模型 ID' }),
      el('th', { class: 'num', text: '输入' }),
      el('th', { class: 'num', text: '输出' }),
      el('th', { class: 'num', text: '思考' }),
      el('th', { class: 'num', text: '缓存读' }),
      el('th', { class: 'num', text: '缓存写' }),
      el('th', { class: 'num', text: '版本' }),
      el('th', { text: '操作' }),
    ]);
    replace(container, [tableScroll(el('table', { class: 'data' }, [el('thead', {}, [head]), el('tbody', {}, rows)]))]);
  }

  function openPriceEditor(existing) {
    var rates = (existing && existing.rates) || {};
    var isNew = !existing;
    var modelInput = el('input', { type: 'text', id: 'price-model', value: existing ? existing.model || '' : '' });
    modelInput.disabled = !isNew;
    var inputField = moneyPairInput({ label: '输入（µUSD / 1M tokens）', micro: intString(rates.input || 0) });
    var outputField = moneyPairInput({ label: '输出（µUSD / 1M tokens）', micro: intString(rates.output || 0) });
    var reasoningMicro = rates.reasoning === null || rates.reasoning === undefined ? '' : intString(rates.reasoning);
    var reasoningField = moneyPairInput({ label: '思考（留空 = 按输出计费）', micro: reasoningMicro });
    var cacheReadField = moneyPairInput({ label: '缓存读（µUSD / 1M tokens）', micro: intString(rates.cache_read || 0) });
    var cacheWriteField = moneyPairInput({ label: '缓存写（µUSD / 1M tokens）', micro: intString(rates.cache_write || 0) });
    var errorNode = el('p', { class: 'inline-error', hidden: true, role: 'alert' });

    var handle = openDialog({
      title: isNew ? '新增价格' : '编辑价格 · ' + existing.model,
      wide: true,
      body: [
        errorNode,
        el('p', {
          class: 'note',
          text: '全部价格字段都是「微美元 / 每 100 万 tokens」的整数（1 USD = 1,000,000 微美元）。左侧填整数微美元，右侧会精确换算美元；只填美元也可以。显式全零表示免费。',
        }),
        el('div', { class: 'form-grid' }, [
          el('div', { class: 'field span-all' }, [
            el('label', { for: 'price-model', text: '精确模型 ID' + (isNew ? '' : '（编辑时不可修改）') }),
            modelInput,
          ]),
          inputField.node,
          outputField.node,
          reasoningField.node,
          cacheReadField.node,
          cacheWriteField.node,
        ]),
        el('p', { class: 'note', text: '计费规则：输入按规范未缓存输入 tokens，输出不含思考 tokens，思考缺失时按输出单价。服务端会对各桶求和后一次性向上取整。' }),
      ],
      footer: [
        el('button', { class: 'btn', type: 'button', text: '取消', onclick: closeDialog }),
        el('button', {
          class: 'btn btn-primary',
          type: 'button',
          text: '保存价格',
          onclick: function () {
            var errors = [];
            var model = modelInput.value.trim();
            if (!model) errors.push('必须填写精确模型 ID');
            var payloadFields = {
              model: model,
              input: readMoneyInput(inputField, '输入价格', errors),
              output: readMoneyInput(outputField, '输出价格', errors),
              // 思考价格是可选的：只有两个输入框都留空才表示「按输出计费」。
              // 只要美元框里有内容就必须校验，否则非法输入会被静默忽略并按输出计费。
              reasoning: moneyPairEmpty(reasoningField) ? '' : readMoneyInput(reasoningField, '思考价格', errors),
              cacheRead: readMoneyInput(cacheReadField, '缓存读价格', errors),
              cacheWrite: readMoneyInput(cacheWriteField, '缓存写价格', errors),
              version: existing ? existing.version : '0',
            };
            if (errors.length) {
              errorNode.hidden = false;
              errorNode.textContent = errors.join('；');
              return;
            }
            errorNode.hidden = true;
            api.putPrice(pricePayload(payloadFields)).then(
              function () {
                handle.close();
                notify('已保存价格：' + model, 'ok');
                loadPrices(true);
              },
              function (error) {
                if (failRequest(error)) return;
                handle.setError(describeError(error));
              }
            );
          },
        }),
      ],
    });
    return handle;
  }

  /* ================================================================== *
   * 上游凭据与额度
   * ================================================================== */

  function loadCredentials(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.credentials.items) return Promise.resolve();
    var seq = ++state.credentials.loadSeq;
    state.credentials.loading = true;
    state.credentials.error = '';
    renderCredentials();
    return api.credentials().then(
      function (data) {
        if (seq !== state.credentials.loadSeq) return;
        state.credentials.loading = false;
        state.credentials.items = (data && (data.files || data.items)) || [];
        renderCredentials();
      },
      function (error) {
        if (seq !== state.credentials.loadSeq) return;
        if (failRequest(error)) return;
        state.credentials.loading = false;
        state.credentials.error = describeError(error);
        renderCredentials();
      }
    );
  }

  function quotaObservationText(entry) {
    var quota = entry.quota;
    if (!quota || typeof quota !== 'object') return '—';
    var parts = [];
    if (quota.observed_at) parts.push('观测 ' + formatDateTime(quota.observed_at));
    var signals = quota.signals;
    if (signals && typeof signals === 'object') {
      Object.keys(signals).forEach(function (key) {
        parts.push(key + '=' + signals[key]);
      });
    }
    if (!parts.length) return '—';
    return parts.join('　');
  }

  function renderCredentials() {
    var container = $('credentials-list');
    if (!container) return;
    var status = $('credentials-status');
    renderErrorBox($('credentials-error'), state.credentials.error, function () {
      loadCredentials(true);
    });
    if (state.credentials.loading && !state.credentials.items) {
      setStatusLine(status, '正在读取上游凭据…');
      renderLoading(container, '加载中…');
      return;
    }
    if (!state.credentials.items) {
      setStatusLine(status, '');
      renderEmpty(container, state.key ? '暂无数据，点击刷新重新读取。' : '请先登录。');
      return;
    }
    setStatusLine(status, '共 ' + formatInt(state.credentials.items.length) + ' 个凭据（接口：GET /v8/management/credentials）');
    if (!state.credentials.items.length) {
      renderEmpty(container, '当前没有可列出的上游凭据。');
      return;
    }

    var rows = state.credentials.items.map(function (entry) {
      var supportsQuota = entry.supports_quota === true;
      var pluginID = entry.quota_plugin_id ? String(entry.quota_plugin_id) : '';
      var authIndex = entry.auth_index ? String(entry.auth_index) : '';
      var name = entry.name || entry.label || entry.id || '—';
      return el('tr', { class: 'row-main' }, [
        el('td', {}, [
          el('div', { text: name }),
          el('div', { class: 'pill-note mono-break', text: entry.id || '—' }),
        ]),
        el('td', {}, [
          el('div', { text: entry.provider || entry.type || '—' }),
          entry.account ? el('div', { class: 'pill-note', text: entry.account }) : null,
          entry.email ? el('div', { class: 'pill-note', text: entry.email }) : null,
        ]),
        el('td', {}, [
          (function () {
            var status = credentialStatus(entry.status);
            return badge(status.label, status.tone);
          })(),
          entry.disabled ? badge('已禁用', 'warn') : null,
          entry.unavailable ? badge('不可用', 'warn') : null,
          entry.status_message ? el('div', { class: 'pill-note', text: entry.status_message }) : null,
          entry.next_retry_after ? el('div', { class: 'pill-note', text: '重试 ' + formatDateTime(entry.next_retry_after) }) : null,
        ]),
        el('td', {}, [el('div', { class: 'mono-break', text: quotaObservationText(entry) })]),
        el('td', {}, [
          supportsQuota ? badge('支持额度查询', 'info') : badge('未上报额度能力', ''),
          entry.quota_provider ? el('div', { class: 'pill-note', text: 'provider ' + entry.quota_provider }) : null,
          pluginID ? el('div', { class: 'pill-note', text: 'plugin ' + pluginID }) : null,
        ]),
        el('td', {}, [
          supportsQuota && pluginID && authIndex
            ? el('button', {
                class: 'btn btn-small',
                type: 'button',
                text: '查询额度',
                onclick: function () {
                  openQuotaDialog(entry, pluginID, authIndex);
                },
              })
            : el('span', { class: 'pill-note', text: supportsQuota ? '缺少 auth_index 或 quota_plugin_id，无法查询' : '不支持查询' }),
        ]),
      ]);
    });

    var head = el('tr', {}, [
      el('th', { text: '名称 / ID' }),
      el('th', { text: '类型' }),
      el('th', { text: '状态' }),
      el('th', { text: '配额观测' }),
      el('th', { text: '额度能力' }),
      el('th', { text: '操作' }),
    ]);
    replace(container, [tableScroll(el('table', { class: 'data' }, [el('thead', {}, [head]), el('tbody', {}, rows)]))]);
  }

  function openQuotaDialog(entry, pluginID, authIndex) {
    var handle = openDialog({
      title: '额度详情 · ' + (entry.name || entry.id || ''),
      wide: true,
      body: [el('div', { class: 'loading-state', text: '正在向上游插件查询额度…' })],
    });
    api.credentialQuota(pluginID, authIndex).then(
      function (data) {
        renderQuotaDetail(handle, entry, pluginID, authIndex, data);
      },
      function (error) {
        if (failRequest(error)) return;
        handle.setError(describeError(error));
        handle.setBody([
          el('p', {
            class: 'note',
            text:
              '查询接口：POST /v8/management/plugins/' + pluginID + '/quota（auth_index=' + authIndex + '）。' +
              '上游插件未实现、未上报或不支持该凭据时，服务端会返回错误，此处如实显示。',
          }),
        ]);
      }
    );
  }

  function renderQuotaDetail(handle, entry, pluginID, authIndex, data) {
    var quota = data || {};
    var rawText = exactStringify(quota);
    var pretty;
    try {
      pretty = prettyJSONLossless(rawText);
    } catch (err) {
      pretty = rawText;
    }
    var sections = [];

    sections.push(
      el('div', { class: 'grid-2' }, [
        kvRow('凭据', entry.name || entry.id || '—'),
        kvRow('provider', entry.provider || entry.type || '—'),
        kvRow('quota plugin', pluginID),
        kvRow('auth_index', authIndex),
      ])
    );

    var subscription = quota.subscription;
    if (subscription && typeof subscription === 'object') {
      sections.push(
        el('div', {}, [
          el('h3', { text: '订阅' }),
          el('div', { class: 'grid-2' }, [
            kvRow('套餐', subscription.plan || '—'),
            kvRow('层级名称', subscription.tierName || subscription.tier_name || '—'),
            kvRow('层级 ID', subscription.tierId || subscription.tier_id || '—'),
          ]),
        ])
      );
    }

    var summary = quota.summary;
    if (summary && summary.length) {
      sections.push(
        el('div', {}, [
          el('h3', { text: '汇总指标' }),
          el(
            'div',
            { class: 'card-grid' },
            summary.map(function (metric) {
              return metricCard(
                metric.label || metric.key || '指标',
                metricValueText(metric),
                metric.format === 'currency' ? '币种 ' + (metric.currency || '未标注') : metric.unit || '',
              );
            })
          ),
        ])
      );
    }

    var groups = quota.groups;
    if (groups && groups.length) {
      sections.push(
        el('div', {}, [
          el('h3', { text: '额度分组' }),
          el(
            'div',
            {},
            groups.map(function (group) {
              var buckets = group.buckets || [];
              return el('div', { class: 'card' }, [
                el('div', { class: 'row-title' }, [el('span', { class: 'key-label', text: group.displayName || group.display_name || '（未命名分组）' })]),
                buckets.length
                  ? tableScroll(
                      el('table', { class: 'data' }, [
                        el('thead', {}, [
                          el('tr', {}, [
                            el('th', { text: '窗口' }),
                            el('th', { text: '说明' }),
                            el('th', { text: '剩余' }),
                            el('th', { text: '重置时间' }),
                          ]),
                        ]),
                        el(
                          'tbody',
                          {},
                          buckets.map(function (bucket) {
                            var fillTone = bucketTone(bucket.remainingFraction);
                            return el('tr', {}, [
                              el('td', { text: bucket.window || '—' }),
                              el('td', { text: bucket.description || '—' }),
                              el('td', {}, [
                                el('div', { text: bucketPercent(bucket.remainingFraction) }),
                                el('div', { class: 'progress' }, [
                                  el('div', {
                                    class: 'progress-fill' + (fillTone ? ' ' + fillTone : ''),
                                    style:
                                      'width:' +
                                      (typeof bucket.remainingFraction === 'number' && isFinite(bucket.remainingFraction)
                                        ? Math.max(0, Math.min(100, bucket.remainingFraction * 100))
                                        : 0) +
                                      '%',
                                  }),
                                ]),
                              ]),
                              el('td', { text: bucket.resetTime ? formatDateTime(bucket.resetTime) : '—' }),
                            ]);
                          })
                        ),
                      ])
                    )
                  : el('p', { class: 'note', text: '该分组没有返回任何额度桶。' }),
              ]);
            })
          ),
        ])
      );
    }

    if (!subscription && (!summary || !summary.length) && (!groups || !groups.length)) {
      sections.push(
        el('p', {
          class: 'note',
          text: '服务端返回的额度响应中没有订阅、汇总指标或分组数据（可能该凭据本身不支持额度上报）。以下为原始响应。',
        })
      );
    }

    if (quota.serverTimeOffsetMs || quota.server_time_offset_ms) {
      sections.push(
        el('p', {
          class: 'note',
          text: '服务端时间偏移：' + intString(quota.serverTimeOffsetMs || quota.server_time_offset_ms) + ' ms',
        })
      );
    }

    sections.push(detailsBlock('原始响应（无损格式化）', codeBlock(pretty)));

    handle.setBody(sections);
  }

  /* ================================================================== *
   * 状态
   * ================================================================== */

  var HEALTH_KNOWN_FIELDS = {
    status: '状态',
    version: '版本',
    retention_months: '历史保留月数',
    capture_enabled: '捕获功能',
    capture: '捕获统计',
    last_error: '最近错误',
  };

  var CAPTURE_KNOWN_FIELDS = {
    database_bytes: '数据库字节',
    wal_bytes: 'WAL 字节',
    queue_bytes: '队列字节',
    dropped: '丢弃计数',
    last_error: '捕获错误',
  };

  function loadHealth(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.health.data) return Promise.resolve();
    var seq = ++state.health.loadSeq;
    state.health.loading = true;
    state.health.error = '';
    renderHealth();
    return api.health().then(
      function (data) {
        if (seq !== state.health.loadSeq) return;
        state.health.loading = false;
        state.health.data = data || {};
        renderHealth();
      },
      function (error) {
        if (seq !== state.health.loadSeq) return;
        if (failRequest(error)) return;
        state.health.loading = false;
        state.health.error = describeError(error);
        renderHealth();
      }
    );
  }

  function renderHealth() {
    var container = $('health-cards');
    var statusLine = $('health-status');
    var extra = $('health-extra');
    if (!container) return;
    renderErrorBox($('health-error'), state.health.error, function () {
      loadHealth(true);
    });
    if (state.health.loading && !state.health.data) {
      setStatusLine(statusLine, '正在读取插件健康状态…');
      replace(container, [el('div', { class: 'loading-state', text: '加载中…' })]);
      if (extra) replace(extra, []);
      return;
    }
    var data = state.health.data;
    if (!data) {
      setStatusLine(statusLine, '');
      replace(container, []);
      if (extra) renderEmpty(extra, state.key ? '暂无数据，点击刷新重新读取。' : '请先登录。');
      return;
    }

    var statusToken = String(data.status || '').toLowerCase();
    var ok = statusToken === 'ok';
    var statusLabel = ok ? '正常' : statusToken === 'degraded' ? '降级' : data.status || '未知';
    setStatusLine(statusLine, '接口：GET ' + MANAGER_BASE + '/api/health');
    var capture = data.capture || {};
    var cards = [
      metricCard('插件状态', statusLabel, ok ? 'status = ok' : 'status = ' + (data.status || '未知') + '，详见最近错误与捕获统计', ok ? 'ok' : 'danger'),
      metricCard('版本', data.version || '—', null),
      metricCard('历史保留月数', data.retention_months === undefined || data.retention_months === null ? '—' : formatInt(data.retention_months), '默认 3 个自然月（UTC）'),
      metricCard('捕获功能', data.capture_enabled ? '已启用' : '未启用', data.capture_enabled ? '可按请求 ID 查看快照' : '禁用时不保存请求体快照', data.capture_enabled ? '' : 'warn'),
      metricCard('捕获数据库', formatBytes(capture.database_bytes), formatInt(capture.database_bytes) + ' B'),
      metricCard('捕获 WAL', formatBytes(capture.wal_bytes), formatInt(capture.wal_bytes) + ' B'),
      metricCard('捕获队列', formatBytes(capture.queue_bytes), '队列以字节为上限'),
      metricCard('捕获丢弃计数', formatInt(capture.dropped), '队列压力或超限时递增', Number(intString(capture.dropped)) > 0 ? 'warn' : ''),
      metricCard('捕获最近错误', capture.last_error || '无', null, capture.last_error ? 'warn' : ''),
      metricCard('财务最近错误', data.last_error || '无', data.last_error ? '财务失败会阻止新的预留' : '无异常记录', data.last_error ? 'danger' : ''),
    ];
    replace(container, cards);

    var extras = [];
    Object.keys(data).forEach(function (key) {
      if (Object.prototype.hasOwnProperty.call(HEALTH_KNOWN_FIELDS, key)) return;
      extras.push(kvRow(key, exactStringify(data[key])));
    });
    var captureExtras = [];
    Object.keys(capture || {}).forEach(function (key) {
      if (Object.prototype.hasOwnProperty.call(CAPTURE_KNOWN_FIELDS, key)) return;
      captureExtras.push(kvRow('capture.' + key, exactStringify(capture[key])));
    });
    if (extra) {
      var pretty;
      try {
        pretty = prettyJSONLossless(exactStringify(data));
      } catch (err) {
        pretty = exactStringify(data);
      }
      replace(extra, [
        el('p', {
          class: 'note',
          text: '未在界面中单独展示的字段会原样列出，便于核对服务端新增的限额或维护信息。',
        }),
        extras.length || captureExtras.length
          ? el('div', { class: 'grid-2' }, extras.concat(captureExtras))
          : el('span', { class: 'pill-note', text: '没有其他字段。' }),
        detailsBlock('原始响应（无损格式化）', codeBlock(pretty)),
      ]);
    }
  }

  /* ================================================================== *
   * 启动
   * ================================================================== */

  function wireUsageForm() {
    var form = $('usage-filters');
    if (form) {
      form.addEventListener('submit', submitUsageFilters);
    }
    var reset = $('usage-reset');
    if (reset) reset.addEventListener('click', resetUsageFilters);
    var quick = $('usage-quick');
    if (quick) {
      quick.addEventListener('change', function () {
        var range = quickRangeFilters(quick.value);
        if (!range) return;
        var fromNode = $('usage-from');
        var toNode = $('usage-to');
        if (fromNode) fromNode.value = range.from;
        if (toNode) toNode.value = range.to;
        state.usage.filters.quick = quick.value;
      });
    }
    ['usage-from', 'usage-to'].forEach(function (id) {
      var node = $(id);
      if (node) {
        node.addEventListener('change', function () {
          var quickSelect = $('usage-quick');
          if (quickSelect) quickSelect.value = '';
        });
      }
    });
    var bucket = $('usage-bucket');
    if (bucket) {
      bucket.addEventListener('change', function () {
        state.usage.filters.bucket = bucket.value;
        loadUsage(false);
      });
    }
    var pageSize = $('usage-page-size');
    if (pageSize) {
      pageSize.addEventListener('change', function () {
        state.usage.filters.pageSize = normalizePageSize(pageSize.value);
        state.usage.filters.page = 1;
        loadUsage(true);
      });
    }
  }

  function wireButtons() {
    var loginForm = $('login-form');
    if (loginForm) loginForm.addEventListener('submit', submitLogin);
    var logoutButton = $('logout-button');
    if (logoutButton) logoutButton.addEventListener('click', logout);

    var keysCreate = $('keys-create');
    if (keysCreate) {
      keysCreate.addEventListener('click', function () {
        openKeyEditor(null);
      });
    }
    var keysRefresh = $('keys-refresh');
    if (keysRefresh) {
      keysRefresh.addEventListener('click', function () {
        loadKeys(true);
      });
    }
    var batchResetBtn = $('btn-batch-reset-tokens');
    if (batchResetBtn) {
      batchResetBtn.addEventListener('click', function () {
        openResetTokensDialog(null);
      });
    }
    var callersCreate = $('callers-create');
    if (callersCreate) {
      callersCreate.addEventListener('click', function () {
        openCallerEditor(null);
      });
    }
    var callersRefresh = $('callers-refresh');
    if (callersRefresh) {
      callersRefresh.addEventListener('click', function () {
        loadCallers(true);
      });
    }
    var fxBtn = $('btn-refresh-exchange-rate');
    if (fxBtn) {
      fxBtn.addEventListener('click', function () {
        fxBtn.disabled = true;
        api.exchangeRate()
          .then(function (res) {
            fxBtn.disabled = false;
            if (res && res.rate) {
              var rateText = String(res.rate);
              state.currencyRate = normalizeRate(res.rate);
              var cr = $('currency-rate');
              if (cr) cr.value = rateText;
              var isStale = Boolean(res.stale);
              var isCached = Boolean(res.cached);
              var msg = '汇率已刷新：1 USD = ' + rateText + ' ' + (res.quote || res.currency || 'CNY');
              if (isStale) {
                msg += '（已过期缓存）';
                notify(msg, 'warn');
              } else if (isCached) {
                msg += '（来自缓存）';
                notify(msg, 'ok');
              } else {
                notify(msg, 'ok');
              }
              rerenderAfterUnitChange();
            }
          })
          .catch(function (err) {
            fxBtn.disabled = false;
            notify('实时汇率刷新失败，已保留当前汇率 ' + state.currencyRate + '（' + describeError(err) + '）', 'warn');
          });
      });
    }
    var pricesCreate = $('prices-create');
    if (pricesCreate) {
      pricesCreate.addEventListener('click', function () {
        openPriceEditor(null);
      });
    }
    var pricesRefresh = $('prices-refresh');
    if (pricesRefresh) {
      pricesRefresh.addEventListener('click', function () {
        loadPrices(true);
      });
    }
    var credentialsRefresh = $('credentials-refresh');
    if (credentialsRefresh) {
      credentialsRefresh.addEventListener('click', function () {
        loadCredentials(true);
      });
    }
    var healthRefresh = $('health-refresh');
    if (healthRefresh) {
      healthRefresh.addEventListener('click', function () {
        loadHealth(true);
      });
    }
  }

  function boot() {
    // 依赖缺失时阻断提交，避免把不精确的账务数据当作有效结果展示。
    if (!global.CMNumbers || !global.CMCharts || !global.echarts) {
      $('login-form').addEventListener('submit', function (event) { event.preventDefault(); });
      $('login-button').disabled = true;
      $('login-error').textContent = '界面资源加载失败，请刷新页面或检查插件资源。';
      $('login-error').hidden = false;
      return;
    }
    // 未预期的脚本错误不能静默失败：至少给出可见提示，便于排查。
    global.addEventListener('error', function (event) {
      notify('界面发生未预期错误：' + ((event && event.message) || '未知错误'), 'error');
    });
    initThemeSync();
    wireUnitControls();
    wireTabs();
    wireOverviewForm();
    wireUsageForm();
    wireAuditForm();
    wireButtons();
    applyOverviewFiltersToForm(state.overview.filters);
    applyUsageFiltersToForm(state.usage.filters);
    renderUsageStatus();

    initAdminPricing();

    var saved = readStoredCredential();
    var rememberBox = $('remember-management-key');
    if (rememberBox && saved.remembered) {
      rememberBox.checked = true;
    }
    if (saved.token) {
      autoConnectSavedCredential(saved.token, saved.remembered);
    } else {
      showLoginView();
      var keyInput = $('management-key');
      if (keyInput) keyInput.focus();
    }
  }

  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
    document.addEventListener('DOMContentLoaded', function () {
      boot(global.CM);
    });
  }
})(typeof window !== 'undefined' ? window : globalThis);
