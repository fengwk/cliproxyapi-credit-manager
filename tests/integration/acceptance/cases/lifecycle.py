"""Lifecycle: unknown usage, client cancel, and durable balance across restart."""

from __future__ import annotations

import http.client
import json
import socket
import time
from pathlib import Path
from urllib.parse import urlparse

from ..mock import Script
from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _key(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    return next((k for k in items if k["id"] == key_id), None)


def _spent(ctx, key_id):
    row = _key(ctx, key_id)
    return row["spent_micro_usd"] if row else None


def _held(ctx, key_id):
    row = _key(ctx, key_id)
    return row["held_micro_usd"] if row else None


@case("lifecycle.unknown_usage_stays_pending",
      "a completion without usage never settles as a free success; the hold is retained")
def unknown_usage(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="lifecycle-unknown")
    ctx.mock.default_script = Script(omit_usage=True)

    response = ctx.client.chat(key, "synthetic-chat")
    check_eq(response.status, 200, "client still receives a completion")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "request recorded")
    # Allow a brief settle window; it must NOT settle.
    time.sleep(0.5)
    request = ctx.find_request(key_id=item["id"])
    check(request["state"] != "settled", f"unknown usage must not settle: {request['state']}")
    check_eq(request["outcome"], "succeeded", "outcome is succeeded but accounting is unknown")
    check(request["held_micro_usd"] > 0, "hold retained for unknown usage")
    check_eq(_spent(ctx, item["id"]), 0, "unknown usage is not billed as zero")
    report = ctx.usage_rows(key_id=item["id"])
    check(report["summary"]["pending"] >= 1, f"pending surfaced in report: {report['summary']}")
    check_eq(report["summary"]["complete_executions"], 0, "no complete execution recorded")
    return {"state": request["state"], "held": request["held_micro_usd"], "spent": _spent(ctx, item["id"])}


@case("lifecycle.client_cancel_pending",
      "an aborted client request does not hang or free the hold; the request stays pending")
def client_cancel(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="lifecycle-cancel")
    ctx.mock.default_script = Script(delay=1.5)

    parsed = urlparse(ctx.host.config.base_url)
    connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=0.4)
    body = json.dumps({"model": "synthetic-chat",
                       "messages": [{"role": "user", "content": "synthetic"}], "stream": True})
    started = time.monotonic()
    try:
        connection.request("POST", "/v1/chat/completions", body=body,
                           headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        response = connection.getresponse()
        response.read(1)
    except (socket.timeout, TimeoutError, OSError, http.client.HTTPException):
        pass
    finally:
        connection.close()
    elapsed = time.monotonic() - started
    check(elapsed < 3.0, f"client cancel must not hang (elapsed {elapsed:.2f}s)")

    # The request reached the upstream mock before cancellation.
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "cancelled request recorded")
    time.sleep(0.8)
    request = ctx.find_request(key_id=item["id"])
    check(request["state"] != "settled", f"cancelled request must not settle free: {request['state']}")
    check(request["held_micro_usd"] > 0, "hold retained after cancel (nothing written off)")
    check_eq(_spent(ctx, item["id"]), 0, "cancel does not bill")
    return {"elapsed_s": round(elapsed, 3), "state": request["state"],
            "held": request["held_micro_usd"], "upstream_calls": ctx.mock.count("/chat/completions")}


@case("lifecycle.restart_retains_balance",
      "a restarted host keeps the settled balance and keeps unfinished work pending")
def restart_retains(ctx):
    root = Path(ctx.artifacts) / "host-restart"
    ctx.recreate_host("restart-a", root=root)
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="lifecycle-restart")

    # One settled success (14) plus one unknown/pending request.
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "settle request accepted")
    ctx.mock.default_script = Script(omit_usage=True)
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "pending request accepted")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "first request settled at 14")
    report_before = ctx.usage_rows(key_id=item["id"])
    pending_id = next((row["id"] for row in report_before["items"] if row["state"] != "settled"), None)
    check(pending_id is not None, f"a pending request exists before restart: {report_before['items']}")

    # Restart against the same data_dir.
    ctx.recreate_host("restart-b", root=root)
    check_eq(ctx.manager.health().json()["status"], "ok", "health ok after restart")
    check_eq(_spent(ctx, item["id"]), 14, "settled balance survives restart")
    report_after = ctx.usage_rows(key_id=item["id"])
    check_eq(report_after["summary"]["cost_micro_usd"], 14, "durable cost after restart")
    pending_after = next((row for row in report_after["items"] if row["id"] == pending_id), None)
    check(pending_after is not None, "unfinished request row survives restart")
    check(pending_after["state"] != "settled", f"unfinished work stays pending, not free: {pending_after['state']}")
    check(pending_after["held_micro_usd"] > 0, "pending hold retained across restart")
    return {"spent_after": _spent(ctx, item["id"]), "pending_state": pending_after["state"],
            "pending_held": pending_after["held_micro_usd"]}
