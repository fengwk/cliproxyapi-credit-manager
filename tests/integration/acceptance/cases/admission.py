"""Soft post-usage admission limits.

Quotas are *soft* limits on actual used amounts: as long as the configured limit
is positive and the actual used is still below it, a request is admitted even
with a huge declared output limit or input payload. There is no estimate, no
reservation and no in-flight token sum, so concurrent in-flight requests can
overrun a quota -- every reliable actual usage is then charged in full, and only
the *next* request is denied once the actual used has reached the limit.

These cases pin the observable consequences of that contract on a real host:
admission never looks at estimated size, unresolved usage never blocks a later
admission, and the billed ledger always equals the actual usage delivered.
"""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

from ..mock import Script
from ..report import check, check_eq, check_no_holds

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


#: One synthetic completion is exactly 14 tokens; at 1 microUSD/token it costs
#: exactly 14 microUSD, so quota arithmetic below is exact.
COST = 14

#: More unresolved requests than the removed `max_pending_requests` default (16),
#: so a lingering backlog gate would reject one of them.
BACKLOG_PROBE = 20


def _key(ctx, key_id):
    return next((row for row in ctx.manager.keys().json()["items"] if row["id"] == key_id), None)


def _settled(ctx, key_id, spent, timeout=15):
    return ctx.wait_until(lambda: (row := _key(ctx, key_id)) and (row if row["spent_micro_usd"] == spent else None),
                          timeout=timeout)


@case("admission.tiny_remaining_admits_large_declared_request",
      "with 1 microUSD of quota left, a large declared body/output is still admitted and billed by actual usage")
def tiny_remaining_admits_large(ctx):
    ctx.set_price("synthetic-chat")
    # Limit 15: the first request consumes 14, leaving a positive remainder of 1.
    item, key = ctx.new_key(label="admission-tiny", quota_micro_usd=15)
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "first request admitted")
    check(_settled(ctx, item["id"], COST) is not None, "first request settled at 14")
    row = _key(ctx, item["id"])
    check_eq(row["quota_used_micro_usd"], COST, "quota baseline consumed to 14")
    check_eq(row["quota_micro_usd"], 15, "the configured limit is positive")
    check_no_holds(row, "key row")

    # A declaration far larger than the 1 microUSD remainder must not be used to
    # reject the request: admission is not an estimate.
    big = "x" * 100_000
    response = ctx.client.chat(key, "synthetic-chat", max_tokens=100_000, content=big)
    check_eq(response.status, 200, "a large declared body/output is admitted on positive remaining quota")
    check_eq(ctx.mock.count("/chat/completions"), 2, "the admitted request reached upstream")

    request = ctx.wait_until(lambda: (r := ctx.find_request(key_id=item["id"])) and (r if r["state"] == "settled" else None))
    check(request is not None, "the large request settles")
    check_no_holds(request, "large declared request")
    check_eq(request["executions"][0]["cost_micro_usd"], COST, "billed by actual usage, not the declaration")
    row = _settled(ctx, item["id"], COST * 2)
    check(row is not None, "key reached the actual 28 microUSD")
    check_eq(row["quota_used_micro_usd"], COST * 2, "actual used exceeds the limit after the overrun")
    check(row["quota_used_micro_usd"] > row["quota_micro_usd"], "the actual charge overran the soft limit")
    return {"declared_max_tokens": 100_000, "body_bytes": len(big),
            "key_spent": row["spent_micro_usd"], "quota_used": row["quota_used_micro_usd"],
            "quota": row["quota_micro_usd"], "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.actual_overrun_then_next_request_denied",
      "actual usage can exceed the quota in full, and only the next admission is denied")
def actual_overrun_then_denied(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-overrun", quota_micro_usd=20)

    # Both requests fit at admission time (used 0 then 14, limit 20) even though
    # the second one pushes the actual total to 28.
    for index in (1, 2):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, f"request {index} admitted")
    row = _settled(ctx, item["id"], COST * 2)
    check(row is not None, "both requests charged in full")
    check_eq(row["spent_micro_usd"], COST * 2, "actual charges are never capped to the quota")
    check_eq(row["quota_used_micro_usd"], COST * 2, "quota baseline follows actual usage past the limit")
    check_no_holds(row, "key row")

    # Only now, with actual used >= limit, is the next request denied.
    upstream_before = ctx.mock.count("/chat/completions")
    denied = ctx.client.chat(key, "synthetic-chat")
    check_eq(denied.status, 429, "the over-quota request is denied with 429 (ErrQuota)")
    check_eq(ctx.mock.count("/chat/completions"), upstream_before, "the denied request never reaches upstream")
    report = ctx.usage_rows(key_id=item["id"], page_size=50)
    check_eq(report["summary"]["requests"], 2, "the denied request is never admitted as a ledger row")
    check_eq(report["summary"]["cost_micro_usd"], COST * 2, "the ledger keeps exactly the actual charges")
    check_no_holds(report["summary"], "usage summary")
    check_eq(_key(ctx, item["id"])["spent_micro_usd"], COST * 2, "denial changes no charged amount")
    return {"denied_status": denied.status, "key_spent": row["spent_micro_usd"],
            "quota_used": row["quota_used_micro_usd"], "limit": row["quota_micro_usd"],
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.concurrent_same_key_admissions_have_no_estimated_holds",
      "four simultaneous same-key requests with 1 microUSD left are all admitted; no request carries an estimated hold")
def concurrent_same_key(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-concurrent", quota_micro_usd=15)
    key_id = item["id"]
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "baseline request admitted")
    check(_settled(ctx, key_id, COST) is not None, "baseline settled at 14 (1 microUSD left)")

    # Delay every upstream answer so all four requests are genuinely in flight at
    # the same time. Under an estimate/reservation model the 1 microUSD remainder
    # could reject them; a post-usage soft limit admits all and bills actual.
    ctx.mock.default_script = Script(delay=0.75)
    with ThreadPoolExecutor(max_workers=4) as pool:
        futures = [pool.submit(ctx.client.chat, key, "synthetic-chat") for _ in range(4)]
        statuses = [future.result().status for future in futures]
    ctx.mock.default_script = Script()

    check_eq(statuses, [200, 200, 200, 200], "all four simultaneous requests are admitted")
    check_eq(ctx.mock.count("/chat/completions"), 5, "every admitted request reached upstream once")
    row = _settled(ctx, key_id, COST * 5, timeout=20)
    check(row is not None, "all five requests charged, none skipped")
    check_eq(row["quota_used_micro_usd"], COST * 5, "actual used overran the limit by every admitted request")

    report = ctx.usage_rows(key_id=key_id, page_size=50)
    check_eq(report["summary"]["requests"], 5, "five accounted requests")
    check_eq(report["summary"]["cost_micro_usd"], COST * 5, "no charge was estimated, capped or lost")
    for request in report["items"]:
        check_no_holds(request, "simultaneous request")
    check_no_holds(report["summary"], "usage summary")
    check_no_holds(row, "key row")
    return {"statuses": statuses, "key_spent": row["spent_micro_usd"],
            "quota_used": row["quota_used_micro_usd"], "limit": row["quota_micro_usd"],
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.unknown_usage_does_not_block_future_admissions",
      "20 unresolved unknown-usage requests stay pending with no hold and block neither the same key nor another key")
def unknown_usage_no_backlog(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-backlog")
    other, other_key = ctx.new_key(label="admission-backlog-other")

    # No usage is delivered for any of these: every one stays pending. A removed
    # per-key pending cap would have rejected one of them.
    ctx.mock.default_script = Script(omit_usage=True)
    for index in range(BACKLOG_PROBE):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, f"unresolved request {index + 1} admitted")
    report = ctx.wait_until(
        lambda: (r := ctx.usage_rows(key_id=item["id"], page_size=50))
        if r["total"] >= BACKLOG_PROBE and r["summary"].get("pending") == BACKLOG_PROBE else None,
        timeout=20)
    check(report is not None, f"{BACKLOG_PROBE} unresolved requests are recorded as pending")
    check_eq(report["total"], BACKLOG_PROBE, "exactly the admitted requests are recorded")
    check_eq(report["summary"]["pending"], BACKLOG_PROBE, "every unresolved request stays pending")
    check_eq(report["summary"]["cost_micro_usd"], 0, "unknown usage is never fabricated as a charge")
    check_eq(_key(ctx, item["id"])["spent_micro_usd"], 0, "no money is held or charged for unknown usage")
    check_no_holds(_key(ctx, item["id"]), "key row with unresolved requests")

    # Another key is unaffected, and a normal request on it still bills actual usage.
    ctx.mock.default_script = Script()
    check_eq(ctx.client.chat(other_key, "synthetic-chat").status, 200, "a second key is admitted")
    row = _settled(ctx, other["id"], COST)
    check(row is not None, "the second key settles normally")
    check_eq(ctx.mock.count("/chat/completions"), BACKLOG_PROBE + 1, "every admitted request reached upstream")
    return {"pending": report["summary"]["pending"], "probe": BACKLOG_PROBE,
            "other_key_spent": row["spent_micro_usd"], "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.actual_usage_and_dedup_preserved",
      "every admitted request keeps one unique execution and the ledger equals the exact sum of actual usage")
def actual_usage_preserved(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-actual")
    key_id = item["id"]
    for index in range(3):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, f"request {index + 1} admitted")
    row = _settled(ctx, key_id, COST * 3)
    check(row is not None, "the key reached the exact expected charge")

    report = ctx.usage_rows(key_id=key_id, page_size=50)
    check_eq(report["summary"]["requests"], 3, "three accounted requests")
    check_eq(report["summary"]["executions"], 3, "one execution per actual usage record")
    check_eq(report["summary"]["cost_micro_usd"], COST * 3, "summary cost equals the exact actual sum")
    check_eq(row["spent_micro_usd"], COST * 3, "key ledger equals the actual sum")
    check_eq(row["quota_used_micro_usd"], COST * 3, "quota baseline equals the actual sum")

    execution_ids = [request["executions"][0]["id"] for request in report["items"] if request.get("executions")]
    check_eq(len(execution_ids), 3, "every request carries its execution")
    check_eq(len(set(execution_ids)), 3, "usage IDs are unique, so no actual usage is charged twice")
    for request in report["items"]:
        check_eq(request["executions"][0]["cost_micro_usd"], COST, "each execution is charged its actual usage once")
        check(request["executions"][0]["id"] != request["id"], "the usage id stays distinct from the request id")
        check_no_holds(request, "accounted request")
    check_no_holds(report["summary"], "usage summary")
    check_no_holds(row, "key row")
    return {"key_spent": row["spent_micro_usd"], "executions": report["summary"]["executions"],
            "cost": report["summary"]["cost_micro_usd"], "upstream_calls": ctx.mock.count("/chat/completions")}
