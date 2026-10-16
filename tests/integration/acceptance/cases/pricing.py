"""Structured pricing: nested rates, exact 64-bit fidelity, rules, tiers, per-image.

The aligned price contract nests the rate sheet under `rates` (all int64 microUSD
per million tokens), keeps `billing_mode`/`per_image_micro_usd` on the price, and
puts context/service tiers on a rule. Saving a price or a rule must never touch
the monetary ledger; only an admitted request is billed.
"""

from __future__ import annotations

from ..report import check, check_eq

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


@case("pricing.nested_rates_signed64_roundtrip_without_ledger",
      "nested rates keep exact 64-bit values across save/reload and saving a price never consumes the ledger")
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

    # Re-saving bumps the stored version, never the ledger.
    check_eq(ctx.manager.put_price(payload).status, 200, "price re-save accepted")
    check_eq(_price(ctx, "pricing-big")["version"], 2, "re-save increments the version")

    # A per-image price keeps its flat rate and billing mode.
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

    # No price operation consumed any ledger.
    summary = ctx.usage_rows(page=1, page_size=5)["summary"]
    check_eq(summary["cost_micro_usd"], 0, "saving prices consumes no ledger cost")
    check_eq(summary["executions"], 0, "saving prices executes nothing")
    overview = ctx.manager.overview().json()
    check_eq(overview["summary"]["cost_micro_usd"], 0, "overview shows no consumption")
    check_eq(overview["models"], [], "no model distribution exists without a request")
    return {"input": row["rates"]["input"], "version": _price(ctx, "pricing-big")["version"],
            "per_image": image_row["per_image_micro_usd"]}


@case("pricing.rules_context_service_tiers_and_per_image_billing",
      "rule context/service tiers resolve exactly, and a native chat can bill per_image while an out-of-range image count is denied before upstream")
def rules_and_per_image(ctx):
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

    # A per-image price bills a native chat by the requested image count.
    check_eq(ctx.manager.put_price({"model": "synthetic-free", "rates": _zero_rates(),
                                    "billing_mode": "per_image", "per_image_micro_usd": 50_000,
                                    "version": 0}).status, 200, "per-image price saved")
    item, key = ctx.new_key(label="pricing-image")
    check_eq(ctx.client.post("/v1/chat/completions",
                             {"model": "synthetic-free", "messages": [{"role": "user", "content": "synthetic"}], "n": 3},
                             {"Authorization": f"Bearer {key}"}).status, 200, "per-image request accepted")
    check_eq(ctx.mock.count("/chat/completions"), 1, "per-image request reached upstream once")
    request = ctx.wait_until(lambda: (r := ctx.find_request(key_id=item["id"])) and (r if r["state"] == "settled" else None), timeout=15)
    check(request is not None, "per-image request settled")
    check_eq(request["executions"][0]["cost_micro_usd"], 150_000, "3 images bill at 3 x the flat rate")
    check_eq(_key_spent(ctx, item["id"]), 150_000, "key ledger matches the per-image charge")

    # Declared image counts are refused for a per-image price before any
    # upstream call: 100 images is outside the supported 1..32 range.
    upstream_before = ctx.mock.count("/chat/completions")
    denied = ctx.client.post("/v1/chat/completions",
                             {"model": "synthetic-free", "messages": [{"role": "user", "content": "synthetic"}], "n": 100},
                             {"Authorization": f"Bearer {key}"})
    check(400 <= denied.status < 500, f"an out-of-range image count is a client error: {denied.status}")
    check_eq(ctx.mock.count("/chat/completions"), upstream_before, "the denied request never reaches upstream")
    report = ctx.usage_rows(key_id=item["id"], page_size=50)
    check_eq(report["summary"]["requests"], 1, "the denied request is never admitted as a ledger row")
    return {"base_input": below["rates"]["input"], "context_input": above["rates"]["input"],
            "service_input": priority["rates"]["input"], "per_image_cost": request["executions"][0]["cost_micro_usd"],
            "denied": denied.status}


def _key_spent(ctx, key_id):
    return next((row["spent_micro_usd"] for row in ctx.manager.keys().json()["items"] if row["id"] == key_id), None)
