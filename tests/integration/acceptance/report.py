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


def check_none(value, message):
    """Require an explicitly null JSON value (never a zero or a missing key)."""
    if value is not None:
        raise CaseFailure(f"{message}: expected null, got {value!r}")


#: Field names that the post-usage architecture removed from keys, requests and
#: reports. Admission never estimates or reserves money, so no response may
#: expose a hold or an estimate again -- absence is asserted rather than a zero.
RETIRED_HOLD_FIELDS = ("held_micro_usd", "initial_hold_micro_usd", "token_estimate")

#: Money ledgers removed by the query-time pricing contract. A key no longer
#: carries spend/used amounts, a request carries no frozen price or manual
#: adjustment, and a summary carries no adjustment total. Costs live only in the
#: derived report, so these fields must never reappear in a JSON payload.
RETIRED_MONEY_FIELDS = (
    "spent_micro_usd",
    "quota_used_micro_usd",
    "daily_used_micro_usd",
    "weekly_used_micro_usd",
    "monthly_used_micro_usd",
    "adjustment_micro_usd",
)

#: Request-level pricing/derivation facts removed with the frozen price and the
#: requested image count. ``price`` here is the request field only; a price-book
#: or rule payload legitimately carries its own ``price`` object.
RETIRED_LEDGER_FIELDS = RETIRED_MONEY_FIELDS + ("price", "price_tiers", "pricing_rule_id", "image_count")


def check_no_holds(payload, label):
    """Fail if a key, request or summary object still exposes a retired field."""
    for field in RETIRED_HOLD_FIELDS:
        check(field not in payload,
              f"{label} must not expose the retired {field!r} field: {sorted(payload)}")


def check_no_ledger_fields(payload, label):
    """Fail if a key/request/summary still exposes a retired money bookkeeping field."""
    for field in RETIRED_LEDGER_FIELDS:
        check(field not in payload,
              f"{label} must not expose the retired {field!r} field: {sorted(payload)}")


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
