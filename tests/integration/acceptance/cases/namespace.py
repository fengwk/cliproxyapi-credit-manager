"""Model namespace: prices follow the actual usage model, never the request alias.

CPA records the client-facing request model separately from the model that
actually executed. Query-time pricing uses ``usage.model`` (the exact actual
model id) and ignores the request alias, so pricing an alias cannot price an
execution that ran on a different model.
"""

from __future__ import annotations

from ..report import check, check_eq, check_none, check_no_ledger_fields

CASES = []

REQUEST_MODEL = "synthetic-alias"
UPSTREAM_MODEL = "synthetic-upstream-name"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


@case("namespace.actual_model_price_bills_the_alias_request",
      "an aliased request bills on the actual usage model, while the request keeps the client alias")
def alias_namespace(ctx):
    ctx.set_price(UPSTREAM_MODEL)
    item, key = ctx.new_key(label="namespace-alias")

    response = ctx.client.chat(key, REQUEST_MODEL)
    check_eq(response.status, 200, "aliased request accepted")

    calls = ctx.mock.calls_for("/chat/completions")
    check_eq(len(calls), 1, "one upstream call")
    check_eq(calls[0]["body"].get("model"), UPSTREAM_MODEL, "upstream received the real model name")

    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "request recorded")
    check_eq(request["model"], REQUEST_MODEL, "request model is the client-requested alias")
    execution = request["executions"][0]
    check_eq(execution["model"], UPSTREAM_MODEL, "execution model is the actual usage model")
    check_eq(execution["response_model"], UPSTREAM_MODEL, "response model is the actual upstream model")
    check(request["model"] != execution["response_model"],
          "request namespace and execution namespace are not conflated")
    check_eq(execution["cost_micro_usd"], 14, "billed on the actual usage model's price")
    check_eq(report["summary"]["cost_micro_usd"], 14, "summary uses the same actual-model price")
    check_eq(report["summary"]["unpriced_executions"], 0, "the execution is priced")
    check_no_ledger_fields(request, "aliased request")
    return {"request_model": request["model"], "execution_model": execution["model"],
            "response_model": execution["response_model"],
            "cost_micro_usd": execution["cost_micro_usd"]}


@case("namespace.alias_price_does_not_price_the_actual_model",
      "pricing only the request alias leaves the actual usage unpriced: the alias never participates in pricing")
def alias_price_ignored(ctx):
    ctx.set_price(REQUEST_MODEL)
    item, key = ctx.new_key(label="namespace-alias-only")

    response = ctx.client.chat(key, REQUEST_MODEL)
    check_eq(response.status, 200, "the alias-priced request is admitted and executed")
    check_eq(ctx.mock.calls_for("/chat/completions")[0]["body"].get("model"), UPSTREAM_MODEL,
             "upstream executed the real model")

    report = ctx.wait_report(item["id"], settled=1)
    request = next((row for row in report["items"] if row["state"] == "settled"), None)
    check(request is not None, "request recorded")
    execution = request["executions"][0]
    check_eq(execution["model"], UPSTREAM_MODEL, "usage carries the actual model")
    check_none(execution["cost_micro_usd"],
               "an alias-only price leaves the actual execution unpriced")
    check_eq(report["summary"]["unpriced_executions"], 1, "the execution is counted as unpriced")
    check_eq(report["summary"]["cost_micro_usd"], 0, "the alias price contributes nothing")

    # Pricing the actual model reprices the same history and proves the alias was
    # never consulted.
    ctx.set_price(UPSTREAM_MODEL)
    report = ctx.wait_report(item["id"], settled=1)
    execution = report["items"][0]["executions"][0]
    check_eq(execution["cost_micro_usd"], 14, "pricing the actual model reprices history")
    check_eq(report["summary"]["unpriced_executions"], 0, "history is now priced")
    return {"request_model": request["model"], "execution_model": execution["model"],
            "cost_after_actual_price": execution["cost_micro_usd"]}
