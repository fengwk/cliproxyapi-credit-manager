"""Public resource shell isolation.

The plugin's public resource base must only ever serve the embedded static shell
and can never reach the management/financial API without authentication.
"""

from __future__ import annotations

from ..report import check, check_eq

CASES = []

# The public resource base is derived by the host from the plugin library id,
# so it must follow --plugin-id rather than the plugin's fixed route name.
def _base(ctx):
    return f"/v0/resource/plugins/{ctx.plugin_id}"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


@case("resource.static_shell_only",
      "the public resource base serves only the embedded static shell, unauthenticated and leak-free")
def static_shell(ctx):
    base = _base(ctx)
    results = {}
    for asset in ("index.html", "app.js", "style.css"):
        status, raw = _get(ctx, f"{base}/{asset}")
        results[asset] = status
        check_eq(status, 200, f"static asset {asset} is publicly served")
        check(len(raw) > 0, f"static asset {asset} is non-empty")
        check(ctx.management_secret.encode() not in raw, f"management secret leaked into {asset}")
    # The shell must not embed any key material or the management secret.
    _, index = _get(ctx, f"{base}/index.html")
    text = index.decode(errors="replace")
    for marker in ("Bearer ", "secret-key", "api_key", ctx.management_secret):
        check(marker not in text, f"static shell must not embed {marker!r}")
    return results


@case("resource.cannot_reach_api",
      "resource paths cannot reach the financial API and unknown resources are not served")
def cannot_reach_api(ctx):
    base = _base(ctx)
    attempts = {
        "api-keys-under-resource": f"{base}/api/keys",
        "traversal": f"{base}/../plugins/credit-manager/api/keys",
        "unknown": f"{base}/does-not-exist",
    }
    statuses = {}
    for name, path in attempts.items():
        status, raw = _get(ctx, path)
        statuses[name] = status
        # A public resource request must never return financial data.
        check(status != 200 or b'"items"' not in raw,
              f"resource path {name} must not serve the financial API: {status} {raw[:120]!r}")
    # A management route reached without credentials is rejected.
    status, _ = _get(ctx, "/v0/management/plugins/credit-manager/api/keys")
    statuses["management-without-auth"] = status
    check_eq(status, 401, "management API still requires authentication")
    return statuses


def _get(ctx, path):
    response = ctx.client.get(path)
    return response.status, response.raw
