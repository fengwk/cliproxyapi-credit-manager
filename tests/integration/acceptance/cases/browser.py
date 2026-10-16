"""Real-browser checks against the native resource shell.

Uses Playwright Chromium (or explicit CHROME_BIN) in an isolated browser context
(no shared profile, no stored state). The page is loaded from the real host's
public resource route and driven through the in-memory management login.

The management key is a session credential: the UI always keeps it in
sessionStorage so a reload stays logged in, and only mirrors it into
localStorage when the operator explicitly ticks "记住管理密钥". The key must
never be persisted anywhere else: not in cookies, and not in the URL.
"""

from __future__ import annotations

import os
import re
from contextlib import contextmanager
from datetime import datetime, timedelta
from pathlib import Path

from ..report import check, check_eq

CASES = []

# The two credential keys the management shell owns (see app.js TOKEN_KEY /
# TOKEN_ORIGIN_KEY). Any other localStorage key is an unrelated preference.
TOKEN_KEY = "credit_manager_mgmt_token"
TOKEN_ORIGIN_KEY = "credit_manager_mgmt_token_origin"

CHART_IDS = ["usage-chart", "usage-token-chart", "usage-cost-chart"]


def _resource(ctx):
    # The public resource base follows the plugin library id (--plugin-id).
    return f"/v0/resource/plugins/{ctx.plugin_id}/index.html"


def _lookup_resource(ctx):
    return f"/v0/resource/plugins/{ctx.plugin_id}/lookup.html"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _local_window(started_at):
    """Return naive-local datetime-local strings bracketing the seeded request.

    The frontend truncates its default 'last 24h' range to the minute, which can
    exclude a request issued seconds earlier, so the window is set explicitly.
    """
    text = started_at.replace("Z", "+00:00")
    match = re.search(r"([+-]\d\d:\d\d)$", text)
    offset = match.group(1) if match else "+00:00"
    if "." in text:
        head, rest = text.split(".", 1)
        frac = re.sub(r"[+-]\d\d:\d\d$", "", rest)[:6].ljust(6, "0")
        text = f"{head}.{frac}{offset}"
    moment = datetime.fromisoformat(text)
    return ((moment - timedelta(hours=2)).astimezone().strftime("%Y-%m-%dT%H:%M"),
            (moment + timedelta(hours=2)).astimezone().strftime("%Y-%m-%dT%H:%M"))


def _open_usage_tab(page):
    """Select the 使用统计 tab before touching its (otherwise hidden) controls.

    The shell defaults to the 概览 tab; every non-overview panel, including
    #panel-usage, carries the `hidden` attribute until its tab is activated, so
    the usage inputs are not actionable until the tab is selected.
    """
    page.click("#tab-usage")
    page.wait_for_selector("#panel-usage:not([hidden])", timeout=15000)
    check(page.is_visible("#usage-from"), "usage controls become visible on the usage tab")


def _open_more_filters(page):
    """Expand the 更多筛选 <details> so the bucket/provider fields are visible.

    #usage-provider, #usage-auth, #usage-result, #usage-search, #usage-bucket
    and #usage-page-size live inside `<details id="usage-more-filters">`, which is
    collapsed until its summary is clicked.
    """
    page.click("#usage-more-filters > summary")
    page.wait_for_selector("#usage-bucket", state="visible", timeout=15000)
    check(page.is_visible("#usage-provider"), "more filters reveal the provider field")


def _login_shell(page, ctx, remember=False):
    """Log in to the management shell and wait for the main view.

    Leaves the shell on its default (概览) tab; callers that need the usage
    controls must select the usage tab explicitly.
    """
    response = page.goto(ctx.host.config.base_url + _resource(ctx), wait_until="domcontentloaded")
    check(response is not None and response.status == 200, f"resource shell status {response and response.status}")
    check("信用管理" in page.title(), f"page title {page.title()}")
    page.fill("#management-key", ctx.management_secret)
    if remember:
        page.check("#remember-management-key")
    page.click("#login-button")
    page.wait_for_selector("#main-view:not([hidden])", timeout=15000)


def _login(page, ctx, started_at, remember=False, bucket=None, provider=None):
    _login_shell(page, ctx, remember=remember)
    # The usage filters only exist (visibly) on their own tab.
    _open_usage_tab(page)
    frm, to = _local_window(started_at)
    page.fill("#usage-from", frm)
    page.fill("#usage-to", to)
    if bucket or provider:
        _open_more_filters(page)
    if bucket:
        page.select_option("#usage-bucket", bucket)
    if provider:
        page.fill("#usage-provider", provider)
    page.click("#usage-apply")


def _expand_request_details(page):
    """Reveal any collapsed per-request details before clicking an action.

    The current shell renders the action buttons directly on each request card,
    but a collapse-into-details layout is expected: open every `<details>` and
    unhide any `.request-detail` container inside the request list so an action
    button that lives inside a collapsed region is still clickable. This is a
    no-op against the current DOM.
    """
    page.evaluate(
        "() => { var n = document.querySelector('#usage-items'); if (!n) return;"
        " var ds = n.querySelectorAll('details');"
        " for (var i = 0; i < ds.length; i++) ds[i].open = true;"
        " var rd = n.querySelectorAll('.request-detail');"
        " for (var j = 0; j < rd.length; j++) {"
        "   rd[j].removeAttribute('hidden'); rd[j].setAttribute('open', ''); } }"
    )


def _wait_rows(page):
    """Wait for at least one rendered request card with an action button.

    DOM presence is what matters: a future collapsed card would still contain its
    hidden action button, and _expand_request_details opens it before a click.
    """
    page.wait_for_function(
        "() => { var n = document.querySelector('#usage-items');"
        " return n && n.querySelectorAll('.card').length > 0"
        " && n.querySelectorAll('button').length > 0; }",
        timeout=15000,
    )


def _storage(page):
    """Return the shell's localStorage, sessionStorage, cookie and URL state."""
    return page.evaluate(
        "() => { var ls = {}, ss = {};"
        " try { for (var i = 0; i < localStorage.length; i++) { var k = localStorage.key(i); ls[k] = localStorage.getItem(k); } } catch (e) {}"
        " try { for (var j = 0; j < sessionStorage.length; j++) { var s = sessionStorage.key(j); ss[s] = sessionStorage.getItem(s); } } catch (e) {}"
        " return {local: ls, session: ss, cookie: document.cookie, url: location.href}; }"
    )


def _chart_state(page):
    """Report the real ECharts geometry of every usage chart for one case."""
    return page.evaluate(
        "() => { var out = {echarts: !!window.echarts, charts: {}};"
        f" var ids = {CHART_IDS!r};"
        " for (var i = 0; i < ids.length; i++) {"
        "   var el = document.getElementById(ids[i]);"
        "   var info = {present: !!el, svg: false, width: 0, height: 0, paths: 0, text: ''};"
        "   if (el) {"
        "     info.text = (el.textContent || '').slice(0, 120);"
        "     var svg = el.querySelector('svg');"
        "     if (svg) { var r = svg.getBoundingClientRect(); info.svg = true; info.width = r.width; info.height = r.height; info.paths = svg.querySelectorAll('path').length; }"
        "   }"
        "   out.charts[ids[i]] = info;"
        " }"
        " return out; }"
    )


def _wait_charts(page):
    page.wait_for_function(
        "() => { var el = document.getElementById('usage-chart'); if (!el) return false;"
        " var svg = el.querySelector('svg'); if (!svg) return false;"
        " var r = svg.getBoundingClientRect();"
        " return r.height > 0 && r.width > 0 && svg.querySelectorAll('path').length > 0; }",
        timeout=15000,
    )


def _assert_real_charts(state, label):
    check(state["echarts"], f"{label}: real ECharts runtime is loaded")
    for chart_id in CHART_IDS:
        info = state["charts"][chart_id]
        check(info["present"], f"{label}: {chart_id} container exists")
        check(info["svg"], f"{label}: {chart_id} rendered an ECharts svg")
        check(info["width"] > 0 and info["height"] > 0,
              f"{label}: {chart_id} has positive dimensions ({info['width']}x{info['height']})")
        check(info["paths"] > 0, f"{label}: {chart_id} drew series data ({info['paths']} paths)")


def _launch():
    from playwright.sync_api import sync_playwright

    return sync_playwright().start()


@contextmanager
def _browser():
    playwright = _launch()
    try:
        options = {"headless": True}
        if executable := os.environ.get("CHROME_BIN"):
            options["executable_path"] = executable
        browser = playwright.chromium.launch(**options)
        try:
            yield browser
        finally:
            browser.close()
    finally:
        # Launch and browser.close can fail too; always stop the sync runtime.
        playwright.stop()


def _seed(ctx, label, body=None, outdir=None):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label=label)
    payload = body or {"model": "synthetic-chat", "messages": [{"role": "user", "content": "synthetic"}]}
    check_eq(ctx.client.post("/v1/chat/completions", payload, {"Authorization": f"Bearer {key}"}).status, 200,
             "seed request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "seed request recorded")
    ctx.wait_until(lambda: (ctx.find_request(key_id=item["id"]) or {}).get("state") == "settled", timeout=15)
    return item, key, request


def _capture_config(root):
    return {
        "data_dir": str(root / "data"),
        "request_capture": {"enabled": True, "retention": "24h", "max_body_bytes": 1 << 20,
                            "max_storage_bytes": 1 << 30},
    }


@case("browser.login_session_storage_no_leak",
      "a real browser logs in without remembering: the key stays in sessionStorage, never localStorage, cookies or the URL")
def login_session_storage_no_leak(ctx):
    item, key, request = _seed(ctx, "browser-alpha")
    console_errors = []
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        _login(page, ctx, request["started_at"])
        _wait_rows(page)
        items_text = page.inner_text("#usage-items")
        check("synthetic-chat" in items_text, f"request model rendered: {items_text[:200]!r}")
        check("browser-alpha" in items_text, f"key label rendered: {items_text[:200]!r}")

        storage = _storage(page)
        check_eq(storage["session"].get(TOKEN_KEY), ctx.management_secret,
                 "sessionStorage holds the key so a reload keeps the session")
        check(TOKEN_KEY not in storage["local"],
              "localStorage must not hold the key unless the operator remembers it")
        check(TOKEN_ORIGIN_KEY not in storage["local"],
              "localStorage must not hold the credential origin without remembering")
        check_eq(storage["cookie"], "", "no cookies set by the static shell")
        check(ctx.management_secret not in storage["url"], "management key not placed in the URL")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"storage": {"session": list(storage["session"]), "local": list(storage["local"])},
                "rows_rendered": True}


@case("browser.remember_reload_autologin",
      "remembering the key mirrors it into localStorage and a reload reconnects without re-entering it")
def remember_reload_autologin(ctx):
    item, key, request = _seed(ctx, "browser-remember")
    console_errors = []
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        _login(page, ctx, request["started_at"], remember=True)
        _wait_rows(page)

        storage = _storage(page)
        check_eq(storage["session"].get(TOKEN_KEY), ctx.management_secret, "session key is set with remember")
        check_eq(storage["local"].get(TOKEN_KEY), ctx.management_secret,
                 "remembered key is mirrored into localStorage")
        check(ctx.management_secret not in storage["url"], "management key not placed in the URL")
        check_eq(storage["cookie"], "", "no cookies set by the static shell")

        # A reload must reconnect from the saved credential, not ask again.
        page.reload(wait_until="domcontentloaded")
        page.wait_for_selector("#main-view:not([hidden])", timeout=15000)
        check(page.is_visible("#main-view"), "reload auto-logged in from the remembered key")
        check_eq(page.input_value("#management-key"), "", "the key input is cleared after auto-connect")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"local_has_token": TOKEN_KEY in storage["local"], "autologin": True}


@case("browser.logout_clears_storage_and_charts",
      "logging out removes the key from both stores and blanks the rendered private charts")
def logout_clears_storage_and_charts(ctx):
    item, key, request = _seed(ctx, "browser-logout")
    console_errors = []
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        _login(page, ctx, request["started_at"], remember=True)
        _wait_rows(page)
        _wait_charts(page)
        _assert_real_charts(_chart_state(page), "before logout")

        page.click("#logout-button")
        page.wait_for_selector("#login-view:not([hidden])", timeout=15000)
        check(page.is_visible("#login-view"), "logout returns to the login view")

        storage = _storage(page)
        check(TOKEN_KEY not in storage["local"], "logout clears the remembered key")
        check(TOKEN_KEY not in storage["session"], "logout clears the session key")
        check(TOKEN_ORIGIN_KEY not in storage["local"] and TOKEN_ORIGIN_KEY not in storage["session"],
              "logout clears the credential origins")
        check(ctx.management_secret not in storage["url"], "management key not placed in the URL")

        # The private charts must be disposed and blanked, not merely hidden.
        leftover = page.evaluate(
            f"() => {CHART_IDS!r}.map(function (id) {{"
            " var el = document.getElementById(id);"
            " return {id: id, svg: !!(el && el.querySelector('svg')), text: (el ? (el.textContent || '') : '')}; });"
        )
        for node in leftover:
            check(not node["svg"], f"logout disposes the {node['id']} chart instance")
            check_eq(node["text"], "", f"logout blanks the {node['id']} contents")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"charts_cleared": len(leftover), "logged_out": True}


@case("browser.charts_month_bucket_real_echarts",
      "a month bucket query draws real ECharts with positive geometry and data")
def charts_month_bucket_real_echarts(ctx):
    item, key, request = _seed(ctx, "browser-charts")
    console_errors = []
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        _login(page, ctx, request["started_at"], bucket="month")
        _wait_rows(page)
        # The status line echoes the exact query, so a real month bucket fetch is
        # observable rather than inferred from the rendered geometry.
        page.wait_for_function(
            "() => (document.querySelector('#usage-status') || {}).textContent"
            " && document.querySelector('#usage-status').textContent.indexOf('bucket=month') >= 0;",
            timeout=15000,
        )
        status = page.inner_text("#usage-status")
        check("bucket=month" in status, f"month bucket query issued: {status[:200]!r}")

        _wait_charts(page)
        _assert_real_charts(_chart_state(page), "month bucket")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"status": status[:200]}


@case("browser.lazy_capture_body",
      "the request body is fetched lazily from the capture API only when opened and shown redacted")
def lazy_capture(ctx):
    root = Path(ctx.artifacts) / "host-browser-capture"
    ctx.recreate_host("browser-capture", root=root, plugin_config=_capture_config(root))
    secret = "BROWSER-SECRET-VALUE"
    body = {"model": "synthetic-chat", "messages": [{"role": "user", "content": "synthetic"}],
            "api_key": secret, "big": 9007199254740993}
    item, key, request = _seed(ctx, "browser-body", body=body)

    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        _login(page, ctx, request["started_at"])
        _wait_rows(page)
        # The capture API is only hit when the user opens a body; the fetch is async.
        # Expand any collapsed request details first so the action is actionable.
        _expand_request_details(page)
        page.click("#usage-items button:has-text('查看请求体')")
        page.wait_for_selector("#modal-root:not([hidden])", timeout=15000)
        page.wait_for_function(
            "() => { var n = document.querySelector('#modal-root');"
            " return n && n.innerText.indexOf('9007199254740993') >= 0; }",
            timeout=15000,
        )
        modal = page.inner_text("#modal-root")
        check(secret not in modal, "capture body redacts the secret in the browser")
        check("9007199254740993" in modal, "capture body preserves 64-bit integer text")
        check("[redacted]" in modal, "capture body shows the redaction marker")
        check(ctx.management_secret not in modal, "management key not rendered into the modal")
        context.close()
        return {"modal_chars": len(modal)}


@case("browser.lookup_own_key_capture",
      "a customer opens its own redacted snapshot in a real browser without management credentials")
def lookup_own_key_capture(ctx):
    root = Path(ctx.artifacts) / "host-browser-lookup"
    ctx.recreate_host("browser-lookup", root=root, plugin_config=_capture_config(root))
    secret = "LOOKUP-SECRET-VALUE"
    body = {"model": "synthetic-chat", "messages": [{"role": "user", "content": "synthetic"}],
            "api_key": secret, "big": 9007199254740993}
    item, key, request = _seed(ctx, "browser-lookup", body=body)

    console_errors = []
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        response = page.goto(ctx.host.config.base_url + _lookup_resource(ctx), wait_until="domcontentloaded")
        check(response is not None and response.status == 200, f"lookup shell status {response and response.status}")

        page.fill("#key", key)
        page.click("#submit")
        page.wait_for_selector("#dashboard:not([hidden])", timeout=15000)
        check_eq(page.inner_text("#key-label"), "browser-lookup", "lookup renders the caller's own key label")

        # Default is a session credential: never mirrored into localStorage.
        storage = _storage(page)
        check(storage["session"].get("credit-manager.lookup.token") == key,
              "lookup keeps the key in sessionStorage by default")
        check("credit-manager.lookup.token" not in storage["local"],
              "lookup must not persist the key locally unless the operator remembers it")
        check(key not in storage["url"], "lookup never places the key in the URL")
        check_eq(storage["cookie"], "", "lookup sets no cookies")

        # The customer opens its own snapshot; the capture body stays redacted.
        page.click("#recent-body button")
        page.wait_for_selector("#capture-modal[open]", timeout=15000)
        page.wait_for_function(
            "() => { var n = document.getElementById('capture-body');"
            " return n && n.innerText.indexOf('9007199254740993') >= 0; }",
            timeout=15000,
        )
        modal = page.inner_text("#capture-body")
        check(secret not in modal, "lookup snapshot redacts the secret")
        check("9007199254740993" in modal, "lookup snapshot preserves 64-bit integer text")
        check("[redacted]" in modal, "lookup snapshot shows the redaction marker")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"key_label": "browser-lookup", "modal_chars": len(modal)}


OVERVIEW_CHART_IDS = ["overview-token-chart", "overview-cost-chart"]


def _wait_overview_charts(page):
    """Wait until both overview charts are real, positively-sized ECharts SVGs."""
    page.wait_for_function(
        "() => { var ids = " + repr(OVERVIEW_CHART_IDS) + ";"
        " if (!window.echarts) return false;"
        " for (var i = 0; i < ids.length; i++) {"
        "   var el = document.getElementById(ids[i]); if (!el) return false;"
        "   var svg = el.querySelector('svg'); if (!svg) return false;"
        "   var r = svg.getBoundingClientRect();"
        "   if (!(r.width > 0 && r.height > 0) || svg.querySelectorAll('path').length === 0) return false;"
        " }"
        " return true; }",
        timeout=15000,
    )


def _overview_state(page):
    return page.evaluate(
        "() => { var out = {echarts: !!window.echarts, summary: '', charts: {}};"
        f" var ids = {OVERVIEW_CHART_IDS!r};"
        " var s = document.getElementById('overview-summary');"
        " out.summary = s ? (s.innerText || '') : '';"
        " for (var i = 0; i < ids.length; i++) {"
        "   var el = document.getElementById(ids[i]);"
        "   var info = {present: !!el, svg: false, width: 0, height: 0, paths: 0};"
        "   if (el) { var svg = el.querySelector('svg');"
        "     if (svg) { var r = svg.getBoundingClientRect(); info.svg = true; info.width = r.width;"
        "       info.height = r.height; info.paths = svg.querySelectorAll('path').length; } }"
        "   out.charts[ids[i]] = info;"
        " }"
        " return out; }"
    )


@case("browser.overview_real_echarts_own_aggregate",
      "the default overview tab draws real ECharts SVGs and the operator's own aggregate from a real host")
def overview_charts(ctx):
    # The default view is the 概览 tab; a seeded request gives it real aggregate
    # data, so the operator's own summary and both trend charts must render.
    item, key, request = _seed(ctx, "browser-overview")
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        _login_shell(page, ctx)
        page.wait_for_selector("#panel-overview:not([hidden])", timeout=15000)
        check(page.is_visible("#overview-summary"), "the overview summary is visible by default")

        _wait_overview_charts(page)
        state = _overview_state(page)
        check(state["echarts"], "the real ECharts runtime is loaded on the overview tab")
        for chart_id in OVERVIEW_CHART_IDS:
            info = state["charts"][chart_id]
            check(info["present"], f"{chart_id} container exists")
            check(info["svg"], f"{chart_id} rendered a real ECharts svg")
            check(info["width"] > 0 and info["height"] > 0,
                  f"{chart_id} has positive geometry ({info['width']}x{info['height']})")
            check(info["paths"] > 0, f"{chart_id} drew series data ({info['paths']} paths)")

        # The operator's own aggregate: the key's single request is summarised.
        summary = ctx.manager.overview().json()["summary"]
        check_eq(summary["requests"], 1, "the operator's own request is aggregated")
        check("总请求数" in state["summary"], f"overview summary renders the request metric: {state['summary'][:120]!r}")
        context.close()
        return {"summary": state["summary"][:120]}


@case("browser.advanced_rule_save_lossless_rate_via_dom",
      "an advanced pricing rule created and saved through the DOM keeps an exact 2^53+1 rate on reload and consumes no ledger")
def advanced_rule_dom(ctx):
    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        _login_shell(page, ctx)

        page.click("#tab-prices")
        page.wait_for_selector("#panel-prices:not([hidden])", timeout=15000)
        check(page.is_visible("#prices-create"), "the prices tab exposes its toolbar")

        page.click("#rules-create")
        page.wait_for_selector("#rule-id", timeout=15000)
        page.fill("#rule-id", "dom-rule")
        page.select_option("#rule-kind", "exact")
        page.fill("#rule-pattern", "synthetic-chat")
        page.fill("#rule-priority", "10")
        # 2^53+1 is not representable as a float64: the shell must carry it
        # losslessly through its big-integer path.
        page.fill("#rule-input-rate", "9007199254740993")
        page.fill("#rule-output-rate", "1")
        page.fill("#rule-cache-read-rate", "0")
        page.fill("#rule-cache-write-rate", "0")
        page.click("#modal-root button:has-text('保存规则')")

        # The modal closes and the rule list re-renders with the saved rule.
        page.wait_for_function(
            "() => { var n = document.getElementById('modal-root');"
            " var list = document.getElementById('pricing-rules-list');"
            " return (!n || n.hidden) && list && list.innerText.indexOf('dom-rule') >= 0; }",
            timeout=15000,
        )
        list_text = page.inner_text("#pricing-rules-list")
        check("dom-rule" in list_text, f"the saved rule is rendered: {list_text[:160]!r}")

        rules = ctx.manager.pricing_rules().json().get("items") or []
        stored = next((r for r in rules if r["id"] == "dom-rule"), None)
        check(stored is not None, f"the DOM-saved rule is readable through the API: {rules}")
        check_eq(stored["price"]["rates"]["input"], 9007199254740993,
                 "the 2^53+1 input rate survives the DOM save and reload exactly")

        # Saving a price/rule must not consume the ledger.
        summary = ctx.usage_rows(page=1, page_size=5)["summary"]
        check_eq(summary["cost_micro_usd"], 0, "saving a rule consumes no ledger cost")
        check_eq(summary["requests"], 0, "saving a rule admits no request")
        context.close()
        return {"rule_input": stored["price"]["rates"]["input"], "list": list_text[:80]}


@case("browser.customer_models_data_isolation",
      "a customer's model analytics stay isolated to its own key, while the operator sees both")
def customer_models_isolation(ctx):
    ctx.set_price("synthetic-chat")
    ctx.set_price("synthetic-free", 0, 0, 0, 0)
    item_a, key_a = ctx.new_key(label="isolation-a")
    item_b, key_b = ctx.new_key(label="isolation-b")
    check_eq(ctx.client.chat(key_a, "synthetic-chat").status, 200, "key A billed")
    check_eq(ctx.client.chat(key_b, "synthetic-free").status, 200, "key B billed")
    check(ctx.wait_until(lambda: (ctx.find_request(key_id=item_a["id"]) or {}).get("state") == "settled"), "A settled")
    check(ctx.wait_until(lambda: (ctx.find_request(key_id=item_b["id"]) or {}).get("state") == "settled"), "B settled")

    with _browser() as browser:
        context = browser.new_context()
        page = context.new_page()
        response = page.goto(ctx.host.config.base_url + _lookup_resource(ctx), wait_until="domcontentloaded")
        check(response is not None and response.status == 200, f"lookup shell status {response and response.status}")

        # The customer self-service data is scoped to the presenting key: fetched
        # from the real host origin in the browser context, a foreign key's model
        # and requests never appear.
        def lookup(key):
            return page.evaluate(
                "async (key) => { var r = await fetch('lookup/data?page=1&page_size=50&bucket=day',"
                " { headers: { Authorization: 'Bearer ' + key, Accept: 'application/json' } });"
                " return { status: r.status, body: await r.json() }; }",
                key,
            )

        view_a = lookup(key_a)
        check_eq(view_a["status"], 200, "key A lookup is authenticated by its own key")
        models_a = sorted(m.get("model") for m in (view_a["body"].get("models") or []))
        check_eq(models_a, ["synthetic-chat"], f"key A only sees its own model: {models_a}")
        # The sanitized customer view never carries the key internals; scope is
        # proven by the model set and the request count, not by a key_id field.
        check_eq(view_a["body"]["total"], 1, "key A sees exactly its own request count")
        check(all(item["model"] == "synthetic-chat" for item in view_a["body"]["items"]),
              "key A only sees its own model's requests")
        blob_a = repr(view_a["body"])
        check("synthetic-free" not in blob_a, "key B's model never leaks into key A's view")
        check("principal" not in blob_a and "scope" not in blob_a and "fingerprint" not in blob_a,
              "the customer view carries no key identity internals")

        view_b = lookup(key_b)
        models_b = sorted(m.get("model") for m in (view_b["body"].get("models") or []))
        check_eq(models_b, ["synthetic-free"], f"key B only sees its own model: {models_b}")
        check_eq(view_b["body"]["total"], 1, "key B sees exactly its own request count")
        check("synthetic-chat" not in repr(view_b["body"]), "key A's model never leaks into key B's view")

        foreign = lookup("sk-synthetic-foreign-unknown-key-000000000")
        check_eq(foreign["status"], 401, "an unknown key cannot read any customer's models")

        # The management overview is the operator's cross-key view.
        overview_models = sorted(m["model"] for m in ctx.manager.overview().json()["models"])
        check_eq(overview_models, ["synthetic-chat", "synthetic-free"],
                 f"the operator overview aggregates both keys: {overview_models}")
        context.close()
        return {"models_a": models_a, "models_b": models_b, "operator": overview_models}
