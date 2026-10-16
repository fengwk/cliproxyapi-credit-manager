/*
 * credit-manager 高级定价规则与模型目录模块（admin-pricing.js）。
 *
 * 只使用 internal/credit 的真实 JSON 契约，任何扁平费率别名都会让后端
 * DisallowUnknownFields 解码失败（400），因此这里只发送嵌套结构：
 *
 *   PricingRule{ id, match_kind, pattern, priority, enabled, price, tiers }
 *   Price{ model, rates:{input,output,cache_read,cache_write,reasoning?},
 *          billing_mode: "", "token", "per_image", per_image_micro_usd }
 *   PriceTier{ kind:"context"|"service", threshold?, service?, label?, rates:{...} }
 *
 * 约定（与后端语义一一对应，不得在编辑中互相转换）：
 *   - 金额都是「微美元 / 每 100 万 tokens」的 64 位有符号整数，且不得为负；
 *   - reasoning 字段缺省（null）表示「按输出单价」，显式 0 表示「免费」；
 *   - 阶梯只挂在规则顶层（rule.tiers），每条阶梯自带嵌套 rates，费率 0 表示继承基础费率；
 *   - 后端只用 PUT /api/pricing-rules 写入；目录接口只读，绝不自动改写任何价格。
 *
 * 目录（GET /api/model-catalog）返回 Snapshot{ items, source, fetched_at, cached, stale, error }，
 * 可能包含数千个模型：界面只渲染当前页（最多 50 行），并在 stale 时如实标注「缓存数据可能已过期」，
 * 绝不把过期数据说成「最新」。
 */
(function (global) {
  'use strict';

  var INT64_MIN = -9223372036854775808n;
  var INT64_MAX = 9223372036854775807n;
  // 与后端 maxPriceTiers 保持一致：一条规则最多 16 个阶梯。
  var MAX_TIERS = 16;
  // 后台上游目录可能有近万个模型：每页最多渲染 50 行。
  var CATALOG_PAGE_SIZE = 50;

  var ctx = null;
  var rules = [];

  /* ------------------------------------------------------------------ *
   * 基础工具
   * ------------------------------------------------------------------ */

  function trimText(value) {
    return value === null || value === undefined ? '' : String(value).trim();
  }

  function intString(value) {
    if (value === null || value === undefined) return '0';
    if (typeof value === 'bigint') return value.toString();
    if (typeof value === 'object' && value.cmIntText) return value.cmIntText;
    return String(value);
  }

  // 可空整数 → 十进制文本：null/undefined 保留为空串（表示「未提供」而非 0）。
  function rateText(value) {
    if (value === null || value === undefined) return '';
    return intString(value);
  }

  function formatRateDisplay(value) {
    if (value === null || value === undefined) return '0';
    var text = intString(value);
    // 复用页面统一的整数格式化口径（app.js / charts.js），保证与价格表一致。
    if (global.CM && typeof global.CM.formatInt === 'function') return global.CM.formatInt(text);
    if (global.CMCharts && typeof global.CMCharts.formatInt === 'function') return global.CMCharts.formatInt(text);
    return text;
  }

  function formatTimestamp(value) {
    if (value === null || value === undefined || value === '') return '';
    if (global.CM && typeof global.CM.formatDateTime === 'function') {
      try {
        return global.CM.formatDateTime(value);
      } catch (err) {
        /* 回退到原始文本 */
      }
    }
    return String(value);
  }

  function isZeroRate(value) {
    if (value === null || value === undefined) return true;
    return /^-?0+$/.test(intString(value));
  }

  function pushError(errors, message) {
    if (errors && typeof errors.push === 'function') errors.push(message);
  }

  function elNode(tag, className) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    return node;
  }

  function buttonNode(label, className, onClick) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = className;
    btn.textContent = label;
    if (onClick) btn.onclick = onClick;
    return btn;
  }

  function textInputNode(className, value, placeholder) {
    var input = document.createElement('input');
    input.type = 'text';
    input.className = className;
    input.value = value === null || value === undefined ? '' : String(value);
    input.placeholder = placeholder || '';
    input.spellcheck = false;
    return input;
  }

  function appendLine(parent, className, text) {
    var line = document.createElement('div');
    if (className) line.className = className;
    line.textContent = text;
    parent.appendChild(line);
    return line;
  }

  // 手工深拷贝：绝不能对可能含 64 位整数（BigInt）的结构调用
  // JSON.parse(JSON.stringify(...))，那会在运行时抛异常并丢掉编辑操作。
  function cloneValue(value) {
    if (value === null || typeof value !== 'object') return value;
    if (Array.isArray(value)) {
      var list = new Array(value.length);
      for (var i = 0; i < value.length; i++) list[i] = cloneValue(value[i]);
      return list;
    }
    var out = {};
    var keys = Object.keys(value);
    for (var k = 0; k < keys.length; k++) out[keys[k]] = cloneValue(value[keys[k]]);
    return out;
  }

  /* ------------------------------------------------------------------ *
   * 64 位整数解析（严格、可测试）
   * ------------------------------------------------------------------ */

  // 返回 BigInt；非法或越界返回 undefined 并登记错误。
  // 空串视为「未填写」而不是 0：必填字段必须显式填写（显式 0 才表示免费）。
  function parseSigned64(value, label, errors) {
    var name = label || '数值';
    var text = trimText(value);
    if (text === '') {
      pushError(errors, name + ' 不能为空');
      return undefined;
    }
    if (!/^-?\d+$/.test(text)) {
      pushError(errors, name + ' 必须为合法的 64 位整数字符串');
      return undefined;
    }
    var big;
    try {
      big = BigInt(text);
    } catch (err) {
      pushError(errors, name + ' 必须为合法的 64 位整数字符串');
      return undefined;
    }
    if (big < INT64_MIN || big > INT64_MAX) {
      pushError(errors, name + ' 超出 64 位有符号整数范围 [-2^63, 2^63-1]');
      return undefined;
    }
    return big;
  }

  // 必填的非负金额（微美元）。越界/负数/空都登记错误并返回 undefined。
  function parseMoney(value, label, errors) {
    var name = label || '金额';
    var big = parseSigned64(value, name, errors);
    if (big === undefined) return undefined;
    if (big < 0n) {
      pushError(errors, name + ' 不能为负数');
      return undefined;
    }
    return big;
  }

  // 可选金额：空串返回 null（表示「未提供」，例如 reasoning 继承输出单价）。
  function parseOptionalMoney(value, label, errors) {
    if (trimText(value) === '') return null;
    return parseMoney(value, label, errors);
  }

  // 阶梯费率 0 表示继承基础费率：空串与非法值都归一到 0（非法值已登记错误）。
  function moneyOrDefault(value, label, errors) {
    var parsed = parseOptionalMoney(value, label, errors);
    if (parsed === null || parsed === undefined) return 0n;
    return parsed;
  }

  /* ------------------------------------------------------------------ *
   * 请求体构造（真实后端契约）
   * ------------------------------------------------------------------ */

  function tierPayload(tier, errors, ordinal) {
    var source = tier || {};
    var label = '阶梯 #' + (ordinal || 1);
    var kind = source.kind === 'service' ? 'service' : 'context';
    var out = { kind: kind };
    if (kind === 'service') {
      var service = trimText(source.service);
      if (!service) pushError(errors, label + '：服务阶梯必须填写服务标识');
      out.service = service;
      out.label = trimText(source.label);
    } else {
      var threshold = parseSigned64(source.threshold, label + ' 门槛 Tokens', errors);
      if (threshold !== undefined && threshold <= 0n) {
        pushError(errors, label + '：上下文阶梯门槛必须为正整数');
      }
      out.threshold = threshold === undefined ? 0n : threshold;
      out.label = trimText(source.label);
    }
    var tierRates = source.rates || {};
    // 费率 0 表示继承基础费率（后端 overlayRates 的语义），因此空串按 0 处理。
    var rates = {
      input: moneyOrDefault(tierRates.input, label + ' 输入费率', errors),
      output: moneyOrDefault(tierRates.output, label + ' 输出费率', errors),
      cache_read: moneyOrDefault(tierRates.cacheRead, label + ' 缓存读费率', errors),
      cache_write: moneyOrDefault(tierRates.cacheWrite, label + ' 缓存写费率', errors),
    };
    // 阶梯的 reasoning 留空才表示继承；显式 0 会覆盖为「免费」。
    var reasoning = parseOptionalMoney(tierRates.reasoning, label + ' 思考费率', errors);
    if (reasoning !== null && reasoning !== undefined) rates.reasoning = reasoning;
    out.rates = rates;
    return out;
  }

  // 把编辑器状态（全部为字符串）转成后端 PricingRule 请求体。
  function buildRulePayload(state, errors) {
    var source = state || {};
    var id = trimText(source.id);
    var pattern = trimText(source.pattern);
    if (!id) pushError(errors, '规则 ID 不能为空');
    if (!pattern) pushError(errors, '匹配表达式 (Pattern) 不能为空');

    var matchKind = source.matchKind === 'exact' ? 'exact'
      : (source.matchKind === 'regexp' ? 'regexp' : 'glob');
    var priority = parseSigned64(source.priority, '优先级', errors);
    var billingMode = source.billingMode === 'per_image' ? 'per_image' : 'token';
    var baseRates = source.baseRates || {};
    // token 计费：基础费率必填（显式 0 = 免费，绝不静默补 0）。
    // per_image 计费：token 费率不参与计费，留空按 0 发送。
    var requireTokenRates = billingMode === 'token';
    var rates = {
      input: requireTokenRates
        ? parseMoney(baseRates.input, '基础输入费率', errors)
        : moneyOrDefault(baseRates.input, '基础输入费率', errors),
      output: requireTokenRates
        ? parseMoney(baseRates.output, '基础输出费率', errors)
        : moneyOrDefault(baseRates.output, '基础输出费率', errors),
      cache_read: requireTokenRates
        ? parseMoney(baseRates.cacheRead, '缓存读取费率', errors)
        : moneyOrDefault(baseRates.cacheRead, '缓存读取费率', errors),
      cache_write: requireTokenRates
        ? parseMoney(baseRates.cacheWrite, '缓存写入费率', errors)
        : moneyOrDefault(baseRates.cacheWrite, '缓存写入费率', errors),
    };
    // reasoning 留空：不下发该字段（后端 *int64 为 nil = 按输出单价）。
    var reasoning = parseOptionalMoney(baseRates.reasoning, '思考费率', errors);
    if (reasoning !== null && reasoning !== undefined) rates.reasoning = reasoning;

    var price = {
      model: pattern,
      rates: rates,
      billing_mode: billingMode,
      per_image_micro_usd: 0n,
    };
    if (billingMode === 'per_image') {
      // 按图计费时单张图片费率是唯一价格来源，必须显式填写（0 表示免费）。
      var perImage = parseMoney(source.perImage, '按图费率', errors);
      price.per_image_micro_usd = perImage === undefined ? 0n : perImage;
    }

    var tiers = [];
    var tierList = Array.isArray(source.tiers) ? source.tiers : [];
    if (tierList.length > MAX_TIERS) {
      pushError(errors, '阶梯数量不能超过 ' + MAX_TIERS + ' 条');
    }
    for (var i = 0; i < tierList.length && i < MAX_TIERS; i++) {
      tiers.push(tierPayload(tierList[i], errors, i + 1));
    }

    return {
      id: id,
      match_kind: matchKind,
      pattern: pattern,
      priority: priority === undefined ? 0n : priority,
      enabled: source.enabled !== false,
      price: price,
      tiers: tiers,
    };
  }

  /* ------------------------------------------------------------------ *
   * 线上数据 → 编辑器状态
   * ------------------------------------------------------------------ */

  function tierToEditorState(tier) {
    var source = tier || {};
    var rates = source.rates || {};
    return {
      kind: source.kind === 'service' ? 'service' : 'context',
      threshold: source.threshold === null || source.threshold === undefined ? '' : intString(source.threshold),
      service: trimText(source.service),
      label: trimText(source.label),
      rates: {
        input: rateText(rates.input),
        output: rateText(rates.output),
        cacheRead: rateText(rates.cache_read),
        cacheWrite: rateText(rates.cache_write),
        reasoning: rateText(rates.reasoning),
      },
    };
  }

  function ruleToEditorState(rule) {
    var source = rule || {};
    var price = source.price || {};
    var rates = price.rates || {};
    return {
      id: trimText(source.id),
      matchKind: source.match_kind === 'exact' ? 'exact'
        : (source.match_kind === 'regexp' ? 'regexp' : 'glob'),
      pattern: trimText(source.pattern),
      priority: source.priority === null || source.priority === undefined ? '10' : intString(source.priority),
      enabled: source.enabled !== false,
      billingMode: price.billing_mode === 'per_image' ? 'per_image' : 'token',
      perImage: rateText(price.per_image_micro_usd),
      baseRates: {
        input: rateText(rates.input),
        output: rateText(rates.output),
        cacheRead: rateText(rates.cache_read),
        cacheWrite: rateText(rates.cache_write),
        reasoning: rateText(rates.reasoning),
      },
      tiers: (Array.isArray(source.tiers) ? source.tiers : []).map(tierToEditorState),
    };
  }

  function defaultRule() {
    return {
      id: 'rule-' + Date.now().toString(36),
      match_kind: 'glob',
      pattern: '',
      priority: 10,
      enabled: true,
      price: { model: '', billing_mode: 'token', rates: {} },
      tiers: [],
    };
  }

  /* ------------------------------------------------------------------ *
   * 规则列表
   * ------------------------------------------------------------------ */

  function ruleRateLines(rule) {
    var price = (rule && rule.price) || {};
    var rates = price.rates || {};
    var lines = [];
    if (price.billing_mode === 'per_image') {
      lines.push('按图: ' + formatRateDisplay(price.per_image_micro_usd) + ' µUSD/张');
    }
    lines.push('入/出: ' + formatRateDisplay(rates.input) + ' / ' + formatRateDisplay(rates.output));
    if (!isZeroRate(rates.cache_read) || !isZeroRate(rates.cache_write)) {
      lines.push('缓存读/写: ' + formatRateDisplay(rates.cache_read) + ' / ' + formatRateDisplay(rates.cache_write));
    }
    if (rates.reasoning === null || rates.reasoning === undefined) {
      lines.push('思考: 继承输出');
    } else {
      lines.push('思考: ' + formatRateDisplay(rates.reasoning));
    }
    return lines;
  }

  function tierSummary(rule) {
    var tiers = Array.isArray(rule && rule.tiers) ? rule.tiers : [];
    if (tiers.length === 0) return '无';
    var descriptions = tiers.map(function (tier) {
      if (tier.kind === 'service') return '服务: ' + (tier.service || tier.label || 'service');
      return '上下文: >' + formatRateDisplay(tier.threshold);
    });
    return tiers.length + ' 级 (' + descriptions.join(', ') + ')';
  }

  function load() {
    if (!ctx || !ctx.api) return Promise.resolve();
    var listEl = document.getElementById('pricing-rules-list');
    var countEl = document.getElementById('rules-count');
    if (countEl) countEl.textContent = '加载中...';

    return ctx.api('/api/pricing-rules')
      .then(function (res) {
        rules = (res && res.items) || [];
        renderRules(rules);
      })
      .catch(function (err) {
        if (listEl) {
          listEl.textContent = '';
          var errBox = elNode('div', 'error-box');
          errBox.textContent = '加载高级规则失败：' + (err && err.message ? err.message : err);
          listEl.appendChild(errBox);
        }
        if (countEl) countEl.textContent = '加载失败';
      });
  }

  function dispose() {
    rules = [];
    ctx = null;
  }

  function renderRules(items) {
    var listEl = document.getElementById('pricing-rules-list');
    var countEl = document.getElementById('rules-count');
    if (countEl) countEl.textContent = '共 ' + items.length + ' 条高级规则';
    if (!listEl) return;
    listEl.textContent = '';

    if (items.length === 0) {
      var empty = elNode('p', 'muted empty-hint');
      empty.textContent = '暂无高级定价规则。点击上方“新增高级规则”可创建通配符、正则、按图计费或上下文/服务等级阶梯规则。';
      listEl.appendChild(empty);
      return;
    }

    var table = document.createElement('table');
    table.className = 'table table-compact rules-table';
    var thead = document.createElement('thead');
    var headRow = document.createElement('tr');
    ['ID', '匹配模式', '类型', '优先级', '计费模式与基础费率', '阶梯规则', '状态', '操作'].forEach(function (header) {
      var th = document.createElement('th');
      th.textContent = header;
      headRow.appendChild(th);
    });
    thead.appendChild(headRow);
    table.appendChild(thead);

    var tbody = document.createElement('tbody');
    items.forEach(function (rule) {
      var tr = document.createElement('tr');

      var tdId = document.createElement('td');
      tdId.className = 'mono';
      tdId.textContent = rule.id;

      var tdPattern = document.createElement('td');
      var code = document.createElement('code');
      code.textContent = rule.pattern;
      tdPattern.appendChild(code);

      var tdKind = document.createElement('td');
      tdKind.textContent = rule.match_kind || 'glob';

      var tdPriority = document.createElement('td');
      tdPriority.className = 'mono';
      tdPriority.textContent = intString(rule.priority !== undefined ? rule.priority : 0);

      var tdRates = document.createElement('td');
      // 服务端字段一律以文本节点写入，绝不拼接 innerHTML。
      ruleRateLines(rule).forEach(function (line) {
        appendLine(tdRates, 'rule-rate-line', line);
      });

      var tdTiers = document.createElement('td');
      tdTiers.textContent = tierSummary(rule);

      var tdStatus = document.createElement('td');
      tdStatus.textContent = rule.enabled ? '启用' : '禁用';
      tdStatus.className = rule.enabled ? 'badge badge-ok' : 'badge badge-warn';

      var tdActions = document.createElement('td');
      var editBtn = buttonNode('编辑', 'btn btn-small', function () {
        openRuleModal(rule);
      });
      var delBtn = buttonNode('删除', 'btn btn-small btn-danger', function () {
        deleteRule(rule);
      });
      delBtn.style.marginLeft = '4px';
      tdActions.appendChild(editBtn);
      tdActions.appendChild(delBtn);

      tr.appendChild(tdId);
      tr.appendChild(tdPattern);
      tr.appendChild(tdKind);
      tr.appendChild(tdPriority);
      tr.appendChild(tdRates);
      tr.appendChild(tdTiers);
      tr.appendChild(tdStatus);
      tr.appendChild(tdActions);

      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    listEl.appendChild(table);
  }

  // 规则删除使用结构化确认弹窗（与 app.js confirmDialog 相同的约定：
  // #confirm-checkbox + 「请先勾选确认项」），绝不使用浏览器原生 alert/confirm/prompt。
  function confirmDeleteRule(rule) {
    if (!ctx || !ctx.showModal) return Promise.resolve(false);
    return new Promise(function (resolve) {
      var chk = document.createElement('input');
      chk.type = 'checkbox';
      chk.id = 'confirm-checkbox';
      var body = [];
      var message = elNode('p', null);
      message.textContent = '确定删除高级定价规则 ' + rule.id + ' (' + rule.pattern + ') 吗？';
      body.push(message);
      var warning = elNode('p', 'hint hint-warn');
      warning.textContent = '删除后该规则将不再参与任何请求计费匹配；已记录的历史用量不受影响。';
      body.push(warning);
      var field = elNode('div', 'field');
      var label = document.createElement('label');
      label.setAttribute('for', 'confirm-checkbox');
      label.textContent = '我确认删除该规则';
      field.appendChild(label);
      field.appendChild(chk);
      body.push(field);

      var modal = ctx.showModal({ title: '删除高级定价规则' });
      modal.setBody(body);
      modal.setFooter([
        buttonNode('取消', 'btn', function () {
          modal.close();
          resolve(false);
        }),
        buttonNode('删除', 'btn btn-danger', function () {
          if (!chk.checked) {
            modal.setError('请先勾选确认项');
            return;
          }
          modal.close();
          resolve(true);
        }),
      ]);
    });
  }

  function deleteRule(rule) {
    if (!ctx || !ctx.api) return;
    confirmDeleteRule(rule).then(function (confirmed) {
      if (!confirmed) return;
      return ctx.api('/api/delete-price-rule', { method: 'POST', body: { id: rule.id } })
        .then(function () {
          if (ctx.toast) ctx.toast('规则已删除', 'ok');
          load();
        })
        .catch(function (err) {
          if (ctx.toast) ctx.toast('删除失败：' + (err && err.message ? err.message : err), 'error');
        });
    });
  }

  /* ------------------------------------------------------------------ *
   * 规则编辑器
   * ------------------------------------------------------------------ */

  function addField(form, label, input, hint) {
    var wrap = elNode('div', 'field');
    var lbl = document.createElement('label');
    if (input.id) lbl.setAttribute('for', input.id);
    lbl.textContent = label;
    wrap.appendChild(lbl);
    wrap.appendChild(input);
    if (hint) {
      var note = elNode('div', 'field-hint');
      note.textContent = hint;
      wrap.appendChild(note);
    }
    form.appendChild(wrap);
    return wrap;
  }

  function selectNode(id, options, value) {
    var select = document.createElement('select');
    if (id) select.id = id;
    options.forEach(function (option) {
      var opt = document.createElement('option');
      opt.value = option.value;
      opt.textContent = option.label;
      select.appendChild(opt);
    });
    select.value = value;
    return select;
  }

  // 打开（新建或编辑）一条高级定价规则。
  // options.asNew           : 强制按「新建」打开（例如从目录导入）。
  // options.requireConfirm  : 必须勾选确认才能写入。
  // options.note            : 弹窗顶部的数据来源/风险说明。
  function openRuleModal(existing, options) {
    if (!ctx || !ctx.showModal) return null;
    var opts = options || {};
    var isEdit = Boolean(existing) && !opts.asNew;
    var state = ruleToEditorState(existing || defaultRule());

    var modal = ctx.showModal({
      title: isEdit ? '编辑高级定价规则 · ' + state.id : '新增高级定价规则',
      wide: true,
    });

    var form = document.createElement('form');
    form.className = 'rule-form form-grid';

    if (opts.note) {
      var note = elNode('p', 'rule-source-note hint');
      note.textContent = opts.note;
      note.style.gridColumn = '1 / -1';
      form.appendChild(note);
    }

    var hint = elNode('p', 'hint');
    hint.textContent = '费率单位是「微美元 / 每 100 万 tokens」的整数（1 USD = 1,000,000 微美元）。所有基础费率必须显式填写：显式 0 表示免费；思考费率留空表示「按输出单价」。';
    hint.style.gridColumn = '1 / -1';
    form.appendChild(hint);

    var inId = textInputNode('mono', state.id, 'rule-xxxx');
    inId.id = 'rule-id';
    inId.disabled = isEdit;
    inId.required = true;
    addField(form, '规则 ID（全局唯一）', inId);

    var selKind = selectNode('rule-kind', [
      { value: 'glob', label: '通配符 (glob, 如 gpt-4o-*)' },
      { value: 'exact', label: '精确匹配 (exact)' },
      { value: 'regexp', label: '正则表达式 (regexp)' },
    ], state.matchKind);
    addField(form, '匹配模式类型', selKind);

    var inPattern = textInputNode('mono', state.pattern, '例如: gpt-4o-* 或 claude-3-5-*');
    inPattern.id = 'rule-pattern';
    inPattern.required = true;
    addField(form, '匹配表达式 (Pattern)', inPattern);

    var inPriority = textInputNode('mono', state.priority, '10');
    inPriority.id = 'rule-priority';
    addField(form, '规则优先级 (64 位整数，数值越大越先匹配)', inPriority);

    var selBillingMode = selectNode('rule-billing', [
      { value: 'token', label: '按 Token 计费 (token)' },
      { value: 'per_image', label: '按图片计费 (per_image)' },
    ], state.billingMode);
    addField(form, '计费模式', selBillingMode);

    var inPerImage = textInputNode('mono', state.perImage, '例如 50000');
    inPerImage.id = 'rule-per-image';
    addField(form, '单张图片费率 (微USD/张，按图模式必填)', inPerImage);

    var inInputRate = textInputNode('mono', state.baseRates.input, '必填，如 2000000');
    inInputRate.id = 'rule-input-rate';
    addField(form, '基础输入费率 (微USD / 1M Tokens)', inInputRate);

    var inOutputRate = textInputNode('mono', state.baseRates.output, '必填，如 6000000');
    inOutputRate.id = 'rule-output-rate';
    addField(form, '基础输出费率 (微USD / 1M Tokens)', inOutputRate);

    var inCacheRead = textInputNode('mono', state.baseRates.cacheRead, '必填，显式 0 = 免费');
    inCacheRead.id = 'rule-cache-read-rate';
    addField(form, '缓存读取命中费率 (微USD / 1M Tokens)', inCacheRead);

    var inCacheWrite = textInputNode('mono', state.baseRates.cacheWrite, '必填，显式 0 = 免费');
    inCacheWrite.id = 'rule-cache-write-rate';
    addField(form, '缓存写入创建费率 (微USD / 1M Tokens)', inCacheWrite);

    var inReasoningRate = textInputNode('mono', state.baseRates.reasoning, '留空 = 按输出单价');
    inReasoningRate.id = 'rule-reasoning-rate';
    addField(form, '思考 Token 费率 (微USD / 1M Tokens)', inReasoningRate,
      '留空：不下发 reasoning 字段，后端按输出单价计费；填写 0 表示思考 tokens 免费。');

    var chkEnabled = document.createElement('input');
    chkEnabled.type = 'checkbox';
    chkEnabled.id = 'rule-enabled';
    chkEnabled.checked = state.enabled;
    var chkLabel = document.createElement('label');
    chkLabel.className = 'checkbox-label';
    chkLabel.appendChild(chkEnabled);
    chkLabel.appendChild(document.createTextNode(' 启用该规则（禁用后命中时直接拒绝计费，绝不伪造零费率）'));
    form.appendChild(chkLabel);

    /* --- 阶梯编辑器 --- */
    var tiersSection = elNode('div', 'tiers-editor-section');
    tiersSection.style.gridColumn = '1 / -1';
    tiersSection.style.marginTop = '1rem';
    tiersSection.style.borderTop = '1px solid var(--border-color, #e5e7eb)';
    tiersSection.style.paddingTop = '1rem';

    var tiersHeader = elNode('div', 'tiers-editor-head');
    tiersHeader.style.display = 'flex';
    tiersHeader.style.justifyContent = 'space-between';
    tiersHeader.style.alignItems = 'center';
    var tiersTitle = document.createElement('h4');
    tiersTitle.textContent = '阶梯定价规则 (Context & Service Tiers)';
    tiersTitle.style.margin = '0';
    var btnAddTier = buttonNode('+ 添加阶梯', 'btn btn-small');
    tiersHeader.appendChild(tiersTitle);
    tiersHeader.appendChild(btnAddTier);
    tiersSection.appendChild(tiersHeader);

    var tiersHint = elNode('p', 'hint');
    tiersHint.textContent = '阶梯只作用于本条规则。费率留空或填 0 表示继承该阶梯对应桶的基础费率；思考费率留空表示继承，显式 0 表示该阶梯思考 tokens 免费。';
    tiersSection.appendChild(tiersHint);

    var tiersList = elNode('div', 'tiers-list');
    tiersSection.appendChild(tiersList);
    form.appendChild(tiersSection);

    var workingTiers = cloneValue(Array.isArray(state.tiers) ? state.tiers : []);

    // 阶梯卡片的输入必须先回写到 workingTiers，再重渲染；
    // 否则「添加/移除阶梯」会丢掉已经输入但尚未保存的内容。
    function captureTiers() {
      var cards = tiersList.querySelectorAll('.tier-item-card');
      for (var i = 0; i < cards.length; i++) {
        if (typeof cards[i]._readInto === 'function') cards[i]._readInto();
      }
    }

    function renderWorkingTiers() {
      tiersList.textContent = '';
      if (workingTiers.length === 0) {
        var emptyTiers = elNode('p', 'muted');
        emptyTiers.textContent = '暂无阶梯规则（请求将按上述基础费率计费）。点击“+ 添加阶梯”配置长上下文或按服务等级覆盖。';
        tiersList.appendChild(emptyTiers);
        return;
      }

      workingTiers.forEach(function (tier) {
        var item = elNode('div', 'tier-item-card');

        var rowTop = elNode('div', 'tier-row');
        rowTop.style.display = 'flex';
        rowTop.style.gap = '0.75rem';
        rowTop.style.alignItems = 'center';

        var selTierKind = selectNode('', [
          { value: 'context', label: '上下文阶梯 (Context)' },
          { value: 'service', label: '服务等级阶梯 (Service)' },
        ], tier.kind);
        selTierKind.className = 'tier-kind';

        var inThreshold = textInputNode('mono tier-threshold', tier.threshold, '门槛 Token 数 (如 128000)');
        var inService = textInputNode('mono tier-service', tier.service, '服务标识 (如 priority, fast)');
        var inLabel = textInputNode('tier-label', tier.label, '阶梯标签描述 (可选)');

        function syncTierKindVisibility() {
          inThreshold.style.display = tier.kind === 'service' ? 'none' : 'inline-block';
          inService.style.display = tier.kind === 'service' ? 'inline-block' : 'none';
        }
        selTierKind.onchange = function () {
          tier.kind = selTierKind.value === 'service' ? 'service' : 'context';
          syncTierKindVisibility();
        };
        syncTierKindVisibility();

        var btnRemoveTier = buttonNode('移除', 'btn btn-small btn-danger');
        btnRemoveTier.onclick = function () {
          captureTiers();
          var at = workingTiers.indexOf(tier);
          if (at >= 0) workingTiers.splice(at, 1);
          renderWorkingTiers();
        };

        rowTop.appendChild(selTierKind);
        rowTop.appendChild(inThreshold);
        rowTop.appendChild(inService);
        rowTop.appendChild(inLabel);
        rowTop.appendChild(btnRemoveTier);
        item.appendChild(rowTop);

        var ratesGrid = elNode('div', 'tier-rates-grid');
        ratesGrid.style.display = 'grid';
        ratesGrid.style.gridTemplateColumns = 'repeat(auto-fit, minmax(130px, 1fr))';
        ratesGrid.style.gap = '0.5rem';
        ratesGrid.style.marginTop = '0.5rem';

        var tierInRate = textInputNode('mono tier-rate-input', tier.rates.input, '阶梯输入费率');
        var tierOutRate = textInputNode('mono tier-rate-output', tier.rates.output, '阶梯输出费率');
        var tierCacheRead = textInputNode('mono tier-rate-cache-read', tier.rates.cacheRead, '阶梯缓存读');
        var tierCacheWrite = textInputNode('mono tier-rate-cache-write', tier.rates.cacheWrite, '阶梯缓存写');
        var tierReasoning = textInputNode('mono tier-rate-reasoning', tier.rates.reasoning, '阶梯思考费率(留空继承)');

        [tierInRate, tierOutRate, tierCacheRead, tierCacheWrite, tierReasoning].forEach(function (node) {
          ratesGrid.appendChild(node);
        });
        item.appendChild(ratesGrid);

        // 把当前卡片的所有输入回写到本条阶梯的数据模型。
        item._readInto = function () {
          tier.kind = selTierKind.value === 'service' ? 'service' : 'context';
          tier.threshold = inThreshold.value;
          tier.service = inService.value;
          tier.label = inLabel.value;
          tier.rates.input = tierInRate.value;
          tier.rates.output = tierOutRate.value;
          tier.rates.cacheRead = tierCacheRead.value;
          tier.rates.cacheWrite = tierCacheWrite.value;
          tier.rates.reasoning = tierReasoning.value;
        };

        tiersList.appendChild(item);
      });
    }

    btnAddTier.onclick = function () {
      captureTiers();
      if (workingTiers.length >= MAX_TIERS) {
        modal.setError('阶梯数量不能超过 ' + MAX_TIERS + ' 条');
        return;
      }
      workingTiers.push({
        kind: 'context',
        threshold: '128000',
        service: '',
        label: '长上下文阶梯',
        rates: { input: '', output: '', cacheRead: '', cacheWrite: '', reasoning: '' },
      });
      renderWorkingTiers();
    };

    renderWorkingTiers();
    modal.setBody([form]);

    var chkConfirm = null;
    if (opts.requireConfirm) {
      var confirmWrap = elNode('label', 'checkbox-label rule-confirm');
      chkConfirm = document.createElement('input');
      chkConfirm.type = 'checkbox';
      chkConfirm.id = 'rule-confirm';
      confirmWrap.appendChild(chkConfirm);
      confirmWrap.appendChild(document.createTextNode(' 我确认将该价格写入系统：写入会立即影响后续计费，且不会回溯已记录的用量。'));
      var confirmSection = elNode('p', 'hint hint-warn');
      confirmSection.style.gridColumn = '1 / -1';
      confirmSection.appendChild(confirmWrap);
      form.appendChild(confirmSection);
    }

    function readEditorState() {
      captureTiers();
      return {
        id: inId.value,
        matchKind: selKind.value,
        pattern: inPattern.value,
        priority: inPriority.value,
        enabled: chkEnabled.checked,
        billingMode: selBillingMode.value,
        perImage: inPerImage.value,
        baseRates: {
          input: inInputRate.value,
          output: inOutputRate.value,
          cacheRead: inCacheRead.value,
          cacheWrite: inCacheWrite.value,
          reasoning: inReasoningRate.value,
        },
        tiers: workingTiers,
      };
    }

    var btnSave = buttonNode('保存规则', 'btn btn-primary');
    btnSave.onclick = function () {
      if (chkConfirm && !chkConfirm.checked) {
        modal.setError('请先勾选确认项，明确该写入会立即影响后续计费。');
        return;
      }
      var errors = [];
      var payload = buildRulePayload(readEditorState(), errors);
      if (errors.length > 0) {
        modal.setError(errors.join('；'));
        return;
      }
      btnSave.disabled = true;
      modal.setError('');
      ctx.api('/api/pricing-rules', { method: 'PUT', body: payload })
        .then(function () {
          if (ctx.toast) ctx.toast('高级定价规则已保存', 'ok');
          modal.close();
          load();
        })
        .catch(function (err) {
          btnSave.disabled = false;
          modal.setError('保存失败：' + (err && err.message ? err.message : err));
        });
    };

    var btnCancel = buttonNode('取消', 'btn btn-ghost', modal.close);

    modal.setFooter([btnSave, btnCancel]);
    return modal;
  }

  /* ------------------------------------------------------------------ *
   * 模型价格目录（只读预览 + 显式确认后导入）
   * ------------------------------------------------------------------ */

  function catalogItems(snapshot) {
    return (snapshot && Array.isArray(snapshot.items)) ? snapshot.items : [];
  }

  function catalogStatusText(snapshot) {
    if (!snapshot || typeof snapshot !== 'object') return '';
    var parts = [];
    if (snapshot.stale) {
      parts.push('⚠ 上游刷新失败，以下为缓存目录数据（可能已过期，不代表当前上游价格）');
    } else if (snapshot.cached) {
      parts.push('以下为缓存中的目录数据（本次未重新拉取上游）');
    } else {
      parts.push('以下为本次从上游实时获取的目录数据');
    }
    if (snapshot.source) parts.push('来源: ' + snapshot.source);
    if (snapshot.fetched_at) parts.push('获取时间: ' + formatTimestamp(snapshot.fetched_at));
    if (snapshot.error) parts.push('错误: ' + snapshot.error);
    return parts.join(' · ');
  }

  function filterCatalog(items, query) {
    var needle = trimText(query).toLowerCase();
    if (!needle) return items.slice();
    return items.filter(function (model) {
      var haystack = [
        (model && model.id) || '',
        (model && model.provider) || '',
        (model && model.name) || '',
      ].join(' ').toLowerCase();
      return haystack.indexOf(needle) >= 0;
    });
  }

  function paginate(items, page, pageSize) {
    var size = pageSize > 0 ? pageSize : CATALOG_PAGE_SIZE;
    var total = items.length;
    var pages = Math.max(1, Math.ceil(total / size));
    var requested = Math.floor(page);
    if (!isFinite(requested) || requested < 1) requested = 1;
    var current = Math.min(requested, pages);
    var start = (current - 1) * size;
    return { page: current, pages: pages, total: total, pageSize: size, items: items.slice(start, start + size) };
  }

  // 目录里的 id 已经带 provider 前缀，规则 id 只是稳定标识，绝不改写模型名。
  function catalogRuleID(model) {
    var slug = trimText(model).replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '');
    return 'catalog-' + (slug || 'model');
  }

  // 目录阶梯费率只有在四个基础桶都齐全时才可导入（缺失不等于 0，绝不静默补零）。
  function completeTierRates(rates) {
    var source = rates || {};
    var fields = ['input', 'output', 'cache_read', 'cache_write'];
    for (var i = 0; i < fields.length; i++) {
      if (source[fields[i]] === null || source[fields[i]] === undefined) return null;
    }
    return {
      input: source.input,
      output: source.output,
      cache_read: source.cache_read,
      cache_write: source.cache_write,
      reasoning: source.reasoning,
    };
  }

  // 把目录条目映射为一条「精确匹配」的 PricingRule 草稿。
  function catalogEntryToRule(entry, snapshot) {
    var model = entry || {};
    var pattern = trimText(model.id);
    var rates = model.rates || {};
    var tiers = [];
    var skipped = 0;
    var sourceTiers = Array.isArray(model.context_tiers) ? model.context_tiers : [];
    sourceTiers.forEach(function (tier) {
      if (!tier) {
        skipped++;
        return;
      }
      var mapped = completeTierRates(tier.rates);
      if (!mapped || tiers.length >= MAX_TIERS) {
        skipped++;
        return;
      }
      tiers.push({
        kind: 'context',
        threshold: tier.threshold,
        label: tier.label || '',
        service: '',
        rates: mapped,
      });
    });
    var rule = {
      id: catalogRuleID(pattern),
      match_kind: 'exact',
      pattern: pattern,
      priority: 10,
      enabled: true,
      price: {
        model: pattern,
        billing_mode: 'token',
        per_image_micro_usd: 0,
        rates: {
          input: rates.input,
          output: rates.output,
          cache_read: rates.cache_read,
          cache_write: rates.cache_write,
          reasoning: rates.reasoning,
        },
      },
      tiers: tiers,
    };
    return { rule: rule, skippedTiers: skipped, totalTiers: sourceTiers.length, snapshot: snapshot || null };
  }

  function catalogImportNote(mapping) {
    var snapshot = (mapping && mapping.snapshot) || {};
    var parts = [];
    var source = snapshot.source ? snapshot.source : '未知来源';
    if (snapshot.stale) {
      parts.push('⚠ 目录是上游刷新失败后的缓存数据（来源: ' + source + '，可能已过期），导入前请自行核对费率。');
    } else if (snapshot.cached) {
      parts.push('目录来自缓存（来源: ' + source + '）。');
    } else {
      parts.push('目录来自本次实时获取（来源: ' + source + '）。');
    }
    parts.push('导入会写入一条精确匹配 (exact) 的高级定价规则；目录只读，获取目录本身绝不会改写任何价格。');
    if (mapping && mapping.totalTiers > 0) {
      parts.push('已带入 ' + (mapping.totalTiers - mapping.skippedTiers) + '/' + mapping.totalTiers + ' 条上下文阶梯（费率不完整的阶梯已跳过）。');
    }
    parts.push('基础费率缺失时必须手工显式填写（0 = 免费）；思考费率留空表示按输出单价。');
    return parts.join(' ');
  }

  function openCatalogModal() {
    if (!ctx || !ctx.api || !ctx.showModal) return null;
    var modal = ctx.showModal({ title: '模型价格目录 (Model Catalog)', wide: true });

    var statusEl = elNode('p', 'status-line catalog-status');
    var searchField = elNode('div', 'field');
    var searchLabel = document.createElement('label');
    searchLabel.setAttribute('for', 'catalog-search');
    searchLabel.textContent = '搜索模型';
    var searchInput = document.createElement('input');
    searchInput.type = 'search';
    searchInput.id = 'catalog-search';
    searchInput.placeholder = '按模型 ID / Provider / 名称过滤，例如 opencode-go';
    searchField.appendChild(searchLabel);
    searchField.appendChild(searchInput);

    var tableWrap = elNode('div', 'catalog-table-wrap');
    var pagerEl = elNode('div', 'catalog-pager muted');

    var container = elNode('div', 'catalog-preview');
    container.appendChild(statusEl);
    container.appendChild(searchField);
    container.appendChild(tableWrap);
    container.appendChild(pagerEl);
    modal.setBody([container]);

    var state = { snapshot: null, query: '', page: 1 };

    function renderTable() {
      var snapshot = state.snapshot || {};
      var all = catalogItems(snapshot);
      statusEl.textContent = catalogStatusText(snapshot);
      tableWrap.textContent = '';
      pagerEl.textContent = '';

      if (all.length === 0) {
        var none = elNode('p', 'muted');
        none.textContent = snapshot.error
          ? '目录不可用：' + snapshot.error
          : '未找到可用模型价格数据。';
        tableWrap.appendChild(none);
        return;
      }

      var filtered = filterCatalog(all, state.query);
      var pageData = paginate(filtered, state.page, CATALOG_PAGE_SIZE);
      state.page = pageData.page;

      if (filtered.length === 0) {
        var noMatch = elNode('p', 'muted');
        noMatch.textContent = '没有匹配「' + state.query + '」的模型。';
        tableWrap.appendChild(noMatch);
        return;
      }

      var table = document.createElement('table');
      table.className = 'table table-compact catalog-table';
      var thead = document.createElement('thead');
      var headRow = document.createElement('tr');
      ['模型 ID', 'Provider', '输入费率', '输出费率', '上下文阶梯', '操作'].forEach(function (header) {
        var th = document.createElement('th');
        th.textContent = header;
        headRow.appendChild(th);
      });
      thead.appendChild(headRow);
      table.appendChild(thead);

      var tbody = document.createElement('tbody');
      pageData.items.forEach(function (model) {
        var tr = elNode('tr', 'catalog-row');
        var rates = model.rates || {};

        var tdModel = document.createElement('td');
        tdModel.className = 'mono';
        tdModel.textContent = model.id || '';

        var tdProvider = document.createElement('td');
        tdProvider.textContent = model.provider || 'default';

        var tdInput = document.createElement('td');
        tdInput.textContent = rates.input === null || rates.input === undefined
          ? '缺失' : formatRateDisplay(rates.input) + ' µUSD/M';

        var tdOutput = document.createElement('td');
        tdOutput.textContent = rates.output === null || rates.output === undefined
          ? '缺失' : formatRateDisplay(rates.output) + ' µUSD/M';

        var tdTiers = document.createElement('td');
        var tiers = Array.isArray(model.context_tiers) ? model.context_tiers : [];
        tdTiers.textContent = tiers.length > 0
          ? tiers.length + ' 条（' + tiers.map(function (tier) {
            return '>' + formatRateDisplay(tier.threshold);
          }).join(', ') + '）'
          : '无';

        var tdAction = document.createElement('td');
        var btnUse = buttonNode('选用', 'btn btn-small btn-primary');
        btnUse.onclick = function () {
          var mapping = catalogEntryToRule(model, snapshot);
          openRuleModal(mapping.rule, {
            asNew: true,
            requireConfirm: true,
            note: catalogImportNote(mapping),
          });
        };
        tdAction.appendChild(btnUse);

        tr.appendChild(tdModel);
        tr.appendChild(tdProvider);
        tr.appendChild(tdInput);
        tr.appendChild(tdOutput);
        tr.appendChild(tdTiers);
        tr.appendChild(tdAction);
        tbody.appendChild(tr);
      });
      table.appendChild(tbody);
      tableWrap.appendChild(table);

      // 分页信息必须如实反映总数与当前页，绝不一次性渲染上万个模型。
      var pagerText = '第 ' + pageData.page + ' / ' + pageData.pages + ' 页 · 共 ' + pageData.total + ' 个模型（每页最多 ' + pageData.pageSize + ' 行）';
      var prevBtn = buttonNode('上一页', 'btn btn-small');
      prevBtn.disabled = pageData.page <= 1;
      prevBtn.onclick = function () {
        state.page = pageData.page - 1;
        renderTable();
      };
      var nextBtn = buttonNode('下一页', 'btn btn-small');
      nextBtn.disabled = pageData.page >= pageData.pages;
      nextBtn.onclick = function () {
        state.page = pageData.page + 1;
        renderTable();
      };
      pagerEl.appendChild(prevBtn);
      var pagerLabel = document.createElement('span');
      pagerLabel.className = 'catalog-pager-text';
      pagerLabel.textContent = pagerText;
      pagerEl.appendChild(pagerLabel);
      pagerEl.appendChild(nextBtn);
    }

    searchInput.oninput = function () {
      state.query = searchInput.value;
      state.page = 1;
      renderTable();
    };

    statusEl.textContent = '正在获取模型价格目录…';
    ctx.api('/api/model-catalog?refresh=1')
      .then(function (res) {
        state.snapshot = res || {};
        state.page = 1;
        renderTable();
      })
      .catch(function (err) {
        modal.setError('获取价格库失败：' + (err && err.message ? err.message : err));
        statusEl.textContent = '';
      });

    modal.setFooter([buttonNode('关闭', 'btn btn-ghost', modal.close)]);
    return modal;
  }

  /* ------------------------------------------------------------------ *
   * 初始化
   * ------------------------------------------------------------------ */

  function init(options) {
    ctx = options || {};
    var btnCreateRule = document.getElementById('rules-create');
    if (btnCreateRule) {
      btnCreateRule.onclick = function () {
        openRuleModal(null);
      };
    }
    var btnLoadCatalog = document.getElementById('btn-load-model-catalog');
    if (btnLoadCatalog) {
      btnLoadCatalog.onclick = function () {
        openCatalogModal();
      };
    }
  }

  global.CMAdminPricing = {
    init: init,
    load: load,
    dispose: dispose,
    // 仅供 Node 单元测试驱动的内部入口与纯函数，不作为页面公共 API 使用。
    __test: {
      parseSigned64: parseSigned64,
      parseMoney: parseMoney,
      parseOptionalMoney: parseOptionalMoney,
      buildRulePayload: buildRulePayload,
      ruleToEditorState: ruleToEditorState,
      tierToEditorState: tierToEditorState,
      defaultRule: defaultRule,
      catalogItems: catalogItems,
      catalogStatusText: catalogStatusText,
      filterCatalog: filterCatalog,
      paginate: paginate,
      catalogRuleID: catalogRuleID,
      completeTierRates: completeTierRates,
      catalogEntryToRule: catalogEntryToRule,
      catalogImportNote: catalogImportNote,
      cloneValue: cloneValue,
      rateText: rateText,
      ruleRateLines: ruleRateLines,
      MAX_TIERS: MAX_TIERS,
      CATALOG_PAGE_SIZE: CATALOG_PAGE_SIZE,
      openRuleModal: openRuleModal,
      openCatalogModal: openCatalogModal,
    },
  };
})(typeof window !== 'undefined' ? window : this);
