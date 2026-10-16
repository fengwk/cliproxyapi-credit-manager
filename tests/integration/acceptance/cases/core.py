"""Core host/plugin contract cases: registration metadata, health, auth gates."""

from __future__ import annotations

import json

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


@case("native.registration",
      "host loads the plugin: enabled, metadata present with the expected bare release version, management menu published")
def registration(ctx):
    raw = ctx.host.plugin_registration()
    entries = {entry["id"]: entry for entry in raw.get("plugins", [])}
    check(ctx.plugin_id in entries, f"plugin {ctx.plugin_id} not listed in {json.dumps(raw)[:400]}")
    entry = entries[ctx.plugin_id]
    check(entry.get("registered"), f"plugin not registered: {entry}")
    check(entry.get("enabled") is True, f"plugin not enabled: {entry}")
    check(entry.get("effective_enabled") is True, f"plugin not effective_enabled: {entry}")
    meta = entry.get("metadata", {})
    check_eq(meta.get("name"), "Credit Manager", "plugin metadata name")
    check_eq(meta.get("author"), "fengwk", "plugin metadata author")
    check(meta.get("github_repository"), "plugin metadata github_repository is required")
    # The reported version must identify the expected build, not just any build.
    ctx.assert_plugin_version(meta.get("version"), "plugin metadata version")
    # The published management menu proves the management_api resource route registered.
    menus = entry.get("menus", [])
    check(any(m.get("path") == f"/v0/resource/plugins/{ctx.plugin_id}/index.html" for m in menus),
          f"management menu not published: {menus}")
    return {"registration": entry, "version": meta.get("version")}


@case("native.health",
      "plugin health reports ok with the expected build version and retention and capture fields")
def health(ctx):
    response = ctx.manager.health()
    check_eq(response.status, 200, "health status code")
    data = response.json()
    check_eq(data.get("status"), "ok", "health status")
    registered = ctx.host.plugin_registration()
    entry = next((e for e in registered.get("plugins", []) if e.get("id") == ctx.plugin_id), {})
    registered_version = (entry.get("metadata") or {}).get("version")
    # The registration metadata is the bare release; the health diagnostic
    # version reports the same release and may carry the build commit.
    ctx.assert_plugin_version(registered_version, "registered metadata version")
    ctx.assert_diagnostics_version(data.get("version"), "health version")
    check_eq((data.get("version") or "").partition("+")[0], (registered_version or "").partition("+")[0],
             "health version must report the same release as the registered metadata")
    check("capture" in data and "retention_months" in data and "capture_enabled" in data,
          f"health fields missing: {sorted(data)}")
    check_eq(data.get("last_error"), "", "health last_error")
    return {"health": data, "registered_version": registered_version}


@case("management.auth_required", "management API without credentials is rejected with 401")
def management_auth(ctx):
    missing = ctx.manager.call("GET", "/api/health", with_auth=False)
    check_eq(missing.status, 401, "unauthenticated management call must be 401")
    wrong = ctx.manager.call("GET", "/api/health", headers={"Authorization": "Bearer wrong-key"})
    check_eq(wrong.status, 401, "wrong management key must be 401")
    ok = ctx.manager.health()
    check_eq(ok.status, 200, "valid management key must be 200")
    return {"missing": missing.status, "wrong": wrong.status}
