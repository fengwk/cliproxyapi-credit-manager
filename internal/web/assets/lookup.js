/*
 * credit-manager 密钥自助查询脚本。
 *
 * 纯原生 JS：无框架、无 bundler、无 CDN，且不使用 innerHTML 写入服务端数据。
 * 页面只读自己的数据：密钥仅通过 Authorization 请求头发送，绝不写入 URL。
 * 除用户显式勾选“记住密钥”外不落盘；落盘时使用独立前缀 credit-manager.lookup.
 * 与财务控制台的管理密钥完全隔离。
 *
 * 趋势图复用共享图表组件 charts.js（全局 CMCharts，基于随包分发的
 * vendor/echarts-5.6.0.min.js）。组件缺席时降级为内置的文本条形，页面仍可用。
 */
(function () {
  'use strict';

  var STORAGE_PREFIX = 'credit-manager.lookup.';
  var REMEMBER_KEY = STORAGE_PREFIX + 'token';
  var PAGE_SIZE = 10;
  var USD_CNY_RATE = 7.2;

  var DATA_URL = new URL('lookup/data', location.href).toString();
  var CAPTURE_URL = new URL('lookup/capture', location.href).toString();

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

  var state = {
    token: '',
    page: 1,
    total: 0,
    pageSize: PAGE_SIZE,
    range: 'today',
    grain: 'day',
    tokenUnit: 'raw',
    trend: [],
  };

  /* ------------------------------------------------------------------ *
   * 纯展示逻辑
   * ------------------------------------------------------------------ */

  function formatInt(value) {
    var n = Number(value);
    return Number.isFinite(n) ? Math.round(n).toLocaleString() : '—';
  }

  function formatMicroUSD(value) {
    var n = Number(value);
    if (!Number.isFinite(n)) return '—';
    var usd = n / 1e6;
    var digits = Math.abs(usd) >= 1 ? 4 : 6;
    return '$' + usd.toLocaleString(undefined, { maximumFractionDigits: digits });
  }

  function formatDateTime(value) {
    if (!value) return '—';
    var date = new Date(value);
    return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
  }

  function formatDay(value) {
    if (!value) return '—';
    var date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    var pad = function (n) { return n < 10 ? '0' + n : String(n); };
    return pad(date.getMonth() + 1) + '-' + pad(date.getDate());
  }

  function requestStatus(item) {
    var outcome = OUTCOME_LABELS[item.outcome] || item.outcome;
    var label = STATE_LABELS[item.state] || item.state;
    if (item.outcome) return outcome + ' · ' + (label || '—');
    return label || '—';
  }

  // 请求数 / Token / 费用三个口径都由共享图表的同一组趋势点渲染。
  // 月维度只需服务端按日聚合，再由图表组件按月合并。
  function serverBucket() {
    return state.grain === 'hour' ? 'hour' : 'day';
  }

  function rangeBounds(mode) {
    if (mode === 'all') return {};
    var start = new Date();
    start.setHours(0, 0, 0, 0);
    if (mode !== 'today') {
      var days = Number(mode);
      if (Number.isFinite(days) && days > 1) start.setDate(start.getDate() - (days - 1));
    }
    return { from: start.toISOString(), to: new Date().toISOString() };
  }

  function buildDataURL() {
    var url = new URL(DATA_URL);
    var bounds = rangeBounds(state.range);
    if (bounds.from) url.searchParams.set('from', bounds.from);
    if (bounds.to) url.searchParams.set('to', bounds.to);
    url.searchParams.set('page', String(state.page));
    url.searchParams.set('page_size', String(state.pageSize));
    url.searchParams.set('bucket', serverBucket());
    return url;
  }

  function buildCaptureURL(id) {
    var url = new URL(CAPTURE_URL);
    url.searchParams.set('id', id);
    return url;
  }

  /* ------------------------------------------------------------------ *
   * 本机存储（独立前缀，仅存用户自己的密钥）
   * ------------------------------------------------------------------ */

  function readRememberedToken() {
    try {
      return localStorage.getItem(REMEMBER_KEY) || sessionStorage.getItem(REMEMBER_KEY) || '';
    } catch (_) {
      return '';
    }
  }

  function tokenIsPersisted() {
    try {
      return localStorage.getItem(REMEMBER_KEY) !== null;
    } catch (_) {
      return false;
    }
  }

  function persistToken(token, remember) {
    try {
      sessionStorage.setItem(REMEMBER_KEY, token);
      if (remember) localStorage.setItem(REMEMBER_KEY, token);
      else localStorage.removeItem(REMEMBER_KEY);
    } catch (_) {
      /* 隐私模式下不可写：仍可在本次会话内使用 */
    }
  }

  function forgetToken() {
    try {
      localStorage.removeItem(REMEMBER_KEY);
      sessionStorage.removeItem(REMEMBER_KEY);
    } catch (_) {
      /* 忽略不可写存储 */
    }
  }

  /* ------------------------------------------------------------------ *
   * DOM
   * ------------------------------------------------------------------ */

  var el = function (id) { return document.getElementById(id); };

  function clear(node) {
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

  /* ------------------------------------------------------------------ *
   * 网络
   * ------------------------------------------------------------------ */

  function apiFetch(url) {
    return fetch(url.toString(), {
      method: 'GET',
      headers: { Authorization: 'Bearer ' + state.token, Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
    });
  }

  async function loadUsage() {
    setBusy(true);
    try {
      var response = await apiFetch(buildDataURL());
      if (response.status === 401) {
        forgetToken();
        state.token = '';
        showGate('密钥无效或已停用，请重新输入。');
        return;
      }
      if (!response.ok) {
        showBanner('查询失败（HTTP ' + response.status + '）。');
        return;
      }
      renderUsage(await response.json());
      hideBanner();
      showDashboard();
    } catch (_) {
      showBanner('网络异常，请稍后重试。');
    } finally {
      setBusy(false);
    }
  }

  async function openSnapshot(id) {
    var response;
    try {
      response = await apiFetch(buildCaptureURL(id));
    } catch (_) {
      showBanner('网络异常，请稍后重试。');
      return;
    }
    if (response.status === 401) {
      showGate('密钥无效或已停用，请重新输入。');
      return;
    }
    if (!response.ok) {
      showBanner('请求快照不可用。');
      return;
    }
    renderSnapshot(await response.json());
    var modal = el('capture-modal');
    if (typeof modal.showModal === 'function') modal.showModal();
  }

  /* ------------------------------------------------------------------ *
   * 渲染
   * ------------------------------------------------------------------ */

  function renderUsage(data) {
    var key = data.key || {};
    var summary = data.summary || {};
    var tokens = summary.tokens || {};

    el('key-label').textContent = key.label ? key.label : '密钥概览';
    el('key-state').textContent = (key.enabled ? '已启用' : '已停用') +
      (key.expires_at ? ' · 到期 ' + formatDateTime(key.expires_at) : '');
    el('retention').textContent = data.retention_from ? '保留自 ' + formatDateTime(data.retention_from) : '';

    var quotas = el('quotas');
    clear(quotas);
    quotas.appendChild(card('已用额度', formatMicroUSD(key.spent_micro_usd)));
    quotas.appendChild(card('在途预占', formatMicroUSD(key.held_micro_usd)));
    quotas.appendChild(card('总额度', key.quota_micro_usd ? formatMicroUSD(key.quota_micro_usd) : '不限'));
    quotas.appendChild(card('日额度', key.daily_micro_usd ? formatMicroUSD(key.daily_micro_usd) : '不限'));
    quotas.appendChild(card('周额度', key.weekly_micro_usd ? formatMicroUSD(key.weekly_micro_usd) : '不限'));
    quotas.appendChild(card('月额度', key.monthly_micro_usd ? formatMicroUSD(key.monthly_micro_usd) : '不限'));
    quotas.appendChild(card('并发上限', key.max_concurrent ? formatInt(key.max_concurrent) : '不限'));
    if (key.allowed_models && key.allowed_models.length) {
      quotas.appendChild(card('允许模型', key.allowed_models.join('、')));
    }

    var stats = el('stats');
    clear(stats);
    stats.appendChild(card('请求数', formatInt(summary.requests)));
    stats.appendChild(card('执行次数', formatInt(summary.executions)));
    stats.appendChild(card('失败请求', formatInt(summary.failed), summary.failed ? 'danger' : null));
    stats.appendChild(card('未结算请求', formatInt(summary.pending), summary.pending ? 'warn' : null));
    stats.appendChild(card('执行成本', formatMicroUSD(summary.cost_micro_usd)));
    stats.appendChild(card('人工补记', formatMicroUSD(summary.adjustment_micro_usd)));
    stats.appendChild(card('总 Token', formatInt(tokens.total)));
    if (typeof summary.cache_read_ratio === 'number') {
      stats.appendChild(card('缓存读取占比', (summary.cache_read_ratio * 100).toFixed(1) + '%'));
    }

    state.trend = data.trend || [];
    renderTrend();
    renderRecent(data.items || [], data.total || 0, data.page || 1, data.page_size || PAGE_SIZE);
  }

  // 共享图表组件负责请求数 / Token / 费用三条趋势；缺席时降级为内置条形。
  function renderTrend() {
    var containers = { requests: el('trend-requests'), tokens: el('trend-tokens'), cost: el('trend-cost') };
    Object.keys(containers).forEach(function (name) { clear(containers[name]); });
    el('trend-empty').hidden = state.trend.length > 0;

    var shared = window.CMCharts;
    if (shared && typeof shared.render === 'function') {
      shared.render(containers, state.trend, {
        bucket: state.grain,
        tokenUnit: state.tokenUnit,
        currency: 'USD',
        rate: USD_CNY_RATE,
      });
      return;
    }
    renderTrendFallback(containers);
  }

  function renderTrendFallback(containers) {
    if (!state.trend.length) return;
    var max = 0;
    state.trend.forEach(function (point) {
      max = Math.max(max, trendValue(point, 'tokens'), trendValue(point, 'requests'), trendValue(point, 'cost'));
    });
    state.trend.forEach(function (point) {
      containers.requests.appendChild(fallbackBar(formatDay(point.time), trendValue(point, 'requests'), max, formatInt));
      containers.tokens.appendChild(fallbackBar(formatDay(point.time), trendValue(point, 'tokens'), max, formatInt));
      containers.cost.appendChild(fallbackBar(formatDay(point.time), trendValue(point, 'cost'), max, formatMicroUSD));
    });
  }

  function trendValue(point, metric) {
    if (metric === 'tokens') return Number((point.tokens || {}).total) || 0;
    if (metric === 'requests') return Number(point.requests) || 0;
    return Number(point.cost_micro_usd) || 0;
  }

  function fallbackBar(label, value, max, format) {
    var row = text('div', 'bar-row');
    row.appendChild(text('span', 'bar-label', label));
    var track = text('span', 'bar-track');
    var fill = text('span', 'bar-fill');
    fill.style.width = (max > 0 ? Math.round((value / max) * 100) : 0) + '%';
    track.appendChild(fill);
    row.appendChild(track);
    row.appendChild(text('span', 'bar-value', format(value)));
    return row;
  }

  function renderRecent(items, total, page, pageSize) {
    var body = el('recent-body');
    clear(body);
    el('recent-state').textContent = total + ' 条记录';
    items.forEach(function (item) {
      var row = document.createElement('tr');
      row.appendChild(text('td', null, formatDateTime(item.started_at)));
      row.appendChild(text('td', 'model', item.model || '—'));
      row.appendChild(text('td', null, requestStatus(item)));
      var executions = item.executions || [];
      var tokens = executions.reduce(function (sum, execution) {
        return sum + Number((execution.tokens || {}).total || 0);
      }, 0);
      row.appendChild(text('td', 'num', formatInt(tokens)));
      var cost = executions.reduce(function (sum, execution) {
        return sum + (execution.cost_micro_usd == null ? 0 : Number(execution.cost_micro_usd));
      }, 0);
      row.appendChild(text('td', 'num', formatMicroUSD(cost + Number(item.adjustment_micro_usd || 0))));
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
    meta.appendChild(document.createTextNode(' · 记录时间 ' + formatDateTime(detail.at) +
      (detail.expires_at ? ' · 保留至 ' + formatDateTime(detail.expires_at) : '')));
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
   * 事件绑定
   * ------------------------------------------------------------------ */

  function syncGrainSwitch() {
    var buttons = document.querySelectorAll('#grain-switch [data-grain]');
    Array.prototype.forEach.call(buttons, function (button) {
      button.classList.toggle('active', button.getAttribute('data-grain') === state.grain);
    });
  }

  function submitForm(event) {
    event.preventDefault();
    var token = el('key').value.trim();
    if (!token) {
      showBanner('请输入密钥。');
      return;
    }
    state.token = token;
    state.page = 1;
    persistToken(token, el('remember-key').checked);
    loadUsage();
  }

  function switchKey() {
    forgetToken();
    state.token = '';
    el('key').value = '';
    el('remember-key').checked = false;
    hideBanner();
    showGate('');
    el('key').focus();
  }

  function bind() {
    el('lookup-form').addEventListener('submit', submitForm);
    el('switch-key').addEventListener('click', switchKey);
    el('refresh').addEventListener('click', function () { state.page = 1; loadUsage(); });
    el('range').addEventListener('change', function () { state.range = el('range').value; state.page = 1; loadUsage(); });
    el('token-unit').addEventListener('change', function () { state.tokenUnit = el('token-unit').value; renderTrend(); });
    el('grain-switch').addEventListener('click', function (event) {
      var target = event.target.closest('[data-grain]');
      if (!target) return;
      state.grain = target.getAttribute('data-grain');
      syncGrainSwitch();
      state.page = 1;
      loadUsage();
    });
    el('prev').addEventListener('click', function () { if (state.page > 1) { state.page -= 1; loadUsage(); } });
    el('next').addEventListener('click', function () { state.page += 1; loadUsage(); });
    el('capture-close').addEventListener('click', function () { el('capture-modal').close(); });
  }

  function boot() {
    bind();
    syncGrainSwitch();
    var remembered = readRememberedToken();
    if (!remembered) {
      showGate('');
      return;
    }
    el('key').value = remembered;
    el('remember-key').checked = tokenIsPersisted();
    state.token = remembered;
    loadUsage();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
