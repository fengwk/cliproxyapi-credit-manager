"""Frontend authentication: candidate locations, order, denial and CPA guard."""

from __future__ import annotations

import json
from urllib.parse import urlencode

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _cost(ctx, key_id):
    return ctx.usage_rows(key_id=key_id, page_size=100)["summary"]["cost_micro_usd"]


def _body(model="synthetic-chat"):
    return {"model": model, "messages": [{"role": "user", "content": "synthetic"}]}


def _probe_locations(ctx, key):
    """Issue one request per native credential location, returning its status."""
    body = _body()
    return {
        "bearer": ctx.client.post("/v1/chat/completions", body, {"Authorization": f"Bearer {key}"}).status,
        "x-goog-api-key": ctx.client.post("/v1/chat/completions", body, {"x-goog-api-key": key}).status,
        "x-api-key": ctx.client.post("/v1/chat/completions", body, {"x-api-key": key}).status,
        "query-key": ctx.client.post(f"/v1/chat/completions?{urlencode({'key': key})}", body, {}).status,
        "query-auth-token": ctx.client.post(f"/v1/chat/completions?{urlencode({'auth_token': key})}", body, {}).status,
    }


def _expect_denied(ctx, status, message):
    check_eq(status, 401, message)


@case("auth.candidate_locations", "all five credential locations authenticate the same key")
def candidate_locations(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="auth-locations")
    results = {}
    # 1. Authorization: Bearer
    results["bearer"] = ctx.client.post("/v1/chat/completions", _body(), {"Authorization": f"Bearer {key}"}).status
    # 2. X-Goog-Api-Key
    results["x-goog-api-key"] = ctx.client.post("/v1/chat/completions", _body(), {"x-goog-api-key": key}).status
    # 3. X-Api-Key
    results["x-api-key"] = ctx.client.post("/v1/chat/completions", _body(), {"x-api-key": key}).status
    # 4. query key
    results["query-key"] = ctx.client.post(f"/v1/chat/completions?{urlencode({'key': key})}", _body(), {}).status
    # 5. query auth_token
    results["query-auth-token"] = ctx.client.post(f"/v1/chat/completions?{urlencode({'auth_token': key})}", _body(), {}).status
    for location, status in results.items():
        check_eq(status, 200, f"candidate location {location}")
    check_eq(ctx.mock.count("/chat/completions"), 5, "each accepted candidate reached upstream once")
    # All five derive the same key's cost: 5 * 14.
    report = ctx.wait_report(item["id"], settled=5)
    check_eq(report["summary"]["cost_micro_usd"], 70, "five calls derive 70 microUSD")
    return {"locations": results, "cost": report["summary"]["cost_micro_usd"]}


@case("auth.invalid_and_absent_denied", "invalid or absent credentials are denied with no upstream call")
def invalid_absent(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="auth-invalid")
    invalid = ctx.client.post("/v1/chat/completions", _body(), {"Authorization": "Bearer synthetic-invalid-key"})
    _expect_denied(ctx, invalid.status, "invalid bearer denied")
    absent = ctx.client.post("/v1/chat/completions", _body(), {})
    _expect_denied(ctx, absent.status, "absent credentials denied")
    goog_invalid = ctx.client.post("/v1/chat/completions", _body(), {"x-goog-api-key": "synthetic-invalid"})
    _expect_denied(ctx, goog_invalid.status, "invalid google key denied")
    check_eq(ctx.mock.count("/chat/completions"), 0, "denied requests must not reach upstream")
    check_eq(_cost(ctx, item["id"]), 0, "no cost on denied requests")
    return {"invalid": invalid.status, "absent": absent.status, "goog_invalid": goog_invalid.status}


@case("auth.invalid_bearer_valid_google", "an invalid Bearer falls through to a valid Google key in the same request")
def invalid_bearer_valid_google(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="auth-google-fallback")
    response = ctx.client.post(
        "/v1/chat/completions", _body(),
        {"Authorization": "Bearer synthetic-invalid-bearer", "x-goog-api-key": key},
    )
    check_eq(response.status, 200, "valid google key accepted despite invalid bearer")
    check_eq(ctx.mock.count("/chat/completions"), 1, "one upstream call")
    check_eq(ctx.wait_report(item["id"], settled=1)["summary"]["cost_micro_usd"], 14,
             "priced to the valid google key")
    return {"status": response.status, "cost": _cost(ctx, item["id"])}


@case("auth.conflicting_first_wins", "two valid keys in one request bill only the first in native candidate order")
def conflicting_first_wins(ctx):
    ctx.set_price("synthetic-chat")
    item_a, key_a = ctx.new_key(label="auth-conflict-a")
    item_b, key_b = ctx.new_key(label="auth-conflict-b")
    # Authorization Bearer is candidate 1, X-Goog-Api-Key is candidate 2.
    response = ctx.client.post(
        "/v1/chat/completions", _body(),
        {"Authorization": f"Bearer {key_a}", "x-goog-api-key": key_b},
    )
    check_eq(response.status, 200, "request accepted")
    check_eq(ctx.wait_report(item_a["id"], settled=1)["summary"]["cost_micro_usd"], 14,
             "first candidate (bearer) derives the cost")
    check_eq(_cost(ctx, item_b["id"]), 0, "second candidate must not be charged")
    # Reverse the order: the google key is still candidate 2, so bearer B wins.
    response = ctx.client.post(
        "/v1/chat/completions", _body(),
        {"Authorization": f"Bearer {key_b}", "x-goog-api-key": key_a},
    )
    check_eq(response.status, 200, "reversed request accepted")
    check_eq(ctx.wait_report(item_b["id"], settled=1)["summary"]["cost_micro_usd"], 14,
             "reversed bearer derives the cost")
    check_eq(_cost(ctx, item_a["id"]), 14, "a must remain at its single derived charge")
    return {"a_cost": _cost(ctx, item_a["id"]), "b_cost": _cost(ctx, item_b["id"])}


@case("auth.original_cpa_key_rejected", "the original CPA access.api-keys client key is rejected while the plugin is exclusive")
def original_cpa_guard(ctx):
    ctx.set_price("synthetic-chat")
    response = ctx.client.post("/v1/chat/completions", _body(), {"Authorization": f"Bearer {ctx.original_client_key}"})
    _expect_denied(ctx, response.status, "original CPA key must not fall back when the plugin is exclusive")
    check_eq(ctx.mock.count("/chat/completions"), 0, "no upstream call for the rejected original key")
    return {"status": response.status}


@case("auth.custom_prefix_key", "a client key with a custom (non sk-) prefix authenticates")
def custom_prefix(ctx):
    ctx.set_price("synthetic-chat")
    # The plugin accepts any material >= 8 chars; it never requires the sk- prefix.
    item, key = ctx.new_key(label="auth-custom", key="custom-prefix-material-0001")
    check_eq(key, "custom-prefix-material-0001", "one-time plaintext echoes the custom material")
    response = ctx.client.post("/v1/chat/completions", _body(), {"Authorization": f"Bearer {key}"})
    check_eq(response.status, 200, "custom-prefix key accepted")
    check_eq(ctx.wait_report(item["id"], settled=1)["summary"]["cost_micro_usd"], 14,
             "custom key derives its cost")
    return {"status": response.status, "key_prefix": key[:12]}


@case("auth.plugin_absent_denies_plugin_keys",
      "a plugin-managed key is only valid while the plugin is loaded: with plugins disabled every credential location is denied with no upstream call")
def plugin_absent_denies(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="deploy-guard")

    # 1. While the plugin is loaded its auth hook injects this key, so it is
    #    accepted at every native credential location and billed normally.
    loaded = _probe_locations(ctx, key)
    for location, status in loaded.items():
        check_eq(status, 200, f"with the plugin loaded, {location} accepts a plugin key")
    check_eq(ctx.mock.count("/chat/completions"), 5, "each accepted location reached upstream once")
    check_eq(ctx.wait_report(item["id"], settled=5)["summary"]["cost_micro_usd"], 70,
             "the plugin key derives cost while loaded")

    # 2. Rebuild the same deployment with the plugin disabled (plugins.enabled
    #    false, library not loaded) while the original CPA access.api-keys guard
    #    stays configured. The plugin key is no longer injected and must be
    #    denied everywhere. The native guard key is deliberately never used here:
    #    this case asserts the denial of a *plugin* key.
    root = ctx.host.config.root
    ctx.mock.reset()
    ctx.recreate_host("deploy-plugin-absent", root=root, enable_plugins=False)

    absent = _probe_locations(ctx, key)
    for location, status in absent.items():
        check(status in (401, 403),
              f"with the plugin absent, {location} must deny the plugin key, got {status}")
    check_eq(ctx.mock.count(), 0, "denied requests must not reach upstream")

    # The plugin really is gone from this deployment: the host reports plugins
    # disabled and the instance neither registered nor effective, and the
    # plugin's own management API is not served at all.
    status, raw = ctx.host.management("GET", "/v0/management/plugins")
    payload = json.loads(raw) if status == 200 else {}
    entry = next((e for e in payload.get("plugins") or [] if e.get("id") == ctx.plugin_id), None)
    check_eq(payload.get("plugins_enabled"), False, "plugins are disabled in this deployment")
    if entry is not None:
        check_eq(entry.get("registered"), False, "the plugin must not be registered when disabled")
        check_eq(entry.get("effective_enabled"), False, "the plugin must not be effective when disabled")
    check_eq(ctx.host.management("GET", "/v0/management/plugins/credit-manager/api/health")[0], 404,
             "the plugin management API is not served when the plugin is disabled")
    return {"plugin_loaded": loaded, "plugin_absent": absent,
            "plugins_enabled": payload.get("plugins_enabled"),
            "registered": (entry or {}).get("registered"),
            "effective_enabled": (entry or {}).get("effective_enabled"),
            "upstream_calls_when_disabled": ctx.mock.count()}
