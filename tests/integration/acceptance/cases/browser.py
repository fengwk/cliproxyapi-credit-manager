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


def _login(page, ctx, started_at, remember=False, bucket=None):
    response = page.goto(ctx.host.config.base_url + _resource(ctx), wait_until="domcontentloaded")
    check(response is not None and response.status == 200, f"resource shell status {response and response.status}")
    check("信用管理" in page.title(), f"page title {page.title()}")
    page.fill("#management-key", ctx.management_secret)
    if remember:
        page.check("#remember-management-key")
    page.click("#login-button")
    page.wait_for_selector("#main-view:not([hidden])", timeout=15000)
    frm, to = _local_window(started_at)
    page.fill("#usage-from", frm)
    page.fill("#usage-to", to)
    if bucket:
        page.select_option("#usage-bucket", bucket)
    page.click("#usage-apply")


def _wait_rows(page):
    page.wait_for_function(
        "() => { var n = document.querySelector('#usage-items');"
        " return n && n.querySelectorAll('button').length > 0; }",
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
      "the customer self-service page reads its own key with a real browser and shows a redacted snapshot")
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
