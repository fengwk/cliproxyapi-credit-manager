"""Query-time accounting: actual usage x current price, never a frozen charge.

Admission never reads a price and never holds money. A report derives each
execution's cost from the recorded actual usage (tokens and ``usage.model``)
plus the price book in force at read time, so adding, changing or deleting a
price reprices history immediately. An execution without a resolvable price has
``cost_micro_usd: null`` (never zero) and is counted in
``summary.unpriced_executions``; an explicit all-zero token price under
complete usage is a priced zero.
"""

from __future__ import annotations

from ..report import check, check_eq, check_no_holds, check_no_ledger_fields, check_none

CASES = []

COST = 14  # 10 input + 4 output tokens at 1 microUSD/token


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _key(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    return next((row for row in items if row["id"] == key_id), None)


def _request(ctx, key_id, state="settled"):
    report = ctx.wait_report(key_id, settled=1 if state == "settled" else 0)
    return next((row for row in report["items"] if row["state"] == state), None)


def _summary(ctx, key_id):
    return ctx.usage_rows(key_id=key_id, page_size=100)["summary"]


@case("billing.exact_cost_nonstream",
      "input 10 / output 4 settles at exactly 14 microUSD from actual usage and the current price")
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

    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "request settled within timeout")
    check_eq(request["outcome"], "succeeded", "request outcome")
    check_no_holds(request, "settled request")
    check_no_ledger_fields(request, "settled request")
    executions = request["executions"]
    check_eq(len(executions), 1, "execution count")
    ex = executions[0]
    tokens = ex["tokens"]
    check_eq(tokens["input"], 10, "canonical input (uncached)")
    check_eq(tokens["output"], 4, "canonical output")
    check_eq(tokens["reasoning"], 0, "reasoning tokens")
    check_eq(tokens["total"], 14, "total tokens")
    check_eq(ex["quality"], "complete", "accounting quality")
    check_eq(ex["cost_micro_usd"], COST, "execution cost microUSD")
    check_eq(ex["response_model"], "synthetic-chat", "response model")
    check(ex["id"], "execution carries a distinct execution id")
    check(ex["id"] != request["id"], "usage/execution id differs from request/lifecycle id")

    summary = report["summary"]
    check_eq(summary["requests"], 1, "summary requests")
    check_eq(summary["executions"], 1, "summary executions")
    check_eq(summary["cost_micro_usd"], COST, "summary derived cost")
    check_eq(summary["complete_executions"], 1, "complete usage is counted independently of pricing")
    check_eq(summary["unpriced_executions"], 0, "priced execution is not unpriced")
    check_no_holds(summary, "usage summary")
    check_no_ledger_fields(summary, "usage summary")

    row = _key(ctx, item["id"])
    check(row is not None, "key row readable")
    check_no_ledger_fields(row, "key row")
    check_no_holds(row, "key row")
    return {"request_id": request["id"], "trace_id": request["trace_id"], "execution": ex,
            "summary": summary}


@case("billing.same_key_history_accumulates",
      "two completions on one key derive a 28 microUSD same-key total from their actual usage")
def same_key_history(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="billing-history")
    for _ in range(2):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "client status")
    check_eq(ctx.mock.count("/chat/completions"), 2, "upstream call count")
    report = ctx.wait_report(item["id"], settled=2)
    summary = report["summary"]
    check_eq(summary["requests"], 2, "two settled requests")
    check_eq(summary["executions"], 2, "two executions")
    check_eq(summary["cost_micro_usd"], COST * 2, "summary cost is the exact derived sum")
    check_eq(summary["unpriced_executions"], 0, "every execution is priced")
    check_no_holds(summary, "usage summary")
    check_no_ledger_fields(summary, "usage summary")
    check_eq(sum(point["cost_micro_usd"] for point in report["trend"]), COST * 2,
             "trend cost sums to the same derived total")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"summary": summary, "trend_points": len(report["trend"])}


@case("billing.all_zero_is_free",
      "an explicit all-zero token price is a priced zero, not an unpriced or denied request")
def free_price(ctx):
    ctx.set_price("synthetic-free", 0, 0, 0, 0)
    item, key = ctx.new_key(label="billing-free")
    check_eq(ctx.client.chat(key, "synthetic-free").status, 200, "client status")
    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "free request still settles")
    check_eq(request["outcome"], "succeeded", "outcome")
    execution = request["executions"][0]
    check_eq(execution["cost_micro_usd"], 0, "free cost is an explicit zero")
    check_eq(execution["quality"], "complete", "zero price keeps complete usage")
    summary = report["summary"]
    check_eq(summary["cost_micro_usd"], 0, "free summary cost")
    check_eq(summary["unpriced_executions"], 0, "a zero price is priced, not unpriced")
    check_eq(summary["complete_executions"], 1, "usage quality independent of price")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"cost": execution["cost_micro_usd"], "unpriced": summary["unpriced_executions"]}


@case("billing.unpriced_execution_is_null_not_free",
      "a fresh key on an empty price book executes normally; its cost is null and it is counted as unpriced")
def unpriced_execution(ctx):
    check_eq(ctx.manager.prices().json()["items"], None, "fresh price book")
    check_eq(ctx.manager.pricing_rules().json()["items"], None, "fresh rule book")
    item, key = ctx.new_key(label="billing-unpriced")

    response = ctx.client.chat(key, "synthetic-unpriced")
    check_eq(response.status, 200, "an unpriced model is executed, not denied")
    check_eq(ctx.mock.count("/chat/completions"), 1, "unpriced request reached upstream")
    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "unpriced complete usage still settles")
    execution = request["executions"][0]
    check_eq(execution["quality"], "complete", "usage quality is independent of pricing")
    check_eq(execution["tokens"]["total"], 14, "actual usage recorded")
    check_none(execution["cost_micro_usd"], "unpriced execution cost is null")
    summary = report["summary"]
    check_eq(summary["cost_micro_usd"], 0, "no priced subtotal for an unpriced execution")
    check_eq(summary["unpriced_executions"], 1, "the unpriced execution is explicit")
    check_eq(summary["complete_executions"], 1, "complete usage does not require a price")

    # Adding a price later reprices the same historical execution.
    ctx.set_price("synthetic-unpriced")
    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check_eq(request["executions"][0]["cost_micro_usd"], COST, "later price reprices history")
    summary = report["summary"]
    check_eq(summary["cost_micro_usd"], COST, "summary reflects the new price")
    check_eq(summary["unpriced_executions"], 0, "the execution is no longer unpriced")
    check_eq(ctx.mock.count("/chat/completions"), 1, "repricing executes nothing")
    return {"unpriced_before": 1, "cost_after": summary["cost_micro_usd"]}


@case("billing.history_reprices_on_rule_add_change_delete",
      "a history line reprices when a rule adds, changes or removes the only effective price; deleting it makes the cost null again")
def history_repricing(ctx):
    item, key = ctx.new_key(label="billing-repricing")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "unpriced request accepted")
    check_eq(ctx.mock.count("/chat/completions"), 1, "only the client request executes")

    def cost_now(label):
        report = ctx.wait_report(item["id"], settled=1)
        check_eq(report["total"], 1, f"{label}: one historical request")
        request = report["items"][0]
        return request["executions"][0]["cost_micro_usd"], report["summary"]

    cost, summary = cost_now("empty book")
    check_none(cost, "no price means null cost")
    check_eq(summary["unpriced_executions"], 1, "empty book counts the execution as unpriced")

    # Add a rule that is the only effective price for the actual model.
    rule = {"id": "history-rule", "match_kind": "exact", "pattern": "synthetic-chat",
            "priority": 5, "enabled": True,
            "price": {"model": "synthetic-chat",
                      "rates": {"input": 1_000_000, "output": 1_000_000,
                                "cache_read": 0, "cache_write": 0}}}
    check_eq(ctx.manager.put_pricing_rule(rule).status, 200, "rule added")
    cost, summary = cost_now("rule added")
    check_eq(cost, COST, "adding the price reprices the historical usage")
    check_eq(summary["unpriced_executions"], 0, "history is now priced")

    # Change the rule's rate: 10 input x 2 + 4 output x 1 = 24 microUSD.
    rule["price"]["rates"] = {"input": 2_000_000, "output": 1_000_000,
                              "cache_read": 0, "cache_write": 0}
    check_eq(ctx.manager.put_pricing_rule(rule).status, 200, "rule changed")
    cost, summary = cost_now("rule changed")
    check_eq(cost, 24, "a changed price immediately reprices history")

    # Delete the last effective price: the same history becomes unpriced again.
    check_eq(ctx.manager.delete_pricing_rule("history-rule").status, 200, "rule deleted")
    cost, summary = cost_now("rule deleted")
    check_none(cost, "deleting the last price makes the historical cost null")
    check_eq(summary["cost_micro_usd"], 0, "no priced subtotal remains")
    check_eq(summary["unpriced_executions"], 1, "deleted price restores the unpriced count")
    check_eq(summary["complete_executions"], 1, "usage quality is unaffected by price changes")
    check_eq(ctx.mock.count("/chat/completions"), 1, "repricing never re-executes")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"empty": None, "added": COST, "changed": 24, "deleted": None,
            "upstream_calls": ctx.mock.count("/chat/completions")}


@case("billing.responses_prepriced_and_unpriced",
      "Responses works on an empty price book with a null cost and is priced by actual usage once a price exists")
def responses_pricing(ctx):
    item, key = ctx.new_key(label="billing-responses")
    headers = {"Authorization": "Bearer " + key}
    body = {"model": "synthetic-chat", "input": "synthetic"}

    unpriced = ctx.client.post("/v1/responses", body, headers)
    check_eq(unpriced.status, 200, "Responses runs without any price")
    check_eq(ctx.mock.count("/chat/completions"), 1, "Responses request reached upstream")
    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "Responses usage settles")
    check_none(request["executions"][0]["cost_micro_usd"], "Responses cost is null while unpriced")
    check_eq(report["summary"]["unpriced_executions"], 1, "Responses unpriced execution is counted")

    ctx.set_price("synthetic-chat")
    priced = ctx.client.post("/v1/responses", body, headers)
    check_eq(priced.status, 200, "Responses runs with a price")
    report = ctx.wait_report(item["id"], settled=2)
    priced_rows = [row for row in report["items"] if row["executions"]
                   and row["executions"][0]["cost_micro_usd"] is not None]
    check_eq(len(priced_rows), 2, "the later price reprices both Responses executions")
    check_eq(report["summary"]["cost_micro_usd"], COST * 2, "derived Responses total")
    check_eq(report["summary"]["unpriced_executions"], 0, "no unpriced execution remains")
    check_eq(ctx.mock.count("/chat/completions"), 2, "pricing changes execute nothing")
    return {"unpriced_status": unpriced.status, "priced_status": priced.status,
            "summary": report["summary"]}


@case("billing.upstream_label_never_exempts",
      "a real CPA usage whose upstream account label is the literal 'warmup' is priced exactly 14 microUSD, never exempted")
def upstream_label_never_exempts(ctx):
    # CPA resolves a usage record's Source from the upstream account, project,
    # email or api-key label -- it is not a trusted execution origin and no label
    # can exempt a customer. Configuring the real upstream credential as the
    # string "warmup" therefore reproduces a customer whose Source collides with
    # a former internal name: the plugin must price it like any other customer.
    original_keys = ctx.openai_upstream_keys
    try:
        ctx.openai_upstream_keys = ["warmup"]
        ctx.recreate_host("billing-upstream-label")
        ctx.set_price("synthetic-chat")
        item, key = ctx.new_key(label="billing-upstream-label")
        response = ctx.client.chat(key, "synthetic-chat")
        check_eq(response.status, 200, "client completion status")

        calls = ctx.mock.calls_for("/chat/completions")
        check_eq(len(calls), 1, "upstream call count")
        check_eq(calls[0]["upstream_key"], "warmup",
                 "upstream credential carries the literal label")

        report = ctx.wait_report(item["id"], settled=1)
        request = next((row for row in report["items"] if row["state"] == "settled"), None)
        check(request is not None, "request settled within timeout")
        check_eq(request["executions"][0]["cost_micro_usd"], COST, "execution cost microUSD")
        check_eq(report["summary"]["cost_micro_usd"], COST, "summary cost")
        check_eq(report["summary"]["unpriced_executions"], 0, "the execution is priced")
        check_no_ledger_fields(_key(ctx, item["id"]), "key row")
        check_eq(ctx.manager.health().json()["status"], "ok", "health stays ok")
        return {"upstream_key": calls[0]["upstream_key"],
                "execution_cost": request["executions"][0]["cost_micro_usd"]}
    finally:
        ctx.openai_upstream_keys = original_keys
