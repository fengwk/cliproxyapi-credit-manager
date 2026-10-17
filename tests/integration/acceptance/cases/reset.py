"""Baseline reset semantics.

`POST /api/reset-spend` re-opens a key's window baselines. It is a quota-baseline
operation only: it must never erase the durable lifetime ledger, must never
remove history, and must never forgive a charge that is still unresolved (the
late actual usage is still recorded when it arrives).
"""

from __future__ import annotations

from ..mock import Script
from ..report import check, check_eq, check_no_holds

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _key(ctx, key_id):
    return next((row for row in ctx.manager.keys().json()["items"] if row["id"] == key_id), None)


def _pending(ctx, key_id):
    request = ctx.find_request(key_id=key_id)
    return request if request and request["state"] != "settled" else None


@case("reset.baseline_restores_quota_only_not_ledger_or_history",
      "reset-spend clears the quota baseline but keeps the lifetime ledger, the history and the unresolved request")
def baseline_reset(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="reset-baseline", quota_micro_usd=1_000_000)
    key_id = item["id"]
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "settled request accepted")
    check(ctx.wait_until(lambda: (_key(ctx, key_id) or {}).get("quota_used_micro_usd") == 14), "baseline consumed to 14")

    # An unresolved request stays pending; a reset must not forgive its late usage.
    ctx.mock.default_script = Script(omit_usage=True)
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "pending request accepted")
    ctx.mock.default_script = Script()
    pending = ctx.wait_until(lambda: _pending(ctx, key_id), timeout=15)
    check(pending is not None, "an unresolved request is retained")
    check_no_holds(pending, "pending request before reset")
    check_no_holds(_key(ctx, key_id), "key row before reset")

    # Validation: a reset needs a window, a scope and a reason.
    check_eq(ctx.manager.reset_spend({"id": key_id, "reason": "no window"}).status, 400, "no window rejected")
    check_eq(ctx.manager.reset_spend({"id": key_id, "total": True}).status, 400, "missing reason rejected")
    check_eq(ctx.manager.reset_spend({"total": True, "reason": "no scope"}).status, 400, "missing scope rejected")
    check_eq(ctx.manager.reset_spend({"ids": ["does-not-exist"], "total": True, "reason": "bad id"}).status, 404,
             "unknown reset id rejected")

    reset = ctx.manager.reset_spend({"id": key_id, "total": True, "reason": "new allocation"})
    check_eq(reset.status, 200, f"reset {reset.text[:160]}")
    check_eq(reset.json()["keys"], 1, "one key reset")
    check_eq(reset.json()["windows"], ["total"], "the total window was reset")

    row = _key(ctx, key_id)
    check_eq(row["quota_used_micro_usd"], 0, "the quota baseline is restored")
    check_eq(row["spent_micro_usd"], 14, "the lifetime ledger is never cleared")
    check_no_holds(row, "key row after reset")

    report = ctx.usage_rows(key_id=key_id, page_size=50)
    check_eq(report["summary"]["cost_micro_usd"], 14, "history keeps its settled cost")
    check(report["summary"]["pending"] >= 1, "history keeps the pending request")
    check_eq(len([r for r in report["items"] if r["state"] == "settled"]), 1, "the settled request row is retained")

    # The retained pending request still settles afterward and adds to the durable ledger.
    settled = ctx.wait_until(lambda: _resolve_pending(ctx, key_id, pending["id"]), timeout=15)
    check(settled is not None and settled["state"] == "settled", "the retained pending request still settles")
    check_eq(_key(ctx, key_id)["spent_micro_usd"], 14, "settling the retained pending adds no invented charge")
    return {"windows": reset.json()["windows"], "spent": row["spent_micro_usd"],
            "pending_state": settled["state"]}


def _resolve_pending(ctx, key_id, request_id):
    report = ctx.usage_rows(key_id=key_id, page_size=50)
    current = next((r for r in report["items"] if r["id"] == request_id), None)
    if current is None or current["state"] == "settled":
        return current
    ctx.manager.resolve(request_id, "release", reason="retained pending settles")
    report = ctx.usage_rows(key_id=key_id, page_size=50)
    return next((r for r in report["items"] if r["id"] == request_id), None)
