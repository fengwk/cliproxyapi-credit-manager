/*
 * credit-manager 无损数字模块（供自助查询页与测试复用）。
 *
 * 目标：账务响应里的整数是 64 位微美元/Token 计数，JSON.parse 会把超过
 * 2^53 的整数悄悄四舍五入。本模块提供
 *   - parse(text)：与 JSON.parse 行为一致，但超出 Number 精度的整数保留为
 *     {cmIntText:'...'} 精确文本，交给共享图表模块 charts.js 做展示换算；
 *   - intText(value)：把 number/bigint/字符串/{cmIntText} 统一成十进制文本，
 *     优先复用 charts.js 的同一实现，保证与其它页面口径一致；
 *   - sumIntTexts(list)：用 BigInt 精确求和，绝不经过浮点；
 *   - isIntText / isSafeInt。
 *
 * 纯原生 JS，UMD：浏览器挂 window.CMNumbers，Node 走 module.exports。
 * 不依赖 app.js，也不引入任何第三方依赖。
 */
(function (root, factory) {
  'use strict';
  var api = factory(root);
  if (typeof module !== 'undefined' && module && module.exports) module.exports = api;
  if (root && (typeof root === 'object' || typeof root === 'function')) root.CMNumbers = api;
})(typeof globalThis !== 'undefined' ? globalThis : (typeof self !== 'undefined' ? self : this), function (root) {
  'use strict';

  var VERSION = '1.0.0';

  // 与 app.js 保持一致：JSON 里的十六位及以上整数可能是 64 位账务整数，
  // 无法用 double 精确表示，解析时替换成带标记的字符串。
  var BIG_INT_MARK = '\u0000CMINT:';
  var BIG_INT_TAIL = '\u0000';
  var BIG_INT_DIGITS = 16;

  function chartsApi() {
    var api = root && root.CMCharts;
    return api && typeof api.intText === 'function' ? api : null;
  }

  function isIntText(text) {
    return /^-?\d+$/.test(String(text == null ? '' : text).trim());
  }

  function isIntValue(value) {
    return !!value && typeof value === 'object' && typeof value.cmIntText === 'string';
  }

  // 精确整数文本：number/bigint/字符串/{cmIntText} 都转成十进制字符串，绝不用指数形式。
  function intText(value) {
    var shared = chartsApi();
    if (shared) return shared.intText(value);
    if (value === null || value === undefined) return '';
    if (typeof value === 'bigint') return value.toString();
    if (isIntValue(value)) return value.cmIntText;
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

  // 精确求和：全部走 BigInt，任何一个元素不是整数文本就跳过（绝不退化成浮点）。
  function sumIntTexts(values) {
    var total = BigInt(0);
    var list = values || [];
    for (var i = 0; i < list.length; i++) {
      var text = intText(list[i]);
      if (!isIntText(text)) continue;
      try {
        total += BigInt(text);
      } catch (err) {
        /* 非整数文本按 0 处理，避免整列显示失败 */
      }
    }
    return total.toString();
  }

  // 逐字符扫描，跳过字符串字面量，只替换真正的数字字面量。
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
        if (isIntText(digits)) return { cmIntText: digits };
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

  // 与 JSON.parse 行为一致，但超出 Number 精度的整数会保留为精确文本。
  function parse(text) {
    var source = String(text == null ? '' : text);
    var rewritten = rewriteNumbers(source, function (literal) {
      if (!isBigIntegerLiteral(literal)) return literal;
      return JSON.stringify(BIG_INT_MARK + literal + BIG_INT_TAIL);
    });
    return reviveBigInts(JSON.parse(rewritten));
  }

  return {
    VERSION: VERSION,
    BIG_INT_DIGITS: BIG_INT_DIGITS,
    isIntText: isIntText,
    isIntValue: isIntValue,
    intText: intText,
    sumIntTexts: sumIntTexts,
    rewriteNumbers: rewriteNumbers,
    isBigIntegerLiteral: isBigIntegerLiteral,
    parse: parse,
  };
});
