"""Acceptance result recording.

Each case records its expectation, what was actually observed, the upstream call
count and the log paths. A case passes only when its assertions hold; a bare
HTTP 200 is never treated as proof of correct accounting.
"""

from __future__ import annotations

import json
import time
import traceback
from pathlib import Path


class CaseFailure(AssertionError):
    pass


def check(condition, message):
    if not condition:
        raise CaseFailure(message)


def check_eq(actual, expected, message):
    if actual != expected:
        raise CaseFailure(f"{message}: expected {expected!r}, got {actual!r}")


class Recorder:
    def __init__(self, artifacts: Path):
        self.artifacts = Path(artifacts)
        self.cases = []
        self.meta = {"started_at": time.time()}

    def run(self, name, fn, expects, mock=None):
        """Run one case function, capturing outcome, expectation and call count."""
        record = {"case": name, "expects": expects}
        before = mock.count() if mock is not None else None
        started = time.time()
        try:
            detail = fn() or {}
            record["pass"] = True
            record["observed"] = detail
        except Exception as error:  # noqa: BLE001 - record every failure verbatim
            record["pass"] = False
            record["observed"] = {
                "error": f"{type(error).__name__}: {error}",
                "traceback": traceback.format_exc(),
            }
        record["duration_s"] = round(time.time() - started, 3)
        if mock is not None:
            calls = mock.calls
            after = len(calls)
            record["upstream_calls_delta"] = after - before
            record["upstream_calls_total"] = after
            record["upstream_calls"] = [
                {"path": row["path"], "upstream_key": row["upstream_key"], "stream": row["stream"]}
                for row in calls[before:]
            ]
        self.cases.append(record)
        return record

    def summary(self):
        passed = sum(1 for case in self.cases if case["pass"])
        return {
            "total": len(self.cases),
            "passed": passed,
            "failed": len(self.cases) - passed,
        }

    def write(self, path, extra=None):
        payload = {
            "meta": self.meta,
            "summary": self.summary(),
            "cases": self.cases,
        }
        if extra:
            payload.update(extra)
        Path(path).write_text(json.dumps(payload, indent=2, default=str))
        return payload
