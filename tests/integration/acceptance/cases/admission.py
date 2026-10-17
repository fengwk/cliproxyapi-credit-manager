"""Admission limits after moving money out of the admission path.

Prices and the four dollar budgets are display-only: they never admit or deny a
request and never gate execution. What still limits a key is identity: enabled/
not-deleted/not-expired, allowed models, actual used model tokens and the
configured concurrency. Unknown usage stays owed (pending) and never becomes a
free success; a missing price stays unpaid (null) and never blocks.
"""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

from ..mock import Script
from ..report import check, check_eq, check_none, check_no_holds, check_no_ledger_fields

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


#: One synthetic completion is exactly 14 tokens; at 1 microUSD/token it costs
#: exactly 14 microUSD, so the derived arithmetic below is exact.
COST = 14

#: More unresolved requests than the removed `max_pending_requests` default (16),
#: so a lingering backlog gate would reject one of them.
BACKLOG_PROBE = 20


def _key(ctx, key_id):
    return next((row for row in ctx.manager.keys().json()["items"] if row["id"] == key_id), None)


def _settled(ctx, key_id, count, timeout=15):
    return ctx.wait_report(key_id, settled=count, timeout=timeout)


def _request(ctx, key_id, state="settled"):
    report = ctx.wait_report(key_id, settled=1 if state == "settled" else 0)
    return next((row for row in report["items"] if row["state"] == state), None)


@case("admission.display_budgets_never_block",
      "a key whose four dollar budgets are all exceeded keeps executing and derives every actual cost")
def budgets_never_block(ctx):
    ctx.set_price("synthetic-chat")
    # Every budget is positive and exceeded after the first 14 microUSD request.
    item, key = ctx.new_key(label="admission-budget", quota_micro_usd=1,
                            daily_micro_usd=1, weekly_micro_usd=1, monthly_micro_usd=1)
    for index in range(3):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
                 f"request {index + 1} admitted with exhausted display budgets")
    check_eq(ctx.mock.count("/chat/completions"), 3, "every admitted request reached upstream")
    report = _settled(ctx, item["id"], 3)
    summary = report["summary"]
    check_eq(summary["requests"], 3, "three admitted requests")
    check_eq(summary["cost_micro_usd"], COST * 3, "over-budget usage is still priced in full")
    check_eq(summary["unpriced_executions"], 0, "every execution is priced")
    check_no_holds(summary, "usage summary")
    check_no_ledger_fields(summary, "usage summary")

    row = _key(ctx, item["id"])
    check_eq(row["spec"]["quota_micro_usd"], 1, "the lifetime budget is kept as a display number")
    check_eq(row["spec"]["daily_micro_usd"], 1, "the daily budget is kept as a display number")
    check_eq(row["spec"]["weekly_micro_usd"], 1, "the weekly budget is kept as a display number")
    check_eq(row["spec"]["monthly_micro_usd"], 1, "the monthly budget is kept as a display number")
    check_no_ledger_fields(row, "key row")
    check_no_holds(row, "key row")
    return {"statuses": 3, "summary": summary, "budgets": row["spec"],
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.unpriced_request_admitted_despite_budget",
      "an unpriced model is admitted even with an exhausted budget; its cost is null until a price exists")
def unpriced_admitted(ctx):
    item, key = ctx.new_key(label="admission-unpriced", quota_micro_usd=1, daily_micro_usd=1)
    response = ctx.client.chat(key, "synthetic-unpriced")
    check_eq(response.status, 200, "missing price never blocks execution")
    check_eq(ctx.mock.count("/chat/completions"), 1, "the request reached upstream")
    report = _settled(ctx, item["id"], 1)
    execution = report["items"][0]["executions"][0]
    check_eq(execution["quality"], "complete", "usage is complete without a price")
    check_none(execution["cost_micro_usd"], "an unpriced execution derives null, never zero")
    check_eq(report["summary"]["unpriced_executions"], 1, "summary names the unpriced execution")
    check_eq(report["summary"]["cost_micro_usd"], 0, "there is no priced subtotal")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")

    # Adding the price reprices the same admitted history; it does not admit it.
    ctx.set_price("synthetic-unpriced")
    report = _settled(ctx, item["id"], 1)
    check_eq(report["summary"]["cost_micro_usd"], COST, "later price reprices the admitted history")
    check_eq(report["summary"]["unpriced_executions"], 0, "history becomes priced")
    check_eq(ctx.mock.count("/chat/completions"), 1, "repricing executes nothing")
    return {"status": response.status, "summary": report["summary"]}


@case("admission.token_limit_still_enforced",
      "an actual used model-token limit still rejects the next request with 429 and no upstream call")
def token_limit(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-token",
                            model_token_limits=[{"model": "synthetic-chat", "total": 10}])
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "first request admitted")
    report = _settled(ctx, item["id"], 1)
    check_eq(report["summary"]["tokens"]["total"], 14, "the first request consumed 14 actual tokens")

    upstream_before = ctx.mock.count("/chat/completions")
    denied = ctx.client.chat(key, "synthetic-chat")
    check_eq(denied.status, 429, "the token limit still rejects the next admission")
    check_eq(ctx.mock.count("/chat/completions"), upstream_before,
             "the denied request never reaches upstream")
    check_eq(ctx.usage_rows(key_id=item["id"], page_size=50)["total"], 1,
             "a denied request is not admitted as a usage row")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"denied_status": denied.status, "tokens": 14, "limit": 10}


@case("admission.concurrency_limit_still_enforced",
      "max_concurrent still rejects a second in-flight request; the slot is released when the first settles")
def concurrency_limit(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-concurrency", max_concurrent=1)
    ctx.mock.default_script = Script(delay=0.75)

    with ThreadPoolExecutor(max_workers=1) as pool:
        first = pool.submit(ctx.client.chat, key, "synthetic-chat")
        check(ctx.wait_until(lambda: ctx.mock.count("/chat/completions") >= 1, timeout=10),
              "the first request is in flight")
        denied = ctx.client.chat(key, "synthetic-chat")
        check_eq(denied.status, 429, "the second request exceeds max_concurrent")
        check_eq(ctx.mock.count("/chat/completions"), 1,
                 "the rejected request never reaches upstream")
        check_eq(first.result().status, 200, "the in-flight request completes")
    ctx.mock.default_script = Script()

    report = _settled(ctx, item["id"], 1)
    check_eq(report["summary"]["cost_micro_usd"], COST, "the admitted request is priced once")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
             "the concurrency slot is released after completion")
    report = _settled(ctx, item["id"], 2)
    check_eq(report["summary"]["requests"], 2, "only the two admitted requests are recorded")
    check_eq(report["summary"]["cost_micro_usd"], COST * 2, "both admitted requests are priced")
    return {"denied_status": denied.status, "summary": report["summary"],
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.model_permission_expiry_and_disabled_still_deny",
      "allowed_models, expires_at and a disabled key still deny before any upstream call")
def permission_and_expiry(ctx):
    ctx.set_price("synthetic-chat")
    restricted, restricted_key = ctx.new_key(label="admission-models",
                                             allowed_models=["synthetic-chat"])
    denied_model = ctx.client.chat(restricted_key, "synthetic-free")
    check_eq(denied_model.status, 403, "a model outside allowed_models is denied")
    check_eq(ctx.mock.count("/chat/completions"), 0, "the denied model never reaches upstream")
    check_eq(ctx.usage_rows(key_id=restricted["id"])["total"], 0, "the denial creates no usage row")

    expired, expired_key = ctx.new_key(label="admission-expired",
                                       expires_at="2020-01-01T00:00:00Z")
    denied_expired = ctx.client.chat(expired_key, "synthetic-chat")
    check_eq(denied_expired.status, 401, "an expired key is denied")
    check_eq(ctx.mock.count("/chat/completions"), 0, "the expired key never reaches upstream")

    disabled, disabled_key = ctx.new_key(label="admission-disabled", enabled=False)
    denied_disabled = ctx.client.chat(disabled_key, "synthetic-chat")
    check_eq(denied_disabled.status, 401, "a disabled key is denied")
    check_eq(ctx.mock.count("/chat/completions"), 0, "the disabled key never reaches upstream")

    # The same key is still usable on an allowed, unexpired request path.
    allowed, allowed_key = ctx.new_key(label="admission-allowed",
                                       allowed_models=["synthetic-chat"])
    check_eq(ctx.client.chat(allowed_key, "synthetic-chat").status, 200, "an allowed model is admitted")
    report = _settled(ctx, allowed["id"], 1)
    check_eq(report["summary"]["cost_micro_usd"], COST, "the allowed request is priced")
    return {"denied_model": denied_model.status, "denied_expired": denied_expired.status,
            "denied_disabled": denied_disabled.status}


@case("admission.unknown_usage_does_not_block_future_admissions",
      "20 unresolved unknown-usage requests stay pending with a null cost and block neither the same key nor another key")
def unknown_usage_no_backlog(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-backlog")
    other, other_key = ctx.new_key(label="admission-backlog-other")

    # No usage is delivered for any of these: every one stays pending. A removed
    # per-key pending cap would have rejected one of them.
    ctx.mock.default_script = Script(omit_usage=True)
    for index in range(BACKLOG_PROBE):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
                 f"unresolved request {index + 1} admitted")

    def pending_report():
        report = ctx.usage_rows(key_id=item["id"], page_size=100)
        if report["total"] == BACKLOG_PROBE and report["summary"]["pending"] == BACKLOG_PROBE:
            return report
        return None

    report = ctx.wait_until(pending_report, timeout=20)
    check(report is not None, f"{BACKLOG_PROBE} unresolved requests are recorded as pending")
    check_eq(report["total"], BACKLOG_PROBE, "exactly the admitted requests are recorded")
    check_eq(report["summary"]["pending"], BACKLOG_PROBE, "every unresolved request stays pending")
    check_eq(report["summary"]["cost_micro_usd"], 0, "unknown usage is never fabricated as a charge")
    check_eq(report["summary"]["complete_executions"], 0,
             "missing usage is not complete, so no cost can be derived from it")
    check_no_holds(report["summary"], "usage summary")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row with unresolved requests")

    # Both the same key and another key still admit and price reliable usage.
    ctx.mock.default_script = Script()
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
             "the same key admits after 20 pending requests")
    check_eq(ctx.client.chat(other_key, "synthetic-chat").status, 200, "a second key is admitted")
    own = _settled(ctx, item["id"], 1)
    other_report = _settled(ctx, other["id"], 1)
    check_eq(own["summary"]["cost_micro_usd"], COST, "the same key prices its reliable usage")
    check_eq(other_report["summary"]["cost_micro_usd"], COST, "the second key prices normally")
    check_eq(ctx.mock.count("/chat/completions"), BACKLOG_PROBE + 2,
             "every admitted request reached upstream")
    return {"pending": report["summary"]["pending"], "probe": BACKLOG_PROBE,
            "other_cost": other_report["summary"]["cost_micro_usd"],
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("admission.actual_usage_and_dedup_preserved",
      "every admitted request keeps one unique execution and the summary equals the exact sum of actual usage")
def actual_usage_preserved(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="admission-actual")
    key_id = item["id"]
    for index in range(3):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, f"request {index + 1} admitted")
    report = _settled(ctx, key_id, 3)
    summary = report["summary"]
    check_eq(summary["requests"], 3, "three accounted requests")
    check_eq(summary["executions"], 3, "one execution per actual usage record")
    check_eq(summary["cost_micro_usd"], COST * 3, "summary cost equals the exact derived sum")
    check_eq(summary["complete_executions"], 3, "three complete executions")

    execution_ids = [row["executions"][0]["id"] for row in report["items"] if row.get("executions")]
    check_eq(len(execution_ids), 3, "every request carries its execution")
    check_eq(len(set(execution_ids)), 3, "usage IDs are unique, so no actual usage is counted twice")
    for row in report["items"]:
        check_eq(row["executions"][0]["cost_micro_usd"], COST,
                 "each execution derives its actual usage once")
        check(row["executions"][0]["id"] != row["id"],
              "the usage id stays distinct from the request id")
        check_no_holds(row, "accounted request")
        check_no_ledger_fields(row, "accounted request")
    check_no_holds(summary, "usage summary")
    check_no_ledger_fields(summary, "usage summary")
    return {"executions": summary["executions"], "cost": summary["cost_micro_usd"],
            "upstream_calls": ctx.mock.count("/chat/completions")}
