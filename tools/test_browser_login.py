import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

import browser_login


TOOLS = Path(__file__).resolve().parent
GOLDEN = TOOLS.parent / "internal/voice/testdata/browser-sign-in.json"
FAMILY = browser_login.SESSION_COOKIE


def jar_cookie(name, value, **overrides):
    cookie = {
        "name": name,
        "value": value,
        "domain": "chatgpt.com",
        "path": "/",
        "expires": -1,
        "httpOnly": True,
        "secure": True,
        "sameSite": "Lax",
    }
    cookie.update(overrides)
    return cookie


def golden_jar():
    return [
        jar_cookie(FAMILY + ".1", "synthetic-golden-chunk-1"),
        jar_cookie(FAMILY + ".0", "synthetic-golden-chunk-0", expires=4102444800.5),
        jar_cookie(FAMILY + ".2", "script-visible", httpOnly=False),
        jar_cookie(FAMILY + ".3", "insecure", secure=False),
        jar_cookie(FAMILY + ".4", "other-path", path="/api"),
        jar_cookie(FAMILY + ".5", "other-host", domain="auth.chatgpt.com"),
        jar_cookie(FAMILY + ".6", "expired", expires=1),
        jar_cookie(FAMILY + ".7", ""),
        jar_cookie("__Secure-next-auth.callback-url", "https%3A%2F%2Fchatgpt.com"),
        jar_cookie("cf_clearance", "synthetic-edge"),
        jar_cookie("oai-did", "synthetic-device", httpOnly=False, secure=False),
    ]


class SessionFamilyTest(unittest.TestCase):
    def test_keeps_only_the_session_family_in_the_go_transfer_shape(self):
        family = browser_login.session_family(golden_jar(), now=1000)
        self.assertEqual({"cookies": family}, json.loads(GOLDEN.read_text()))

    def test_unchunked_and_absent_sessions(self):
        self.assertEqual(
            browser_login.session_family([jar_cookie(FAMILY, "whole", domain=".chatgpt.com")], 0),
            [{"name": FAMILY, "value": "whole"}],
        )
        self.assertEqual(browser_login.session_family([jar_cookie("cf_clearance", "x")], 0), [])

    def test_rejects_ambiguous_or_invalid_families(self):
        for name, jar in (
            ("gap", [jar_cookie(FAMILY + ".0", "a"), jar_cookie(FAMILY + ".2", "c")]),
            ("base and chunks", [jar_cookie(FAMILY, "a"), jar_cookie(FAMILY + ".0", "b")]),
            ("too many", [jar_cookie(f"{FAMILY}.{i}", "v") for i in range(17)]),
            ("non-canonical", [jar_cookie(FAMILY + ".00", "a")]),
            (
                "duplicate host",
                [jar_cookie(FAMILY, "a"), jar_cookie(FAMILY, "b", domain=".chatgpt.com")],
            ),
            ("separator", [jar_cookie(FAMILY, "a;b")]),
            ("space", [jar_cookie(FAMILY, "a b")]),
            ("non-ASCII", [jar_cookie(FAMILY, "значение")]),
            ("oversized", [jar_cookie(FAMILY, "v" * 4097)]),
        ):
            with self.subTest(name=name), self.assertRaises(browser_login.UserError) as rejected:
                browser_login.session_family(jar, 0)
            self.assertEqual(str(rejected.exception), browser_login.INCOMPLETE)


class BrowserProgramTest(unittest.TestCase):
    def test_chrome_override_is_a_literal_absolute_executable(self):
        with tempfile.TemporaryDirectory() as directory:
            program = Path(directory) / "Google Chrome; $(touch MARKER) `x`"
            program.write_text("#!/bin/sh\n")
            program.chmod(0o700)
            self.assertEqual(
                browser_login.browser_program(str(program)), str(program)
            )
            program.chmod(0o600)
            for value in (str(program), "chrome", str(Path(directory) / "missing"), directory):
                with self.subTest(value=value), self.assertRaises(browser_login.UserError):
                    browser_login.browser_program(value)

    def test_default_chrome_uses_the_installed_program(self):
        with tempfile.TemporaryDirectory() as directory:
            program = Path(directory) / "google-chrome"
            program.write_text("#!/bin/sh\n")
            program.chmod(0o700)
            with (
                mock.patch.object(browser_login.sys, "platform", "linux"),
                mock.patch.object(browser_login.shutil, "which", return_value=str(program)),
            ):
                self.assertEqual(browser_login.browser_program(""), str(program))
            program.unlink()
            with (
                mock.patch.object(browser_login.sys, "platform", "linux"),
                mock.patch.object(browser_login.shutil, "which", return_value=None),
                self.assertRaises(browser_login.UserError) as missing,
            ):
                browser_login.browser_program("")
            self.assertEqual(str(missing.exception), browser_login.CANNOT_START)

    def test_debugging_environment_is_removed(self):
        environ = {
            "DEBUG": "pw:protocol",
            "DEBUG_FILE": "/tmp/protocol.log",
            "PWDEBUG": "1",
            "PW_TEST_SOURCE_TRANSFORM": "x",
            "PLAYWRIGHT_DRIVER_PATH": "x",
            "NODE_OPTIONS": "--inspect",
            "PATH": "/usr/bin",
            "HOME": "/home/user",
            "DISPLAY": ":0",
        }
        browser_login.scrub_environment(environ)
        self.assertEqual(environ, {"PATH": "/usr/bin", "HOME": "/home/user", "DISPLAY": ":0"})


class FakeContext:
    def __init__(self, browser, profile):
        self.browser = browser
        self.profile = profile
        self.pages = [types.SimpleNamespace(goto=self.goto, is_closed=lambda: self.closed)]
        self.closed = False

    def goto(self, url, wait_until):
        self.browser.visited.append((url, wait_until))
        if self.browser.fail == "goto":
            raise RuntimeError("navigation private-marker")

    def cookies(self, url):
        self.browser.cookie_urls.append(url)
        if self.browser.fail == "cookies":
            raise RuntimeError("Target closed private-marker")
        return self.browser.jars.pop(0)

    def close(self):
        self.closed = True


class FakeBrowser:
    def __init__(self, jars=None, fail=None):
        self.jars = list(jars or [golden_jar()])
        self.fail = fail
        self.visited, self.cookie_urls, self.launches = [], [], []
        self.context = None
        self.stopped = False
        self.environment = None
        self.terminated = False

    def launch(self, command, **options):
        self.environment = dict(os.environ)
        profile = next(arg.split("=", 1)[1] for arg in command if arg.startswith("--user-data-dir="))
        info = os.stat(profile)
        self.launches.append((profile, stat.S_IMODE(info.st_mode), command))
        self.process = types.SimpleNamespace(
            poll=lambda: 1 if self.fail == "launch" or self.terminated else None,
            terminate=self.terminate,
            wait=lambda **kwargs: 0,
        )
        return self.process

    def terminate(self):
        self.terminated = True

    def connect_over_cdp(self, address, timeout, no_defaults):
        if not no_defaults:
            raise RuntimeError("default browser options must not be overwritten")
        if self.fail == "connect interrupt":
            raise KeyboardInterrupt
        if self.fail == "connect timeout":
            raise RuntimeError("private-marker connection details")
        self.address = address
        profile = self.launches[0][0]
        self.context = FakeContext(self, profile)
        self.contexts = [self.context]
        return self

    def close(self):
        self.context.close()

    def module(self):
        browser = self

        class Playwright:
            chromium = browser

            def stop(self):
                browser.stopped = True

        class Manager:
            def start(self):
                return Playwright()

        sync_api = types.ModuleType("playwright.sync_api")
        sync_api.sync_playwright = Manager
        return {"playwright": types.ModuleType("playwright"), "playwright.sync_api": sync_api}


class CaptureLifecycleTest(unittest.TestCase):
    def run_capture(self, browser, confirm=lambda: True, chrome=""):
        notes = []
        with (
            mock.patch.dict(sys.modules, browser.module()),
            mock.patch.object(browser_login, "browser_program", return_value="/synthetic/chrome"),
            mock.patch.object(browser_login.subprocess, "Popen", side_effect=browser.launch),
            mock.patch.dict(os.environ, DEBUG="pw:protocol", PWDEBUG="1"),
        ):
            try:
                return browser_login.capture(chrome, confirm, notes.append), notes
            finally:
                self.assertNotIn("DEBUG", browser.environment or {})
                self.assertNotIn("PWDEBUG", browser.environment or {})

    def assert_cleaned(self, browser):
        profile, mode, command = browser.launches[0]
        self.assertEqual(mode, 0o700)
        self.assertIn("--remote-debugging-address=127.0.0.1", command)
        port = next(arg.split("=", 1)[1] for arg in command if arg.startswith("--remote-debugging-port="))
        self.assertGreater(int(port), 0)
        self.assertNotIn("--enable-automation", command)
        self.assertNotIn("--no-sandbox", command)
        self.assertTrue(browser.terminated or browser.fail == "launch")
        self.assertFalse(os.path.lexists(profile), "browser profile survived")
        self.assertTrue(browser.stopped)
        if browser.context is not None:
            self.assertTrue(browser.context.closed)

    def test_success_returns_family_and_removes_profile(self):
        browser = FakeBrowser()
        family, notes = self.run_capture(browser)
        self.assertEqual({"cookies": family}, json.loads(GOLDEN.read_text()))
        self.assertEqual(browser.visited, [(browser_login.LOGIN_URL, "commit")])
        self.assertEqual(browser.cookie_urls, [browser_login.ORIGIN])
        self.assertEqual(notes, [browser_login.PROMPT])
        self.assert_cleaned(browser)

    def test_waits_for_another_enter_until_signed_in(self):
        browser = FakeBrowser(jars=[[jar_cookie("cf_clearance", "x")], golden_jar()])
        family, notes = self.run_capture(browser)
        self.assertEqual(len(family), 2)
        self.assertEqual(notes, [browser_login.PROMPT, browser_login.RETRY])
        self.assert_cleaned(browser)

    def test_cancel_and_interrupt_clean_up(self):
        for name, confirm, expected in (
            ("EOF", lambda: False, browser_login.Canceled),
            ("Ctrl-C", mock.Mock(side_effect=KeyboardInterrupt), KeyboardInterrupt),
        ):
            with self.subTest(name=name):
                browser = FakeBrowser()
                with self.assertRaises(expected):
                    self.run_capture(browser, confirm)
                self.assertEqual(browser.cookie_urls, [])
                self.assert_cleaned(browser)

    def test_closed_sign_in_window_saves_nothing(self):
        browser = FakeBrowser()
        def confirm():
            browser.context.close()
            return True
        with self.assertRaises(browser_login.Canceled):
            self.run_capture(browser, confirm)
        self.assertEqual(browser.cookie_urls, [])
        self.assert_cleaned(browser)

    def test_browser_failures_are_fixed_messages(self):
        for fail, message in (
            ("launch", browser_login.CANNOT_START),
            ("goto", "could not open the ChatGPT sign-in page"),
            ("cookies", "Chrome closed before sign-in finished"),
        ):
            with self.subTest(fail=fail):
                browser = FakeBrowser(fail=fail)
                with self.assertRaises(browser_login.UserError) as rejected:
                    self.run_capture(browser)
                self.assertIn(message, str(rejected.exception))
                self.assertNotIn("private", str(rejected.exception))
                self.assertIsNone(rejected.exception.__cause__)
                self.assertTrue(rejected.exception.__suppress_context__)
                self.assert_cleaned(browser)

    def test_interruption_and_timeout_during_start_stop_chrome(self):
        browser = FakeBrowser(fail="connect interrupt")
        with self.assertRaises(KeyboardInterrupt):
            self.run_capture(browser)
        self.assert_cleaned(browser)
        browser = FakeBrowser(fail="connect timeout")
        with (
            mock.patch.object(browser_login.time, "monotonic", side_effect=[0, 16]),
            self.assertRaises(browser_login.UserError) as timed_out,
        ):
            self.run_capture(browser)
        self.assertEqual(str(timed_out.exception), browser_login.CANNOT_START)
        self.assert_cleaned(browser)

    def test_missing_playwright_is_a_fixed_message(self):
        with (
            mock.patch.dict(sys.modules, {"playwright": None, "playwright.sync_api": None}),
            mock.patch.object(browser_login, "browser_program", return_value="/synthetic/chrome"),
            mock.patch.dict(os.environ),
            mock.patch.object(browser_login.tempfile, "mkdtemp") as profile,
        ):
            with self.assertRaises(browser_login.UserError) as rejected:
                browser_login.capture("", lambda: True, print)
        self.assertIn("Playwright is not available", str(rejected.exception))
        profile.assert_not_called()


class MainTest(unittest.TestCase):
    def run_main(self, browser, stdin, output):
        with tempfile.TemporaryFile("w+") as diagnostics:
            saved = os.dup(2)
            try:
                os.dup2(diagnostics.fileno(), 2)
                with (
                    mock.patch.dict(sys.modules, browser.module()),
                    mock.patch.object(browser_login, "browser_program", return_value="/synthetic/chrome"),
                    mock.patch.object(browser_login.subprocess, "Popen", side_effect=browser.launch),
                    mock.patch.object(sys, "stdin", stdin),
                ):
                    code = browser_login.main(["--chrome=", str(output)])
            finally:
                os.dup2(saved, 2)
                os.close(saved)
            diagnostics.seek(0)
            return code, diagnostics.read()

    def test_writes_private_transfer_file(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "sign-in.json"
            code, diagnostics = self.run_main(FakeBrowser(), mock.Mock(readline=lambda: "\n"), output)
            self.assertEqual(code, 0, diagnostics)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            self.assertEqual(json.loads(output.read_text()), json.loads(GOLDEN.read_text()))
            self.assertEqual(diagnostics, browser_login.PROMPT + "\n")
            code, diagnostics = self.run_main(FakeBrowser(), mock.Mock(readline=lambda: "\n"), output)
            self.assertEqual(code, 1)
            self.assertEqual(diagnostics.splitlines()[-1], "Error: Cannot create the private sign-in file.")

    def test_failure_and_cancel_save_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "sign-in.json"
            code, diagnostics = self.run_main(
                FakeBrowser(fail="launch"), mock.Mock(readline=lambda: "\n"), output
            )
            self.assertEqual(code, 1)
            self.assertEqual(diagnostics, f"Error: {browser_login.CANNOT_START}\n")
            code, diagnostics = self.run_main(FakeBrowser(), mock.Mock(readline=lambda: ""), output)
            self.assertEqual(code, 130)
            self.assertEqual(diagnostics, browser_login.PROMPT + "\n")
            malformed = [jar_cookie(FAMILY, "private-marker", expires="private-marker")]
            code, diagnostics = self.run_main(
                FakeBrowser(jars=[malformed]), mock.Mock(readline=lambda: "\n"), output
            )
            self.assertEqual(code, 1)
            self.assertEqual(
                diagnostics.splitlines()[-1],
                "Error: Browser sign-in failed unexpectedly. Nothing was saved.",
            )
            self.assertNotIn("private-marker", diagnostics)
            self.assertFalse(output.exists())


FIXTURE = r"""
import json, os, sys, tempfile
sys.path.insert(0, sys.argv[1])
import browser_login

chrome, headed, golden = sys.argv[2], sys.argv[3] == "1", json.loads(open(sys.argv[4]).read())
profiles, external, contexts, notes, automated = [], [], [], [], []
real_mkdtemp = tempfile.mkdtemp

def recording_mkdtemp(*args, **kwargs):
    path = real_mkdtemp(*args, **kwargs)
    profiles.append((path, os.stat(path).st_mode & 0o777))
    return path

tempfile.mkdtemp = recording_mkdtemp
family = browser_login.SESSION_COOKIE
set_cookies = "\n".join([
    family + ".0=synthetic-golden-chunk-0; Path=/; Secure; HttpOnly; SameSite=Lax",
    family + ".1=synthetic-golden-chunk-1; Path=/; Secure; HttpOnly; SameSite=Lax",
    family + ".2=script-visible; Path=/; Secure; SameSite=Lax",
    "__Secure-next-auth.callback-url=https%3A%2F%2Fchatgpt.com; Path=/; Secure; HttpOnly",
    "cf_clearance=synthetic-edge; Path=/; Secure; HttpOnly",
    "oai-did=synthetic-device; Path=/",
])

def fixture(route):
    if not route.request.url.startswith("https://chatgpt.com/"):
        external.append(1)
        route.abort()
        return
    route.fulfill(status=200, content_type="text/html", headers={"Set-Cookie": set_cookies},
                  body="<title>Station sign-in fixture</title><p>Synthetic page. No account.</p>")

def prepare(context):
    context.route("**/*", fixture)
    contexts.append(context)
    automated.append(context.pages[0].evaluate("navigator.webdriver"))

def confirm():
    contexts[0].pages[0].wait_for_load_state("load")
    return True

captured = browser_login.capture(chrome, confirm, notes.append, headless=not headed, prepare=prepare)
print(json.dumps({
    "real_chrome_launched": bool(contexts),
    "headed_browser_not_automated": not headed or automated == [False],
    "private_profile_mode_0700": profiles[0][1] == 0o700,
    "profile_removed_after_capture": not os.path.lexists(profiles[0][0]),
    "http_only_family_matches_go_transfer": {"cookies": captured} == golden,
    "decoy_cookies_dropped": [c["name"] for c in captured] == [family + ".0", family + ".1"],
    "user_prompted_once": notes == [browser_login.PROMPT],
    "no_requests_left_chatgpt_fixture": not external,
}))
"""


@unittest.skipUnless(
    os.environ.get("STATION_BROWSER_TEST") == "1",
    "set STATION_BROWSER_TEST=1 to drive installed Chrome against a synthetic fixture",
)
class RealChromeFixtureTest(unittest.TestCase):
    def test_capture_through_installed_chrome(self):
        uv = os.environ.get("UV") or "uv"
        headed = os.environ.get("STATION_BROWSER_HEADED", "")
        result = subprocess.run(
            [uv, "run", "--script", str(TOOLS / "browser_login.py"), "--help"],
            capture_output=True,
            text=True,
            timeout=300,
        )
        self.assertEqual(result.returncode, 0, result.stderr[-2000:])
        result = subprocess.run(
            [
                uv, "run", "--no-project", "--with", "playwright==1.64.1", "python", "-I", "-B", "-c",
                FIXTURE, str(TOOLS), os.environ.get("CHROME", ""), headed, str(GOLDEN),
            ],
            capture_output=True,
            text=True,
            timeout=300,
        )
        self.assertEqual(result.returncode, 0, result.stderr[-2000:])
        verdict = json.loads(result.stdout.splitlines()[-1])
        self.assertTrue(all(verdict.values()), verdict)


if __name__ == "__main__":
    unittest.main()
