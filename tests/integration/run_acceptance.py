#!/usr/bin/env python3
"""Real-native CPA acceptance harness for the credit-manager plugin.

Usage:
  python3 run_acceptance.py --cpa <cpa-binary> --plugin <plugin-library> \
      --artifacts <tmpdir> [--only <substring>] [--modules core,accounting,...]

The harness launches an isolated CLIProxyAPI host (random loopback port,
synthetic management secret, synthetic client keys) with the plugin library and
a loopback upstream mock. It never reads real profiles, credentials or
databases, and never contacts an external account. Every child process is
stopped in a finally block and a result.json is always written.
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from acceptance.context import Context, load_specs  # noqa: E402
from acceptance.mock import Script  # noqa: E402
from acceptance.report import Recorder  # noqa: E402

DEFAULT_MODULES = ["abi", "core", "accounting", "admission", "auth", "capture", "report", "resources",
                   "namespace", "lifecycle", "retry", "passive", "affinity", "gemini", "browser",
                   "lookup", "management", "keyops", "reset", "audit", "pricing", "reference"]


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True)
    parser.add_argument("--plugin", required=True)
    parser.add_argument("--artifacts", required=True)
    parser.add_argument("--plugin-id", default="credit-manager")
    parser.add_argument("--only", default="", help="run only cases whose name contains this substring")
    parser.add_argument("--modules", default="", help="comma separated case module names (default: all present)")
    parser.add_argument("--expected-version", default="0.1.0",
                        help="exact bare plugin release version the metadata must report (default: 0.1.0)")
    parser.add_argument("--expected-commit", default="",
                        help="explicit 40-hex build commit the health diagnostic version must carry; when "
                             "omitted the release part must match and any commit part must be a 40-hex hash")
    return parser.parse_args(argv)


def resolve_expected(args):
    """Validate the expected build identity, failing closed on misuse."""
    version = (args.expected_version or "").strip()
    if not version:
        return None, None, "--expected-version must not be empty"
    commit = (args.expected_commit or "").strip().lower()
    if commit and re.fullmatch(r"[0-9a-f]{40}", commit) is None:
        return None, None, f"--expected-commit must be a 40-hex commit, got {args.expected_commit!r}"
    return version, commit, None


def reset_artifacts(artifacts: Path) -> None:
    """Remove harness-owned host directories so each run starts from clean state.

    Only directories this harness creates are touched, so the isolated host
    databases and logs from the previous run do not leak counts into this one.
    """
    if not artifacts.exists():
        return
    for child in artifacts.iterdir():
        if child.is_dir() and (child.name.startswith("host") or child.name == "abi-probe-data"):
            shutil.rmtree(child, ignore_errors=True)


def resolve_specs(recorder, args, artifacts):
    """Resolve case specs, failing closed on unknown modules or an empty match.

    A requested module that does not exist, or a filter that matches no case,
    must never be silently reported as a green run.
    """
    requested = [m for m in (args.modules.split(",") if args.modules else DEFAULT_MODULES) if m]
    unknown = [m for m in requested if not (HERE / "acceptance" / "cases" / f"{m}.py").exists()]
    if unknown:
        recorder.write(artifacts / "result.json",
                       extra={"build": {"error": f"unknown case modules: {', '.join(unknown)}"}})
        print(f"[ERROR] unknown case modules: {', '.join(unknown)}", file=sys.stderr)
        return None, requested
    try:
        specs = load_specs(requested, only=args.only)
    except Exception as error:  # noqa: BLE001
        recorder.write(artifacts / "result.json", extra={"build": {"error": f"case import failed: {error}"}})
        print(f"[ERROR] case import failed: {error}", file=sys.stderr)
        return None, requested
    if not specs:
        recorder.write(artifacts / "result.json",
                       extra={"build": {"error": "no cases matched --only/modules filter",
                                        "modules": requested, "only": args.only}})
        print("[ERROR] no cases matched; refusing to report success", file=sys.stderr)
        return None, requested
    return specs, requested


def main(argv=None):
    args = parse_args(argv)
    artifacts = Path(args.artifacts)
    artifacts.mkdir(parents=True, exist_ok=True)
    reset_artifacts(artifacts)
    recorder = Recorder(artifacts)

    expected_version, expected_commit, expected_error = resolve_expected(args)
    if expected_error:
        recorder.write(artifacts / "result.json", extra={"build": {"error": expected_error}})
        print(f"[ERROR] {expected_error}", file=sys.stderr)
        return 2

    specs, modules = resolve_specs(recorder, args, artifacts)
    if specs is None:
        return 2

    ctx = Context(artifacts, Path(args.cpa), Path(args.plugin), plugin_id=args.plugin_id,
                  expected_version=expected_version, expected_commit=expected_commit)
    build = {"cpa": str(Path(args.cpa).resolve()), "plugin": str(Path(args.plugin).resolve()),
             "plugin_id": args.plugin_id, "modules": modules, "cases_planned": len(specs)}
    build.update(ctx.version_metadata())

    started = time.time()
    try:
        ctx.mock.start()
        # A default host is booted so host-plugins.json evidence exists; every
        # case then runs against its own freshly booted isolated host.
        ctx.build_host(root=artifacts / "host")
        ctx.host.start()
        ctx.wire_clients()
        # Persist the raw host registration for evidence.
        try:
            raw = ctx.host.plugin_registration()
            (artifacts / "host-plugins.json").write_text(json.dumps(raw, indent=2))
        except Exception as error:  # noqa: BLE001
            (artifacts / "host-plugins.error.txt").write_text(str(error))

        for spec in specs:
            # Every case starts from a fresh isolated host (its own SQLite DB and
            # config), so no case can contaminate another's ledger or limits.
            try:
                ctx.recreate_host(spec["name"])
            except Exception as error:  # noqa: BLE001 - boot failure is its own result
                recorder.cases.append({"case": spec["name"], "expects": spec["expects"], "pass": False,
                                       "observed": {"error": f"host boot failed: {type(error).__name__}: {error}"}})
                print(f"[FAIL] {spec['name']} (host boot)", file=sys.stderr)
                continue
            ctx.mock.reset()
            ctx.mock.default_script = Script()
            ctx.mock.fail_first = 0
            record = recorder.run(spec["name"], lambda fn=spec["fn"]: fn(ctx), spec["expects"], mock=ctx.mock)
            status = "PASS" if record["pass"] else "FAIL"
            print(f"[{status}] {spec['name']}", file=sys.stderr)
    except Exception as error:  # noqa: BLE001 - boot failure is itself a result
        build["boot_error"] = f"{type(error).__name__}: {error}"
        print(f"[BOOT-FAIL] {error}", file=sys.stderr)
    finally:
        if ctx.host is not None:
            ctx.host.stop()
        ctx.mock.stop()

    extra = {
        "build": build,
        "elapsed_s": round(time.time() - started, 3),
        "host_root": str(artifacts / "host"),
        "host_log": str(artifacts / "host" / "host.log"),
    }
    payload = recorder.write(artifacts / "result.json", extra=extra)
    print(json.dumps(payload["summary"]), file=sys.stderr)
    # Fail closed: a boot failure or a zero-case run is never a green result.
    if build.get("boot_error"):
        return 2
    if payload["summary"]["total"] == 0:
        return 2
    # Exit non-zero when any case failed, so build/run status stays separable.
    return 0 if payload["summary"]["failed"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
