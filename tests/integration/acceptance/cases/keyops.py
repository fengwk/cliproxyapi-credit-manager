"""Caller attribution and key lifecycle.

Covers the aligned caller metadata contract and the key operations that must
never rewrite history: reveal, rotate and soft delete all keep the usage log,
the derived costs and an unresolved request intact, while admission is governed
by the key itself -- disabling a caller's attribution metadata must not disable
its key. Manual money resolution no longer exists, so an unresolved request
stays pending instead of being settled by an operator.
"""

from __future__ import annotations

from ..mock import Script
from ..report import check, check_eq, check_no_holds, check_no_ledger_fields

CASES = []

COST = 14


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _key(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    return next((row for row in items if row["id"] == key_id), None)


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
    check_no_ledger_fields(item, "created key")

    # The identity is derived server-side and must be stable across reads.
    row = _key(ctx, item["id"])
    check_eq(row["principal"], principal, "principal stable across reads")
    check_eq(row["scope"], scope, "scope stable across reads")
    check_eq(row["fingerprint"], fingerprint, "fingerprint stable across reads")
    check_eq(row["caller_id"], "caller-meta", "caller attribution surfaced on the key")

    ctx.set_price("synthetic-chat")
    check_eq(_chat(ctx, key).status, 200, "attributed key bills normally")
    first = ctx.wait_report(item["id"], settled=1)
    check_eq(first["summary"]["cost_micro_usd"], COST, "first request derives its cost")

    # Disabling the caller is pure metadata: the key keeps authenticating.
    updated = ctx.manager.update_caller({"id": "caller-meta", "display_name": "Synthetic Team", "enabled": False})
    check_eq(updated.status, 200, f"caller update {updated.text[:120]}")
    check_eq(updated.json()["item"]["enabled"], False, "caller metadata disabled")
    check_eq(_chat(ctx, key).status, 200, "disabling the caller does not disable its key")
    second = ctx.wait_report(item["id"], settled=2)
    check_eq(second["summary"]["cost_micro_usd"], COST * 2, "the key keeps deriving cost")
    check_no_ledger_fields(_key(ctx, item["id"]), "key row")
    return {"principal": principal, "scope": scope,
            "cost": second["summary"]["cost_micro_usd"]}


@case("keyops.reveal_rotate_delete_preserve_history_and_pending",
      "reveal/rotate/soft-delete keep the usage history, derived costs and an unresolved request")
def reveal_rotate_delete(ctx):
    item, key = _create_key(ctx, label="keyops-lifecycle")
    key_id = item["id"]
    ctx.set_price("synthetic-chat")
    check_eq(_chat(ctx, key).status, 200, "settled request accepted")
    check(ctx.wait_report(key_id, settled=1) is not None, "first request settles")

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
    check(ctx.wait_report(key_id, settled=2) is not None, "rotation preserves the history")

    # Open an unresolved request: it must survive every later key operation.
    ctx.mock.default_script = Script(omit_usage=True)
    check_eq(_chat(ctx, new_key).status, 200, "pending request accepted")
    ctx.mock.default_script = Script()
    pending = ctx.wait_until(lambda: _pending_request(ctx, key_id), timeout=15)
    check(pending is not None, "a pending request exists before delete")
    check_no_holds(pending, "pending request before delete")

    deleted = ctx.manager.delete_key(key_id, "synthetic decommission")
    check_eq(deleted.status, 200, f"soft delete {deleted.text[:120]}")
    check_eq(deleted.json().get("ok"), True, "soft delete returns ok")

    row = _key(ctx, key_id)
    check(row is not None, "a soft-deleted key still appears in the list")
    check(row.get("deleted_at"), "soft delete stamps deleted_at")
    check_eq(row["spec"]["enabled"], False, "soft delete disables the key")
    check_no_ledger_fields(row, "soft-deleted key row")
    check_no_holds(row, "soft-deleted key row")
    check_eq(_chat(ctx, new_key).status, 401, "a soft-deleted key is denied")
    check_eq(ctx.manager.reveal(key_id).status, 409, "a soft-deleted key cannot be revealed")
    after = ctx.manager.delete_key(key_id, "synthetic decommission")
    check_eq(after.status, 200, "repeated delete is an idempotent no-op")

    report = ctx.usage_rows(key_id=key_id, page_size=100)
    check_eq(report["summary"]["cost_micro_usd"], COST * 2, "history keeps its derived cost")
    check_eq(report["summary"]["requests"], 3, "history keeps all admitted requests")
    check(report["summary"]["pending"] >= 1, "history keeps the pending request")
    pending_after = next((r for r in report["items"] if r["id"] == pending["id"]), None)
    check(pending_after is not None, "the pending request row survives soft delete")
    check(pending_after["state"] != "settled", "soft delete never writes off a pending request")
    check_no_holds(pending_after, "pending request after soft delete")
    check_no_ledger_fields(pending_after, "pending request after soft delete")
    return {"revealed": revealed.status, "rotated": rotated.status, "deleted": deleted.status,
            "cost": report["summary"]["cost_micro_usd"],
            "pending": report["summary"]["pending"]}


def _pending_request(ctx, key_id):
    request = ctx.find_request(key_id=key_id)
    if request and request["state"] != "settled":
        return request
    return None
