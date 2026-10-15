"""Native CPA session-affinity scheduling.

The plugin must not replace or weaken the host's own session-aware credential
selection: with ``routing.session-affinity: true`` (and the default round-robin
strategy) the host keeps a session's requests on one credential.

Two independent paths are asserted, each with two synthetic upstream
credentials configured so "one credential" is a real claim and not a
same-provider-entry coincidence:

* an **explicit** ``X-Session-ID`` header shared by three requests whose user
  content differs every time (so the binding can not be a payload-hash
  artefact);
* the host's **longest-common-prefix** conversation binding with no explicit
  session header at all: the system+user prefix is stable and each request
  appends the previous assistant answer and a new user turn.

The host contract exercised here lives in
``sdk/cliproxy/auth/selector_lcp_test.go``
(``TestSessionAffinitySelectorLCPPreservesBindingAcrossConversationGrowth``),
and the config keys are ``routing.session-affinity`` /
``routing.session-affinity-ttl`` (not nested under ``routing.retry``).
"""

from __future__ import annotations

from ..report import check, check_eq

CASES = []

MODEL = "synthetic-chat"
SESSION_HEADER = "X-Session-ID"
SESSION_ID = "synthetic-affinity"
EXPECTED_COST = 42  # three requests at the exact 14 microUSD synthetic cost


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


# -- helpers ---------------------------------------------------------------

def _items(ctx, key_id):
    return ctx.usage_rows(key_id=key_id).get("items") or []


def _spent(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    row = next((k for k in items if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


def _collect(ctx, key_id, count):
    """Wait for `count` accounted requests and return (auth_ids, costs)."""
    items = ctx.wait_until(
        lambda: (_items(ctx, key_id) if len(_items(ctx, key_id)) >= count else None), timeout=15)
    check(items is not None, f"expected {count} accounted requests, got {len(_items(ctx, key_id))}")
    check_eq(len(items), count, "exactly the expected number of customer requests is accounted")
    executions = [item["executions"][0] for item in items if item.get("executions")]
    check_eq(len(executions), count, "every request recorded an execution")
    return [ex["auth_id"] for ex in executions], [ex.get("cost_micro_usd") for ex in executions]


def _assert_single_credential(ctx, key_id, auth_ids, costs, mock_calls, label):
    check_eq(len(set(auth_ids)), 1, f"{label}: all requests used one credential: {auth_ids}")
    upstream = {call["upstream_key"] for call in mock_calls}
    check_eq(len(upstream), 1, f"{label}: all requests used one upstream key: {sorted(upstream)}")
    check(upstream <= set(ctx.openai_upstream_keys),
          f"{label}: upstream key is a synthetic credential: {sorted(upstream)}")
    check_eq(sum(cost for cost in costs if cost), EXPECTED_COST,
             f"{label}: three requests cost {EXPECTED_COST} in total: {costs}")
    check_eq(_spent(ctx, key_id), EXPECTED_COST, f"{label}: the client key was charged {EXPECTED_COST}")


def _post(ctx, key, messages, extra_headers=None):
    headers = {"Authorization": f"Bearer {key}"}
    headers.update(extra_headers or {})
    return ctx.client.post("/v1/chat/completions",
                           {"model": MODEL, "stream": False, "messages": messages}, headers)


# -- cases -----------------------------------------------------------------

@case("affinity.explicit_session_header_binds_credential",
      "an explicit X-Session-ID keeps three round-robin requests on one credential with two credentials available")
def explicit_session_header(ctx):
    ctx.recreate_host("affinity-explicit", session_affinity=True)
    ctx.set_price(MODEL)
    check_eq(len(ctx.openai_upstream_keys), 2, "two upstream credentials are configured")
    item, key = ctx.new_key(label="affinity-explicit")

    ctx.mock.reset()
    # Same session header, different user content each time: the binding must
    # come from the session, not from a repeated payload.
    contents = ("synthetic-turn-1", "synthetic-turn-2", "synthetic-turn-3")
    for index, content in enumerate(contents, start=1):
        response = _post(ctx, key, [{"role": "user", "content": content}],
                         {SESSION_HEADER: SESSION_ID})
        check_eq(response.status, 200, f"affinity request {index} succeeds")

    calls = ctx.mock.calls_for("/chat/completions")
    check_eq(len(calls), 3, "three upstream calls")
    auth_ids, costs = _collect(ctx, item["id"], 3)
    _assert_single_credential(ctx, item["id"], auth_ids, costs, calls, "explicit session header")
    return {"session_header": SESSION_HEADER, "session_id": SESSION_ID,
            "auth_ids": auth_ids, "upstream_keys": sorted({c["upstream_key"] for c in calls}),
            "requests": len(auth_ids), "costs": costs, "key_spent": _spent(ctx, item["id"])}


@case("affinity.lcp_conversation_growth_binds_credential",
      "with no explicit session header, a grown conversation prefix keeps the same credential with two credentials available")
def lcp_conversation_growth(ctx):
    ctx.recreate_host("affinity-lcp", session_affinity=True)
    ctx.set_price(MODEL)
    check_eq(len(ctx.openai_upstream_keys), 2, "two upstream credentials are configured")
    item, key = ctx.new_key(label="affinity-lcp")

    ctx.mock.reset()
    # A stable system+user prefix; each request appends the previous assistant
    # answer and one new user turn, so the longest common prefix grows.
    stable = {"role": "system", "content": "synthetic-stable-system"}
    first = [stable, {"role": "user", "content": "synthetic-first"}]
    second = first + [{"role": "assistant", "content": "synthetic-answer-1"},
                      {"role": "user", "content": "synthetic-continue"}]
    third = second + [{"role": "assistant", "content": "synthetic-answer-2"},
                      {"role": "user", "content": "synthetic-next"}]
    for index, messages in enumerate((first, second, third), start=1):
        response = _post(ctx, key, messages)  # no session header at all
        check_eq(response.status, 200, f"conversation growth request {index} succeeds")

    calls = ctx.mock.calls_for("/chat/completions")
    check_eq(len(calls), 3, "three upstream calls")
    auth_ids, costs = _collect(ctx, item["id"], 3)
    _assert_single_credential(ctx, item["id"], auth_ids, costs, calls, "lcp conversation growth")
    return {"message_counts": [len(m) for m in (first, second, third)],
            "auth_ids": auth_ids, "upstream_keys": sorted({c["upstream_key"] for c in calls}),
            "requests": len(auth_ids), "costs": costs, "key_spent": _spent(ctx, item["id"])}
