"""Reference catalog and FX: management-authenticated, never a live public fetch.

The plugin's `/api/model-catalog` and `/api/exchange-rate` routes try to fetch
reference prices and a display-only FX quote from public HTTPS endpoints. The
acceptance host pins egress (HTTP(S)_PROXY) to a closed loopback port, so the
run must never reach the public internet: the routes must answer with the
plugin's safe "unavailable" envelope instead of fabricated live prices, and both
must stay behind management authentication. Neither route may touch the ledger.
"""

from __future__ import annotations

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _unavailable(body, label):
    check_eq(body.get("source"), "unavailable", f"{label}: egress is blocked so the source must be unavailable")
    check(body.get("error"), f"{label}: an unavailable envelope must carry its error")
    check_eq(body.get("cached"), False, f"{label}: nothing is fabricated from a cache")
    check(body.get("fetched_at"), f"{label}: the envelope is timestamped")


@case("reference.catalog_and_fx_require_auth_and_never_fetch_live_prices",
      "the reference catalog and FX routes are management-gated and, with egress blocked, answer unavailable rather than live data")
def reference_no_live_fetch(ctx):
    for suffix in ("/api/model-catalog", "/api/exchange-rate"):
        check_eq(ctx.manager.call("GET", suffix, with_auth=False).status, 401,
                 f"{suffix} without a credential")
        check_eq(ctx.manager.call("GET", suffix, headers={"Authorization": "Bearer wrong"}).status, 401,
                 f"{suffix} with a wrong credential")

    catalog = ctx.manager.model_catalog()
    check_eq(catalog.status, 502, f"catalog with no reachable upstream is a bad-gateway: {catalog.text[:200]}")
    catalog_body = catalog.json()
    check_eq(catalog_body.get("items"), [], "no reference model is fabricated")
    _unavailable(catalog_body, "catalog")

    fx = ctx.manager.exchange_rate()
    check_eq(fx.status, 502, f"FX with no reachable upstream is a bad-gateway: {fx.text[:200]}")
    fx_body = fx.json()
    check_eq(fx_body.get("base"), "USD", "FX base currency")
    check_eq(fx_body.get("quote"), "CNY", "FX quote currency")
    check("rate" not in fx_body, "no exchange rate is invented")
    _unavailable(fx_body, "fx")

    summary = ctx.usage_rows(page=1, page_size=5)["summary"]
    check_eq(summary["cost_micro_usd"], 0, "reference reads consume no ledger")
    check_eq(summary["executions"], 0, "reference reads execute nothing")
    return {"catalog": catalog_body.get("source"), "fx": fx_body.get("source")}
