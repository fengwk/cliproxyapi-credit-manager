"""Frontend authentication: candidate locations, order, denial and CPA guard."""

from __future__ import annotations

from urllib.parse import urlencode

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _spent(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    row = next((k for k in items if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


def _body(model="synthetic-chat"):
    return {"model": model, "messages": [{"role": "user", "content": "synthetic"}]}


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
    # All five bill the same key: 5 * 14.
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 70), f"key spent after 5 calls: {_spent(ctx, item['id'])}")
    return {"locations": results, "key_spent": _spent(ctx, item["id"])}


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
    check_eq(_spent(ctx, item["id"]), 0, "no billing on denied requests")
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
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "billed to the valid google key")
    return {"status": response.status, "key_spent": _spent(ctx, item["id"])}


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
    check(ctx.wait_until(lambda: _spent(ctx, item_a["id"]) == 14), "first candidate (bearer) billed")
    check_eq(_spent(ctx, item_b["id"]), 0, "second candidate must not be billed")
    # Reverse the order: the google key is still candidate 2, so bearer B wins.
    response = ctx.client.post(
        "/v1/chat/completions", _body(),
        {"Authorization": f"Bearer {key_b}", "x-goog-api-key": key_a},
    )
    check_eq(response.status, 200, "reversed request accepted")
    check(ctx.wait_until(lambda: _spent(ctx, item_b["id"]) == 14), "reversed bearer billed")
    check_eq(_spent(ctx, item_a["id"]), 14, "a must remain at its single charge")
    return {"a_spent": _spent(ctx, item_a["id"]), "b_spent": _spent(ctx, item_b["id"])}


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
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "custom key billed")
    return {"status": response.status, "key_prefix": key[:12]}
