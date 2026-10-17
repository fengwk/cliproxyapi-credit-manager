"""Upstream credential retry / failover.

The host transparently retries a failing upstream credential so the client
issues exactly one request. The plugin records *independent* usage statistics
per attempt: the failed attempt is an unknown-cost execution (never written off
as free), the successful attempt is charged. Because a request carries one
unknown execution, it stays pending -- the host must never settle unknown
accounting as zero, and nothing is held or estimated while it is unresolved.
"""

from __future__ import annotations

from ..report import check, check_eq, check_no_holds, check_no_ledger_fields

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _key(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    return next((k for k in items if k["id"] == key_id), None)


def _request(ctx, key_id):
    return ctx.wait_until(lambda: ctx.find_request(key_id=key_id), timeout=15)


@case("retry.upstream_failover_one_client_request",
      "a 429 on the first upstream credential fails over; the plugin bills exactly once and counts both attempts")
def failover(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="retry-failover")
    # The very first upstream call answers 429; the retry (other credential) succeeds.
    ctx.mock.fail_first = 1
    ctx.mock.fail_status = 429

    response = ctx.client.chat(key, "synthetic-chat")
    check_eq(response.status, 200, "client sees success after transparent failover")
    check(b"synthetic answer" in response.raw, "answer text present")

    calls = ctx.mock.calls_for("/chat/completions")
    check_eq(len(calls), 2, "host retried the upstream exactly once")
    credentials = {call["upstream_key"] for call in calls}
    check_eq(len(credentials), 2, f"failover used two distinct upstream credentials: {credentials}")
    for call in calls:
        check(call["upstream_key"] in ctx.openai_upstream_keys, f"unexpected upstream key {call['upstream_key']}")
        check(call["upstream_key"] != key, "client key leaked upstream")

    # Exactly one client request is accounted, with independent per-attempt usage.
    report = ctx.usage_rows(key_id=item["id"])
    check_eq(report["total"], 1, "one client request accounted, not two")
    check_eq(report["summary"]["executions"], 2, "usage statistics are per attempt (2 attempts)")
    request = _request(ctx, item["id"])
    check(request is not None, "request recorded")
    executions = request["executions"]
    check_eq(len(executions), 2, "two execution records for the two attempts")
    failed = [ex for ex in executions if ex["failed"]]
    success = [ex for ex in executions if not ex["failed"]]
    check_eq(len(failed), 1, "one failed attempt")
    check_eq(len(success), 1, "one successful attempt")
    check_eq(failed[0].get("status_code"), 429, "failed attempt records the upstream status")
    check_eq(failed[0]["quality"], "unknown", "failed attempt accounting is unknown")
    check(failed[0].get("cost_micro_usd") is None, "failed attempt is not written off as a zero charge")
    check_eq(success[0]["quality"], "complete", "successful attempt accounting is complete")
    check_eq(success[0]["cost_micro_usd"], 14, "successful attempt charged exactly 14")
    check(failed[0]["auth_id"] != success[0]["auth_id"], "attempts used different upstream credentials")
    # The unknown failed attempt keeps the request open, never free and never held.
    check_eq(request["outcome"], "succeeded", "request outcome is succeeded")
    check_eq(request["state"], "pending", "unknown attempt keeps the request pending")
    check_no_holds(request, "pending request with an unknown attempt")
    summary = ctx.wait_report(item["id"])["summary"]
    check_eq(summary["cost_micro_usd"], 14, "only the successful attempt is priced, not twice")
    check_eq(summary["complete_executions"], 1, "one complete usage drives the cost")
    check_no_holds(_key(ctx, item["id"]), "key row")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"upstream_calls": len(calls), "credentials": sorted(credentials),
            "client_requests": report["total"], "summary": summary,
            "request_state": request["state"],
            "attempts": [{"failed": ex["failed"], "quality": ex["quality"], "cost": ex.get("cost_micro_usd"),
                          "status": ex.get("status_code")} for ex in executions]}


@case("retry.all_upstream_failures_do_not_free",
      "when every upstream attempt fails the client gets an error and nothing is billed as free")
def all_failures(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="retry-all-fail")
    ctx.mock.fail_first = 99
    ctx.mock.fail_status = 429

    response = ctx.client.chat(key, "synthetic-chat", timeout=40)
    check(response.status >= 400, f"client sees an upstream error, got {response.status}")
    check(ctx.mock.count("/chat/completions") >= 1, "at least one upstream attempt reached the mock")
    summary = ctx.usage_rows(key_id=item["id"], page_size=50)["summary"]
    check_eq(summary["cost_micro_usd"], 0, "no cost exists when the request never succeeded")
    request = ctx.find_request(key_id=item["id"])
    if request is not None:
        check(request["state"] != "settled" or request["outcome"] != "succeeded",
              "a fully failed request must not settle as a priced success")
        check(not any(ex.get("cost_micro_usd") for ex in request["executions"]),
              "no execution may carry a cost for a fully failed request")
        check_no_holds(request, "fully failed request")
        check_no_ledger_fields(request, "fully failed request")
    return {"status": response.status, "upstream_calls": ctx.mock.count("/chat/completions"),
            "summary": summary}
