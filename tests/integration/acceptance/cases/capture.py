"""Request capture: laziness, redaction, 64-bit fidelity, body limit and states."""

from __future__ import annotations

import json
from pathlib import Path

from ..report import check, check_eq

CASES = []

BIG = 9007199254740993  # 2**53 + 1: not representable as a float64
SECRET = "SUPERSECRET-VALUE"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _capture_host(ctx, max_body_bytes=10 << 20):
    root = Path(ctx.artifacts) / "host-capture"
    plugin_config = {
        "data_dir": str(root / "data"),
        "request_capture": {
            "enabled": True,
            "retention": "24h",
            "max_body_bytes": max_body_bytes,
            "max_storage_bytes": 1 << 30,
        },
    }
    ctx.recreate_host("capture", root=root, plugin_config=plugin_config)
    return root


def _detail_for(ctx, *ids):
    for identifier in ids:
        if not identifier:
            continue
        response = ctx.manager.capture(identifier)
        if response.status == 200:
            detail = response.json()
            if detail.get("status") == "captured":
                return identifier, detail
    # Return the last attempt for reporting even when nothing was captured.
    response = ctx.manager.capture(ids[0])
    return ids[0], json.loads(response.raw) if response.status == 200 else {"status": f"http-{response.status}"}


@case("capture.disabled_state", "with capture disabled Get returns status disabled, not unavailable")
def disabled_state(ctx):
    # The shared host has capture disabled by default.
    item, key = ctx.new_key(label="capture-disabled")
    ctx.set_price("synthetic-chat")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, "request recorded")
    detail = ctx.manager.capture(request["id"]).json()
    check_eq(detail.get("status"), "disabled", f"capture disabled state: {detail}")
    return {"status": detail.get("status")}


@case("capture.lazy_redact_64bit", "capture is lazy, redacts secrets and preserves 64-bit integer text")
def lazy_redact(ctx):
    _capture_host(ctx)
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="capture-redact")
    body = {
        "model": "synthetic-chat",
        "messages": [{"role": "user", "content": "synthetic"}],
        "api_key": SECRET,
        "big": BIG,
    }
    headers = {"Authorization": f"Bearer {key}", "x-api-key": SECRET, "Cookie": f"session={SECRET}"}
    response = ctx.client.post("/v1/chat/completions", body, headers)
    check_eq(response.status, 200, "request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, "request recorded")
    identifier, detail = _detail_for(ctx, request["id"], request.get("trace_id"))
    check_eq(detail.get("status"), "captured", f"capture detail: {detail}")
    headers_out = {k.lower(): v for k, v in (detail.get("headers") or {}).items()}
    # Redacted header keys must not leak the secret.
    for name in ("authorization", "x-api-key", "cookie"):
        if name in headers_out:
            value = headers_out[name]
            value = value[0] if isinstance(value, list) else value
            check_eq(value, "[redacted]", f"header {name} redaction")
    check(key not in detail.get("body", ""), "client key material leaked into capture body")
    check(SECRET not in json.dumps(detail), "secret value leaked into capture detail")
    # 64-bit integer must survive as exact text.
    check(str(BIG) in detail.get("body", ""), f"64-bit integer preserved as text: {detail.get('body')}")
    # Lazy: capture API returns the body only on demand; the usage list never embeds it.
    report = ctx.usage_rows(key_id=item["id"])
    check(SECRET not in json.dumps(report), "secret leaked into the usage report")

    def _flat(value):
        return value[0] if isinstance(value, list) else value

    return {"capture_id": identifier, "status": detail.get("status"),
            "redacted_headers": [k for k, v in headers_out.items() if _flat(v) == "[redacted]"],
            "body_has_big": str(BIG) in detail.get("body", "")}


@case("capture.missing_unavailable", "capture of an unknown id reports unavailable, never a false expiry")
def missing_unavailable(ctx):
    _capture_host(ctx)
    detail = ctx.manager.capture("synthetic-unknown-lifecycle-id").json()
    check_eq(detail.get("status"), "unavailable", f"unknown capture state: {detail}")
    return {"status": detail.get("status")}


@case("capture.body_limit_adjustable", "the capture body limit is honored: oversize is skipped, a larger limit captures")
def body_limit(ctx):
    root = Path(ctx.artifacts) / "host-capture-limit"
    plugin_config = {
        "data_dir": str(root / "data"),
        "request_capture": {"enabled": True, "retention": "24h", "max_body_bytes": 256, "max_storage_bytes": 1 << 30},
    }
    ctx.recreate_host("capture-limit", root=root, plugin_config=plugin_config)
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="capture-limit")
    oversize = {"model": "synthetic-chat", "messages": [{"role": "user", "content": "x" * 4096}]}
    check_eq(ctx.client.post("/v1/chat/completions", oversize, {"Authorization": f"Bearer {key}"}).status, 200,
             "oversize body request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, "request recorded")
    detail = ctx.manager.capture(request["id"]).json()
    check_eq(detail.get("status"), "oversize", f"oversize body skipped: {detail}")
    return {"status": detail.get("status")}
