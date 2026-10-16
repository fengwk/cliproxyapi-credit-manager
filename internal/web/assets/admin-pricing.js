(function (global) {
  'use strict';

  var ctx = null;
  var rules = [];

  function safeText(str) {
    if (str === null || str === undefined) return '';
    return String(str);
  }

  function intString(val) {
    if (val === null || val === undefined) return '0';
    if (typeof val === 'bigint') return val.toString();
    if (typeof val === 'object' && val.cmIntText) return val.cmIntText;
    return String(val);
  }

  function parseSigned64(str, label, errors) {
    var s = String(str == null ? '' : str).trim();
    if (s === '') return 0;
    if (!/^-?\d+$/.test(s)) {
      if (errors) errors.push((label || '数值') + ' 必须为合法的 64 位整数字符串');
      return 0;
    }
    if (typeof BigInt === 'function') {
      try {
        return BigInt(s);
      } catch (_) {}
    }
    return Number(s);
  }

  function formatRateDisplay(rate) {
    if (rate === null || rate === undefined) return '0';
    var s = intString(rate);
    if (global.CMNumbers && typeof global.CMNumbers.formatInt === 'function') {
      return global.CMNumbers.formatInt(s);
    }
    return s;
  }

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
          var errBox = ctx.el ? ctx.el('div', { class: 'error-box', text: '加载高级规则失败：' + (err.message || err) }) : document.createElement('div');
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
      var empty = document.createElement('p');
      empty.className = 'muted empty-hint';
      empty.textContent = '暂无高级定价规则。点击上方“新增高级规则”可创建通配符、正则、按图计费或上下文/服务等级阶梯规则。';
      listEl.appendChild(empty);
      return;
    }

    var table = document.createElement('table');
    table.className = 'table table-compact rules-table';
    var thead = document.createElement('thead');
    var headRow = document.createElement('tr');
    ['ID', '匹配模式', '类型', '优先级', '计费模式与基础费率', '阶梯规则', '状态', '操作'].forEach(function (h) {
      var th = document.createElement('th');
      th.textContent = h;
      headRow.appendChild(th);
    });
    thead.appendChild(headRow);
    table.appendChild(thead);

    var tbody = document.createElement('tbody');
    items.forEach(function (rule) {
      var tr = document.createElement('tr');
      var price = rule.price || {};
      var isPerImage = price.billing_mode === 'per_image';

      var rateSummary = [];
      if (isPerImage) {
        rateSummary.push('按图: ' + formatRateDisplay(price.per_image_micro_usd) + ' µUSD/张');
      }
      rateSummary.push('入/出: ' + formatRateDisplay(price.input_rate) + ' / ' + formatRateDisplay(price.output_rate));
      if (price.cache_read_rate || price.cache_write_rate) {
        rateSummary.push('缓存读/写: ' + formatRateDisplay(price.cache_read_rate) + ' / ' + formatRateDisplay(price.cache_write_rate));
      }
      if (price.reasoning_rate) {
        rateSummary.push('思考: ' + formatRateDisplay(price.reasoning_rate));
      }

      var tiersSummary = '无';
      if (rule.tiers && rule.tiers.length > 0) {
        var tierDesc = rule.tiers.map(function (t) {
          if (t.kind === 'service') return '服务: ' + (t.service || t.label || 'service');
          return '上下文: >' + formatRateDisplay(t.threshold);
        }).join(', ');
        tiersSummary = rule.tiers.length + ' 级 (' + tierDesc + ')';
      }

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
      tdRates.innerHTML = rateSummary.map(function (s) { return '<div>' + safeText(s) + '</div>'; }).join('');

      var tdTiers = document.createElement('td');
      tdTiers.textContent = tiersSummary;
      if (rule.tiers && rule.tiers.length > 0) {
        tdTiers.className = 'pill-note';
      }

      var tdStatus = document.createElement('td');
      tdStatus.textContent = rule.enabled ? '启用' : '禁用';
      tdStatus.className = rule.enabled ? 'badge badge-ok' : 'badge badge-warn';

      var tdActions = document.createElement('td');
      var editBtn = document.createElement('button');
      editBtn.type = 'button';
      editBtn.className = 'btn btn-small';
      editBtn.textContent = '编辑';
      editBtn.onclick = function () {
        openRuleModal(rule);
      };
      var delBtn = document.createElement('button');
      delBtn.type = 'button';
      delBtn.className = 'btn btn-small btn-danger';
      delBtn.textContent = '删除';
      delBtn.style.marginLeft = '4px';
      delBtn.onclick = function () {
        deleteRule(rule);
      };
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

  function openRuleModal(existing) {
    if (!ctx || !ctx.showModal) return;
    var isEdit = Boolean(existing);
    var rule = existing ? JSON.parse(JSON.stringify(existing)) : {
      id: 'rule-' + Date.now().toString(36),
      match_kind: 'glob',
      pattern: '',
      priority: 10,
      enabled: true,
      price: {
        model: '',
        billing_mode: 'token',
        input_rate: 0,
        output_rate: 0,
        cache_read_rate: 0,
        cache_write_rate: 0,
        reasoning_rate: 0,
        per_image_micro_usd: 0
      },
      tiers: []
    };

    var modal = ctx.showModal({
      title: isEdit ? '编辑高级定价规则 · ' + rule.id : '新增高级定价规则',
      wide: true
    });

    var form = document.createElement('form');
    form.className = 'rule-form form-grid';

    function addField(label, input, hint) {
      var wrap = document.createElement('div');
      wrap.className = 'field';
      var lbl = document.createElement('label');
      lbl.textContent = label;
      wrap.appendChild(lbl);
      wrap.appendChild(input);
      if (hint) {
        var h = document.createElement('div');
        h.className = 'field-hint';
        h.textContent = hint;
        wrap.appendChild(h);
      }
      form.appendChild(wrap);
      return wrap;
    }

    var inId = document.createElement('input');
    inId.type = 'text';
    inId.className = 'mono';
    inId.value = rule.id || '';
    inId.disabled = isEdit;
    inId.required = true;
    addField('规则 ID（全局唯一）', inId);

    var selKind = document.createElement('select');
    ['glob', 'exact', 'regexp'].forEach(function (k) {
      var opt = document.createElement('option');
      opt.value = k;
      opt.textContent = k === 'glob' ? '通配符 (glob, 如 gpt-4o-*)' : (k === 'exact' ? '精确匹配 (exact)' : '正则表达式 (regexp)');
      if (rule.match_kind === k) opt.selected = true;
      selKind.appendChild(opt);
    });
    addField('匹配模式类型', selKind);

    var inPattern = document.createElement('input');
    inPattern.type = 'text';
    inPattern.className = 'mono';
    inPattern.value = rule.pattern || '';
    inPattern.placeholder = '例如: gpt-4o-* 或 claude-3-5-*';
    inPattern.required = true;
    addField('匹配表达式 (Pattern)', inPattern);

    var inPriority = document.createElement('input');
    inPriority.type = 'text';
    inPriority.className = 'mono';
    inPriority.value = intString(rule.priority !== undefined ? rule.priority : 10);
    addField('规则优先级 (64 位整数，数值越大越先匹配)', inPriority);

    var selBillingMode = document.createElement('select');
    [
      { id: 'token', name: '按 Token 计费 (token)' },
      { id: 'per_image', name: '按图片计费 (per_image)' }
    ].forEach(function (m) {
      var opt = document.createElement('option');
      opt.value = m.id;
      opt.textContent = m.name;
      if (rule.price && (rule.price.billing_mode === m.id || (!rule.price.billing_mode && m.id === 'token'))) opt.selected = true;
      selBillingMode.appendChild(opt);
    });
    addField('计费模式', selBillingMode);

    var inPerImage = document.createElement('input');
    inPerImage.type = 'text';
    inPerImage.className = 'mono';
    inPerImage.value = intString(rule.price && rule.price.per_image_micro_usd !== undefined ? rule.price.per_image_micro_usd : 0);
    var perImageWrap = addField('单张图片费率 (微USD/张，按图模式生效)', inPerImage);

    var inInputRate = document.createElement('input');
    inInputRate.type = 'text';
    inInputRate.className = 'mono';
    inInputRate.value = intString(rule.price && rule.price.input_rate !== undefined ? rule.price.input_rate : 0);
    addField('基础输入费率 (微USD / 1M Tokens)', inInputRate);

    var inOutputRate = document.createElement('input');
    inOutputRate.type = 'text';
    inOutputRate.className = 'mono';
    inOutputRate.value = intString(rule.price && rule.price.output_rate !== undefined ? rule.price.output_rate : 0);
    addField('基础输出费率 (微USD / 1M Tokens)', inOutputRate);

    var inCacheRead = document.createElement('input');
    inCacheRead.type = 'text';
    inCacheRead.className = 'mono';
    inCacheRead.value = intString(rule.price && rule.price.cache_read_rate !== undefined ? rule.price.cache_read_rate : 0);
    addField('缓存读取命中费率 (微USD / 1M Tokens)', inCacheRead);

    var inCacheWrite = document.createElement('input');
    inCacheWrite.type = 'text';
    inCacheWrite.className = 'mono';
    inCacheWrite.value = intString(rule.price && rule.price.cache_write_rate !== undefined ? rule.price.cache_write_rate : 0);
    addField('缓存写入创建费率 (微USD / 1M Tokens)', inCacheWrite);

    var inReasoningRate = document.createElement('input');
    inReasoningRate.type = 'text';
    inReasoningRate.className = 'mono';
    inReasoningRate.value = intString(rule.price && rule.price.reasoning_rate !== undefined ? rule.price.reasoning_rate : 0);
    addField('思考 Token 费率 (微USD / 1M Tokens)', inReasoningRate);

    var chkEnabled = document.createElement('input');
    chkEnabled.type = 'checkbox';
    chkEnabled.checked = rule.enabled !== false;
    var chkLabel = document.createElement('label');
    chkLabel.className = 'checkbox-label';
    chkLabel.appendChild(chkEnabled);
    chkLabel.appendChild(document.createTextNode(' 启用该规则（禁用后命中时直接拒绝计费，绝不伪造零费率）'));
    form.appendChild(chkLabel);

    // 阶梯编辑器 (Structured Service & Context Tiers)
    var tiersSection = document.createElement('div');
    tiersSection.className = 'tiers-editor-section';
    tiersSection.style.gridColumn = '1 / -1';
    tiersSection.style.marginTop = '1rem';
    tiersSection.style.borderTop = '1px solid var(--border-color, #e5e7eb)';
    tiersSection.style.paddingTop = '1rem';

    var tiersHeader = document.createElement('div');
    tiersHeader.style.display = 'flex';
    tiersHeader.style.justifyContent = 'space-between';
    tiersHeader.style.alignItems = 'center';
    var thTitle = document.createElement('h4');
    thTitle.textContent = '阶梯定价规则 (Context & Service Tiers)';
    thTitle.style.margin = '0';
    var btnAddTier = document.createElement('button');
    btnAddTier.type = 'button';
    btnAddTier.className = 'btn btn-small';
    btnAddTier.textContent = '+ 添加阶梯';

    tiersHeader.appendChild(thTitle);
    tiersHeader.appendChild(btnAddTier);
    tiersSection.appendChild(tiersHeader);

    var tiersList = document.createElement('div');
    tiersList.className = 'tiers-list';
    tiersList.style.marginTop = '0.5rem';
    tiersSection.appendChild(tiersList);

    var workingTiers = Array.isArray(rule.tiers) ? rule.tiers.slice() : [];

    function renderWorkingTiers() {
      tiersList.textContent = '';
      if (workingTiers.length === 0) {
        var emptyT = document.createElement('p');
        emptyT.className = 'muted';
        emptyT.textContent = '暂无阶梯规则（请求将按上述基础费率计费）。点击“+ 添加阶梯”配置长上下文或按服务等级覆盖。';
        tiersList.appendChild(emptyT);
        return;
      }

      workingTiers.forEach(function (tier, idx) {
        var item = document.createElement('div');
        item.className = 'tier-item-card';
        item.style.padding = '0.75rem';
        item.style.marginBottom = '0.5rem';
        item.style.border = '1px solid var(--border-color, #e5e7eb)';
        item.style.borderRadius = '6px';
        item.style.background = 'var(--panel-bg, #f9fafb)';

        var rowTop = document.createElement('div');
        rowTop.style.display = 'flex';
        rowTop.style.gap = '0.75rem';
        rowTop.style.alignItems = 'center';

        var selTierKind = document.createElement('select');
        selTierKind.className = 'tier-kind';
        [
          { id: 'context', name: '上下文阶梯 (Context)' },
          { id: 'service', name: '服务等级阶梯 (Service)' }
        ].forEach(function (k) {
          var o = document.createElement('option');
          o.value = k.id;
          o.textContent = k.name;
          if (tier.kind === k.id) o.selected = true;
          selTierKind.appendChild(o);
        });

        var inThreshold = document.createElement('input');
        inThreshold.type = 'text';
        inThreshold.className = 'mono';
        inThreshold.placeholder = '门槛 Token 数 (如 128000)';
        inThreshold.value = intString(tier.threshold || 0);
        inThreshold.style.display = tier.kind === 'service' ? 'none' : 'inline-block';

        var inService = document.createElement('input');
        inService.type = 'text';
        inService.className = 'mono';
        inService.placeholder = '服务标识 (如 priority, fast)';
        inService.value = tier.service || '';
        inService.style.display = tier.kind === 'service' ? 'inline-block' : 'none';

        selTierKind.onchange = function () {
          tier.kind = selTierKind.value;
          inThreshold.style.display = tier.kind === 'service' ? 'none' : 'inline-block';
          inService.style.display = tier.kind === 'service' ? 'inline-block' : 'none';
        };

        var inLabel = document.createElement('input');
        inLabel.type = 'text';
        inLabel.placeholder = '阶梯标签描述 (可选)';
        inLabel.value = tier.label || '';

        var btnRemoveTier = document.createElement('button');
        btnRemoveTier.type = 'button';
        btnRemoveTier.className = 'btn btn-small btn-danger';
        btnRemoveTier.textContent = '移除';
        btnRemoveTier.onclick = function () {
          workingTiers.splice(idx, 1);
          renderWorkingTiers();
        };

        rowTop.appendChild(selTierKind);
        rowTop.appendChild(inThreshold);
        rowTop.appendChild(inService);
        rowTop.appendChild(inLabel);
        rowTop.appendChild(btnRemoveTier);
        item.appendChild(rowTop);

        // Tier rates
        var ratesGrid = document.createElement('div');
        ratesGrid.style.display = 'grid';
        ratesGrid.style.gridTemplateColumns = 'repeat(auto-fit, minmax(130px, 1fr))';
        ratesGrid.style.gap = '0.5rem';
        ratesGrid.style.marginTop = '0.5rem';

        var r = tier.rates || {};
        var tierInRate = document.createElement('input');
        tierInRate.type = 'text';
        tierInRate.className = 'mono';
        tierInRate.placeholder = '阶梯输入费率';
        tierInRate.value = intString(r.input || 0);

        var tierOutRate = document.createElement('input');
        tierOutRate.type = 'text';
        tierOutRate.className = 'mono';
        tierOutRate.placeholder = '阶梯输出费率';
        tierOutRate.value = intString(r.output || 0);

        var tierCacheRead = document.createElement('input');
        tierCacheRead.type = 'text';
        tierCacheRead.className = 'mono';
        tierCacheRead.placeholder = '阶梯缓存读';
        tierCacheRead.value = intString(r.cache_read || 0);

        var tierReasoning = document.createElement('input');
        tierReasoning.type = 'text';
        tierReasoning.className = 'mono';
        tierReasoning.placeholder = '阶梯思考费率';
        tierReasoning.value = intString(r.reasoning || 0);

        ratesGrid.appendChild(tierInRate);
        ratesGrid.appendChild(tierOutRate);
        ratesGrid.appendChild(tierCacheRead);
        ratesGrid.appendChild(tierReasoning);
        item.appendChild(ratesGrid);

        item.dataset.idx = String(idx);
        item._getTierData = function (errors) {
          return {
            kind: selTierKind.value,
            threshold: selTierKind.value === 'context' ? parseSigned64(inThreshold.value, '阶梯门槛 Tokens', errors) : 0,
            service: selTierKind.value === 'service' ? inService.value.trim() : '',
            label: inLabel.value.trim(),
            rates: {
              input: parseSigned64(tierInRate.value, '阶梯输入费率', errors),
              output: parseSigned64(tierOutRate.value, '阶梯输出费率', errors),
              cache_read: parseSigned64(tierCacheRead.value, '阶梯缓存读费率', errors),
              cache_write: 0,
              reasoning: parseSigned64(tierReasoning.value, '阶梯思考费率', errors)
            }
          };
        };

        tiersList.appendChild(item);
      });
    }

    btnAddTier.onclick = function () {
      workingTiers.push({
        kind: 'context',
        threshold: 128000,
        service: '',
        label: '长上下文阶梯',
        rates: {
          input: 0,
          output: 0,
          cache_read: 0,
          cache_write: 0,
          reasoning: 0
        }
      });
      renderWorkingTiers();
    };

    renderWorkingTiers();
    form.appendChild(tiersSection);

    modal.setBody([form]);

    var btnSave = document.createElement('button');
    btnSave.type = 'button';
    btnSave.className = 'btn btn-primary';
    btnSave.textContent = '保存规则';
    btnSave.onclick = function () {
      var errors = [];
      var collectedTiers = [];
      var tierCards = tiersList.querySelectorAll('.tier-item-card');
      for (var t = 0; t < tierCards.length; t++) {
        if (typeof tierCards[t]._getTierData === 'function') {
          collectedTiers.push(tierCards[t]._getTierData(errors));
        }
      }

      var bMode = selBillingMode.value === 'per_image' ? 'per_image' : 'token';
      var payload = {
        id: inId.value.trim(),
        match_kind: selKind.value,
        pattern: inPattern.value.trim(),
        priority: parseSigned64(inPriority.value, '优先级', errors),
        enabled: chkEnabled.checked,
        price: {
          model: inPattern.value.trim(),
          billing_mode: bMode,
          input_rate: parseSigned64(inInputRate.value, '基础输入费率', errors),
          output_rate: parseSigned64(inOutputRate.value, '基础输出费率', errors),
          cache_read_rate: parseSigned64(inCacheRead.value, '缓存读取费率', errors),
          cache_write_rate: parseSigned64(inCacheWrite.value, '缓存写入费率', errors),
          reasoning_rate: parseSigned64(inReasoningRate.value, '思考费率', errors),
          per_image_micro_usd: bMode === 'per_image' ? parseSigned64(inPerImage.value, '按图费率', errors) : 0
        },
        tiers: collectedTiers
      };

      if (!payload.id || !payload.pattern) {
        modal.setError('规则 ID 与匹配模式不能为空');
        return;
      }
      if (errors.length > 0) {
        modal.setError(errors.join('；'));
        return;
      }

      btnSave.disabled = true;
      modal.setError('');
      ctx.api('/api/pricing-rules', { method: 'PUT', body: payload })
        .then(function () {
          if (ctx.toast) ctx.toast('高级定价规则已成功保存');
          modal.close();
          load();
        })
        .catch(function (err) {
          btnSave.disabled = false;
          modal.setError('保存失败：' + (err.message || err));
        });
    };

    var btnCancel = document.createElement('button');
    btnCancel.type = 'button';
    btnCancel.className = 'btn btn-ghost';
    btnCancel.textContent = '取消';
    btnCancel.onclick = modal.close;

    modal.setFooter([btnSave, btnCancel]);
  }

  function deleteRule(rule) {
    if (!ctx || !ctx.api) return;
    if (!confirm('确定删除高级定价规则 ' + rule.id + ' (' + rule.pattern + ') 吗？\n删除后该规则将不再参与任何请求计费匹配。')) return;
    ctx.api('/api/delete-price-rule', { method: 'POST', body: { id: rule.id } })
      .then(function () {
        if (ctx.toast) ctx.toast('规则已删除');
        load();
      })
      .catch(function (err) {
        alert('删除失败：' + (err.message || err));
      });
  }

  function openCatalogModal() {
    if (!ctx || !ctx.showModal) return;
    var modal = ctx.showModal({
      title: '模型价格目录 (Model Catalog)',
      wide: true
    });

    var loadingP = document.createElement('p');
    loadingP.className = 'muted';
    loadingP.textContent = '正在获取官方最新模型价格目录...';
    modal.setBody([loadingP]);

    ctx.api('/api/model-catalog?refresh=1')
      .then(function (res) {
        var catalog = (res && (res.models || res.items)) || [];
        if (catalog.length === 0) {
          var p = document.createElement('p');
          p.className = 'muted';
          p.textContent = '未找到可用模型价格数据。';
          modal.setBody([p]);
          return;
        }

        var container = document.createElement('div');
        container.className = 'catalog-preview';

        var hint = document.createElement('p');
        hint.className = 'hint';
        hint.textContent = '以下为最新官方参考价格，选择模型可一键导入为系统的精确价格规则：';
        container.appendChild(hint);

        var table = document.createElement('table');
        table.className = 'table table-compact catalog-table';
        var thead = document.createElement('thead');
        var trh = document.createElement('tr');
        ['模型名称', 'Provider', '输入费率', '输出费率', '操作'].forEach(function (h) {
          var th = document.createElement('th');
          th.textContent = h;
          trh.appendChild(th);
        });
        thead.appendChild(trh);
        table.appendChild(thead);

        var tbody = document.createElement('tbody');
        catalog.forEach(function (m) {
          var tr = document.createElement('tr');
          var tdModel = document.createElement('td');
          tdModel.textContent = m.model || m.id;
          var tdProvider = document.createElement('td');
          tdProvider.textContent = m.provider || 'default';
          var tdIn = document.createElement('td');
          tdIn.textContent = formatRateDisplay(m.input_rate) + ' 微USD/M';
          var tdOut = document.createElement('td');
          tdOut.textContent = formatRateDisplay(m.output_rate) + ' 微USD/M';

          var tdAct = document.createElement('td');
          var btnApply = document.createElement('button');
          btnApply.type = 'button';
          btnApply.className = 'btn btn-small btn-primary';
          btnApply.textContent = '导入此价格';
          btnApply.onclick = function () {
            var bMode = m.billing_mode === 'per_image' ? 'per_image' : 'token';
            var inRate = m.input_rate;
            var outRate = m.output_rate;
            if (bMode === 'token') {
              if (inRate === undefined || inRate === null || outRate === undefined || outRate === null) {
                var manual = prompt(
                  '模型 ' + (m.model || m.id) + ' 在官方目录中缺少完整费率（缺失输入或输出费率）。\n请输入微美元费率（格式: 输入,输出；如 2000000,6000000），或输入 free 标记为显式免费：',
                  'free'
                );
                if (!manual) return;
                if (manual.trim().toLowerCase() === 'free') {
                  inRate = 0;
                  outRate = 0;
                } else {
                  var parts = manual.split(',');
                  if (parts.length >= 2 && /^-?\d+$/.test(parts[0].trim()) && /^-?\d+$/.test(parts[1].trim())) {
                    inRate = parseSigned64(parts[0].trim());
                    outRate = parseSigned64(parts[1].trim());
                  } else {
                    alert('费率输入格式无效，导入已取消。');
                    return;
                  }
                }
              }
            }
            btnApply.disabled = true;
            var priceBody = {
              model: m.model || m.id,
              billing_mode: bMode,
              input_rate: inRate !== undefined && inRate !== null ? inRate : 0,
              output_rate: outRate !== undefined && outRate !== null ? outRate : 0,
              cache_read_rate: m.cache_read_rate !== undefined && m.cache_read_rate !== null ? m.cache_read_rate : 0,
              cache_write_rate: m.cache_write_rate !== undefined && m.cache_write_rate !== null ? m.cache_write_rate : 0,
              reasoning_rate: m.reasoning_rate !== undefined && m.reasoning_rate !== null ? m.reasoning_rate : 0,
              per_image_micro_usd: bMode === 'per_image' ? (m.per_image_micro_usd || 0) : 0
            };
            ctx.api('/api/prices', { method: 'PUT', body: priceBody })
              .then(function () {
                if (ctx.toast) ctx.toast('已成功导入 ' + priceBody.model + ' 价格');
                if (ctx.onExactApplied) ctx.onExactApplied();
                btnApply.textContent = '已导入';
              })
              .catch(function (e) {
                btnApply.disabled = false;
                alert('导入失败：' + (e.message || e));
              });
          };
          tdAct.appendChild(btnApply);

          tr.appendChild(tdModel);
          tr.appendChild(tdProvider);
          tr.appendChild(tdIn);
          tr.appendChild(tdOut);
          tr.appendChild(tdAct);
          tbody.appendChild(tr);
        });
        table.appendChild(tbody);
        container.appendChild(table);

        modal.setBody([container]);
      })
      .catch(function (err) {
        modal.setError('获取价格库失败：' + (err.message || err));
      });

    var btnClose = document.createElement('button');
    btnClose.type = 'button';
    btnClose.className = 'btn btn-ghost';
    btnClose.textContent = '关闭';
    btnClose.onclick = modal.close;
    modal.setFooter([btnClose]);
  }

  global.CMAdminPricing = {
    init: init,
    load: load,
    dispose: dispose
  };
})(typeof window !== 'undefined' ? window : this);
