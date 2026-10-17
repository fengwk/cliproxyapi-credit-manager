"""Passive boundary: no background work and no retired configuration.

The plugin no longer owns background model execution: it declares no executor/
router/scheduler callback, registers no host model-execution bridge, and starts
no model-probe worker. The optional warmup feature and its `warmup.*` settings were removed
entirely, and `max_pending_requests` (the per-key unresolved-request cap) went
with the admission backlog gate. Those settings must now be a clear *config
error*, never silently ignored or re-enabled.

These cases assert observable host behavior: upstream call counts, the host log,
the plugin registration state and the management response.
"""

from __future__ import annotations

import json
import time
from pathlib import Path

from ..report import check, check_eq, check_no_holds, check_no_ledger_fields

CASES = []

MODEL = "synthetic-chat"

#: Debug logging makes the host emit its credential-selection line for every
#: model execution, so its absence proves no host-side execution happened.
DEBUG_LOGS = "observability:\n  logs:\n    debug: true"

#: Retired plugin settings, mapped to the YAML a legacy deployment would carry
#: and to the hot-reload payload that must now be refused. A deployment that
#: still carries them must fail its configuration instead of running with the
#: old behavior silently.
RETIRED_SETTINGS = {
    "warmup": ("      warmup:\n        enabled: false\n", {"warmup": {"enabled": False}}),
    "max_pending_requests": ("      max_pending_requests: 16\n", {"max_pending_requests": 16}),
}


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _cost(ctx, key_id):
    return ctx.usage_rows(key_id=key_id, page_size=100)["summary"]["cost_micro_usd"]


def _credential_lines(ctx):
    log = (ctx.host.config.root / "host.log").read_text(errors="replace")
    return [line for line in log.splitlines() if "Use API key" in line]


def _log_text(ctx):
    return (ctx.host.config.root / "host.log").read_text(errors="replace").lower()


@case("passive.no_background_execution",
      "with no client request the plugin issues no upstream activity and selects no credential")
def no_background_execution(ctx):
    ctx.recreate_host("passive-quiet", extra_top_yaml=DEBUG_LOGS)
    item, _key = ctx.new_key(label="passive-quiet")
    ctx.set_price(MODEL)
    # Give any hypothetical worker several seconds to act.
    time.sleep(2.5)
    check_eq(ctx.mock.count(), 0, "no upstream call without a client request")
    check_eq(_credential_lines(ctx), [],
             "the host selected no credential because nothing executed")
    check_eq(ctx.usage_rows()["total"], 0, "no usage rows without traffic")
    check_eq(_cost(ctx, item["id"]), 0, "no derived cost without traffic")
    idle = next((k for k in ctx.manager.keys().json()["items"] if k["id"] == item["id"]), {})
    check_no_holds(idle, "idle key row")
    check_no_ledger_fields(idle, "idle key row")
    payload = ctx.manager.health().json()
    check_eq(payload.get("status"), "ok", "health is ok with the passive plugin")
    check_eq(payload.get("last_error"), "", "health reports no background error")
    return {"upstream_calls": ctx.mock.count(), "usage_total": 0,
            "health": payload.get("status"), "last_error": payload.get("last_error")}


@case("passive.retired_settings_rejected",
      "saved retired settings refuse plugin registration; removing them restores service without losing the ledger")
def retired_settings_rejected(ctx):
    ctx.recreate_host("passive-retired")
    item, key = ctx.new_key(label="passive-retired")
    ctx.set_price(MODEL)

    statuses = {}
    for setting, (_yaml, body) in RETIRED_SETTINGS.items():
        response = ctx.manager.config_call("PATCH", body=body)
        statuses[setting] = response.status
        # CPA saves generic plugin config before its file watcher reloads the
        # library. HTTP 200 acknowledges the save, not successful registration.
        check_eq(response.status, 200, "CPA saved the generic configuration")
        check(ctx.wait_until(lambda: f"field {setting} not found" in _log_text(ctx)),
              f"plugin validation must name the retired {setting!r} field")

        def unregistered():
            status, raw = ctx.host.management("GET", "/v0/management/plugins")
            if status != 200:
                return False
            entries = json.loads(raw).get("plugins", [])
            entry = next((e for e in entries if e["id"] == ctx.plugin_id), None)
            return entry is not None and not entry.get("registered")

        check(ctx.wait_until(unregistered), "the invalid saved configuration never registers")
        before = ctx.mock.count()
        check_eq(ctx.client.chat(key, MODEL).status, 401, "the native guard denies the key without its plugin")
        check_eq(ctx.mock.count(), before, "invalid configuration cannot execute customer traffic")

        restored = ctx.manager.config_call("PATCH", body={setting: None})
        check_eq(restored.status, 200, "the retired field is removed from the saved configuration")
        registered = ctx.host.plugin_registration(timeout=15)
        entry = next((e for e in registered.get("plugins", []) if e["id"] == ctx.plugin_id), {})
        check(entry.get("registered"), "the corrected configuration restores registration")
        check_eq(ctx.manager.health().json()["status"], "ok", "the corrected configuration restores health")

    check_eq(ctx.client.chat(key, MODEL).status, 200, "the restored plugin authenticates the original key")
    check_eq(ctx.wait_report(item["id"], settled=1)["summary"]["cost_micro_usd"], 14,
             "the original history survives both reloads and still derives its cost")
    check_eq(ctx.mock.count("/chat/completions"), 1, "exactly the accepted request reached upstream")
    return {"statuses": statuses, "cost": _cost(ctx, item["id"]), "health": "ok"}


@case("passive.boot_rejects_retired_settings",
      "a deployment configured with a retired setting refuses plugin registration with a clear config error")
def boot_rejects_retired(ctx):
    root = Path(ctx.artifacts) / "host-passive-boot"
    # Replace the case host deliberately: stop it first so the rejected boot does
    # not leave the previous CPA process (and its bound port) running.
    ctx.stop_host()
    ctx.build_host(root=root, extra_plugin_yaml=RETIRED_SETTINGS["warmup"][0])
    ctx.host.start()
    registration = ctx.host.plugin_registration(timeout=8)
    entry = next((e for e in registration.get("plugins") or [] if e.get("id") == ctx.plugin_id), None)
    check(entry is None or not entry.get("registered"),
          f"a retired warmup setting must not register the plugin: {entry}")
    log = _log_text(ctx)
    check("warmup" in log, "the host log carries the retired-key config error")
    check_eq(ctx.mock.count(), 0, "a refused deployment executes nothing")
    return {"registered": (entry or {}).get("registered"), "log_mentions_warmup": True}
