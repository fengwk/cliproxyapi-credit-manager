"""Harness contract tests that need neither the plugin library nor a host.

They keep the case inventory, the retired-field boundary and the runner's module
list honest, so a renamed module or a reintroduced estimate/hold/ledger field is
caught by `make static` instead of silently shrinking coverage.
"""

import ast
import re
import unittest
from pathlib import Path

import run_acceptance
from acceptance.cases import passive
from acceptance.report import (RETIRED_HOLD_FIELDS, RETIRED_LEDGER_FIELDS,
                               RETIRED_MONEY_FIELDS, CaseFailure, check_no_holds,
                               check_no_ledger_fields)

CASES_DIR = Path(__file__).resolve().parent / "acceptance" / "cases"


class HarnessInventoryTests(unittest.TestCase):
    def test_runner_module_inventory_matches_case_files(self):
        # The runner must neither name a missing module nor silently ignore an
        # existing case file, which would drop coverage without failing a run.
        present = {path.stem for path in CASES_DIR.glob("*.py") if path.stem != "__init__"}
        self.assertEqual(set(run_acceptance.DEFAULT_MODULES), present,
                         "DEFAULT_MODULES and acceptance/cases/*.py diverged")
        self.assertNotIn("warmup", run_acceptance.DEFAULT_MODULES,
                         "the removed warmup module must not return to the inventory")

    def test_every_module_declares_at_least_one_case(self):
        from acceptance.context import load_specs

        for module in run_acceptance.DEFAULT_MODULES:
            with self.subTest(module=module):
                self.assertTrue(load_specs([module]), f"{module} declares no cases")

    def test_case_files_never_read_a_retired_hold_field(self):
        # Reading `payload["held_micro_usd"]` (or an estimate) is how the obsolete
        # reservation architecture was asserted; only `check_no_holds` may name
        # those fields, and only to prove their absence.
        offenders = self._scan_case_files(RETIRED_HOLD_FIELDS, allow_checks=("check_no_holds",))
        self.assertEqual(offenders, [], f"retired hold/estimate fields are still asserted: {offenders}")

    def test_case_files_never_read_a_retired_money_field(self):
        # The query-time pricing rewrite removed key ledgers and manual
        # adjustments. A case that reads one of these fields would mean the old
        # money bookkeeping silently returned.
        offenders = self._scan_case_files(RETIRED_MONEY_FIELDS, allow_checks=("check_no_ledger_fields",))
        self.assertEqual(offenders, [], f"retired money fields are still asserted: {offenders}")

    def test_case_files_never_read_a_frozen_request_price(self):
        offenders = self._scan_case_files(("price_tiers", "pricing_rule_id"),
                                          allow_checks=("check_no_ledger_fields",))
        self.assertEqual(offenders, [], f"frozen pricing request fields are still asserted: {offenders}")

    def _scan_case_files(self, fields, allow_checks):
        offenders = []
        pattern = re.compile(r"""\[\s*["']([a-z_]+)["']\s*\]|\.get\(\s*["']([a-z_]+)["']""")
        allowed = set(allow_checks)
        for path in sorted(CASES_DIR.glob("*.py")):
            source = path.read_text(encoding="utf-8")
            for match in pattern.finditer(source):
                field = match.group(1) or match.group(2)
                if field not in fields:
                    continue
                line = source.count("\n", 0, match.start()) + 1
                line_text = source.splitlines()[line - 1]
                if any(check in line_text for check in allowed):
                    continue
                offenders.append(f"{path.name}:{line}: {field}")
        return offenders

    def test_retired_settings_are_pinned_for_rejection(self):
        # The passive boundary must keep testing the two removed settings as
        # configuration errors, in both the boot and hot-reload paths.
        self.assertEqual(set(passive.RETIRED_SETTINGS), {"warmup", "max_pending_requests"})
        for name, (yaml_snippet, patch_body) in passive.RETIRED_SETTINGS.items():
            with self.subTest(setting=name):
                self.assertIn(f"{name}:", yaml_snippet)
                self.assertTrue(yaml_snippet.startswith("      "),
                                "the retired YAML must sit under plugins.configs.<id>")
                self.assertIn(name, patch_body)


class NoHoldBoundaryTests(unittest.TestCase):
    def test_check_no_holds_rejects_every_retired_field(self):
        for field in RETIRED_HOLD_FIELDS:
            with self.subTest(field=field):
                with self.assertRaises(CaseFailure):
                    check_no_holds({"id": "x", field: 0}, "synthetic request")

    def test_check_no_holds_accepts_a_clean_payload(self):
        # A clean key/request carries only current fields; the absence check must
        # not be a blanket field-name ban.
        check_no_holds({"id": "x", "state": "settled", "spec": {"quota_micro_usd": 14}},
                       "synthetic key")

    def test_check_no_ledger_fields_rejects_every_retired_money_field(self):
        for field in RETIRED_MONEY_FIELDS:
            with self.subTest(field=field):
                with self.assertRaises(CaseFailure):
                    check_no_ledger_fields({"id": "x", field: 14}, "synthetic key")

    def test_check_no_ledger_fields_rejects_a_frozen_request_price(self):
        for field in ("price", "price_tiers", "pricing_rule_id", "image_count"):
            with self.subTest(field=field):
                with self.assertRaises(CaseFailure):
                    check_no_ledger_fields({"id": "x", field: 0}, "synthetic request")

    def test_check_no_ledger_fields_accepts_current_key_and_request(self):
        # A key keeps its four display budgets, and a request keeps service_tier;
        # neither is a money ledger.
        check_no_ledger_fields(
            {"id": "x",
             "spec": {"quota_micro_usd": 1, "daily_micro_usd": 1,
                      "weekly_micro_usd": 1, "monthly_micro_usd": 1},
             "service_tier": "priority"},
            "synthetic key/request")

    def test_case_modules_parse(self):
        # Cheap guard: every case module must still be syntactically valid Python
        # after the architecture rewrite (parse only, no import side effects).
        for path in sorted(CASES_DIR.glob("*.py")):
            with self.subTest(module=path.name):
                ast.parse(path.read_text(encoding="utf-8"), str(path))


if __name__ == "__main__":
    unittest.main()
