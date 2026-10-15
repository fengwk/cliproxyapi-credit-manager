"""Native Gemini generateContent / streamGenerateContent through the plugin."""

from __future__ import annotations

from ..report import check, check_eq

CASES = []

MODEL = "gemini-2.5-flash"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _spent(ctx, key_id):
    row = next((k for k in ctx.manager.keys().json()["items"] if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


@case("gemini.native_generate", "native Gemini generateContent authenticates, forwards the upstream key and bills")
def generate(ctx):
    ctx.set_price(MODEL)
    item, key = ctx.new_key(label="gemini-generate")
    response = ctx.client.gemini(key, MODEL, "generateContent")
    check_eq(response.status, 200, "generateContent status")
    check(b"synthetic answer" in response.raw, "gemini answer present")
    calls = ctx.mock.calls_for("/v1beta")
    check_eq(len(calls), 1, "one gemini upstream call")
    check_eq(calls[0]["upstream_key"], ctx.gemini_upstream_keys[0], "upstream gemini credential used")
    for call in ctx.mock.calls_for("/v1beta"):
        check(call["upstream_key"] != key, "client key leaked upstream")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "gemini request billed 14")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check_eq(request["executions"][0]["cost_micro_usd"], 14, "execution cost")
    return {"status": response.status, "key_spent": _spent(ctx, item["id"])}


@case("gemini.native_stream", "native Gemini streamGenerateContent (alt=sse) bills through the plugin")
def stream(ctx):
    ctx.set_price(MODEL)
    item, key = ctx.new_key(label="gemini-stream")
    response = ctx.client.gemini(key, MODEL, "streamGenerateContent")
    check_eq(response.status, 200, "streamGenerateContent status")
    calls = ctx.mock.calls_for("/v1beta")
    check_eq(len(calls), 1, "one gemini streaming upstream call")
    check(calls[0]["stream"], "upstream call was streaming")
    check_eq(calls[0]["upstream_key"], ctx.gemini_upstream_keys[0], "upstream gemini credential used")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14, timeout=20), "gemini stream billed 14")
    return {"status": response.status, "key_spent": _spent(ctx, item["id"])}
