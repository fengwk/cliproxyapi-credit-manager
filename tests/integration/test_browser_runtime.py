"""Browser startup failures must not leak Playwright into subsequent cases."""

import os
import unittest
from unittest.mock import MagicMock, patch

from acceptance.cases import browser


class BrowserRuntimeTests(unittest.TestCase):
    def test_case_launch_failure_always_stops_runtime(self):
        # Exercise every declared browser case dynamically, not just the cleanup
        # helper, so adding a case cannot silently escape this boundary test and
        # the module count stays derived from CASES rather than hardcoded.
        case_fns = [spec["fn"] for spec in browser.CASES]
        self.assertGreater(len(case_fns), 0, "browser module declares no cases")
        for case in case_fns:
            with self.subTest(case=case.__name__):
                driver = MagicMock()
                driver.chromium.launch.side_effect = RuntimeError("launch failed")
                ctx = MagicMock()
                ctx.artifacts = "/synthetic"
                # The cases seed a host before they launch a browser; make the
                # synthetic context support that setup so every case still
                # reaches (and fails at) the browser launch boundary.
                ctx.new_key.return_value = ({}, "key")
                ctx.set_price.return_value = MagicMock(status=200)
                ctx.client.chat.return_value = MagicMock(status=200)
                ctx.client.post.return_value = MagicMock(status=200)
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
