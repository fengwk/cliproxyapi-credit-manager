"""Browser startup failures must not leak Playwright into subsequent cases."""

import os
import unittest
from unittest.mock import MagicMock, patch

from acceptance.cases import browser


class BrowserRuntimeTests(unittest.TestCase):
    def test_case_launch_failure_always_stops_runtime(self):
        # Exercise both real case entrypoints, not just the cleanup helper.
        for case in (browser.login_and_usage, browser.lazy_capture):
            with self.subTest(case=case.__name__):
                driver = MagicMock()
                driver.chromium.launch.side_effect = RuntimeError("launch failed")
                ctx = MagicMock()
                ctx.artifacts = "/synthetic"
                with patch.object(browser, "_launch", return_value=driver), patch.object(
                    browser, "_seed", return_value=({}, "key", {"started_at": "2026-01-01T00:00:00Z"})
                ):
                    with self.assertRaisesRegex(RuntimeError, "launch failed"):
                        case(ctx)
                driver.stop.assert_called_once_with()

    def test_close_failure_still_stops_runtime(self):
        driver = MagicMock()
        driver.chromium.launch.return_value.close.side_effect = RuntimeError("close failed")
        with patch.object(browser, "_launch", return_value=driver):
            with self.assertRaisesRegex(RuntimeError, "close failed"):
                with browser._browser():
                    pass
        driver.stop.assert_called_once_with()

    def test_explicit_chrome_is_used_without_fallback(self):
        # A bad executable is an explicit failure, never a silent browser swap.
        driver = MagicMock()
        driver.chromium.launch.side_effect = RuntimeError("bad executable")
        with patch.object(browser, "_launch", return_value=driver), patch.dict(
            os.environ, {"CHROME_BIN": "/synthetic/chrome"}
        ):
            with self.assertRaisesRegex(RuntimeError, "bad executable"):
                with browser._browser():
                    pass
        driver.chromium.launch.assert_called_once_with(headless=True, executable_path="/synthetic/chrome")
        driver.stop.assert_called_once_with()

    def test_default_browser_and_normal_cleanup(self):
        driver = MagicMock()
        with patch.object(browser, "_launch", return_value=driver), patch.dict(os.environ):
            os.environ.pop("CHROME_BIN", None)
            with browser._browser() as instance:
                self.assertIs(instance, driver.chromium.launch.return_value)
        driver.chromium.launch.assert_called_once_with(headless=True)
        instance.close.assert_called_once_with()
        driver.stop.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
