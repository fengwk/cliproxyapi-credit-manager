/*
 * credit-manager 偏好设置与凭据持久化模块。
 *
 * 管理主题同步、计量与货币偏好以及跨会话/Tab 的同源凭据持久化。
 * 纯原生 JS，支持浏览器与 Node（用于测试）。
 */
(function (root, factory) {
  'use strict';
  var api = factory(root);
  if (typeof module !== 'undefined' && module && module.exports) module.exports = api;
  if (root && (typeof root === 'object' || typeof root === 'function')) root.CMPreferences = api;
})(typeof globalThis !== 'undefined' ? globalThis : (typeof self !== 'undefined' ? self : this), function (root) {
  'use strict';

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

  function readStoredTheme(storage) {
    var store = storage || (typeof localStorage !== 'undefined' ? localStorage : null);
    if (!store) return 'auto';
    try {
      var raw = parseStoredPreference(store.getItem(CPA_THEME_KEY));
      var candidate = String(raw || 'auto').toLowerCase();
      return THEMES.indexOf(candidate) >= 0 ? candidate : 'auto';
    } catch (_) {
      return 'auto';
    }
  }

  function resolvePalette(theme, systemIsDark) {
    var t = THEMES.indexOf(theme) >= 0 ? theme : 'auto';
    if (t === 'auto') return systemIsDark ? 'dark' : 'white';
    return t;
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

  function readStoredCredential(sessionStore, localStore, origin) {
    var currentOrigin = origin || (typeof location !== 'undefined' ? location.origin : '');
    if (sessionStore) {
      try {
        var sessOrigin = sessionStore.getItem(TOKEN_ORIGIN_KEY);
        if (!sessOrigin || sessOrigin === currentOrigin) {
          var sessToken = (sessionStore.getItem(TOKEN_KEY) || '').trim();
          if (sessToken) return { token: sessToken, remembered: false };
        }
      } catch (_) {}
    }
    if (localStore) {
      try {
        var locOrigin = localStore.getItem(TOKEN_ORIGIN_KEY);
        if (!locOrigin || locOrigin === currentOrigin) {
          var locToken = (localStore.getItem(TOKEN_KEY) || '').trim();
          if (locToken) return { token: locToken, remembered: true };
        }
      } catch (_) {}
    }
    return { token: '', remembered: false };
  }

  function persistCredential(sessionStore, localStore, origin, token, remember) {
    var trimmed = String(token || '').trim();
    var currentOrigin = origin || (typeof location !== 'undefined' ? location.origin : '');
    if (!trimmed) {
      clearStoredCredential(sessionStore, localStore);
      return;
    }
    if (sessionStore) {
      try {
        sessionStore.setItem(TOKEN_KEY, trimmed);
        sessionStore.setItem(TOKEN_ORIGIN_KEY, currentOrigin);
      } catch (_) {}
    }
    if (localStore) {
      try {
        if (remember) {
          localStore.setItem(TOKEN_KEY, trimmed);
          localStore.setItem(TOKEN_ORIGIN_KEY, currentOrigin);
        } else {
          localStore.removeItem(TOKEN_KEY);
          localStore.removeItem(TOKEN_ORIGIN_KEY);
        }
      } catch (_) {}
    }
  }

  function clearStoredCredential(sessionStore, localStore) {
    if (sessionStore) {
      try {
        sessionStore.removeItem(TOKEN_KEY);
        sessionStore.removeItem(TOKEN_ORIGIN_KEY);
      } catch (_) {}
    }
    if (localStore) {
      try {
        localStore.removeItem(TOKEN_KEY);
        localStore.removeItem(TOKEN_ORIGIN_KEY);
      } catch (_) {}
    }
  }

  function readUnitPreferences(storage) {
    var store = storage || (typeof localStorage !== 'undefined' ? localStorage : null);
    var tokenUnit = 'raw';
    var currency = 'USD';
    var rate = DEFAULT_USD_CNY_RATE;
    if (store) {
      try {
        var u = store.getItem(TOKEN_UNIT_KEY);
        if (u) tokenUnit = normalizeTokenUnit(u);
        var c = store.getItem(CURRENCY_KEY);
        if (c) currency = normalizeCurrency(c);
        var r = store.getItem(USD_CNY_RATE_KEY);
        if (r) rate = normalizeRate(r);
      } catch (_) {}
    }
    return { tokenUnit: tokenUnit, currency: currency, currencyRate: rate };
  }

  function persistUnitPreferences(storage, prefs) {
    var store = storage || (typeof localStorage !== 'undefined' ? localStorage : null);
    if (!store || !prefs) return;
    try {
      if (prefs.tokenUnit) store.setItem(TOKEN_UNIT_KEY, normalizeTokenUnit(prefs.tokenUnit));
      if (prefs.currency) store.setItem(CURRENCY_KEY, normalizeCurrency(prefs.currency));
      if (prefs.currencyRate !== undefined) store.setItem(USD_CNY_RATE_KEY, String(normalizeRate(prefs.currencyRate)));
    } catch (_) {}
  }

  return {
    TOKEN_KEY: TOKEN_KEY,
    TOKEN_ORIGIN_KEY: TOKEN_ORIGIN_KEY,
    TOKEN_UNIT_KEY: TOKEN_UNIT_KEY,
    CURRENCY_KEY: CURRENCY_KEY,
    USD_CNY_RATE_KEY: USD_CNY_RATE_KEY,
    CPA_THEME_KEY: CPA_THEME_KEY,
    THEMES: THEMES,
    TOKEN_UNITS: TOKEN_UNITS,
    CURRENCIES: CURRENCIES,
    DEFAULT_USD_CNY_RATE: DEFAULT_USD_CNY_RATE,
    parseStoredPreference: parseStoredPreference,
    readStoredTheme: readStoredTheme,
    resolvePalette: resolvePalette,
    normalizeTokenUnit: normalizeTokenUnit,
    normalizeCurrency: normalizeCurrency,
    normalizeRate: normalizeRate,
    readStoredCredential: readStoredCredential,
    persistCredential: persistCredential,
    clearStoredCredential: clearStoredCredential,
    readUnitPreferences: readUnitPreferences,
    persistUnitPreferences: persistUnitPreferences,
  };
});
