"""Management surface: every declared route is behind management authentication.

The plugin declares its management routes to the host (internal/plugin/management.go);
the host authenticates the whole `/v0/management/plugins/...` prefix with the
management secret. This module pins that contract for the full aligned surface,
including the routes added with the aligned features (callers, audit, overview,
pricing rules, reference catalog, FX).
"""

from __future__ import annotations

from ..report import check, check_eq
from ..api import MANAGER_BASE

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


#: suffix -> methods, mirrored from internal/plugin/management.go. The read
#: routes are also asserted to answer with the correct credential.
MANAGEMENT_ROUTES = {
    "/api/keys": ("GET", "POST", "PUT"),
    "/api/rotate": ("POST",),
    "/api/reveal": ("POST",),
    "/api/delete-key": ("POST",),
    "/api/reset-spend": ("POST",),
    "/api/callers": ("GET", "POST", "PUT"),
    "/api/audit": ("GET",),
    "/api/overview": ("GET",),
    "/api/model-catalog": ("GET",),
    "/api/exchange-rate": ("GET",),
    "/api/prices": ("GET", "PUT"),
    "/api/pricing-rules": ("GET", "PUT"),
    "/api/delete-price-rule": ("POST",),
    "/api/price-preview": ("GET",),
    "/api/usage": ("GET",),
    "/api/capture": ("GET",),
    "/api/resolve": ("POST",),
    "/api/health": ("GET",),
}

#: Read routes that must answer 200 with the management secret. The reference
#: catalog and FX are deliberately excluded: they are checked in the `reference`
#: module (their upstream is blocked so they answer 502, never live data).
READ_ROUTES = ["/api/keys", "/api/callers", "/api/audit", "/api/overview",
               "/api/prices", "/api/pricing-rules", "/api/usage", "/api/health"]

SAMPLE_QUERY = {"/api/capture": {"id": "synthetic-unknown"}, "/api/price-preview": {"model": "synthetic-chat"}}


def _call(ctx, method, suffix, **kwargs):
    return ctx.manager.call(method, suffix, **kwargs)


def _probe(ctx, method, suffix, credential):
    """One credential-gate probe followed by a resetting authenticated read.

    The host bans a client IP for 30 minutes after 5 consecutive failed
    management attempts (maxFailures=5), and a *successful* request resets that
    counter. A valid GET between probes keeps the matrix from tripping the ban,
    so every route is measured by the gate itself rather than by the ban.
    """
    if credential == "missing":
        response = _call(ctx, method, suffix, with_auth=False)
    else:
        response = _call(ctx, method, suffix, headers={"Authorization": "Bearer wrong-management-secret"})
    check(response.status in (401, 403),
          f"{method} {suffix} with a {credential} credential answered {response.status}")
    check("\"items\"" not in response.text and "\"status\"" not in response.text,
          f"{credential} credential on {suffix} must not leak data: {response.text[:120]}")
    reset = _call(ctx, "GET", "/api/health")
    check_eq(reset.status, 200, "an authenticated read resets the host failure counter")
    return response.status


@case("management.all_routes_reject_missing_and_wrong_credentials",
      "every declared management route denies both a missing and a wrong credential without leaking data")
def all_routes_require_auth(ctx):
    seen = 0
    for suffix, methods in MANAGEMENT_ROUTES.items():
        for method in methods:
            _probe(ctx, method, suffix, "missing")
            _probe(ctx, method, suffix, "wrong")
            seen += 2
    check(seen >= 30, f"route matrix covered ({seen} probes)")
    return {"probes": seen, "routes": len(MANAGEMENT_ROUTES)}


@case("management.authenticated_read_routes_succeed",
      "the read routes answer with the management secret rather than the auth gate")
def authenticated_reads(ctx):
    statuses = {}
    for suffix in READ_ROUTES:
        query = SAMPLE_QUERY.get(suffix)
        response = _call(ctx, "GET", suffix, query=query)
        check(response.status == 200, f"{suffix} with the management secret answered {response.status}: {response.text[:160]}")
        statuses[suffix] = response.status
    return {"statuses": statuses, "prefix": MANAGER_BASE}
