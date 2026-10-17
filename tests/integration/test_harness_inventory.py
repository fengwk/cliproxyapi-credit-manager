"""Harness contract tests that need neither the plugin library nor a host.

They keep the case inventory, the retired-field boundary and the runner's module
list honest, so a renamed module or a reintroduced estimate/hold field is caught
by `make static` instead of silently shrinking coverage.
"""

import ast
import re
import unittest
from pathlib import Path

import run_acceptance
from acceptance.cases import passive
from acceptance.report import RETIRED_HOLD_FIELDS, CaseFailure, check_no_holds

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
        offenders = []
        for path in sorted(CASES_DIR.glob("*.py")):
            source = path.read_text(encoding="utf-8")
            for field in RETIRED_HOLD_FIELDS:
                patterns = (re.compile(rf"""\[\s*["']{field}["']\s*\]"""),
                            re.compile(rf"""\.get\(\s*["']{field}["']"""))
                if any(pattern.search(source) for pattern in patterns):
                    offenders.append(f"{path.name}: {field}")
        self.assertEqual(offenders, [], f"retired hold/estimate fields are still asserted: {offenders}")

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
        # A clean key/request carries only actual-used fields; the absence check
        # must not be a blanket field-name ban.
        check_no_holds({"id": "x", "spent_micro_usd": 14, "quota_used_micro_usd": 14}, "synthetic key")

    def test_case_modules_parse(self):
        # Cheap guard: every case module must still be syntactically valid Python
        # after the architecture rewrite (parse only, no import side effects).
        for path in sorted(CASES_DIR.glob("*.py")):
            with self.subTest(module=path.name):
                ast.parse(path.read_text(encoding="utf-8"), str(path))


if __name__ == "__main__":
    unittest.main()
