"""Token baseline reset.

``POST /api/reset-tokens`` re-opens the actual consumed model-token baseline. It
does not touch the usage log, the derived cost, the request history or the
dollar budgets: a reset changes future Token admission only.
"""

from __future__ import annotations

from ..mock import Script
from ..report import check, check_eq, check_no_holds, check_no_ledger_fields

CASES = []

COST = 14


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


@case("reset.tokens_baseline_only_keeps_history_and_costs",
      "reset-tokens clears only the token baseline: the usage history and derived costs are unchanged")
def tokens_reset(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="reset-tokens",
                            model_token_limits=[{"model": "synthetic-chat", "total": 10}])
    key_id = item["id"]

    # Open an unresolved request first: unreliable usage consumes no token
    # baseline, and a later reset must not change its state.
    ctx.mock.default_script = Script(omit_usage=True)
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "pending request accepted")
    ctx.mock.default_script = Script()
    pending = ctx.wait_until(lambda: _pending(ctx, key_id), timeout=15)
    check(pending is not None, "an unresolved request is retained")

    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "first reliable request accepted")
    check(ctx.wait_report(key_id, settled=1) is not None, "first reliable request settled")
    denied = ctx.client.chat(key, "synthetic-chat")
    check_eq(denied.status, 429, "the consumed token baseline rejects the next request")
    check_no_holds(pending, "pending request before reset")
    check_no_ledger_fields(pending, "pending request before reset")

    before = ctx.usage_rows(key_id=key_id, page_size=100)
    check_eq(before["summary"]["cost_micro_usd"], COST, "cost before reset")
    check_eq(before["summary"]["pending"], 1, "pending row before reset")

    # Validation: a reset needs a window, a scope and a reason.
    check_eq(ctx.manager.reset_tokens({"id": key_id, "reason": "no window"}).status, 400,
             "no window rejected")
    check_eq(ctx.manager.reset_tokens({"id": key_id, "total": True}).status, 400,
             "missing reason rejected")
    check_eq(ctx.manager.reset_tokens({"total": True, "reason": "no scope"}).status, 400,
             "missing scope rejected")
    check_eq(ctx.manager.reset_tokens({"ids": ["does-not-exist"], "total": True, "reason": "bad id"}).status,
             404, "unknown reset id rejected")

    reset = ctx.manager.reset_tokens({"id": key_id, "total": True, "reason": "new token allocation"})
    check_eq(reset.status, 200, f"reset {reset.text[:160]}")
    check_eq(reset.json()["keys"], 1, "one key reset")
    check_eq(reset.json()["windows"], ["total"], "the total token window was reset")

    # The log and its derived cost are untouched by a token reset.
    after = ctx.usage_rows(key_id=key_id, page_size=100)
    check_eq(after["summary"]["cost_micro_usd"], COST, "a token reset never rewrites derived costs")
    check_eq(after["summary"]["requests"], before["summary"]["requests"],
             "a token reset keeps every request row")
    check_eq(after["summary"]["pending"], 1, "the pending request is untouched")
    settled_after = next((row for row in after["items"] if row["id"] == pending["id"]), None)
    check(settled_after is not None and settled_after["state"] != "settled",
          "the unresolved request stays unresolved")
    check_no_ledger_fields(_key(ctx, key_id), "key row after reset")
    check_no_holds(_key(ctx, key_id), "key row after reset")

    # The token window is open again while the dollar budgets stay display-only.
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
             "the reset token baseline admits the next request")
    report = ctx.wait_report(key_id, settled=2)
    check_eq(report["summary"]["cost_micro_usd"], COST * 2,
             "the new request adds its own derived cost")
    check_eq(report["summary"]["pending"], 1, "the retained pending request is still present")
    return {"windows": reset.json()["windows"],
            "cost_before": before["summary"]["cost_micro_usd"],
            "cost_after": report["summary"]["cost_micro_usd"],
            "pending": report["summary"]["pending"]}
