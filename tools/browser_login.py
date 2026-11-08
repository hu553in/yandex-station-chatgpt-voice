# /// script
# requires-python = ">=3.10"
# dependencies = ["playwright==1.64.1"]
# ///
"""Sign in to ChatGPT in a private Chrome window and save only its session cookies."""

import argparse
import contextlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time

SESSION_COOKIE = "__Secure-next-auth.session-token"
MAX_CHUNKS = 16
MAX_VALUE_BYTES = 4096
ORIGIN = "https://chatgpt.com"
LOGIN_URL = ORIGIN + "/auth/login"
UNSAFE_ENVIRONMENT = ("DEBUG", "PWDEBUG", "PW_", "PLAYWRIGHT_", "NODE_")
PROMPT = (
    "A private Chrome window opened. Sign in to ChatGPT there.\n"
    "When your chats appear, come back here and press Enter."
)
RETRY = "No ChatGPT sign-in found yet. Finish signing in, then press Enter again."
CANNOT_START = (
    "Cannot start Chrome. Install Google Chrome, or set CHROME in config.mk "
    "to the full path of a Chrome or Chromium program."
)
INCOMPLETE = "ChatGPT sign-in cookies are incomplete or unsupported. Run make login again."


class UserError(Exception):
    pass


class Canceled(Exception):
    pass


def valid_value(value):
    return (
        isinstance(value, str)
        and 0 < len(value) <= MAX_VALUE_BYTES
        and all(0x21 <= ord(c) <= 0x7E and c not in '",;\\' for c in value)
    )


def session_family(cookies, now):
    found = {}
    for cookie in cookies:
        name = cookie.get("name")
        if not isinstance(name, str) or (
            name != SESSION_COOKIE and not name.startswith(SESSION_COOKIE + ".")
        ):
            continue
        expires = cookie.get("expires", -1)
        if (
            cookie.get("domain") not in ("chatgpt.com", ".chatgpt.com")
            or cookie.get("path") != "/"
            or cookie.get("secure") is not True
            or cookie.get("httpOnly") is not True
            or (expires != -1 and expires <= now)
            or not cookie.get("value")
        ):
            continue
        if name in found or not valid_value(cookie["value"]):
            raise UserError(INCOMPLETE)
        found[name] = cookie["value"]
    names = (
        [SESSION_COOKIE]
        if list(found) == [SESSION_COOKIE]
        else [f"{SESSION_COOKIE}.{i}" for i in range(len(found))]
    )
    if len(found) > MAX_CHUNKS or sorted(found) != sorted(names):
        raise UserError(INCOMPLETE)
    return [{"name": name, "value": found[name]} for name in names]


def browser_program(chrome):
    explicit = bool(chrome)
    if not chrome:
        chrome = (
            "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
            if sys.platform == "darwin"
            else shutil.which("google-chrome") or shutil.which("google-chrome-stable") or ""
        )
        if not chrome:
            raise UserError(CANNOT_START)
    path = Path(chrome)
    if not path.is_absolute() or not path.is_file() or not os.access(path, os.X_OK):
        if not explicit:
            raise UserError(CANNOT_START)
        raise UserError(
            "CHROME must be the full path of a Chrome or Chromium program."
        )
    return str(path)


@contextlib.contextmanager
def running_chrome(playwright, program, profile, headless):
    process = None
    browser = None
    try:
        try:
            with socket.socket() as selected:
                selected.bind(("127.0.0.1", 0))
                port = selected.getsockname()[1]
            command = [
                program,
                "--user-data-dir=" + profile,
                "--remote-debugging-address=127.0.0.1",
                "--remote-debugging-port=" + str(port),
                "--no-first-run",
                "--no-default-browser-check",
            ]
            if headless:
                command.append("--headless=new")
            command.append("about:blank")
            process = subprocess.Popen(
                command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
            )
            deadline = time.monotonic() + 15
            while process.poll() is None:
                try:
                    browser = playwright.chromium.connect_over_cdp(
                        f"http://127.0.0.1:{port}", timeout=1000, no_defaults=True
                    )
                    break
                except Exception:
                    if time.monotonic() >= deadline:
                        break
                    time.sleep(0.1)
            if browser is None:
                raise UserError(CANNOT_START)
            context = browser.contexts[0]
        except Exception:
            raise UserError(CANNOT_START) from None
        yield context
    finally:
        if browser is not None:
            _quietly(browser.close)
        if process is not None:
            stop_chrome(process)


def stop_chrome(process):
    if process.poll() is None:
        with contextlib.suppress(ProcessLookupError):
            process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def scrub_environment(environ):
    for name in list(environ):
        if name.upper().startswith(UNSAFE_ENVIRONMENT):
            del environ[name]


def remove_profile(profile):
    for _ in range(20):
        shutil.rmtree(profile, ignore_errors=True)
        if not os.path.lexists(profile):
            return
        time.sleep(0.25)
    raise UserError(
        f"Could not remove the temporary browser profile {profile}. "
        "Delete it yourself; it holds your ChatGPT sign-in."
    )


def capture(chrome, confirm, notify, *, headless=False, prepare=None):
    program = browser_program(chrome)
    scrub_environment(os.environ)
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        raise UserError(
            "Playwright is not available. Run make login, which installs it with uv."
        ) from None
    profile = tempfile.mkdtemp(prefix="station-login-")
    try:
        with contextlib.ExitStack() as cleanup:
            try:
                playwright = sync_playwright().start()
                cleanup.callback(_quietly, playwright.stop)
                context = cleanup.enter_context(
                    running_chrome(playwright, program, profile, headless)
                )
            except Exception:
                raise UserError(CANNOT_START) from None
            try:
                if prepare is not None:
                    prepare(context)
                page = context.pages[0] if context.pages else context.new_page()
                page.goto(LOGIN_URL, wait_until="commit")
            except Exception:
                raise UserError(
                    "Chrome could not open the ChatGPT sign-in page. "
                    "Check this computer's internet connection and run make login again."
                ) from None
            notify(PROMPT)
            while True:
                if not confirm() or page.is_closed():
                    raise Canceled
                try:
                    cookies = context.cookies(ORIGIN)
                except Exception:
                    raise UserError(
                        "Chrome closed before sign-in finished. Run make login again."
                    ) from None
                family = session_family(cookies, time.time())
                if family:
                    return family
                notify(RETRY)
    finally:
        remove_profile(profile)


def _quietly(close):
    with contextlib.suppress(Exception):
        close()


def write_private(path, cookies):
    data = json.dumps({"cookies": cookies}, separators=(",", ":")).encode()
    try:
        fd = os.open(
            path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600
        )
    except OSError:
        raise UserError("Cannot create the private sign-in file.") from None
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
    except OSError:
        with contextlib.suppress(OSError):
            os.unlink(path)
        raise UserError("Cannot write the private sign-in file.") from None


@contextlib.contextmanager
def suppress_browser_diagnostics():
    sys.stderr.flush()
    saved = os.dup(2)
    try:
        with open(os.devnull, "wb") as devnull:
            os.dup2(devnull.fileno(), 2)
        with os.fdopen(os.dup(saved), "w") as terminal:
            yield terminal
    finally:
        os.dup2(saved, 2)
        os.close(saved)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chrome", default="")
    parser.add_argument("output", type=Path)
    args = parser.parse_args(argv)
    with suppress_browser_diagnostics() as terminal:

        def notify(message):
            print(message, file=terminal, flush=True)

        try:
            cookies = capture(args.chrome, lambda: sys.stdin.readline() != "", notify)
            write_private(args.output, cookies)
        except UserError as error:
            notify(f"Error: {error}")
            return 1
        except (Canceled, KeyboardInterrupt):
            return 130
        except Exception:
            notify("Error: Browser sign-in failed unexpectedly. Nothing was saved.")
            return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
