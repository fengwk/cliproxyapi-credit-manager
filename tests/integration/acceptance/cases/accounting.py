"""Billing semantics: exact cost, canonical buckets, free prices and unpriced denial."""

from __future__ import annotations

import json

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _tokens(execution):
    return execution["tokens"]


def _find_key(manager, key_id):
    items = manager.keys().json()["items"]
    return next((row for row in items if row["id"] == key_id), None)


def _settled(ctx, key_id):
    def probe():
        request = ctx.find_request(key_id=key_id)
        if request and request["state"] == "settled":
            return request
        return None
    return ctx.wait_until(probe, timeout=15)


@case("billing.exact_cost_nonstream", "input 10 / output 4 settles at exactly 14 microUSD (1 microUSD/token)")
def exact_cost(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="billing-exact")
    response = ctx.client.chat(key, "synthetic-chat")
    check_eq(response.status, 200, "client completion status")
    check(b"synthetic answer" in response.raw, "answer text present")
    check_eq(ctx.mock.count("/chat/completions"), 1, "upstream call count")
    # The plugin must never bill by re-parsing the Bearer; upstream must see the
    # synthetic upstream credential, never the synthetic client key.
    for call in ctx.mock.calls_for("/chat/completions"):
        check(call["upstream_key"] in ctx.openai_upstream_keys, f"unexpected upstream key {call['upstream_key']}")
        check(call["upstream_key"] != key, "client key leaked upstream")

    request = _settled(ctx, item["id"])
    check(request is not None, "request settled within timeout")
    check_eq(request["state"], "settled", "request state")
    check_eq(request["outcome"], "succeeded", "request outcome")
    check_eq(request["held_micro_usd"], 0, "hold released on settle")
    executions = request["executions"]
    check_eq(len(executions), 1, "execution count")
    ex = executions[0]
    tokens = _tokens(ex)
    check_eq(tokens["input"], 10, "canonical input (uncached)")
    check_eq(tokens["output"], 4, "canonical output")
    check_eq(tokens["reasoning"], 0, "reasoning tokens")
    check_eq(tokens["total"], 14, "total tokens")
    check_eq(ex["quality"], "complete", "accounting quality")
    check_eq(ex["cost_micro_usd"], 14, "execution cost microUSD")
    check_eq(ex["response_model"], "synthetic-chat", "response model")
    check(ex["id"], "execution carries a distinct execution id")
    check(ex["id"] != request["id"], "usage/execution id differs from request/lifecycle id")

    row = _find_key(ctx.manager, item["id"])
    check_eq(row["spent_micro_usd"], 14, "key spent ledger")
    check_eq(row["held_micro_usd"], 0, "key hold released")
    return {"request_id": request["id"], "trace_id": request["trace_id"], "execution": ex,
            "key_spent": row["spent_micro_usd"]}


@case("billing.same_key_ledger", "two completions on one key accumulate a 28 microUSD same-key ledger")
def same_key_ledger(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="billing-ledger")
    for _ in range(2):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "client status")
    check_eq(ctx.mock.count("/chat/completions"), 2, "upstream call count")
    row = ctx.wait_until(lambda: (r := _find_key(ctx.manager, item["id"])) and (r if r["spent_micro_usd"] == 28 else None))
    check(row is not None, "key spent reached 28")
    check_eq(row["spent_micro_usd"], 28, "accumulated key spent")
    check_eq(row["held_micro_usd"], 0, "no dangling hold")
    report = ctx.usage_rows(key_id=item["id"])
    check_eq(report["summary"]["requests"], 2, "two settled requests")
    check_eq(report["summary"]["cost_micro_usd"], 28, "summary cost")
    check_eq(report["summary"]["executions"], 2, "two executions")
    return {"key_spent": row["spent_micro_usd"], "summary": report["summary"]}


@case("billing.all_zero_is_free", "an all-zero price is free, not a denial")
def free_price(ctx):
    ctx.set_price("synthetic-free", 0, 0, 0, 0)
    item, key = ctx.new_key(label="billing-free")
    check_eq(ctx.client.chat(key, "synthetic-free").status, 200, "client status")
    request = _settled(ctx, item["id"])
    check(request is not None, "free request still settles")
    check_eq(request["outcome"], "succeeded", "outcome")
    check_eq(request["executions"][0]["cost_micro_usd"], 0, "free cost")
    row = _find_key(ctx.manager, item["id"])
    check_eq(row["spent_micro_usd"], 0, "free key spent")
    return {"cost": request["executions"][0]["cost_micro_usd"]}


@case("billing.unpriced_denied_then_other_key_ok", "an unpriced model is denied with no upstream call and does not poison other keys")
def unpriced_denied(ctx):
    ctx.set_price("synthetic-chat")
    item_a, key_a = ctx.new_key(label="billing-priced-a")
    item_b, key_b = ctx.new_key(label="billing-priced-b")
    denied = ctx.client.chat(key_a, "synthetic-unpriced")
    check_eq(denied.status, 403, "unpriced model denied")
    denied_body = denied.json()
    check("not available" in json.dumps(denied_body) or "not permitted" in json.dumps(denied_body),
          f"denial body: {denied_body}")
    check_eq(ctx.mock.count("/chat/completions"), 0, "denied request must not reach upstream")
    # The same key still works on a priced model.
    check_eq(ctx.client.chat(key_a, "synthetic-chat").status, 200, "key A still usable after denial")
    # A different key is unaffected.
    check_eq(ctx.client.chat(key_b, "synthetic-chat").status, 200, "key B usable")
    check_eq(ctx.manager.health().json()["status"], "ok", "health stays ok; no fail-closed poison")
    check(ctx.wait_until(lambda: _find_key(ctx.manager, item_a["id"])["spent_micro_usd"] == 14), "key A settled")
    check(ctx.wait_until(lambda: _find_key(ctx.manager, item_b["id"])["spent_micro_usd"] == 14), "key B settled")
    return {"denied_status": denied.status, "upstream_calls": ctx.mock.count("/chat/completions")}


@case("billing.real_reserved_label_credential_is_billed",
      "a real CPA usage whose upstream account label is the reserved word 'warmup' is billed exactly 14 microUSD, never exempted")
def reserved_label_still_billed(ctx):
    # CPA resolves a usage record's Source from the upstream account, project,
    # email or api-key label -- it is not a trusted execution origin. Configuring
    # the real upstream credential as "warmup" therefore reproduces a customer
    # whose Source collides with a reserved word: the plugin must bill it like
    # any other customer and never treat the label as a billing exemption.
    original_keys = ctx.openai_upstream_keys
    try:
        ctx.openai_upstream_keys = ["warmup"]
        ctx.recreate_host("billing-reserved-label")
        ctx.set_price("synthetic-chat")
        item, key = ctx.new_key(label="billing-reserved-label")
        response = ctx.client.chat(key, "synthetic-chat")
        check_eq(response.status, 200, "client completion status")

        calls = ctx.mock.calls_for("/chat/completions")
        check_eq(len(calls), 1, "upstream call count")
        check_eq(calls[0]["upstream_key"], "warmup",
                 "upstream credential carries the reserved-word label")

        request = _settled(ctx, item["id"])
        check(request is not None, "request settled within timeout")
        check_eq(request["state"], "settled", "request state")
        check_eq(request["held_micro_usd"], 0, "hold released on settle")
        check_eq(request["executions"][0]["cost_micro_usd"], 14, "execution cost microUSD")
        row = _find_key(ctx.manager, item["id"])
        check_eq(row["spent_micro_usd"], 14, "key spent ledger")
        check_eq(row["held_micro_usd"], 0, "key hold released")
        check_eq(ctx.manager.health().json()["status"], "ok", "health stays ok")
        return {"upstream_key": calls[0]["upstream_key"], "key_spent": row["spent_micro_usd"],
                "execution_cost": request["executions"][0]["cost_micro_usd"]}
    finally:
        ctx.openai_upstream_keys = original_keys
