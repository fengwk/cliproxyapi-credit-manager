"""Background warmup probes.

Warmup is a host-side capability: the plugin asks the host to execute a pinned,
minimal model call on a configured credential. The probe is a *real* host model
execution -- it shows up in the host log and as exactly one upstream call -- yet
it must never create a customer ledger row: the host resolves usage attribution
from the client authentication path, so a probe with no client identity can not
be attributed to a customer key. The plugin's own accounting interceptors are
therefore skipped for the probe (upstream 1, ledger 0).

These cases assert the observable host behavior, not a mock-host unit conclusion:
the expected/actual upstream call count and the host log's credential-selection
lines are the evidence. The pinned credential id and provider are read back from
CPA's own accounting of a real request in the *same* host, because the id is
derived from the host's (random) upstream base URL and is host-specific.

A customer request -- including one that forges the warmup session header -- is
always billed: the header is a correlation label only, never a billing gate.
"""

from __future__ import annotations

import re
import time

from ..api import auth_headers
from ..report import check, check_eq

CASES = []

MODEL = "synthetic-chat"
WARMUP_HEADER = "X-Credit-Manager-Warmup-Session"

# Debug logging makes the host emit its credential-selection line for every
# model execution, which is the log evidence these cases assert on.
DEBUG_LOGS = "observability:\n  logs:\n    debug: true"


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


# -- helpers ---------------------------------------------------------------

def _is_probe(call):
    """A warmup probe is the plugin's minimal request: one "." turn, 1 token."""
    body = call.get("body") or {}
    if body.get("model") != MODEL or body.get("max_tokens") != 1:
        return False
    messages = body.get("messages") or []
    return (len(messages) == 1
            and messages[0].get("role") == "user"
            and messages[0].get("content") == ".")


def _probes(ctx):
    return [call for call in ctx.mock.calls_for("/chat/completions") if _is_probe(call)]


def _boot(ctx, name):
    """Boot a dedicated host with debug logs so credential selection is visible."""
    ctx.recreate_host(f"warmup-{name}", extra_top_yaml=DEBUG_LOGS)
    return ctx.host


def _health(ctx):
    payload = ctx.manager.health().json()
    return payload.get("status"), (payload.get("last_error") or "")


def _spent(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    row = next((k for k in items if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


def _held(ctx, key_id):
    items = ctx.manager.keys().json()["items"]
    row = next((k for k in items if k["id"] == key_id), None)
    return row["held_micro_usd"] if row else None


def _credential_lines(ctx):
    log = (ctx.host.config.root / "host.log").read_text(errors="replace")
    return [line for line in log.splitlines() if "Use API key" in line]


def _unscoped_credential_lines(ctx):
    """Credential selections with no client request scope (the probe itself)."""
    out = []
    for line in _credential_lines(ctx):
        match = re.match(r"^\[[^\]]+\] \[([^\]]*)\]", line)
        if match and set(match.group(1).strip()) == {"-"}:
            out.append(line)
    return out


def _discover_credential(ctx):
    """Read CPA's own credential id + provider for a real request in this host."""
    item, key = ctx.new_key(label="warmup-discovery")
    response = ctx.client.chat(key, MODEL)
    check_eq(response.status, 200, "discovery request succeeds")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "discovery request recorded")
    execution = request["executions"][0]
    check(execution["auth_id"], "CPA reports a concrete credential id")
    check(execution["provider"], "CPA reports a concrete provider")
    return execution["auth_id"], execution["provider"]


def _patch_warmup(ctx, auth_id=None, provider=None, interval="1h", enabled=True):
    warmup = {"enabled": enabled}
    if enabled:
        warmup["interval"] = interval
        warmup["targets"] = [{"auth_id": auth_id, "provider": provider,
                              "model": MODEL, "protocol": "openai"}]
    response = ctx.manager.config_call("PATCH", body={"warmup": warmup})
    check_eq(response.status, 200, "warmup reconfiguration is accepted")


# -- cases -----------------------------------------------------------------

@case("warmup.default_off_no_background",
      "with no warmup configuration the plugin issues no background upstream activity and no credential selection")
def default_off(ctx):
    _boot(ctx, "default-off")
    item, _key = ctx.new_key(label="warmup-off")
    ctx.set_price(MODEL)
    time.sleep(2.0)
    check_eq(ctx.mock.count(), 0, "no upstream call without a client request")
    check_eq(_credential_lines(ctx), [],
             "the host selected no credential because nothing executed")
    check_eq(ctx.usage_rows()["total"], 0, "no ledger rows without traffic")
    check_eq(_spent(ctx, item["id"]), 0, "no key charged")
    check_eq(_held(ctx, item["id"]), 0, "no key hold")
    status, last_error = _health(ctx)
    check_eq(status, "ok", "health is ok when warmup is off")
    return {"upstream_calls": ctx.mock.count(), "usage_total": 0,
            "health": status, "last_error": last_error}


@case("warmup.enabled_fixed_target_no_customer_ledger",
      "an enabled warmup issues a real pinned host execution (upstream 1, host log shows it) and creates no customer ledger row")
def enabled_fixed_target(ctx):
    _boot(ctx, "enabled-fixed")
    ctx.set_price(MODEL)
    auth_id, provider = _discover_credential(ctx)
    baseline = ctx.usage_rows()["total"]
    # A second key never sends a request: it must stay untouched by warmup.
    idle, _idle_key = ctx.new_key(label="warmup-idle")

    ctx.mock.reset()
    _patch_warmup(ctx, auth_id, provider)

    probes = ctx.wait_until(lambda: _probes(ctx) or None, timeout=10)
    check(probes, "the enabled warmup issued a real upstream probe")
    check_eq(len(_probes(ctx)), 1, "exactly one probe per target per pass")
    probe = _probes(ctx)[0]
    check(probe["upstream_key"] in ctx.openai_upstream_keys,
          f"the probe used a configured upstream credential, got {probe['upstream_key']}")
    check_eq(probe["body"].get("model"), MODEL, "the probe pinned the configured model")
    check_eq(probe["body"].get("max_tokens"), 1, "the probe is the minimal 1-token request")

    # The whole point: a real host execution that creates no customer accounting.
    check_eq(ctx.usage_rows()["total"], baseline,
             "the probe created no customer ledger row")
    check_eq(_spent(ctx, idle["id"]), 0, "the probe charged no customer key")
    check_eq(_held(ctx, idle["id"]), 0, "the probe reserved no customer hold")
    status, last_error = _health(ctx)
    check_eq(status, "ok", "health stays ok while probing")

    # Host-log evidence: the probe ran as a host model execution with no client scope.
    unscoped = _unscoped_credential_lines(ctx)
    check(len(unscoped) >= 1,
          "host log records the probe credential selection without a client request scope")
    return {"auth_id": auth_id, "provider": provider, "probes": len(_probes(ctx)),
            "probe_upstream_key": probe["upstream_key"], "usage_total": baseline,
            "probe_credential_log_lines": len(unscoped), "health": status,
            "last_error": last_error}


@case("warmup.pinned_target_has_no_fallback",
      "a warmup target with an unknown AuthID fails instead of falling back to another credential")
def pinned_no_fallback(ctx):
    _boot(ctx, "pinned-no-fallback")
    ctx.set_price(MODEL)
    auth_id, provider = _discover_credential(ctx)
    ctx.mock.reset()

    # Same provider/model, but an AuthID that exists in no host.
    _patch_warmup(ctx, "openai-compatibility:synthetic-openai:absent-id", provider)
    time.sleep(2.0)
    check_eq(len(_probes(ctx)), 0,
             "no probe when the pinned credential id is unknown (no host-side fallback)")
    status, last_error = _health(ctx)
    check_eq(status, "degraded", "a failed probe degrades health")
    check("warmup probe failed" in last_error, f"health reports the probe failure: {last_error!r}")

    # Restoring the real credential resumes probing: the failure is contained.
    out = {"bogus_auth_id": "openai-compatibility:synthetic-openai:absent-id",
           "probes_with_bogus_id": 0, "health": status, "last_error": last_error}
    ctx.mock.reset()
    _patch_warmup(ctx, auth_id, provider)
    check(ctx.wait_until(lambda: _probes(ctx) or None, timeout=10),
          "restoring the real credential id resumes probing")
    out["probes_after_restore"] = len(_probes(ctx))
    return out


@case("warmup.parallel_same_account_request_billed",
      "a customer request runs alongside the warmup worker on the same account and is billed normally")
def parallel_billed(ctx):
    _boot(ctx, "parallel")
    ctx.set_price(MODEL)
    auth_id, provider = _discover_credential(ctx)
    baseline = ctx.usage_rows()["total"]
    item, key = ctx.new_key(label="warmup-customer")

    ctx.mock.reset()
    # A 1s interval keeps the worker probing for the duration of the case.
    _patch_warmup(ctx, auth_id, provider, interval="1s")
    check(ctx.wait_until(lambda: _probes(ctx) or None, timeout=10), "the warmup worker is probing")

    response = ctx.client.chat(key, MODEL)
    check_eq(response.status, 200, "the customer request succeeds while warmup runs")
    check(b"synthetic answer" in response.raw, "the customer request is answered upstream")

    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "the customer request is recorded")
    execution = request["executions"][0]
    # The account is the configured entry; the host may pick any of its api
    # keys, so assert the same entry/provider rather than one exact key id.
    check_eq(execution["provider"], provider,
             "the customer request used the same upstream provider/account")
    check(execution["auth_id"].startswith("openai-compatibility:synthetic-openai:"),
          f"the customer request used the same configured account entry: {execution['auth_id']}")
    check_eq(execution["cost_micro_usd"], 14, "the customer request was charged exactly 14")
    check_eq(_spent(ctx, item["id"]), 14, "the customer key was charged 14")
    check_eq(_held(ctx, item["id"]), 0, "the settled customer request holds nothing")
    check_eq(ctx.usage_rows()["total"], baseline + 1,
             "only the customer request was accounted; probes added no ledger rows")
    check(len(_probes(ctx)) >= 1, "probes kept running in parallel with the customer request")
    return {"auth_id": auth_id, "customer_cost": execution["cost_micro_usd"], "customer_auth_id": execution["auth_id"],
            "customer_spent": _spent(ctx, item["id"]), "customer_held": _held(ctx, item["id"]),
            "probes": len(_probes(ctx)), "usage_total": ctx.usage_rows()["total"]}


@case("warmup.forged_session_header_still_billed",
      "a customer that forges the warmup session header is still billed in full")
def forged_header_billed(ctx):
    _boot(ctx, "forged")
    ctx.set_price(MODEL)
    auth_id, provider = _discover_credential(ctx)
    baseline = ctx.usage_rows()["total"]
    item, key = ctx.new_key(label="warmup-forger")

    ctx.mock.reset()
    _patch_warmup(ctx, auth_id, provider, interval="1s")
    check(ctx.wait_until(lambda: _probes(ctx) or None, timeout=10), "the warmup worker is probing")

    headers = auth_headers(key)
    headers[WARMUP_HEADER] = "warmup-forged-by-customer"
    response = ctx.client.post("/v1/chat/completions",
                               {"model": MODEL, "stream": False,
                                "messages": [{"role": "user", "content": "synthetic"}]},
                               headers=headers)
    check_eq(response.status, 200, "the forging request succeeds")

    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]), timeout=15)
    check(request is not None, "the forging request is recorded")
    execution = request["executions"][0]
    check_eq(execution["quality"], "complete", "the forging request is settled as a normal billed request")
    check_eq(execution["cost_micro_usd"], 14, "the forging header did not reduce the charge")
    check_eq(_spent(ctx, item["id"]), 14, "the forging key was charged the full 14")
    check_eq(ctx.usage_rows()["total"], baseline + 1,
             "the forging request was accounted like any other customer request")
    return {"forged_header": WARMUP_HEADER, "cost": execution["cost_micro_usd"],
            "spent": _spent(ctx, item["id"]), "usage_total": ctx.usage_rows()["total"]}


@case("warmup.reconfigure_disabled_stops_worker",
      "reconfiguring warmup to disabled stops the background worker with no residual probe")
def reconfigure_disabled(ctx):
    _boot(ctx, "reconfigure")
    ctx.set_price(MODEL)
    auth_id, provider = _discover_credential(ctx)
    baseline = ctx.usage_rows()["total"]

    ctx.mock.reset()
    _patch_warmup(ctx, auth_id, provider, interval="1s")
    check(ctx.wait_until(lambda: len(_probes(ctx)) >= 2, timeout=10),
          "the warmup worker probes repeatedly while enabled")
    running = len(_probes(ctx))

    _patch_warmup(ctx, enabled=False)
    time.sleep(0.5)
    quiesced = len(_probes(ctx))
    # Wait several intervals; a residual worker would keep incrementing this.
    time.sleep(3.5)
    check_eq(len(_probes(ctx)), quiesced,
             "no probe after warmup was reconfigured to disabled (no residual worker)")
    check_eq(ctx.usage_rows()["total"], baseline,
             "warmup never added a ledger row, before or after disabling")
    status, last_error = _health(ctx)
    check_eq(status, "ok", "health is ok after the worker is stopped")
    return {"probes_while_enabled": running, "probes_after_disable": quiesced,
            "usage_total": baseline, "health": status, "last_error": last_error}
