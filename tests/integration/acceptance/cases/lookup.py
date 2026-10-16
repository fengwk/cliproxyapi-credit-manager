"""Customer self-service lookup: unauthenticated transport, own-key scope, no cross-key oracle.

The two public routes under the plugin resource base (`lookup/data`,
`lookup/capture`) are reached without host management authentication, so they
must authenticate the customer's own ledger key themselves. These cases prove
the boundary the frontend relies on: an explicit Bearer customer key is the only
accepted credential, a foreign or unknown snapshot is indistinguishable from a
missing one, and the reported trend uses true UTC calendar months.
"""

from __future__ import annotations

import datetime

from ..report import check, check_eq

CASES = []

# 2**53 + 1: a 64-bit counter that a float64 cannot represent exactly.
BIG = 9007199254740993


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _base(ctx):
    return f"/v0/resource/plugins/{ctx.plugin_id}"


def _get(ctx, path, key=None, headers=None):
    merged = dict(headers or {})
    if key is not None:
        merged["Authorization"] = f"Bearer {key}"
    return ctx.client.get(path, headers=merged or None)


def _seed(ctx, label):
    """Create one enabled key with a priced model and one settled request."""
    item, key = ctx.new_key(label=label, quota_micro_usd=BIG)
    ctx.set_price("synthetic-chat")
    response = ctx.client.chat(key, "synthetic-chat")
    check_eq(response.status, 200, f"{label}: priced request accepted through the proxy")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, f"{label}: request recorded for its own key")
    return item, key, request


@case("lookup.customer_bearer_required",
      "lookup/data accepts only an explicit Bearer customer key; management and forged credentials are 401")
def customer_bearer_required(ctx):
    path = f"{_base(ctx)}/lookup/data"
    item, key, _ = _seed(ctx, "lookup-auth")

    statuses = {}
    for name, kwargs in (
        ("no-credential", {}),
        ("management-secret", {"headers": {"Authorization": f"Bearer {ctx.management_secret}"}}),
        ("wrong-scheme", {"headers": {"Authorization": f"Basic {key}"}}),
        ("bare-token", {"headers": {"Authorization": key}}),
        ("unknown-key", {"key": "synthetic-unknown-lookup-key"}),
    ):
        response = _get(ctx, path, **kwargs)
        statuses[name] = response.status
        check_eq(response.status, 401, f"lookup/data rejects {name}")
        check(key.encode() not in response.raw, f"lookup/data must not echo the key for {name}")

    response = _get(ctx, path, key=key)
    statuses["own-key"] = response.status
    check_eq(response.status, 200, "lookup/data accepts the customer's own key")
    payload = response.json()
    check_eq(payload["key"]["id"], item["id"], "lookup/data reports the authenticated key only")
    check_eq(payload["key"]["quota_micro_usd"], BIG, "64-bit quota survives JSON without precision loss")
    for forbidden in ("principal", "scope", "fingerprint", "hmac", "material"):
        check(forbidden not in response.text, f"lookup/data must not expose {forbidden}")

    # Non-GET verbs never reach the read path: the host refuses unknown
    # resource methods, so a write can never be treated as a read.
    posted = ctx.client.post(path, {"probe": True}, headers={"Authorization": f"Bearer {key}"})
    statuses["post"] = posted.status
    check(posted.status in {404, 405}, f"lookup/data refuses non-GET: {posted.status}")
    check(b'"items"' not in posted.raw, "a non-GET lookup request must not return financial data")
    return statuses


@case("lookup.scope_is_own_key_only",
      "lookup/data stays inside the authenticated key even when key_id/model/search are forged")
def scope_is_own_key_only(ctx):
    alpha_item, alpha_key, alpha_request = _seed(ctx, "lookup-alpha")
    beta_item, beta_key, beta_request = _seed(ctx, "lookup-beta")
    path = f"{_base(ctx)}/lookup/data"

    own = _get(ctx, path, key=alpha_key).json()
    own_ids = {item["id"] for item in own["items"]}
    check(alpha_request["id"] in own_ids, "own request is listed")
    check(beta_request["id"] not in own_ids, "another key's request is not listed")
    check_eq(own["key"]["id"], alpha_item["id"], "reported key is the authenticated one")
    check(all(item["model"] == "synthetic-chat" for item in own["items"]), "items carry their own model")

    # Every attacker-controlled selector is ignored in favour of the key itself:
    # a forged key_id cannot re-scope the query, and the other selectors can only
    # narrow the caller's own set.
    forged = _get(
        ctx,
        f"{path}?key_id={beta_item['id']}&model=synthetic-chat&page=1&page_size=50",
        key=alpha_key,
    ).json()
    forged_ids = {item["id"] for item in forged["items"]}
    check(forged_ids == own_ids, f"a forged key_id neither widens nor re-scopes the selection: {sorted(forged_ids)}")
    check(beta_request["id"] not in forged_ids, "cross-key key_id cannot pull another key's request")
    check_eq(forged["total"], own["total"], "total follows the authenticated key, not the forged key_id")

    # A free-text search matching the other key's label narrows inside the
    # caller's own set; it must never surface the other key's request.
    narrowed = _get(
        ctx,
        f"{path}?key_id={beta_item['id']}&search=lookup-beta&page=1&page_size=50",
        key=alpha_key,
    ).json()
    check_eq(narrowed["items"], [], "searching the other key's label returns nothing for this key")

    # The sanitized request shape drops upstream credential placement and raw payloads.
    text = _get(ctx, path, key=alpha_key).text
    for forbidden in ("auth_id", "auth_index", "executor", '"raw"', "principal", "fingerprint"):
        check(forbidden not in text, f"lookup/data response must not contain {forbidden}")
    check(alpha_key not in text and beta_key not in text, "lookup/data must not echo key material")
    return {"own": sorted(own_ids), "forged": sorted(forged_ids)}


@case("lookup.capture_is_not_a_cross_key_oracle",
      "lookup/capture returns the same generic 404 for a foreign, unknown or unavailable snapshot")
def capture_is_not_a_cross_key_oracle(ctx):
    alpha_item, alpha_key, alpha_request = _seed(ctx, "lookup-capture-alpha")
    beta_item, beta_key, _ = _seed(ctx, "lookup-capture-beta")
    path = f"{_base(ctx)}/lookup/capture"

    own = _get(ctx, f"{path}?id={alpha_request['id']}", key=alpha_key)
    check_eq(own.status, 200, "own request resolves for its own key")
    detail = own.json()
    check(detail.get("status") in {"captured", "disabled", "expired", "oversize", "dropped"},
          f"own snapshot reports a contract status: {detail.get('status')!r}")
    check_eq(detail.get("request_id") or alpha_request["id"], alpha_request["id"], "own snapshot names its request")

    foreign = _get(ctx, f"{path}?id={alpha_request['id']}", key=beta_key)
    unknown = _get(ctx, f"{path}?id=req-does-not-exist-0000", key=beta_key)
    check_eq(foreign.status, 404, "another key's request id is not served")
    check_eq(unknown.status, 404, "an unknown request id is not served")
    check_eq(foreign.raw, unknown.raw, "foreign and unknown snapshots are byte-identical (no existence oracle)")
    check(alpha_request["id"].encode() not in foreign.raw, "the 404 must not echo the probed request id")

    missing = _get(ctx, path, key=alpha_key)
    check_eq(missing.status, 400, "a capture request without an id is invalid")
    unauthenticated = _get(ctx, f"{path}?id={alpha_request['id']}")
    check_eq(unauthenticated.status, 401, "lookup/capture still requires the customer Bearer key")
    return {"own": own.status, "foreign": foreign.status, "unknown": unknown.status}


@case("lookup.trend_uses_calendar_months",
      "bucket=month returns month-aligned UTC points that reconcile with the key summary")
def trend_uses_calendar_months(ctx):
    item, key, request = _seed(ctx, "lookup-month")
    path = f"{_base(ctx)}/lookup/data"
    now = datetime.datetime.now(datetime.timezone.utc)
    month_start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    window = f"from={month_start.isoformat().replace('+00:00', 'Z')}&to={now.isoformat().replace('+00:00', 'Z')}"

    monthly = _get(ctx, f"{path}?bucket=month&{window}&page_size=50", key=key)
    check_eq(monthly.status, 200, "monthly trend query succeeds")
    payload = monthly.json()
    trend = payload["trend"]
    check(len(trend) >= 1, "monthly trend has at least the current month")
    prefix = month_start.strftime("%Y-%m-01T00:00:00")
    for point in trend:
        check(point["time"].startswith(prefix),
              f"month bucket point is the UTC first of the month: {point['time']}")
    check_eq(sum(point["requests"] for point in trend), payload["summary"]["requests"],
             "monthly trend reconciles with the summary")

    daily = _get(ctx, f"{path}?bucket=day&{window}&page_size=50", key=key).json()
    check(all(point["time"].endswith("T00:00:00Z") for point in daily["trend"]),
          "day bucket points are UTC midnights")
    check_eq(sum(point["requests"] for point in daily["trend"]), daily["summary"]["requests"],
             "daily trend reconciles with the summary")

    hourly = _get(ctx, f"{path}?bucket=hour&{window}&page_size=50", key=key).json()
    check(len(hourly["trend"]) >= 1, "hour bucket returns points")

    # The customer key never reaches an upstream account: only its own request is reported.
    check_eq(payload["key"]["id"], item["id"], "the monthly report is scoped to the caller's key")
    check(request["id"] in {entry["id"] for entry in payload["items"]},
          "the caller's own request is listed in the monthly report")
    return {"months": [point["time"] for point in trend], "days": len(daily["trend"]), "hours": len(hourly["trend"])}
