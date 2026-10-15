"""Model namespace: request/execution/response identity must not be conflated.

A generic alias maps a client-facing request model to a different upstream model
name. The plugin must bill the requested model's price and keep the request
model distinct from the actual upstream response model.
"""

from __future__ import annotations

from ..report import check, check_eq

CASES = []

REQUEST_MODEL = "synthetic-alias"
UPSTREAM_MODEL = "synthetic-upstream-name"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _spent(ctx, key_id):
    row = next((k for k in ctx.manager.keys().json()["items"] if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


@case("namespace.alias_request_vs_upstream_response",
      "an aliased model bills on the requested name and reports the actual upstream response model separately")
def alias_namespace(ctx):
    ctx.set_price(REQUEST_MODEL)
    item, key = ctx.new_key(label="namespace-alias")

    response = ctx.client.chat(key, REQUEST_MODEL)
    check_eq(response.status, 200, "aliased request accepted")

    calls = ctx.mock.calls_for("/chat/completions")
    check_eq(len(calls), 1, "one upstream call")
    check_eq(calls[0]["body"].get("model"), UPSTREAM_MODEL, "upstream received the real model name")

    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, "request recorded")
    check_eq(request["model"], REQUEST_MODEL, "request model is the client-requested alias")
    execution = request["executions"][0]
    check_eq(execution["response_model"], UPSTREAM_MODEL, "response model is the actual upstream model")
    check(request["model"] != execution["response_model"],
          "request namespace and execution response namespace are not conflated")
    check_eq(execution["cost_micro_usd"], 14, "billed on the requested model's price")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "key charged 14")
    # The plugin must not record the upstream name as the request model.
    check_eq(request["model"], REQUEST_MODEL, "request model never rewritten to the upstream name")
    return {"request_model": request["model"], "response_model": execution["response_model"],
            "execution_model": execution.get("model")}
