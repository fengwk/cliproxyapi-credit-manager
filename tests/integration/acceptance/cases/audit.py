"""Management audit trail.

Every mutating management operation is written to a single ordered, paged audit
trail and the disclosure is transactional. This case exercises the key, rule and
token-reset operations the aligned surface exposes and asserts they are recorded
with their identifiers, newest first, and never with a plaintext key. Manual
money resolution no longer exists, so no resolve action is expected.
"""

from __future__ import annotations

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


EXPECTED_ACTIONS = {"create", "update", "rotate", "reveal", "delete", "reset",
                    "caller_create", "price_update", "rule_update", "rule_delete"}


@case("audit.records_key_rule_and_token_reset_operations",
      "key, pricing-rule and token-reset operations are all audited, newest first, without any plaintext key")
def audit_trail(ctx):
    caller = ctx.manager.create_caller({"id": "caller-audit", "display_name": "Audit Team", "enabled": True})
    check_eq(caller.status, 201, f"caller create {caller.text[:120]}")

    created = ctx.manager.create_key({"label": "audit-key", "enabled": True, "caller_id": "caller-audit",
                                      "quota_micro_usd": 0, "daily_micro_usd": 0, "weekly_micro_usd": 0,
                                      "monthly_micro_usd": 0, "max_concurrent": 0, "allowed_models": []})
    check_eq(created.status, 201, f"key create {created.text[:120]}")
    key_id, plaintext = created.json()["item"]["id"], created.json()["key"]

    updated = ctx.manager.update_key(key_id, {"label": "audit-key-2", "enabled": True, "quota_micro_usd": 0,
                                              "daily_micro_usd": 0, "weekly_micro_usd": 0, "monthly_micro_usd": 0,
                                              "max_concurrent": 0, "allowed_models": []})
    check_eq(updated.status, 200, f"key update {updated.text[:120]}")
    check_eq(ctx.manager.rotate(key_id).status, 200, "rotate audited")
    check_eq(ctx.manager.reveal(key_id).status, 200, "reveal audited")

    check_eq(ctx.set_price("synthetic-chat").status, 200, "price upsert audited")
    rule = {"id": "audit-rule", "match_kind": "exact", "pattern": "synthetic-chat", "priority": 3, "enabled": True,
            "price": {"model": "synthetic-chat", "rates": {"input": 1_000_000, "output": 1_000_000,
                                                           "cache_read": 0, "cache_write": 0},
                      "billing_mode": "token", "version": 0}}
    check_eq(ctx.manager.put_pricing_rule(rule).status, 200, "rule upsert audited")
    check_eq(ctx.manager.delete_pricing_rule("audit-rule").status, 200, "rule delete audited")

    # A token reset is audited with its scope and names the token baseline.
    check_eq(ctx.manager.reset_tokens({"id": key_id, "total": True, "reason": "audited reset"}).status, 200,
             "token reset audited")
    check_eq(ctx.manager.delete_key(key_id, "audited delete").status, 200, "delete audited")

    page = ctx.manager.audit(limit=500)
    check_eq(page.status, 200, f"audit read {page.text[:120]}")
    body = page.json()
    check(body["total"] >= len(EXPECTED_ACTIONS), f"audit holds every operation: total={body['total']}")
    actions = {entry["action"] for entry in body["items"]}
    missing = EXPECTED_ACTIONS - actions
    check(missing == set(), f"audit is missing operations: {sorted(missing)} (saw {sorted(actions)})")
    check("resolve" not in actions, "no manual resolution action exists")

    by_action = {}
    for entry in body["items"]:
        by_action.setdefault(entry["action"], []).append(entry)
    for entry in by_action["create"]:
        check(entry["key_id"], "every create audit carries its key id")
        check(entry["detail"].startswith("fingerprint="), f"create detail: {entry['detail']!r}")
    check(any(entry["key_id"] == key_id for entry in by_action["create"]),
          "the audited key create carries its key id")
    reveal_entry = by_action["reveal"][0]
    check_eq(reveal_entry["key_id"], key_id, "reveal audit carries its key id")
    for entry in by_action["rule_update"] + by_action["rule_delete"]:
        check_eq(entry["detail"], "rule_id=audit-rule", f"rule audit detail {entry['detail']!r}")
    reset_detail = by_action["reset"][0]["detail"]
    check("tokens" in reset_detail, f"reset audit detail names the token baseline: {reset_detail!r}")
    check_eq(by_action["delete"][0]["reason"], "audited delete", "delete audit carries its reason")

    seqs = [entry["seq"] for entry in body["items"]]
    check(seqs == sorted(seqs, reverse=True), "audit is ordered newest first")
    blob = repr(body)
    check(plaintext not in blob, "no plaintext key is ever written to the audit trail")
    return {"total": body["total"], "actions": sorted(actions)}
