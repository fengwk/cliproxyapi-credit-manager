/*
 * credit-manager 密钥自助查询脚本。
 *
 * 纯原生 JS：无框架、无 bundler、无 CDN，且不使用 innerHTML/eval 写入服务端数据。
 * 页面只读自己的数据：密钥仅通过 Authorization 请求头发送，绝不写入 URL。
 * 除用户显式勾选“记住密钥”外不落盘；落盘时使用独立前缀 credit-manager.lookup.
 * 与财务控制台的管理密钥完全隔离。
 *
 * 复用共享模块：
 *   - numbers.js（CMNumbers）：无损 JSON 解析，超过 2^53 的账务整数保留精确文本；
 *   - charts.js（CMCharts）：精确整数/金额/Token 文本与主题化趋势图（vendor/echarts）。
 * 共享模块缺席时逐项降级，页面仍可用。主题/计量偏好使用本文件自带的读取与归一化
 * 逻辑（不含额外的偏好模块依赖），沿用控制台的存储键，保证两个页面口径一致。
 *
 * 生命周期：所有查询都带 epoch + AbortController；切换密钥、退出或收到 401 时立刻
 * 取消在途请求、清空私有 DOM 与凭据、关闭快照弹窗并释放图表实例，避免上一个密钥的
 * 数据或图表残留在页面上。
 */
(function (root, factory) {
  'use strict';
  var api = factory(root);
  if (typeof module !== 'undefined' && module && module.exports) module.exports = api;
  if (root && (typeof root === 'object' || typeof root === 'function')) root.CMLookup = api;
  if (typeof document !== 'undefined' && typeof document.getElementById === 'function') {
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { api.boot(); });
    else api.boot();
  }
})(typeof globalThis !== 'undefined' ? globalThis : (typeof self !== 'undefined' ? self : this), function (root) {
  'use strict';

  var STORAGE_PREFIX = 'credit-manager.lookup.';
  var REMEMBER_KEY = STORAGE_PREFIX + 'token';
  var REMEMBER_ORIGIN_KEY = STORAGE_PREFIX + 'token-origin';
  var PAGE_SIZE = 10;
  var DEFAULT_USD_CNY_RATE = 7.2;
  var GRAINS = ['hour', 'day', 'month'];

  var OUTCOME_LABELS = { succeeded: '成功', failed: '失败', rejected: '拒绝', canceled: '取消' };
  var STATE_LABELS = { active: '进行中', pending: '待定', settled: '已结算' };
  var SNAPSHOT_LABELS = {
    captured: '已捕获',
    expired: '已过保留期',
    disabled: '请求采集未启用',
    oversize: '请求体超出上限，未保存',
    dropped: '快照已丢弃',
    unavailable: '快照不可用',
  };

  /* ------------------------------------------------------------------ *
   * 共享模块访问（缺失时逐项降级，绝不因为缺模块整体失败）
   * ------------------------------------------------------------------ */

  var preferences = (function () {
    var CPA_THEME_KEY = 'cli-proxy-theme';
    var THEMES = ['auto', 'light', 'white', 'dark'];
    var TOKEN_UNIT_KEY = 'credit_manager_token_unit';
    var CURRENCY_KEY = 'credit_manager_currency';
    var USD_CNY_RATE_KEY = 'credit_manager_usd_cny_rate';

    // CPA 把主题写成 JSON 或纯字符串，两种都要能读。
    function parseStoredTheme(value) {
      if (!value) return '';
      try {
        var parsed = JSON.parse(value);
        return parsed && typeof parsed === 'object'
          ? (parsed.theme || (parsed.state && parsed.state.theme) || parsed.language || '')
          : parsed;
      } catch (err) {
        return value;
      }
    }

    return {
      CPA_THEME_KEY: CPA_THEME_KEY,
      THEMES: THEMES,
      TOKEN_UNIT_KEY: TOKEN_UNIT_KEY,
      CURRENCY_KEY: CURRENCY_KEY,
      USD_CNY_RATE_KEY: USD_CNY_RATE_KEY,
      readTheme: function (store) {
        if (!store) return 'auto';
        try {
          var candidate = String(parseStoredTheme(store.getItem(CPA_THEME_KEY)) || 'auto').toLowerCase();
          return THEMES.indexOf(candidate) >= 0 ? candidate : 'auto';
        } catch (err) {
          return 'auto';
        }
      },
      resolvePalette: function (theme, systemIsDark) {
        var chosen = THEMES.indexOf(theme) >= 0 ? theme : 'auto';
        if (chosen === 'auto') return systemIsDark ? 'dark' : 'white';
        return chosen;
      },
      readUnits: function (store) {
        var units = { tokenUnit: 'raw', currency: 'USD', currencyRate: DEFAULT_USD_CNY_RATE };
        if (!store) return units;
        try {
          var unit = store.getItem(TOKEN_UNIT_KEY);
          if (unit) units.tokenUnit = normalizeTokenUnit(unit);
          var currency = store.getItem(CURRENCY_KEY);
          if (currency) units.currency = normalizeCurrency(currency);
          var rate = store.getItem(USD_CNY_RATE_KEY);
          if (rate) units.currencyRate = normalizeRate(rate);
        } catch (err) {
          /* 存储被禁用时直接用默认值 */
        }
        return units;
      },
      writeUnits: function (store, units) {
        if (!store || !units) return;
        try {
          store.setItem(TOKEN_UNIT_KEY, normalizeTokenUnit(units.tokenUnit));
          store.setItem(CURRENCY_KEY, normalizeCurrency(units.currency));
          store.setItem(USD_CNY_RATE_KEY, String(normalizeRate(units.currencyRate)));
        } catch (err) {
          /* 忽略不可写存储 */
        }
      },
    };
  })();

  function numbers() {
    return root && root.CMNumbers ? root.CMNumbers : null;
  }

  function charts() {
    return root && root.CMCharts ? root.CMCharts : null;
  }

  // 共享模块是随包发布必需资源：缺失时启动即报错，绝不退化出错误数字。
  function modulesReady() {
    var lossless = numbers();
    var shared = charts();
    return !!(lossless && typeof lossless.parse === 'function' &&
      shared && typeof shared.render === 'function' && typeof shared.formatMoneyMicro === 'function');
  }

  function parseBody(text) {
    return numbers().parse(text);
  }

  function intText(value) {
    return numbers().intText(value);
  }

  function formatInt(value) {
    return charts().formatInt(value);
  }

  function formatToken(value, unit) {
    return charts().tokenText(value, unit);
  }

  // 金额文本与货币后缀都由共享模块负责，这里不再自行拼接货币。
  function formatMoney(micro, currency, rate) {
    return charts().formatMoneyMicro(micro, currency, rate);
  }

  // 共享模块不再导出日期格式化，这里保留一个最小的本地实现。
  function formatMoment(value) {
    if (!value) return '—';
    var date = new Date(value);
    return isNaN(date.getTime()) ? String(value) : date.toLocaleString();
  }

  /* ------------------------------------------------------------------ *
   * 纯逻辑（可独立测试）：范围、查询串、凭据存储、汇总
   * ------------------------------------------------------------------ */

  function normalizeGrain(value) {
    var text = String(value == null ? '' : value).toLowerCase();
    return GRAINS.indexOf(text) >= 0 ? text : 'day';
  }

  function normalizeTokenUnit(value) {
    return ['raw', 'k', 'wan', 'm'].indexOf(value) >= 0 ? value : 'raw';
  }

  function normalizeCurrency(value) {
    return value === 'CNY' ? 'CNY' : 'USD';
  }

  function normalizeRate(value) {
    var n = Number(value);
    return isFinite(n) && n > 0 ? n : DEFAULT_USD_CNY_RATE;
  }

  // 时间范围：全部使用浏览器本地自然日边界（today/7/30/90/all）。
  function rangeBounds(mode, now) {
    if (mode === 'all') return {};
    var current = now instanceof Date ? new Date(now.getTime()) : new Date();
    var start = new Date(current.getTime());
    start.setHours(0, 0, 0, 0);
    if (mode !== 'today') {
      var days = Number(mode);
      if (isFinite(days) && days > 1) start.setDate(start.getDate() - (days - 1));
    }
    return { from: start.toISOString(), to: current.toISOString() };
  }

  // 查询串：bucket 与服务端契约一致，用户选择的维度原样下发（时/日/月），
  // 绝不在客户端把月降级成日再自行合并。
  function buildDataQuery(state, now) {
    var bounds = rangeBounds(state.range, now);
    var params = [];
    if (bounds.from) params.push(['from', bounds.from]);
    if (bounds.to) params.push(['to', bounds.to]);
    params.push(['bucket', normalizeGrain(state.grain)]);
    params.push(['page', String(state.page)]);
    params.push(['page_size', String(state.pageSize)]);
    if (state.result) params.push(['result', state.result]);
    if (state.model) params.push(['model', state.model]);
    return params;
  }

  function buildDataURL(base, state, now) {
    var url = new URL(base);
    buildDataQuery(state, now).forEach(function (pair) { url.searchParams.set(pair[0], pair[1]); });
    return url;
  }

  function buildCaptureURL(base, id) {
    var url = new URL(base);
    url.searchParams.set('id', id);
    return url;
  }

  function originOf(fallback) {
    try {
      return String(root.location.origin || '');
    } catch (err) {
      return fallback || '';
    }
  }

  // 会话优先、本地兜底，且两侧各自独立 try：任何一侧不可用都不影响另一侧。
  // remembered 只表示“本地也持久化了同一个凭据”，与会话凭据的存在与否无关。
  function readStoredToken(sessionStore, localStore, origin) {
    var current = origin || originOf('');
    var sessionToken = '';
    var localToken = '';
    if (sessionStore) {
      try {
        var sessionOrigin = sessionStore.getItem(REMEMBER_ORIGIN_KEY);
        var sessionValue = String(sessionStore.getItem(REMEMBER_KEY) || '').trim();
        if (sessionValue && (!sessionOrigin || sessionOrigin === current)) sessionToken = sessionValue;
      } catch (err) {
        /* 会话存储不可用：继续尝试本地存储 */
      }
    }
    if (localStore) {
      try {
        var localOrigin = localStore.getItem(REMEMBER_ORIGIN_KEY);
        var localValue = String(localStore.getItem(REMEMBER_KEY) || '').trim();
        if (localValue && (!localOrigin || localOrigin === current)) localToken = localValue;
      } catch (err) {
        /* 本地存储不可用：本次会话内仍可手工输入 */
      }
    }
    if (sessionToken) return { token: sessionToken, remembered: !!localToken && localToken === sessionToken };
    if (localToken) return { token: localToken, remembered: true };
    return { token: '', remembered: false };
  }

  function persistToken(sessionStore, localStore, origin, token, remember) {
    var current = origin || originOf('');
    var value = String(token || '').trim();
    if (sessionStore) {
      try {
        if (value) {
          sessionStore.setItem(REMEMBER_KEY, value);
          sessionStore.setItem(REMEMBER_ORIGIN_KEY, current);
        } else {
          sessionStore.removeItem(REMEMBER_KEY);
          sessionStore.removeItem(REMEMBER_ORIGIN_KEY);
        }
      } catch (err) {
        /* 会话存储不可写：忽略 */
      }
    }
    if (localStore) {
      try {
        if (value && remember) {
          localStore.setItem(REMEMBER_KEY, value);
          localStore.setItem(REMEMBER_ORIGIN_KEY, current);
        } else {
          localStore.removeItem(REMEMBER_KEY);
          localStore.removeItem(REMEMBER_ORIGIN_KEY);
        }
      } catch (err) {
        /* 本地存储不可写：忽略 */
      }
    }
  }

  function clearToken(sessionStore, localStore) {
    if (sessionStore) {
      try {
        sessionStore.removeItem(REMEMBER_KEY);
        sessionStore.removeItem(REMEMBER_ORIGIN_KEY);
      } catch (err) {
        /* 忽略不可用存储 */
      }
    }
    if (localStore) {
      try {
        localStore.removeItem(REMEMBER_KEY);
        localStore.removeItem(REMEMBER_ORIGIN_KEY);
      } catch (err) {
        /* 忽略不可用存储 */
      }
    }
  }

  function requestStatus(item) {
    var outcome = OUTCOME_LABELS[item && item.outcome] || (item && item.outcome) || '';
    var label = STATE_LABELS[item && item.state] || (item && item.state) || '';
    return outcome ? outcome + ' · ' + (label || '—') : label || '—';
  }

  // 精确合计：共享无损模块直接求和，缺少模块时启动阶段的错误提示已经阻断页面。
  function sumExact(values) {
    return numbers().sumIntTexts(values);
  }

  function executionTokens(executions) {
    return sumExact((executions || []).map(function (execution) {
      return (execution && execution.tokens ? execution.tokens.total : 0) || 0;
    }));
  }

  function executionCost(executions) {
    return sumExact((executions || []).map(function (execution) {
      return execution && execution.cost_micro_usd != null ? execution.cost_micro_usd : 0;
    }));
  }

  // 模型 Token 限额：后端只返回已配置的上限，0 表示该周期不限。
  function tokenLimitText(limit) {
    var value = intText(limit);
    if (value === '' || value === '0') return '不限';
    return formatToken(limit, 'raw');
  }

  var api = {
    VERSION: '1.0.0',
    STORAGE_PREFIX: STORAGE_PREFIX,
    REMEMBER_KEY: REMEMBER_KEY,
    REMEMBER_ORIGIN_KEY: REMEMBER_ORIGIN_KEY,
    normalizeGrain: normalizeGrain,
    normalizeTokenUnit: normalizeTokenUnit,
    normalizeCurrency: normalizeCurrency,
    normalizeRate: normalizeRate,
    rangeBounds: rangeBounds,
    buildDataQuery: buildDataQuery,
    buildDataURL: buildDataURL,
    buildCaptureURL: buildCaptureURL,
    readStoredToken: readStoredToken,
    persistToken: persistToken,
    clearToken: clearToken,
    requestStatus: requestStatus,
    executionTokens: executionTokens,
    executionCost: executionCost,
    tokenLimitText: tokenLimitText,
    parseBody: parseBody,
    modulesReady: modulesReady,
    preferences: preferences,
    boot: boot,
  };

  /* ------------------------------------------------------------------ *
   * 运行时状态与 DOM
   * ------------------------------------------------------------------ */

  var DATA_URL = 'lookup/data';
  var CAPTURE_URL = 'lookup/capture';
  var MODULE_ERROR = '页面资源加载不完整（charts.js / numbers.js 未加载），无法显示精确账务数据，请刷新或联系维护者。';

  var state = {
    token: '',
    page: 1,
    total: 0,
    pageSize: PAGE_SIZE,
    range: 'today',
    grain: 'day',
    tokenUnit: 'raw',
    currency: 'USD',
    rate: DEFAULT_USD_CNY_RATE,
    trend: [],
    payload: null,
  };

  // epoch 让任何一次迟到的响应失效；inflight 让上一次请求被真正取消。
  var epoch = 0;
  var inflight = null;

  function el(id) {
    return document.getElementById(id);
  }

  function clear(node) {
    if (!node) return;
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function text(tag, className, value) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (value !== undefined && value !== null) node.textContent = String(value);
    return node;
  }

  function card(label, value, tone) {
    var box = text('div', 'card');
    box.appendChild(text('span', 'card-label', label));
    box.appendChild(text('span', 'card-value' + (tone ? ' ' + tone : ''), value));
    return box;
  }

  function showBanner(message) {
    var banner = el('banner');
    banner.textContent = message;
    banner.hidden = false;
  }

  function hideBanner() {
    el('banner').hidden = true;
  }

  function showGate(message) {
    el('dashboard').hidden = true;
    el('switch-key').hidden = true;
    el('login-gate').hidden = false;
    if (message) showBanner(message);
    else hideBanner();
  }

  function showDashboard() {
    el('login-gate').hidden = true;
    el('dashboard').hidden = false;
    el('switch-key').hidden = false;
  }

  function setBusy(busy) {
    el('submit').disabled = busy;
    el('refresh').disabled = busy;
  }

  var store = function (name) {
    try {
      return root[name] || null;
    } catch (err) {
      return null;
    }
  };

  function sessionStore() { return store('sessionStorage'); }
  function localStore() { return store('localStorage'); }

  /* ------------------------------------------------------------------ *
   * 生命周期：取消在途请求、清空私有数据、释放图表
   * ------------------------------------------------------------------ */

  // 每一次新的用户操作都会作废上一次请求：递增 epoch 并 abort 进行中的 fetch。
  function newEpoch() {
    epoch += 1;
    if (inflight && typeof inflight.abort === 'function') {
      try {
        inflight.abort();
      } catch (err) {
        /* 已经结束的请求 abort 失败可以忽略 */
      }
    }
    inflight = null;
    return epoch;
  }

  function disposeCharts() {
    var shared = charts();
    if (shared && typeof shared.disposeAll === 'function') {
      try {
        shared.disposeAll();
      } catch (err) {
        /* 图表释放失败不应阻断清理 */
      }
    }
  }

  function closeSnapshot() {
    var modal = el('capture-modal');
    if (modal && modal.open && typeof modal.close === 'function') modal.close();
    clear(el('capture-body'));
  }

  // 清空所有来自密钥的私有 DOM，并释放图表实例。
  function resetPrivateUI() {
    state.trend = [];
    state.payload = null;
    ['quotas', 'token-limits', 'stats', 'recent-body'].forEach(function (id) {
      clear(el(id));
    });
    ['key-label', 'key-state', 'retention', 'recent-state', 'token-limits-state', 'page-info'].forEach(function (id) {
      if (el(id)) el(id).textContent = '';
    });
    el('trend-empty').hidden = true;
    el('prev').disabled = true;
    el('next').disabled = true;
    disposeCharts();
    ['trend-requests', 'trend-tokens', 'trend-cost'].forEach(function (id) {
      clear(el(id));
    });
    closeSnapshot();
  }

  // 退出/切换密钥/凭据失效：取消请求、清空凭据与私有数据、回到输入界面。
  function dropSession(message) {
    newEpoch();
    state.token = '';
    clearToken(sessionStore(), localStore());
    if (el('key')) el('key').value = '';
    if (el('remember-key')) el('remember-key').checked = false;
    resetPrivateUI();
    state.page = 1;
    state.total = 0;
    setBusy(false);
    showGate(message || '');
  }

  /* ------------------------------------------------------------------ *
   * 网络
   * ------------------------------------------------------------------ */

  function baseURL(relative) {
    try {
      return new URL(relative, root.location.href).toString();
    } catch (err) {
      return relative;
    }
  }

  function apiFetch(url, signal) {
    return fetch(url, {
      method: 'GET',
      headers: { Authorization: 'Bearer ' + state.token, Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      signal: signal,
    });
  }

  function isStale(myEpoch) {
    return myEpoch !== epoch;
  }

  async function loadUsage(options) {
    var opts = options || {};
    var myEpoch = newEpoch();
    var controller = typeof AbortController === 'function' ? new AbortController() : null;
    inflight = controller;
    setBusy(true);
    try {
      var response = await apiFetch(buildDataURL(baseURL(DATA_URL), state).toString(), controller ? controller.signal : undefined);
      if (isStale(myEpoch)) return;
      if (response.status === 401) {
        dropSession('密钥无效或已停用，请重新输入。');
        return;
      }
      if (!response.ok) {
        showBanner('查询失败（HTTP ' + response.status + '）。');
        return;
      }
      var payload;
      try {
        // 解析失败一律报错退出：绝不用部分/退化的数字冒充账务数据。
        payload = parseBody(await response.text());
      } catch (err) {
        if (isStale(myEpoch)) return;
        showBanner('查询响应无法解析，请稍后重试。');
        return;
      }
      if (isStale(myEpoch)) return;
      renderUsage(payload);
      hideBanner();
      showDashboard();
      if (opts.remember) persistToken(sessionStore(), localStore(), originOf(''), state.token, opts.remember === 'local');
    } catch (err) {
      if (isStale(myEpoch) || (err && err.name === 'AbortError')) return;
      showBanner('网络异常，请稍后重试。');
    } finally {
      if (!isStale(myEpoch)) {
        inflight = null;
        setBusy(false);
      }
    }
  }

  async function openSnapshot(id) {
    var myEpoch = newEpoch();
    var controller = typeof AbortController === 'function' ? new AbortController() : null;
    inflight = controller;
    var response;
    try {
      response = await apiFetch(buildCaptureURL(baseURL(CAPTURE_URL), id).toString(), controller ? controller.signal : undefined);
    } catch (err) {
      if (isStale(myEpoch) || (err && err.name === 'AbortError')) return;
      showBanner('网络异常，请稍后重试。');
      return;
    }
    if (isStale(myEpoch)) return;
    if (response.status === 401) {
      dropSession('密钥无效或已停用，请重新输入。');
      return;
    }
    if (!response.ok) {
      // 其它密钥、未知请求与不可用快照一律是同一种结果，页面不区分。
      showBanner('请求快照不可用。');
      return;
    }
    var detail;
    try {
      detail = parseBody(await response.text());
    } catch (err) {
      showBanner('请求快照响应无法解析。');
      return;
    }
    if (isStale(myEpoch)) return;
    inflight = null;
    renderSnapshot(detail);
    var modal = el('capture-modal');
    if (modal && typeof modal.showModal === 'function') modal.showModal();
  }

  /* ------------------------------------------------------------------ *
   * 渲染
   * ------------------------------------------------------------------ */

  function renderQuotas(key) {
    var quotas = el('quotas');
    clear(quotas);
    quotas.appendChild(card('已用额度', formatMoney(key.spent_micro_usd, state.currency, state.rate)));
    quotas.appendChild(card('在途预占', formatMoney(key.held_micro_usd, state.currency, state.rate)));
    quotas.appendChild(card('总额度', key.quota_micro_usd ? formatMoney(key.quota_micro_usd, state.currency, state.rate) : '不限'));
    quotas.appendChild(card('日额度', key.daily_micro_usd ? formatMoney(key.daily_micro_usd, state.currency, state.rate) : '不限'));
    quotas.appendChild(card('周额度', key.weekly_micro_usd ? formatMoney(key.weekly_micro_usd, state.currency, state.rate) : '不限'));
    quotas.appendChild(card('月额度', key.monthly_micro_usd ? formatMoney(key.monthly_micro_usd, state.currency, state.rate) : '不限'));
    quotas.appendChild(card('并发上限', key.max_concurrent ? formatInt(key.max_concurrent) : '不限'));
    if (key.allowed_models && key.allowed_models.length) {
      quotas.appendChild(card('允许模型', key.allowed_models.join('、')));
    } else {
      quotas.appendChild(card('允许模型', '不限'));
    }
  }

  function renderTokenLimits(key) {
    var container = el('token-limits');
    clear(container);
    var limits = key.model_token_limits || [];
    el('token-limits-state').textContent = limits.length
      ? limits.length + ' 个模型设有 Token 上限'
      : '未设置模型 Token 上限';
    if (!limits.length) {
      container.appendChild(card('模型 Token 限额', '未设置'));
      return;
    }
    limits.forEach(function (limit) {
      var box = text('div', 'card');
      box.appendChild(text('span', 'card-label', limit.model || '未标记模型'));
      box.appendChild(text('span', 'card-value', '总量 ' + tokenLimitText(limit.total)));
      box.appendChild(text('span', 'card-note', '日 ' + tokenLimitText(limit.daily) +
        ' · 周 ' + tokenLimitText(limit.weekly) +
        ' · 月 ' + tokenLimitText(limit.monthly)));
      container.appendChild(box);
    });
  }

  function renderStats(summary) {
    var tokens = summary.tokens || {};
    var stats = el('stats');
    clear(stats);
    stats.appendChild(card('请求数', formatInt(summary.requests)));
    stats.appendChild(card('执行次数', formatInt(summary.executions)));
    stats.appendChild(card('失败请求', formatInt(summary.failed), summary.failed ? 'danger' : null));
    stats.appendChild(card('未结算请求', formatInt(summary.pending), summary.pending ? 'warn' : null));
    stats.appendChild(card('执行成本', formatMoney(summary.cost_micro_usd, state.currency, state.rate)));
    stats.appendChild(card('人工补记', formatMoney(summary.adjustment_micro_usd, state.currency, state.rate)));
    ['input', 'output', 'reasoning', 'cache_read', 'cache_write', 'total'].forEach(function (slot) {
      if (tokens[slot] === undefined && slot !== 'total') return;
      stats.appendChild(card('Token·' + tokenSlotLabel(slot), formatToken(tokens[slot], state.tokenUnit)));
    });
    if (typeof summary.cache_read_ratio === 'number') {
      stats.appendChild(card('缓存读取占比', (summary.cache_read_ratio * 100).toFixed(1) + '%'));
    }
  }

  function tokenSlotLabel(slot) {
    return {
      input: '输入',
      output: '输出',
      reasoning: '思考',
      cache_read: '缓存读',
      cache_write: '缓存写',
      total: '合计',
    }[slot] || slot;
  }

  function renderUsage(data) {
    state.payload = data;
    renderAll();
  }

  // 单位/货币/汇率只影响展示：整页按保留的最新响应重绘，避免只有图表换了货币
  // 而额度、汇总与明细仍是旧口径。
  function renderAll() {
    var data = state.payload;
    if (!data) return;
    var key = data.key || {};
    var summary = data.summary || {};

    el('key-label').textContent = key.label ? key.label : '密钥概览';
    el('key-state').textContent = (key.enabled ? '已启用' : '已停用') +
      (key.expires_at ? ' · 到期 ' + formatMoment(key.expires_at) : '');
    el('retention').textContent = data.retention_from ? '保留自 ' + formatMoment(data.retention_from) : '';

    renderQuotas(key);
    renderTokenLimits(key);
    renderStats(summary);

    state.trend = data.trend || [];
    renderTrend();
    renderRecent(data.items || [], data.total || 0, data.page || 1, data.page_size || PAGE_SIZE);
  }

  // 趋势图只由共享组件绘制（随包发布的必需资源）。
  //
  // 关键：共享渲染器按容器复用 ECharts 实例，因此在这里绝不清空容器——
  // 一旦移除已有 canvas/svg，实例就与 DOM 脱钩，重新渲染只会得到空白图。
  function renderTrend() {
    var containers = { requests: el('trend-requests'), tokens: el('trend-tokens'), cost: el('trend-cost') };
    el('trend-empty').hidden = state.trend.length > 0;
    charts().render(containers, state.trend, {
      bucket: state.grain,
      tokenUnit: state.tokenUnit,
      currency: state.currency,
      rate: state.rate,
    });
  }

  function renderRecent(items, total, page, pageSize) {
    var body = el('recent-body');
    clear(body);
    el('recent-state').textContent = formatInt(total) + ' 条记录';
    items.forEach(function (item) {
      var row = document.createElement('tr');
      row.appendChild(text('td', null, formatMoment(item.started_at)));
      row.appendChild(text('td', 'model', item.model || '—'));
      row.appendChild(text('td', null, requestStatus(item)));
      row.appendChild(text('td', 'num', formatToken(executionTokens(item.executions), state.tokenUnit)));
      row.appendChild(text('td', 'num', formatMoney(executionCost(item.executions), state.currency, state.rate)));
      var action = text('td', null);
      var button = text('button', null, '查看快照');
      button.type = 'button';
      button.addEventListener('click', function () { openSnapshot(item.id); });
      action.appendChild(button);
      row.appendChild(action);
      body.appendChild(row);
    });
    if (!items.length) {
      var empty = document.createElement('tr');
      var cell = text('td', 'muted', '当前时间范围内暂无请求。');
      cell.colSpan = 6;
      empty.appendChild(cell);
      body.appendChild(empty);
    }
    var pages = pageSize > 0 ? Math.max(1, Math.ceil(total / pageSize)) : 1;
    el('page-info').textContent = '第 ' + page + ' / ' + pages + ' 页';
    el('prev').disabled = page <= 1;
    el('next').disabled = page >= pages;
  }

  function renderSnapshot(detail) {
    el('capture-title').textContent = '请求 ' + (detail.request_id || '');
    var body = el('capture-body');
    clear(body);

    var meta = text('p', 'snapshot-meta');
    meta.appendChild(text('span', 'snapshot-status', SNAPSHOT_LABELS[detail.status] || detail.status || '未知'));
    meta.appendChild(document.createTextNode(' · 记录时间 ' + formatMoment(detail.at) +
      (detail.expires_at ? ' · 保留至 ' + formatMoment(detail.expires_at) : '')));
    body.appendChild(meta);

    if (detail.status !== 'captured') {
      body.appendChild(text('p', 'muted', '该请求的请求头与请求体当前不可用。'));
      return;
    }

    var headers = detail.headers || {};
    var names = Object.keys(headers).sort(function (a, b) { return a.toLowerCase().localeCompare(b.toLowerCase()); });
    if (names.length) {
      var headerSection = text('div', 'snapshot-section');
      headerSection.appendChild(text('h3', null, '请求头'));
      var list = text('ul', 'header-list');
      names.forEach(function (name) {
        var values = Array.isArray(headers[name]) ? headers[name] : [headers[name]];
        list.appendChild(text('li', null, name + ': ' + values.join(', ')));
      });
      headerSection.appendChild(list);
      body.appendChild(headerSection);
    }

    var bodySection = text('div', 'snapshot-section');
    bodySection.appendChild(text('h3', null, '请求体'));
    bodySection.appendChild(text('pre', 'body', detail.body ? detail.body : '（空）'));
    body.appendChild(bodySection);
  }

  /* ------------------------------------------------------------------ *
   * 主题与展示偏好
   * ------------------------------------------------------------------ */

  function systemIsDark() {
    try {
      return !!(root.matchMedia && root.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (err) {
      return false;
    }
  }

  function applyTheme() {
    var theme = preferences.readTheme(localStore());
    var palette = preferences.resolvePalette(theme, systemIsDark());
    try {
      document.documentElement.dataset.palette = palette;
    } catch (err) {
      /* 非标准 DOM：忽略 */
    }
    // 图表需要在主题切换后重新上色，否则会停留在旧底色。
    var chartApi = charts();
    if (chartApi && typeof chartApi.refreshTheme === 'function') {
      try {
        chartApi.refreshTheme(palette);
      } catch (err) {
        /* 忽略刷新失败 */
      }
    }
  }

  function syncUnitControls() {
    var unit = el('token-unit');
    if (unit) unit.value = state.tokenUnit;
    var currency = el('currency');
    if (currency) currency.value = state.currency;
    var rate = el('rate');
    if (rate) rate.value = String(state.rate);
    var cny = state.currency === 'CNY';
    if (el('rate')) el('rate').hidden = !cny;
    if (el('rate-label')) el('rate-label').hidden = !cny;
  }

  function readUnitPreferences() {
    var units = preferences.readUnits(localStore());
    state.tokenUnit = normalizeTokenUnit(units.tokenUnit);
    state.currency = normalizeCurrency(units.currency);
    state.rate = normalizeRate(units.currencyRate);
  }

  function persistUnitPreferences() {
    preferences.writeUnits(localStore(), {
      tokenUnit: state.tokenUnit,
      currency: state.currency,
      currencyRate: state.rate,
    });
  }

  /* ------------------------------------------------------------------ *
   * 事件绑定
   * ------------------------------------------------------------------ */

  function syncGrainSwitch() {
    var buttons = document.querySelectorAll('#grain-switch [data-grain]');
    Array.prototype.forEach.call(buttons, function (button) {
      button.classList.toggle('active', button.getAttribute('data-grain') === state.grain);
    });
  }

  function submitForm() {
    var token = el('key').value.trim();
    if (!token) {
      showBanner('请输入密钥。');
      return;
    }
    state.token = token;
    state.page = 1;
    var remember = el('remember-key').checked ? 'local' : 'session';
    loadUsage({ remember: remember });
  }

  function switchKey() {
    dropSession('');
    el('key').focus();
  }

  function bind() {
    // 提交永远先 preventDefault：即使脚本不全，密钥也绝不会被浏览器原生提交进 URL。
    el('lookup-form').addEventListener('submit', function (event) {
      event.preventDefault();
      submitForm();
    });
    el('switch-key').addEventListener('click', switchKey);
    el('refresh').addEventListener('click', function () { state.page = 1; loadUsage(); });
    el('range').addEventListener('change', function () { state.range = el('range').value; state.page = 1; loadUsage(); });
    // 单位/货币/汇率改变展示口径，整页（额度、汇总、明细、趋势）一起重绘。
    el('token-unit').addEventListener('change', function () {
      state.tokenUnit = normalizeTokenUnit(el('token-unit').value);
      persistUnitPreferences();
      renderAll();
    });
    el('currency').addEventListener('change', function () {
      state.currency = normalizeCurrency(el('currency').value);
      persistUnitPreferences();
      syncUnitControls();
      renderAll();
    });
    el('rate').addEventListener('change', function () {
      state.rate = normalizeRate(el('rate').value);
      persistUnitPreferences();
      syncUnitControls();
      renderAll();
    });
    el('grain-switch').addEventListener('click', function (event) {
      var target = event.target.closest('[data-grain]');
      if (!target) return;
      state.grain = normalizeGrain(target.getAttribute('data-grain'));
      syncGrainSwitch();
      state.page = 1;
      loadUsage();
    });
    el('prev').addEventListener('click', function () { if (state.page > 1) { state.page -= 1; loadUsage(); } });
    el('next').addEventListener('click', function () { state.page += 1; loadUsage(); });
    el('capture-close').addEventListener('click', closeSnapshot);

    // CPA 主题（dark/light/auto，含跨标签页写入）与系统配色变化都要同步，
    // 并让已渲染的图表按新底色重新上色。
    root.addEventListener('storage', function (event) {
      if (!event || event.key === null || event.key === preferences.CPA_THEME_KEY) {
        applyTheme();
      }
    });
    try {
      if (root.matchMedia) {
        var query = root.matchMedia('(prefers-color-scheme: dark)');
        var listener = function () { applyTheme(); };
        if (typeof query.addEventListener === 'function') query.addEventListener('change', listener);
        else if (typeof query.addListener === 'function') query.addListener(listener);
      }
    } catch (err) {
      /* 不支持主题查询的环境忽略 */
    }
  }

  // 就绪标记：脚本已绑定事件，静态外壳之外的验收/调试可以据此等待，而不是靠
  // 猜测解析完成时机（HTML 元素在脚本执行前就已存在）。
  function markReady() {
    try {
      document.documentElement.dataset.lookupReady = '1';
    } catch (err) {
      /* 非标准 DOM：忽略 */
    }
  }

  function boot() {
    // 先绑定提交处理（含 preventDefault），再做任何其它判断。
    el('lookup-form').addEventListener('submit', function (event) {
      event.preventDefault();
      if (!modulesReady()) {
        showBanner(MODULE_ERROR);
        return;
      }
      submitForm();
    });
    if (!modulesReady()) {
      // 必需共享资源缺失：明确报错并阻断，绝不用退化数字冒充账务数据。
      el('dashboard').hidden = true;
      el('switch-key').hidden = true;
      el('login-gate').hidden = false;
      el('submit').disabled = true;
      showBanner(MODULE_ERROR);
      markReady();
      return;
    }
    bind();
    applyTheme();
    readUnitPreferences();
    syncUnitControls();
    syncGrainSwitch();
    markReady();
    var remembered = readStoredToken(sessionStore(), localStore(), originOf(''));
    if (!remembered.token) {
      showGate('');
      return;
    }
    el('key').value = remembered.token;
    el('remember-key').checked = remembered.remembered;
    state.token = remembered.token;
    loadUsage();
  }

  return api;
});
