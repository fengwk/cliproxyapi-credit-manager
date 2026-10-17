"""Pricing book and rules: stored prices are inputs to query-time costs only.

Saving a price or a rule never touches a ledger because no ledger exists: a
cost is derived from actual usage and the price in force when a report is read.
Rules select an effective price for the actual usage model; a disabled rule
takes no part in pricing (it is not a model permission gate), and a per-image
price has no provable image count in SDK usage, so its executions stay unpriced.
"""

from __future__ import annotations

from ..report import check, check_eq, check_none, check_no_ledger_fields

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


TWO53_PLUS_1 = 9007199254740993          # not representable as a float64
INT64_MAX = 9223372036854775807
LARGE = 123456789012345678


def _prices(ctx):
    return ctx.manager.prices().json().get("items") or []


def _price(ctx, model):
    return next((row for row in _prices(ctx) if row["model"] == model), None)


def _zero_rates():
    return {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}


def _settled_row(ctx, key_id):
    report = ctx.wait_report(key_id, settled=1)
    return report, next((row for row in report["items"] if row["state"] == "settled"), None)


@case("pricing.nested_rates_signed64_roundtrip_without_ledger",
      "nested rates keep exact 64-bit values across save/reload and saving a price derives no cost")
def nested_rates(ctx):
    payload = {"model": "pricing-big",
               "rates": {"input": TWO53_PLUS_1, "output": LARGE, "cache_read": 0,
                         "cache_write": 7, "reasoning": TWO53_PLUS_1},
               "version": 0}
    saved = ctx.manager.put_price(payload)
    check_eq(saved.status, 200, f"price save {saved.text[:160]}")

    row = _price(ctx, "pricing-big")
    check(row is not None, "price reloaded")
    check_eq(row["rates"]["input"], TWO53_PLUS_1, "2^53+1 input rate round-trips exactly")
    check_eq(row["rates"]["output"], LARGE, "large output rate round-trips exactly")
    check_eq(row["rates"]["reasoning"], TWO53_PLUS_1, "reasoning rate round-trips exactly")
    check_eq(row["rates"]["cache_write"], 7, "cache_write rate round-trips")
    check_eq(row["version"], 1, "first save is version 1")
    check(row.get("billing_mode") in (None, "", "token"), f"default billing mode is token: {row.get('billing_mode')!r}")

    # The int64 ceiling is also accepted verbatim on save/reload.
    ceiling = dict(payload, model="pricing-ceiling",
                   rates={"input": INT64_MAX, "output": 0, "cache_read": 0, "cache_write": 0})
    check_eq(ctx.manager.put_price(ceiling).status, 200, "int64 ceiling accepted")
    check_eq(_price(ctx, "pricing-ceiling")["rates"]["input"], INT64_MAX, "int64 ceiling round-trips")

    # Re-saving bumps the stored version; there is no ledger to consume.
    check_eq(ctx.manager.put_price(payload).status, 200, "price re-save accepted")
    check_eq(_price(ctx, "pricing-big")["version"], 2, "re-save increments the version")

    # A per-image price keeps its flat rate and billing mode as a catalogue value.
    image = {"model": "pricing-image", "rates": _zero_rates(), "billing_mode": "per_image",
             "per_image_micro_usd": TWO53_PLUS_1, "version": 0}
    check_eq(ctx.manager.put_price(image).status, 200, "per-image price accepted")
    image_row = _price(ctx, "pricing-image")
    check_eq(image_row["billing_mode"], "per_image", "per-image billing mode stored")
    check_eq(image_row["per_image_micro_usd"], TWO53_PLUS_1, "per-image rate round-trips exactly")

    # Signedness / mode validation is part of the contract.
    bad_negative = {"model": "pricing-bad", "rates": {"input": -1, "output": 0, "cache_read": 0, "cache_write": 0}, "version": 0}
    check_eq(ctx.manager.put_price(bad_negative).status, 400, "negative rate rejected")
    bad_mode = {"model": "pricing-bad", "rates": _zero_rates(), "billing_mode": "token",
                "per_image_micro_usd": 5, "version": 0}
    check_eq(ctx.manager.put_price(bad_mode).status, 400, "a per-image rate requires per_image billing")
    unknown_mode = {"model": "pricing-bad", "rates": _zero_rates(), "billing_mode": "flat", "version": 0}
    check_eq(ctx.manager.put_price(unknown_mode).status, 400, "unknown billing mode rejected")

    # No price operation derives any cost or execution.
    summary = ctx.usage_rows(page=1, page_size=5)["summary"]
    check_eq(summary["cost_micro_usd"], 0, "saving prices derives no cost")
    check_eq(summary["executions"], 0, "saving prices executes nothing")
    check_eq(summary["unpriced_executions"], 0, "there is no execution to be unpriced")
    overview = ctx.manager.overview().json()
    check_eq(overview["summary"]["cost_micro_usd"], 0, "overview shows no derived cost")
    check_eq(overview["models"], [], "no model distribution exists without a request")
    return {"input": row["rates"]["input"], "version": _price(ctx, "pricing-big")["version"],
            "per_image": image_row["per_image_micro_usd"]}


@case("pricing.rules_tiers_preview_and_actual_model_billing",
      "rule context/service tiers resolve exactly in preview and a matching rule prices the actual usage model")
def rules_and_tiers(ctx):
    rule = {"id": "tier-rule", "match_kind": "exact", "pattern": "synthetic-chat", "priority": 9, "enabled": True,
            "price": {"model": "synthetic-chat", "rates": {"input": 2_000_000, "output": 6_000_000,
                                                           "cache_read": 0, "cache_write": 0},
                      "billing_mode": "token", "version": 0},
            "tiers": [{"kind": "context", "threshold": 1000, "label": "1K",
                       "rates": {"input": 4_000_000, "output": 0, "cache_read": 0, "cache_write": 0}},
                      {"kind": "service", "service": "priority",
                       "rates": {"input": 9_000_000, "output": 0, "cache_read": 0, "cache_write": 0}}]}
    saved = ctx.manager.put_pricing_rule(rule)
    check_eq(saved.status, 200, f"rule save {saved.text[:200]}")
    stored = saved.json()["item"]
    check_eq(stored["price"]["model"], "synthetic-chat", "rule price model follows the pattern")
    check_eq([t["kind"] for t in stored["tiers"]], ["context", "service"], "both tier kinds round-trip")
    check_eq(stored["tiers"][1]["service"], "priority", "service tier keeps its service field")

    listed = ctx.manager.pricing_rules().json().get("items") or []
    check(any(r["id"] == "tier-rule" for r in listed), "rule listed")

    # A context tier only applies once its threshold is reached.
    below = ctx.manager.price_preview(model="synthetic-chat", input_tokens=500).json()
    above = ctx.manager.price_preview(model="synthetic-chat", input_tokens=5000).json()
    check_eq(below["rates"]["input"], 2_000_000, "below the context threshold the base input rate applies")
    check_eq(above["rates"]["input"], 4_000_000, "at/above the threshold the context tier applies")
    check_eq(above["rule_id"], "tier-rule", "preview names the winning rule")

    # A service tier is selected by the requested service tier.
    priority = ctx.manager.price_preview(model="synthetic-chat", input_tokens=5000, service_tier="priority").json()
    check_eq(priority["rates"]["input"], 9_000_000, "service tier rate applies for the named service")
    check_eq(priority["service_tier"], "priority", "preview echoes the service tier")

    # The same rule prices a real execution by its actual usage model: the base
    # rate (the 1K context tier is not reached) gives 10 x 2 + 4 x 6 = 44.
    item, key = ctx.new_key(label="pricing-rule-live")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "rule-priced request accepted")
    report, request = _settled_row(ctx, item["id"])
    check(request is not None, "rule-priced request settled")
    check_eq(request["executions"][0]["cost_micro_usd"], 44,
             "the rule prices the actual usage through its base rates")
    check_eq(report["summary"]["unpriced_executions"], 0, "the execution is priced by the rule")
    check_no_ledger_fields(request, "rule-priced request")
    return {"base_input": below["rates"]["input"], "context_input": above["rates"]["input"],
            "service_input": priority["rates"]["input"],
            "live_cost": request["executions"][0]["cost_micro_usd"]}


@case("pricing.per_image_has_no_actual_count_and_stays_unpriced",
      "a per-image price never bills the requested n: SDK usage carries no image count, so the cost is null")
def per_image_unpriced(ctx):
    check_eq(ctx.manager.put_price({"model": "synthetic-free", "rates": _zero_rates(),
                                    "billing_mode": "per_image", "per_image_micro_usd": 50_000,
                                    "version": 0}).status, 200, "per-image price saved")
    item, key = ctx.new_key(label="pricing-image")
    response = ctx.client.post("/v1/chat/completions",
                               {"model": "synthetic-free",
                                "messages": [{"role": "user", "content": "synthetic"}], "n": 3},
                               {"Authorization": f"Bearer {key}"})
    check_eq(response.status, 200, "a per-image request is admitted and executed")
    check_eq(ctx.mock.count("/chat/completions"), 1, "per-image request reached upstream once")

    report, request = _settled_row(ctx, item["id"])
    check(request is not None, "per-image request settled")
    execution = request["executions"][0]
    check_eq(execution["quality"], "complete", "token usage is complete without an image count")
    check_none(execution["cost_micro_usd"],
               "the requested n is never treated as an actual image count")
    check_eq(report["summary"]["unpriced_executions"], 1, "the per-image execution is unpriced")
    check_eq(report["summary"]["cost_micro_usd"], 0, "no per-image charge is invented")
    check("image_count" not in request, "the request carries no image count fact")
    check_eq(ctx.mock.count("/chat/completions"), 1, "querying the report executes nothing")
    return {"status": response.status, "requested_n": 3,
            "cost_micro_usd": execution["cost_micro_usd"],
            "unpriced": report["summary"]["unpriced_executions"]}


@case("pricing.disabled_rule_never_blocks_execution",
      "a disabled matching rule is ignored for pricing and never acts as a model permission denial")
def disabled_rule(ctx):
    ctx.set_price("synthetic-chat")
    disabled = {"id": "disabled-price", "match_kind": "exact", "pattern": "synthetic-chat",
                "priority": 9, "enabled": False,
                "price": {"model": "synthetic-chat",
                          "rates": {"input": 3_000_000, "output": 3_000_000,
                                    "cache_read": 0, "cache_write": 0}}}
    check_eq(ctx.manager.put_pricing_rule(disabled).status, 200, "disabled rule saved")

    preview = ctx.manager.price_preview(model="synthetic-chat").json()
    check_eq(preview["rates"]["input"], 1_000_000,
             "a disabled rule does not participate in pricing")

    item, key = ctx.new_key(label="pricing-disabled-rule")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200,
             "the model is not denied by a disabled rule")
    report, request = _settled_row(ctx, item["id"])
    check(request is not None, "the request settled")
    check_eq(request["executions"][0]["cost_micro_usd"], 14,
             "the enabled price book entry prices the execution")

    # A model covered only by a disabled rule stays executable and unpriced.
    disabled_other = {"id": "disabled-other", "match_kind": "exact", "pattern": "synthetic-free",
                      "priority": 9, "enabled": False,
                      "price": {"model": "synthetic-free",
                                "rates": {"input": 5_000_000, "output": 5_000_000,
                                          "cache_read": 0, "cache_write": 0}}}
    check_eq(ctx.manager.put_pricing_rule(disabled_other).status, 200, "second disabled rule saved")
    other_item, other_key = ctx.new_key(label="pricing-disabled-unpriced")
    check_eq(ctx.client.chat(other_key, "synthetic-free").status, 200,
             "a disabled rule is not a permission blacklist")
    other_report, other_request = _settled_row(ctx, other_item["id"])
    check(other_request is not None, "the unpriced request settled")
    check_none(other_request["executions"][0]["cost_micro_usd"],
               "a disabled rule contributes no price")
    check_eq(other_report["summary"]["unpriced_executions"], 1, "the execution is unpriced")
    return {"status": 200, "priced_cost": request["executions"][0]["cost_micro_usd"],
            "unpriced_cost": other_request["executions"][0]["cost_micro_usd"]}
