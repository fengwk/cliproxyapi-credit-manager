"""Passive boundary: no background work and no retired configuration.

The plugin no longer owns any background execution: it declares no executor/
router/scheduler callback, registers no host model-execution bridge, and starts
no worker. The optional warmup feature and its `warmup.*` settings were removed
entirely, and `max_pending_requests` (the per-key unresolved-request cap) went
with the admission backlog gate. Those settings must now be a clear *config
error*, never silently ignored or re-enabled.

These cases assert observable host behavior: upstream call counts, the host log,
the plugin registration state and the management response.
"""

from __future__ import annotations

import time
from pathlib import Path

from ..report import check, check_eq, check_no_holds

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


def _spent(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    row = next((k for k in items if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


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
    check_eq(ctx.usage_rows()["total"], 0, "no ledger rows without traffic")
    check_eq(_spent(ctx, item["id"]), 0, "no key charged")
    check_no_holds(next((k for k in ctx.manager.keys().json()["items"] if k["id"] == item["id"]), {}),
                   "idle key row")
    payload = ctx.manager.health().json()
    check_eq(payload.get("status"), "ok", "health is ok with the passive plugin")
    check_eq(payload.get("last_error"), "", "health reports no background error")
    return {"upstream_calls": ctx.mock.count(), "usage_total": 0,
            "health": payload.get("status"), "last_error": payload.get("last_error")}


@case("passive.retired_settings_rejected",
      "retired warmup/max_pending_requests settings are a clear config error that never poisons the running plugin")
def retired_settings_rejected(ctx):
    ctx.recreate_host("passive-retired")
    item, key = ctx.new_key(label="passive-retired")
    ctx.set_price(MODEL)

    statuses = {}
    for setting, (_yaml, body) in RETIRED_SETTINGS.items():
        response = ctx.manager.config_call("PATCH", body=body)
        statuses[setting] = response.status
        # A configuration that cannot be applied is refused (documented 400 for a
        # parse error, 409 for a refused reload) rather than accepted as a no-op.
        check(response.status in (400, 409),
              f"a retired {setting} setting must be refused, got {response.status}")
        evidence = (response.text + _log_text(ctx)).lower()
        check(setting in evidence,
              f"the rejection must name the retired {setting!r} setting: {response.text[:200]!r}")

    # The refused reconfiguration leaves the previous configuration serving:
    # health stays ok and a normal request is still admitted and billed.
    check_eq(ctx.manager.health().json()["status"], "ok", "a rejected reload does not poison health")
    check_eq(ctx.client.chat(key, MODEL).status, 200, "the plugin keeps serving after a rejected reload")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 14), "the request after the rejection is billed normally")
    check_eq(ctx.mock.count("/chat/completions"), 1, "exactly the accepted request reached upstream")
    return {"statuses": statuses, "key_spent": _spent(ctx, item["id"]), "health": "ok"}


@case("passive.boot_rejects_retired_settings",
      "a deployment configured with a retired setting refuses plugin registration with a clear config error")
def boot_rejects_retired(ctx):
    root = Path(ctx.artifacts) / "host-passive-boot"
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
