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
    active: '进行中',
    pending: '待定',
    settled: '已结算',
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

  var BIG_INT_MARK = '\u0000CMINT:';
  var BIG_INT_TAIL = '\u0000';
  var BIG_INT_DIGITS = 16; // 超过 15 位（即 >= 16 位）的整数在 64 位 JSON 中可能超出 Number 精度

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

  function formatMicroUSD(micro) {
    var usd = microToUSDText(micro);
    return usd === null ? '—' : usd + ' USD';
  }

  function formatMicroUSDPair(micro) {
    var usd = microToUSDText(micro);
    if (usd === null) return '—';
    return usd + ' USD（' + formatInt(micro) + ' µUSD）';
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
      'T' + pad2(date.getHours()) + ':' + pad2(date.getMinutes())
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
    var out = '';
    var index = 0;
    var length = text.length;
    while (index < length) {
      var ch = text.charAt(index);
      if (ch === '"') {
        var start = index;
        index++;
        while (index < length) {
          var cur = text.charAt(index);
          if (cur === '\\') {
            index += 2;
            continue;
          }
          index++;
          if (cur === '"') break;
        }
        out += text.slice(start, index);
        continue;
      }
      if (ch === '-' || (ch >= '0' && ch <= '9')) {
        var numStart = index;
        index++;
        while (index < length && /[0-9eE+\-.]/.test(text.charAt(index))) index++;
        out += replaceNumber(text.slice(numStart, index));
        continue;
      }
      out += ch;
      index++;
    }
    return out;
  }

  function isBigIntegerLiteral(literal) {
    var text = String(literal);
    if (!/^-?\d+$/.test(text)) return false;
    var digits = text.charAt(0) === '-' ? text.slice(1) : text;
    return digits.length >= BIG_INT_DIGITS;
  }

  function reviveBigInts(value) {
    if (typeof value === 'string') {
      if (value.length > BIG_INT_MARK.length + 1 &&
          value.indexOf(BIG_INT_MARK) === 0 &&
          value.slice(-1) === BIG_INT_TAIL) {
        var digits = value.slice(BIG_INT_MARK.length, -1);
        if (isIntegerText(digits)) return intVal(digits);
      }
      return value;
    }
    if (Array.isArray(value)) {
      for (var i = 0; i < value.length; i++) value[i] = reviveBigInts(value[i]);
      return value;
    }
    if (value && typeof value === 'object') {
      var keys = Object.keys(value);
      for (var k = 0; k < keys.length; k++) value[keys[k]] = reviveBigInts(value[keys[k]]);
      return value;
    }
    return value;
  }

  // 与 JSON.parse 行为一致，但超出 Number 精度的整数会保留为精确文本（CMInt）。
  function parseJSON(text) {
    var source = String(text == null ? '' : text);
    var rewritten = rewriteNumbers(source, function (literal) {
      if (!isBigIntegerLiteral(literal)) return literal;
      return JSON.stringify(BIG_INT_MARK + literal + BIG_INT_TAIL);
    });
    return reviveBigInts(JSON.parse(rewritten));
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

  var USAGE_BUCKETS = ['hour', 'day'];

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

  // 只有服务端标记为 pending 的请求才可人工 Resolve：active 仍在执行并占用并发与持有金额，
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
    if (!rates || typeof rates !== 'object') return '无价格记录（拒绝计费）';
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

  var state = {
    key: '',
    // 会话纪元：登录尝试、登出与会话失效时递增。请求在发起时捕获纪元，
    // 纪元变化后到达的任何响应（含失败）都被判定为过期，不得写回状态、渲染或触发登出。
    sessionEpoch: 0,
    tab: 'usage',
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
    prices: { items: null, loading: false, error: '', loadSeq: 0 },
    credentials: { items: null, loading: false, error: '', quota: {}, loadSeq: 0 },
    health: { data: null, loading: false, error: '', loadSeq: 0 },
  };

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
    resolve: function (payload) {
      return request(managerPath('/api/resolve'), { method: 'POST', body: payload });
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
    usageFiltersFromForm: usageFiltersFromForm,
    defaultUsageFilters: defaultUsageFilters,
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

  var SVG_NS = 'http://www.w3.org/2000/svg';

  function svgEl(tag, attrs, children) {
    var node = document.createElementNS(SVG_NS, tag);
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
    var microInput = el('input', { type: 'text', inputmode: 'numeric', class: 'money-micro', value: opts.micro === undefined ? '0' : String(opts.micro), placeholder: '整数微美元' });
    var usdInput = el('input', { type: 'text', inputmode: 'decimal', class: 'money-usd', placeholder: '美元（可留空）' });
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
      el('label', { text: opts.label || '金额' }),
      el('div', { class: 'input-pair' }, [microInput, usdInput]),
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
      status.textContent = '已登录（密钥仅存于内存）';
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
    state.keys = { items: null, loading: false, error: '', loadSeq: 0 };
    state.prices = { items: null, loading: false, error: '', loadSeq: 0 };
    state.credentials = { items: null, loading: false, error: '', quota: {}, loadSeq: 0 };
    state.health = { data: null, loading: false, error: '', loadSeq: 0 };
  }

  // 已渲染的私有内容：登出/会话失效时必须从 DOM 中抹掉，不能只隐藏容器。
  var PRIVATE_RENDER_TARGETS = [
    'usage-status',
    'usage-summary',
    'usage-quality-note',
    'usage-chart',
    'usage-legend',
    'usage-total',
    'usage-retention',
    'usage-items',
    'usage-pagination',
    'keys-status',
    'keys-list',
    'prices-status',
    'prices-list',
    'credentials-status',
    'credentials-list',
    'health-status',
    'health-cards',
    'health-extra',
  ];

  var PRIVATE_ERROR_TARGETS = ['usage-error', 'keys-error', 'prices-error', 'credentials-error', 'health-error'];

  function clearRenderedPrivateContent() {
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
  }

  function handleSessionExpired(message) {
    state.sessionEpoch++;
    state.key = '';
    clearSessionState();
    clearRenderedPrivateContent();
    closeDialog();
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

  function submitLogin(event) {
    if (event) event.preventDefault();
    var input = $('management-key');
    var button = $('login-button');
    var errorNode = $('login-error');
    var key = input ? input.value : '';
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
        showMainView();
        activateTab('usage', false);
        state.usage.filters = defaultUsageFilters();
        applyUsageFiltersToForm(state.usage.filters);
        loadUsage(true);
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
          if (error.kind === 'auth') {
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
    clearSessionState();
    clearRenderedPrivateContent();
    closeDialog();
    showLoginView();
    var keyInput = $('management-key');
    if (keyInput) keyInput.value = '';
    notify('已清除内存中的管理密钥');
  }

  /* ---------------- 标签页 ---------------- */

  var TAB_LOADERS = {
    usage: function () {},
    keys: function () {
      loadKeys(false);
    },
    prices: function () {
      loadPrices(false);
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

  // 汇总卡片的纯描述：金额/标签/说明都可单测，避免界面误把执行成本、人工补记与请求计数混在一起。
  // 契约：summary.failed 是失败请求数（failed/canceled/rejected），summary.pending 是未结算请求数
  // （active+pending），summary.cost_micro_usd 只统计匹配的执行，summary.adjustment_micro_usd 是
  // 管理员 Resolve 的人工记账，必须单独展示。
  function usageSummaryCards(summary, filters) {
    var scoped = hasExecutionScope(filters);
    var requests = summaryValue(summary, 'requests');
    var executions = summaryValue(summary, 'executions');
    var failed = summaryValue(summary, 'failed');
    var pending = summaryValue(summary, 'pending');
    var cost = summaryValue(summary, 'cost_micro_usd');
    var adjustment = summaryValue(summary, 'adjustment_micro_usd');
    var completeExecutions = summaryValue(summary, 'complete_executions');
    var tokens = summary.tokens || {};
    var ratio = summary.cache_read_ratio;

    var ratioText = '—';
    if (ratio !== null && ratio !== undefined && typeof ratio === 'number' && isFinite(ratio)) {
      ratioText = (ratio * 100).toFixed(2) + '%';
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
        label: '未结算请求',
        value: formatInt(pending),
        sub: pending
          ? '进行中或待定：已知用量照常入账，未知部分待核账'
          : '没有进行中或待定的请求',
        tone: Number(intString(pending)) ? 'warn' : '',
      },
      {
        label: '执行成本',
        value: formatMicroUSD(cost),
        sub:
          formatInt(cost) + ' µUSD　仅匹配范围内的执行计量' + (scoped ? '，已按 Provider/auth_id 过滤' : ''),
      },
      {
        label: '人工补记',
        value: formatMicroUSD(adjustment),
        sub:
          formatInt(adjustment) +
          ' µUSD　管理员 Resolve 记账，不计入执行成本' +
          (scoped ? '；该金额按请求汇总，不受 Provider/auth_id 过滤' : ''),
        tone: Number(intString(adjustment)) ? 'warn' : '',
      },
      { label: '完整质量执行', value: formatInt(completeExecutions), sub: 'quality = complete' },
      {
        label: '缓存读占比',
        value: ratioText,
        sub: ratio === null || ratio === undefined ? '服务端未提供该指标' : '只覆盖完整质量执行，说明见下方',
      },
      { label: 'tokens 合计', value: formatInt(tokens.total), sub: '规范 tokens' },
      { label: '输入 tokens', value: formatInt(tokens.input), sub: 'canonical uncached input' },
      { label: '输出 tokens', value: formatInt(tokens.output), sub: '不含思考 tokens' },
      { label: '缓存读 tokens', value: formatInt(tokens.cache_read) },
      { label: '缓存写 tokens', value: formatInt(tokens.cache_write) },
      { label: '思考 tokens', value: formatInt(tokens.reasoning) },
    ];
  }

  // 汇总下方的口径说明：明确哪里是匹配执行、哪里是请求级人工记账。
  function usageScopeNote(summary, filters) {
    var executions = summary ? summaryValue(summary, 'executions') : 0;
    var completeExecutions = summary ? summaryValue(summary, 'complete_executions') : 0;
    var coverage = '—';
    var execCount = Number(intString(executions));
    var completeCount = Number(intString(completeExecutions));
    if (isFinite(execCount) && execCount > 0) {
      coverage = ((completeCount / execCount) * 100).toFixed(1) + '%';
    }
    var text =
      '质量覆盖说明：缓存读占比只覆盖质量判定为「完整（complete）」的执行；' +
      '完整执行占比 = ' + formatInt(completeExecutions) + ' / ' + formatInt(executions) + ' = ' + coverage + '。' +
      '「部分（partial）」或「未知（unknown）」的执行不计入占比，也不会被当作 0 结算。';
    text +=
      '口径说明：执行成本与明细只统计当前筛选范围内的执行，人工补记按请求汇总，' +
      '两者相加仍不等于全 Key 账本支出（账本还包含其它请求与保留策略之外的记录）。';
    if (hasExecutionScope(filters)) {
      text +=
        ' 当前已按 Provider/auth_id 过滤：明细只包含匹配的执行，人工补记仍是整笔请求的记账，' +
        '因此不能把该合计当作全 Key 台账。';
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

  function trendNumber(point, key) {
    var value = point ? point[key] : 0;
    var num = Number(intString(value));
    return isFinite(num) ? num : 0;
  }

  // 纯计算：把 Report.trend 转成可直接绘制的几何模型，便于单测与复用。
  function buildTrendChart(points, bucket) {
    var width = 960;
    var height = 260;
    var padding = { top: 18, right: 64, bottom: 36, left: 64 };
    var plot = {
      x: padding.left,
      y: padding.top,
      w: width - padding.left - padding.right,
      h: height - padding.top - padding.bottom,
    };
    var list = (points || []).map(function (point) {
      return {
        time: point.time,
        requests: trendNumber(point, 'requests'),
        executions: trendNumber(point, 'executions'),
        cost: trendNumber(point, 'cost_micro_usd'),
        costText: formatMicroUSD(point.cost_micro_usd),
        adjustment: trendNumber(point, 'adjustment_micro_usd'),
        adjustmentText: formatMicroUSD(point.adjustment_micro_usd),
        failed: trendNumber(point, 'failed'),
        pending: trendNumber(point, 'pending'),
      };
    });
    var maxValue = 0;
    var maxCost = 0;
    list.forEach(function (point) {
      maxValue = Math.max(maxValue, point.requests, point.executions);
      maxCost = Math.max(maxCost, point.cost);
    });
    if (maxValue <= 0) maxValue = 1;

    var slot = list.length ? plot.w / list.length : plot.w;
    var barWidth = Math.max(2, Math.min(16, slot * 0.3));
    var yFor = function (value) {
      return plot.y + plot.h - (value / maxValue) * plot.h;
    };
    var costYFor = function (value) {
      if (maxCost <= 0) return plot.y + plot.h;
      return plot.y + plot.h - (value / maxCost) * plot.h;
    };

    var bars = [];
    var costPoints = [];
    var xLabels = [];

    list.forEach(function (point, index) {
      var center = plot.x + slot * index + slot / 2;
      var requestsY = yFor(point.requests);
      var executionsY = yFor(point.executions);
      bars.push({
        key: 'requests',
        x: center - barWidth - 1,
        y: requestsY,
        w: barWidth,
        h: plot.y + plot.h - requestsY,
        title: formatDateTime(point.time) + '　请求次数：' + formatInt(point.requests),
      });
      bars.push({
        key: 'executions',
        x: center + 1,
        y: executionsY,
        w: barWidth,
        h: plot.y + plot.h - executionsY,
        title:
          formatDateTime(point.time) +
          '　执行次数：' + formatInt(point.executions) +
          '　失败请求：' + formatInt(point.failed) +
          '　未结算请求：' + formatInt(point.pending) +
          '　执行成本：' + point.costText +
          '　人工补记：' + point.adjustmentText,
      });
      var costX = center;
      costPoints.push({
        x: costX,
        y: costYFor(point.cost),
        title: formatDateTime(point.time) + '　执行成本：' + point.costText + '（不含人工补记）',
      });
      xLabels.push({ x: center, text: bucket === 'hour' ? formatHourLabel(point.time) : formatDayLabel(point.time) });
    });

    // 标签过多时只保留首、中、尾，避免重叠。
    var visibleLabels = xLabels;
    if (xLabels.length > 8) {
      var middle = Math.floor(xLabels.length / 2);
      visibleLabels = [xLabels[0], xLabels[middle], xLabels[xLabels.length - 1]];
    }

    var yTicks = [0, 0.5, 1].map(function (fraction) {
      var value = Math.round(maxValue * fraction);
      return { y: yFor(value), text: formatInt(value) };
    });

    var linePath = costPoints
      .map(function (point, index) {
        return (index === 0 ? 'M' : 'L') + point.x.toFixed(1) + ' ' + point.y.toFixed(1);
      })
      .join(' ');

    return {
      width: width,
      height: height,
      plot: plot,
      bars: bars,
      costPoints: costPoints,
      linePath: costPoints.length > 1 ? linePath : '',
      showCostLine: maxCost > 0 && costPoints.length > 0,
      maxValue: maxValue,
      maxCost: maxCost,
      xLabels: visibleLabels,
      yTicks: yTicks,
      count: list.length,
    };
  }

  function renderTrendChart(container, model) {
    var svg = svgEl('svg', {
      viewBox: '0 0 ' + model.width + ' ' + model.height,
      role: 'img',
      'aria-label': '趋势图：请求次数、执行次数与成本',
      preserveAspectRatio: 'none',
    });

    model.yTicks.forEach(function (tick) {
      svg.appendChild(svgEl('line', { class: 'grid-line', x1: model.plot.x, x2: model.plot.x + model.plot.w, y1: tick.y, y2: tick.y }));
      svg.appendChild(
        svgEl('text', { class: 'axis-label', x: model.plot.x - 8, y: tick.y + 3, 'text-anchor': 'end' }, [tick.text])
      );
    });

    svg.appendChild(
      svgEl('line', {
        class: 'axis-line',
        x1: model.plot.x,
        x2: model.plot.x + model.plot.w,
        y1: model.plot.y + model.plot.h,
        y2: model.plot.y + model.plot.h,
      })
    );

    model.bars.forEach(function (bar) {
      var rect = svgEl('rect', {
        class: bar.key === 'requests' ? 'bar-requests' : 'bar-executions',
        x: bar.x,
        y: bar.y,
        width: bar.w,
        height: Math.max(0, bar.h),
        rx: 1.5,
      });
      rect.appendChild(svgEl('title', {}, [bar.title]));
      svg.appendChild(rect);
    });

    if (model.showCostLine && model.linePath) {
      svg.appendChild(svgEl('path', { class: 'cost-line', d: model.linePath }));
      model.costPoints.forEach(function (point) {
        var dot = svgEl('circle', { class: 'cost-dot', cx: point.x, cy: point.y, r: 2.5 });
        dot.appendChild(svgEl('title', {}, [point.title]));
        svg.appendChild(dot);
      });
    }

    model.xLabels.forEach(function (label) {
      svg.appendChild(svgEl('text', { class: 'axis-label', x: label.x, y: model.plot.y + model.plot.h + 16, 'text-anchor': 'middle' }, [label.text]));
    });

    replace(container, [svg]);
  }

  function renderUsageTrend(trend) {
    var container = $('usage-chart');
    var legend = $('usage-legend');
    if (!container) return;
    if (!trend || !trend.length) {
      renderEmpty(container, '当前筛选范围内没有可绘制的趋势数据。');
      if (legend) replace(legend, []);
      return;
    }
    var model = buildTrendChart(trend, state.usage.filters.bucket);
    renderTrendChart(container, model);
    if (legend) {
      var items = [
        { color: '#4c8dff', label: '请求次数（每个入站请求一行）' },
        { color: '#2f6b52', label: '执行次数（实际执行，含重试）' },
      ];
      if (model.showCostLine) items.push({ color: '#d29922', label: '执行成本（USD，独立右侧刻度；不含人工补记）', line: true });
      replace(
        legend,
        items.map(function (item) {
          return el('span', { class: 'legend-item' }, [
            el('span', {
              class: 'legend-swatch' + (item.line ? ' line' : ''),
              style: 'background:' + item.color,
            }),
            el('span', { text: item.label }),
          ]);
        })
      );
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
    var settled = execution.cost_micro_usd !== undefined && execution.cost_micro_usd !== null;
    var costText = settled ? formatMicroUSD(execution.cost_micro_usd) : '未结算';
    var quality = String(execution.quality == null ? '' : execution.quality).toLowerCase();
    var costSub = settled
      ? formatInt(execution.cost_micro_usd) + ' µUSD'
      : quality === 'partial' || quality === 'unknown'
        ? '质量非完整（' + quality + '）：服务端未给出结算金额，不是 0'
        : '服务端未给出结算金额（不是 0）';

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

  function requestPriceText(request) {
    if (priceMissing(request.price)) return '无价格记录（拒绝计费，不按 0 计价）';
    var summary = ratesSummaryText(request.price.rates);
    if (summary) return summary;
    var rates = request.price.rates || {};
    var parts = [
      '输入 ' + formatInt(rates.input) + ' µUSD/1M',
      '输出 ' + formatInt(rates.output) + ' µUSD/1M',
      '思考 ' + reasoningRateText(rates),
      '缓存读 ' + formatInt(rates.cache_read) + ' µUSD/1M',
      '缓存写 ' + formatInt(rates.cache_write) + ' µUSD/1M',
    ];
    return parts.join('　·　') + '　（价格版本 ' + formatInt(request.price.version) + '）';
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

    var adjustmentMicro = request.adjustment_micro_usd === undefined || request.adjustment_micro_usd === null ? 0 : request.adjustment_micro_usd;
    var adjustmentNonZero = intString(adjustmentMicro) !== '0';

    var head = el('div', { class: 'row-title' }, [
      el('span', { class: 'key-label mono', text: '请求 ' + shortId(request.id), title: String(request.id || '') }),
      badge(outcomeLabel(request.outcome), outcomeTone(request.outcome)),
      badge(stateLabel(request.state), isPendingRequest(request) ? 'warn' : ''),
      el('span', { class: 'pill-note', text: 'trace ' + (request.trace_id || '—') }),
      el('span', { class: 'pill-note', text: 'Key ' + keyLabel }),
      el('span', { class: 'pill-note', text: '开始 ' + formatDateTime(request.started_at) }),
      el('span', {
        class: 'pill-note',
        text: request.completed_at ? '完成 ' + formatDateTime(request.completed_at) : '尚未完成',
      }),
      el('span', { class: 'pill-note', text: '执行 ' + formatInt(executions.length) + ' 次' }),
      el('span', { class: 'pill-note', text: '持有 ' + formatMicroUSD(request.held_micro_usd) }),
      // 人工补记必须始终可见，不能藏在执行列表里与执行成本混算。
      el('span', {
        class: 'pill-note' + (adjustmentNonZero ? ' is-warn' : ''),
        text: '人工补记 ' + formatMicroUSD(adjustmentMicro),
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
      isPendingRequest(request)
        ? el('button', {
            class: 'btn btn-small',
            type: 'button',
            text: '处理待定',
            onclick: function () {
              openResolveDialog(request);
            },
          })
        : null,
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
      el('div', { class: 'kv kv-wide' }, [
        el('span', { class: 'kv-k', text: '价格' }),
        el('span', { class: 'kv-v', text: requestPriceText(request) }),
      ]),
      el('div', { class: 'kv kv-wide' }, [
        el('span', { class: 'kv-k', text: '人工补记（adjustment_micro_usd）' }),
        el('span', { class: 'kv-v', text: formatMicroUSDPair(adjustmentMicro) + '（管理员 Resolve 记账，独立于执行成本）' }),
      ]),
    ]);

    var scoped = hasExecutionScope(state.usage.filters);

    return el('div', { class: 'card' }, [
      head,
      relation.namespaceOnly ? el('p', { class: 'note', text: relation.note }) : null,
      scoped
        ? el('p', {
            class: 'note',
            text:
              '当前按 Provider/auth_id 过滤：下方明细与该卡片的执行相关数字只包含匹配的执行，' +
              '而人工补记是该请求的全部人工记账，因此两者不能当作全 Key 账本的对账口径。',
          })
        : null,
      meta,
      actions,
      executions.length
        ? executionTable(request)
        : el('p', {
            class: 'note',
            text: isPendingRequest(request)
              ? '该请求尚无执行记录，状态为待定：已知用量照常入账，未知部分需要管理员在「处理待定」中释放或扣费。'
              : '该请求尚无执行记录；进行中的请求只持有金额，等执行事件到达后才会计费。',
          }),
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
        replace(viewContainer, [headersTable]);
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

    handle.setBody([
      el('div', { class: 'row-title' }, [
        badge(status.label, status.tone),
        el('span', { class: 'pill-note', text: '请求 ID ' + (data.request_id || '—') }),
        el('span', { class: 'pill-note', text: '记录时间 ' + formatDateTime(data.at) }),
        el('span', { class: 'pill-note', text: '过期时间 ' + formatDateTime(data.expires_at) }),
      ]),
      el('p', {
        class: 'note',
        text: '该快照是执行入口处的请求快照（不是上游网络的原始字节），敏感头与明显的密钥字段已在写入前被替换。',
      }),
      tabsBar,
      viewContainer,
    ]);
    renderView();
  }

  /* ---------------- 待定请求的财务处理 ---------------- */

  function openResolveDialog(request) {
    var actionSelect = el('select', { id: 'resolve-action' }, [
      el('option', { value: 'release', text: '释放（release）：不扣费，归还持有金额' }),
      el('option', { value: 'charge', text: '扣费（charge）：按填写的金额入账' }),
    ]);
    var amount = moneyPairInput({ label: '金额', micro: '0' });
    var reasonInput = el('input', { type: 'text', id: 'resolve-reason', placeholder: '必填，会写入账本' });
    var confirmInput = el('input', { type: 'checkbox', id: 'resolve-confirm' });
    var handle = openDialog({
      title: '处理待定请求 · ' + shortId(request.id),
      body: [
        el('div', { class: 'grid-2' }, [
          kvRow('请求 ID', request.id || '—'),
          kvRow('请求模型', request.model || '—'),
          kvRow('持有金额', formatMicroUSDPair(request.held_micro_usd)),
          kvRow('状态', stateLabel(request.state) + ' / ' + outcomeLabel(request.outcome)),
        ]),
        el('p', {
          class: 'hint hint-warn',
          text:
            '这是一次财务操作：账本只追加、不会覆盖历史，请确认金额与理由准确。金额以整数微美元提交（1 USD = 1,000,000 微美元），可使用美元输入自动精确换算。',
        }),
        el('div', { class: 'form-grid' }, [
          el('div', { class: 'field' }, [el('label', { for: 'resolve-action', text: '处理方式' }), actionSelect]),
          el('div', { class: 'field' }, [el('label', { for: 'resolve-reason', text: '理由（必填）' }), reasonInput]),
          amount.node,
        ]),
        el('div', { class: 'field' }, [
          el('label', { for: 'resolve-confirm', text: '我确认这是一次财务处理，且理由与金额会被永久记录' }),
          confirmInput,
        ]),
      ],
      footer: [
        el('button', { class: 'btn', type: 'button', text: '取消', onclick: closeDialog }),
        el('button', {
          class: 'btn btn-primary',
          type: 'button',
          text: '提交处理',
          onclick: function () {
            submitResolve(handle, request, {
              action: actionSelect.value,
              amountPair: amount,
              reason: reasonInput.value,
              confirmed: confirmInput.checked,
            });
          },
        }),
      ],
    });
    return handle;
  }

  function submitResolve(handle, request, form) {
    var errors = [];
    var reason = String(form.reason || '').trim();
    if (!reason) errors.push('必须填写处理理由');
    if (!form.confirmed) errors.push('请先勾选财务处理确认项');
    var micro = readMoneyInput(form.amountPair, '金额', errors);
    if (form.action === 'charge' && (micro === '0' || micro === '')) {
      errors.push('扣费金额必须大于 0');
    }
    if (form.action === 'release' && micro !== '0') {
      errors.push('释放不应对金额扣费，请将金额填为 0');
    }
    if (errors.length) {
      handle.setError(errors.join('；'));
      return;
    }
    handle.setError('');
    var payload = {
      id: request.id,
      action: form.action,
      amount_micro_usd: intVal(micro),
      reason: reason,
    };
    api.resolve(payload).then(
      function () {
        notify('已提交财务处理：' + (form.action === 'release' ? '释放' : '扣费') + ' ' + formatMicroUSD(micro), 'ok');
        handle.close();
        loadUsage(true);
      },
      function (error) {
        if (failRequest(error)) return;
        handle.setError(describeError(error));
      }
    );
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
    if (!select) return;
    var current = select.value;
    var options = [el('option', { value: '', text: '全部' })];
    var items = state.keys.items || [];
    items.forEach(function (key) {
      var label = (key.spec && key.spec.label) || '';
      options.push(
        el('option', {
          value: key.id,
          text: (label ? label + ' · ' : '') + (key.fingerprint || shortId(key.id)),
        })
      );
    });
    replace(select, options);
    select.value = current || '';
    if (!select.value) select.value = '';
  }

  function limitLine(label, value) {
    var text = intString(value);
    if (text === '0') return el('div', { class: 'nowrap', text: label + '：不限' });
    return el('div', { class: 'nowrap', text: label + '：' + microToUSDText(text) + ' USD · ' + formatInt(text) + ' µUSD' });
  }

  function quotaProgress(key) {
    var spec = key.spec || {};
    var limit = intString(spec.quota_micro_usd);
    var spent = intString(key.spent_micro_usd);
    if (limit === '0') return el('span', { class: 'pill-note', text: '总额度不限' });
    var percent = ratioPercent(spent, limit);
    return el('div', {}, [
      el('div', { class: 'progress' }, [
        el('div', {
          class: 'progress-fill' + (percent !== null && percent >= 90 ? ' danger' : percent !== null && percent >= 70 ? ' warn' : ''),
          style: 'width:' + (percent === null ? 0 : Math.max(0, Math.min(100, percent))) + '%',
        }),
      ]),
      el('div', { class: 'pill-note', text: '已用 ' + (percent === null ? '—' : percent.toFixed(2) + '%') + '（' + formatMicroUSD(spent) + ' / ' + formatMicroUSD(limit) + '）' }),
    ]);
  }

  // 精确百分比（0.01% 精度），避免大整数相除的浮点偏差。
  function ratioPercent(part, whole) {
    var a = intString(part);
    var b = intString(whole);
    if (!isIntegerText(a) || !isIntegerText(b)) return null;
    var bigA = BigInt(a);
    var bigB = BigInt(b);
    if (bigB === 0n) return null;
    if (bigA < 0n) bigA = -bigA;
    if (bigB < 0n) bigB = -bigB;
    return Number((bigA * 10000n) / bigB) / 100;
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
      return el('tr', { class: 'row-main' }, [
        el('td', {}, [
          el('div', { text: spec.label || '（未命名）' }),
          el('div', { class: 'pill-note mono-break', text: key.id || '—' }),
        ]),
        el('td', {}, [
          badge(spec.enabled ? '已启用' : '已禁用', spec.enabled ? 'ok' : 'warn'),
          spec.expires_at ? el('div', { class: 'pill-note', text: '过期 ' + formatDateTime(spec.expires_at) }) : null,
        ]),
        el('td', {}, [el('div', { class: 'mono-break', text: key.fingerprint || '—' })]),
        el('td', {}, [el('div', { class: 'mono-break', text: key.principal || '—' })]),
        el('td', { class: 'num' }, [
          el('div', { text: formatMicroUSD(key.spent_micro_usd) }),
          el('div', { class: 'pill-note', text: formatInt(key.spent_micro_usd) + ' µUSD' }),
          key.held_micro_usd && intString(key.held_micro_usd) !== '0'
            ? el('div', { class: 'pill-note', text: '持有 ' + formatMicroUSD(key.held_micro_usd) })
            : null,
          quotaProgress(key),
        ]),
        el('td', { class: 'limits' }, [
          limitLine('总额度', spec.quota_micro_usd),
          limitLine('日', spec.daily_micro_usd),
          limitLine('周', spec.weekly_micro_usd),
          limitLine('月', spec.monthly_micro_usd),
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
              text: '编辑',
              onclick: function () {
                openKeyEditor(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: spec.enabled ? '禁用' : '启用',
              onclick: function () {
                toggleKeyEnabled(key);
              },
            }),
            el('button', {
              class: 'btn btn-small',
              type: 'button',
              text: '轮换密钥',
              onclick: function () {
                openRotateDialog(key);
              },
            }),
          ]),
        ]),
      ]);
    });

    var head = el('tr', {}, [
      el('th', { text: '标签 / Key ID' }),
      el('th', { text: '状态' }),
      el('th', { text: '指纹' }),
      el('th', { text: 'Principal' }),
      el('th', { class: 'num', text: '已消费 / 持有' }),
      el('th', { class: 'num', text: '额度' }),
      el('th', { class: 'num', text: '并发' }),
      el('th', { text: '允许模型 / token 限额' }),
      el('th', { text: '操作' }),
    ]);
    replace(container, [tableScroll(el('table', { class: 'data' }, [el('thead', {}, [head]), el('tbody', {}, rows)]))]);
  }

  function openKeyEditor(existing) {
    var spec = (existing && existing.spec) || {};
    var creating = !existing;

    var labelInput = el('input', { type: 'text', id: 'key-label', value: spec.label || '' });
    var enabledInput = el('input', { type: 'checkbox', id: 'key-enabled' });
    enabledInput.checked = creating ? true : spec.enabled === true;
    var quotaField = moneyPairInput({ label: '总额度（微美元）', micro: intString(spec.quota_micro_usd || 0) });
    var dailyField = moneyPairInput({ label: '每日额度', micro: intString(spec.daily_micro_usd || 0) });
    var weeklyField = moneyPairInput({ label: '每周额度', micro: intString(spec.weekly_micro_usd || 0) });
    var monthlyField = moneyPairInput({ label: '每月额度', micro: intString(spec.monthly_micro_usd || 0) });
    var concurrentInput = el('input', { type: 'text', inputmode: 'numeric', id: 'key-concurrent', value: intString(spec.max_concurrent || 0) });
    var allowedInput = el('textarea', { id: 'key-allowed', rows: 4, placeholder: '每行一个精确模型 ID；留空表示允许全部模型' });
    allowedInput.value = (spec.allowed_models || []).join('\n');
    var limitsInput = el('textarea', { id: 'key-limits', rows: 6, placeholder: '[{"model":"gpt-5","total":1000000,"daily":0,"weekly":0,"monthly":0}]' });
    limitsInput.value = modelTokenLimitsToText(spec.model_token_limits);
    var expiresInput = el('input', { type: 'datetime-local', id: 'key-expires' });
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
            el('label', { for: 'key-enabled', text: '启用（新建默认启用）' }),
            enabledInput,
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
              quotaMicroUSD: readMoneyInput(quotaField, '总额度', errors),
              dailyMicroUSD: readMoneyInput(dailyField, '每日额度', errors),
              weeklyMicroUSD: readMoneyInput(weeklyField, '每周额度', errors),
              monthlyMicroUSD: readMoneyInput(monthlyField, '每月额度', errors),
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
        el('p', { class: 'hint hint-warn', text: '明文密钥只显示这一次，关闭后无法再次获取。请复制并保存到安全位置。' }),
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

  /* ================================================================== *
   * 价格
   * ================================================================== */

  function loadPrices(force) {
    if (!state.key) return Promise.resolve();
    if (!force && state.prices.items) return Promise.resolve();
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
    if (!state.prices.items.length) {
      renderEmpty(container, '还没有价格记录。缺失价格的模型会被拒绝计费，不会被当作免费。');
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
    // 未预期的脚本错误不能静默失败：至少给出可见提示，便于排查。
    global.addEventListener('error', function (event) {
      notify('界面发生未预期错误：' + ((event && event.message) || '未知错误'), 'error');
    });
    wireTabs();
    wireUsageForm();
    wireButtons();
    applyUsageFiltersToForm(state.usage.filters);
    renderUsageStatus();
    showLoginView();
    var keyInput = $('management-key');
    if (keyInput) keyInput.focus();
  }

  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
    document.addEventListener('DOMContentLoaded', function () {
      boot(global.CM);
    });
  }
})(typeof window !== 'undefined' ? window : globalThis);
