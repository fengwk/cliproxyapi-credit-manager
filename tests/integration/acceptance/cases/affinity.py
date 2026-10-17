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


def _cost(ctx, key_id):
    return ctx.usage_rows(key_id=key_id, page_size=100)["summary"]["cost_micro_usd"]


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
    check_eq(sum(cost for cost in costs if cost is not None), EXPECTED_COST,
             f"{label}: three requests derive {EXPECTED_COST} in total: {costs}")
    check_eq(_cost(ctx, key_id), EXPECTED_COST,
             f"{label}: the report derives exactly {EXPECTED_COST}")


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
            "requests": len(auth_ids), "costs": costs, "cost": _cost(ctx, item["id"])}


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
            "requests": len(auth_ids), "costs": costs, "cost": _cost(ctx, item["id"])}


IDENTITY_SESSION_ID = "synthetic-affinity-identity"


@case("affinity.key_identity_change_keeps_session_binding",
      "reassigning the caller and rotating the key material leaves an existing session pinned to the same credential")
def identity_change_keeps_binding(ctx):
    ctx.recreate_host("affinity-identity", session_affinity=True)
    ctx.set_price(MODEL)
    check_eq(len(ctx.openai_upstream_keys), 2, "two upstream credentials are configured")

    check_eq(ctx.manager.create_caller({"id": "affinity-caller-a", "display_name": "Affinity A"}).status, 201,
             "first caller created")
    check_eq(ctx.manager.create_caller({"id": "affinity-caller-b", "display_name": "Affinity B"}).status, 201,
             "second caller created")
    item, key = ctx.new_key(label="affinity-identity", caller_id="affinity-caller-a")
    key_id = item["id"]

    # Bind the session to one credential with two round-robin credentials live.
    ctx.mock.reset()
    for turn in ("synthetic-identity-1", "synthetic-identity-2"):
        check_eq(_post(ctx, key, [{"role": "user", "content": turn}], {SESSION_HEADER: IDENTITY_SESSION_ID}).status,
                 200, "binding request succeeds")
    auth_ids, _costs = _collect(ctx, key_id, 2)
    bound_auth = set(auth_ids)
    bound_upstream = {call["upstream_key"] for call in ctx.mock.calls_for("/chat/completions")}
    check_eq(len(bound_auth), 1, f"the session binds to one credential: {auth_ids}")
    check_eq(len(bound_upstream), 1, f"the session binds to one upstream key: {sorted(bound_upstream)}")

    # Change the key's attribution and its caller metadata, then rotate the key
    # material. None of these are scheduling inputs, so the session binding must
    # survive unchanged.
    reassigned = ctx.manager.update_key(key_id, {"label": item["spec"]["label"], "enabled": True,
                                                 "caller_id": "affinity-caller-b", "quota_micro_usd": 0,
                                                 "daily_micro_usd": 0, "weekly_micro_usd": 0, "monthly_micro_usd": 0,
                                                 "max_concurrent": 0, "allowed_models": []})
    check_eq(reassigned.status, 200, f"caller reassignment {reassigned.text[:120]}")
    check_eq(reassigned.json()["item"]["caller_id"], "affinity-caller-b", "the key now carries the new caller")
    check_eq(ctx.manager.update_caller({"id": "affinity-caller-a", "display_name": "Affinity A", "enabled": False}).status,
             200, "the old caller metadata is disabled")
    rotated = ctx.manager.rotate(key_id)
    check_eq(rotated.status, 200, f"key rotation {rotated.text[:120]}")
    rotated_key = rotated.json()["key"]
    check(rotated_key != key, "rotation changes the client material")
    check_eq(_post(ctx, key, [{"role": "user", "content": "retired"}], {SESSION_HEADER: IDENTITY_SESSION_ID}).status,
             401, "the retired client key is denied")

    # Continue the identical session with the rotated key.
    for turn in ("synthetic-identity-3", "synthetic-identity-4"):
        check_eq(_post(ctx, rotated_key, [{"role": "user", "content": turn}], {SESSION_HEADER: IDENTITY_SESSION_ID}).status,
                 200, "post-change session request succeeds")

    items = ctx.wait_until(lambda: (_items(ctx, key_id) if len(_items(ctx, key_id)) >= 4 else None), timeout=15)
    check(items is not None, f"expected four accounted requests, got {len(_items(ctx, key_id))}")
    check_eq(len(items), 4, "four customer requests are accounted (one denied request is not admitted)")
    executions = [item["executions"][0] for item in items if item.get("executions")]
    check_eq(len(executions), 4, "every admitted request recorded an execution")
    after_auth = {ex["auth_id"] for ex in executions}
    upstream_after = {call["upstream_key"] for call in ctx.mock.calls_for("/chat/completions")}
    check_eq(after_auth, bound_auth, f"the session credential is unchanged across the identity change: {after_auth}")
    check_eq(upstream_after, bound_upstream,
             f"the session upstream credential is unchanged: {sorted(upstream_after)}")
    check_eq(sum(ex.get("cost_micro_usd") or 0 for ex in executions), 56, "four requests derive 56 microUSD")
    check_eq(_cost(ctx, key_id), 56, "the rotated key keeps the same derived history total")
    return {"bound_auth": sorted(bound_auth), "upstream": sorted(upstream_after), "requests": len(executions),
            "cost": _cost(ctx, key_id)}
