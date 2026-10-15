"""Real-browser checks against the native resource shell.

Uses a locally cached Playwright Chromium in an isolated browser context (no
shared profile, no stored state). The page is loaded from the real host's public
resource route and driven through the in-memory management login. The management
key must never be persisted to localStorage, sessionStorage, cookies or the URL.
"""

from __future__ import annotations

import re
from datetime import datetime, timedelta
from pathlib import Path

from ..report import check, check_eq

CASES = []

def _resource(ctx):
    # The public resource base follows the plugin library id (--plugin-id).
    return f"/v0/resource/plugins/{ctx.plugin_id}/index.html"


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


def _login(page, ctx, started_at):
    response = page.goto(ctx.host.config.base_url + _resource(ctx), wait_until="domcontentloaded")
    check(response is not None and response.status == 200, f"resource shell status {response and response.status}")
    check("信用管理" in page.title(), f"page title {page.title()}")
    page.fill("#management-key", ctx.management_secret)
    page.click("#login-button")
    page.wait_for_selector("#main-view:not([hidden])", timeout=15000)
    frm, to = _local_window(started_at)
    page.fill("#usage-from", frm)
    page.fill("#usage-to", to)
    page.click("#usage-apply")


def _wait_rows(page):
    page.wait_for_function(
        "() => { var n = document.querySelector('#usage-items');"
        " return n && n.querySelectorAll('button').length > 0; }",
        timeout=15000,
    )


def _launch():
    from playwright.sync_api import sync_playwright

    return sync_playwright().start()


def _seed(ctx, label, body=None):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label=label)
    payload = body or {"model": "synthetic-chat", "messages": [{"role": "user", "content": "synthetic"}]}
    check_eq(ctx.client.post("/v1/chat/completions", payload, {"Authorization": f"Bearer {key}"}).status, 200,
             "seed request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "seed request recorded")
    ctx.wait_until(lambda: (ctx.find_request(key_id=item["id"]) or {}).get("state") == "settled", timeout=15)
    return item, key, request


@case("browser.login_and_usage_no_leak",
      "a real browser logs in with the in-memory key, renders usage and persists no credentials")
def login_and_usage(ctx):
    item, key, request = _seed(ctx, "browser-alpha")
    playwright = _launch()
    browser = playwright.chromium.launch(headless=True)
    console_errors = []
    try:
        context = browser.new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: console_errors.append(str(error)))
        _login(page, ctx, request["started_at"])
        _wait_rows(page)
        items_text = page.inner_text("#usage-items")
        check("synthetic-chat" in items_text, f"request model rendered: {items_text[:200]!r}")
        check("browser-alpha" in items_text, f"key label rendered: {items_text[:200]!r}")

        storage = page.evaluate(
            "() => ({ls: Object.keys(localStorage).length, ss: Object.keys(sessionStorage).length,"
            " cookie: document.cookie, url: location.href})"
        )
        check_eq(storage["ls"], 0, "no localStorage entries")
        check_eq(storage["ss"], 0, "no sessionStorage entries")
        check_eq(storage["cookie"], "", "no cookies set by the static shell")
        check(ctx.management_secret not in storage["url"], "management key not placed in the URL")
        check_eq(console_errors, [], "no uncaught page errors")
        context.close()
        return {"storage": storage, "rows_rendered": True}
    finally:
        browser.close()
        playwright.stop()


@case("browser.lazy_capture_body",
      "the request body is fetched lazily from the capture API only when opened and shown redacted")
def lazy_capture(ctx):
    root = Path(ctx.artifacts) / "host-browser-capture"
    ctx.recreate_host("browser-capture", root=root, plugin_config={
        "data_dir": str(root / "data"),
        "request_capture": {"enabled": True, "retention": "24h", "max_body_bytes": 1 << 20,
                            "max_storage_bytes": 1 << 30},
    })
    secret = "BROWSER-SECRET-VALUE"
    body = {"model": "synthetic-chat", "messages": [{"role": "user", "content": "synthetic"}],
            "api_key": secret, "big": 9007199254740993}
    item, key, request = _seed(ctx, "browser-body", body=body)

    playwright = _launch()
    browser = playwright.chromium.launch(headless=True)
    try:
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
    finally:
        browser.close()
        playwright.stop()
