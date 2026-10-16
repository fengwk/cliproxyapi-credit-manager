"""Caller attribution and key lifecycle.

Covers the aligned caller metadata contract and the key operations that must
never rewrite history: reveal, rotate and soft delete all keep the ledger, the
usage history and an unfinished hold intact, while admission is governed by the
key itself -- disabling a caller's attribution metadata must not disable its key.
"""

from __future__ import annotations

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
    return next((row for row in items if row["id"] == key_id), None)


def _settled(ctx, key_id, spent):
    return ctx.wait_until(lambda: (row := _key(ctx, key_id)) and (row if row["spent_micro_usd"] == spent else None), timeout=15)


def _chat(ctx, key, model="synthetic-chat"):
    return ctx.client.chat(key, model)


def _create_key(ctx, **spec):
    body = {"label": "synthetic", "enabled": True, "quota_micro_usd": 0, "daily_micro_usd": 0,
            "weekly_micro_usd": 0, "monthly_micro_usd": 0, "max_concurrent": 0, "allowed_models": []}
    body.update(spec)
    response = ctx.manager.create_key(body)
    check_eq(response.status, 201, f"key create {response.text[:160]}")
    data = response.json()
    return data["item"], data["key"]


@case("keyops.caller_metadata_stable_principal_scope",
      "a key carries a server-generated stable principal/scope and a caller's disabled metadata never disables the key")
def caller_metadata(ctx):
    created = ctx.manager.create_caller({"id": "caller-meta", "display_name": "Synthetic Team", "enabled": True})
    check_eq(created.status, 201, f"caller create {created.text[:160]}")
    check_eq(created.json()["item"]["enabled"], True, "caller starts enabled")

    unknown = ctx.manager.create_key({"label": "bad", "caller_id": "caller-does-not-exist"})
    check_eq(unknown.status, 400, f"unknown caller attribution rejected: {unknown.text[:120]}")

    item, key = _create_key(ctx, label="keyops-caller", caller_id="caller-meta")
    principal, scope, fingerprint = item["principal"], item["scope"], item["fingerprint"]
    check(principal.startswith("cmk:"), f"principal is a server id: {principal!r}")
    check(len(scope) == 64 and all(c in "0123456789abcdef" for c in scope), f"scope is 64-hex: {scope!r}")
    check(fingerprint and len(fingerprint) == 16, f"fingerprint is a short hex id: {fingerprint!r}")
    check("key" not in item["spec"], "create response never echoes stored material inside item.spec")

    # The identity is derived server-side and must be stable across reads.
    row = _key(ctx, item["id"])
    check_eq(row["principal"], principal, "principal stable across reads")
    check_eq(row["scope"], scope, "scope stable across reads")
    check_eq(row["fingerprint"], fingerprint, "fingerprint stable across reads")
    check_eq(row["caller_id"], "caller-meta", "caller attribution surfaced on the key")

    ctx.set_price("synthetic-chat")
    check_eq(_chat(ctx, key).status, 200, "attributed key bills normally")
    check(_settled(ctx, item["id"], 14) is not None, "first request settled at 14")

    # Disabling the caller is pure metadata: the key keeps authenticating.
    updated = ctx.manager.update_caller({"id": "caller-meta", "display_name": "Synthetic Team", "enabled": False})
    check_eq(updated.status, 200, f"caller update {updated.text[:120]}")
    check_eq(updated.json()["item"]["enabled"], False, "caller metadata disabled")
    check_eq(_chat(ctx, key).status, 200, "disabling the caller does not disable its key")
    check(_settled(ctx, item["id"], 28) is not None, "key keeps billing after caller disable")
    return {"principal": principal, "scope": scope, "spent": _key(ctx, item["id"])["spent_micro_usd"]}


@case("keyops.reveal_rotate_delete_preserve_history_and_pending",
      "reveal/rotate/soft-delete keep the ledger, the request history and an unfinished hold, and the pending request still settles")
def reveal_rotate_delete(ctx):
    item, key = _create_key(ctx, label="keyops-lifecycle")
    key_id = item["id"]
    ctx.set_price("synthetic-chat")
    check_eq(_chat(ctx, key).status, 200, "settled request accepted")
    check(_settled(ctx, key_id, 14) is not None, "ledger reaches 14")

    revealed = ctx.manager.reveal(key_id)
    check_eq(revealed.status, 200, f"reveal {revealed.text[:120]}")
    check_eq(revealed.json()["key"], key, "reveal returns the recoverable plaintext")

    rotated = ctx.manager.rotate(key_id)
    check_eq(rotated.status, 200, f"rotate {rotated.text[:120]}")
    new_key = rotated.json()["key"]
    check(new_key != key, "rotation changes the material")
    check_eq(rotated.json()["item"]["id"], key_id, "rotation keeps the key id")
    check_eq(_chat(ctx, key).status, 401, "the retired plaintext is denied")
    check_eq(_chat(ctx, new_key).status, 200, "the rotated plaintext is accepted")
    check(_settled(ctx, key_id, 28) is not None, "rotation preserves the ledger")

    # Open an unfinished request: it holds budget and must survive every later
    # key operation.
    ctx.mock.default_script = Script(omit_usage=True)
    check_eq(_chat(ctx, new_key).status, 200, "pending request accepted")
    ctx.mock.default_script = Script()
    pending = ctx.wait_until(lambda: _pending_request(ctx, key_id), timeout=15)
    check(pending is not None, "a pending request exists before delete")
    held_before = pending["held_micro_usd"]
    check(held_before > 0, "pending request holds budget")

    deleted = ctx.manager.delete_key(key_id, "synthetic decommission")
    check_eq(deleted.status, 200, f"soft delete {deleted.text[:120]}")
    check_eq(deleted.json().get("ok"), True, "soft delete returns ok")

    row = _key(ctx, key_id)
    check(row is not None, "a soft-deleted key still appears in the list")
    check(row.get("deleted_at"), "soft delete stamps deleted_at")
    check_eq(row["spec"]["enabled"], False, "soft delete disables the key")
    check_eq(row["spent_micro_usd"], 28, "soft delete keeps the ledger")
    check_eq(_chat(ctx, new_key).status, 401, "a soft-deleted key is denied")
    check_eq(ctx.manager.reveal(key_id).status, 409, "a soft-deleted key cannot be revealed")
    after = ctx.manager.delete_key(key_id, "synthetic decommission")
    check_eq(after.status, 200, "repeated delete is an idempotent no-op")

    report = ctx.usage_rows(key_id=key_id, page_size=50)
    check_eq(report["summary"]["cost_micro_usd"], 28, "history keeps its settled cost")
    check(report["summary"]["pending"] >= 1, "history keeps the pending request")
    pending_after = next((r for r in report["items"] if r["id"] == pending["id"]), None)
    check(pending_after is not None, "the pending request row survives soft delete")
    check(pending_after["state"] != "settled", "soft delete never writes off a pending request")
    check(pending_after["held_micro_usd"] > 0, "the hold survives soft delete")

    # The retained pending request can still be resolved after the key is gone.
    resolved = ctx.manager.resolve(pending["id"], "release", reason="retained pending settles")
    check_eq(resolved.status, 200, f"resolve retained pending {resolved.text[:120]}")
    settled = ctx.wait_until(lambda: _request(ctx, key_id, pending["id"]), timeout=15)
    check(settled is not None and settled["state"] == "settled", f"retained pending settles: {settled and settled['state']}")
    check_eq(_key(ctx, key_id)["spent_micro_usd"], 28, "release does not invent a charge")
    return {"revealed": revealed.status, "rotated": rotated.status, "deleted": deleted.status,
            "pending_held": held_before, "final_spent": _key(ctx, key_id)["spent_micro_usd"]}


def _pending_request(ctx, key_id):
    request = ctx.find_request(key_id=key_id)
    if request and request["state"] != "settled":
        return request
    return None


def _request(ctx, key_id, request_id):
    report = ctx.usage_rows(key_id=key_id, page_size=50)
    return next((r for r in report["items"] if r["id"] == request_id), None)
