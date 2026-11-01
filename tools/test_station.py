import contextlib
import errno
import hashlib
import io
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock

import station


SOURCE = Path(__file__).resolve().parent.parent
SETTING_NAMES = ("VOICE", "BACKEND_MODEL", "BACKEND_THINKING_EFFORT")


def fake_station_main():
    root = Path(os.environ["STATION_TEST_ROOT"])
    (root / "exports.json").write_text(
        json.dumps({name: os.environ.get(name) for name in SETTING_NAMES})
    )
    overrides = r"""
process() {
    mkdir -p "$runtime"
    running=0
    if [ -f "$runtime/client.pid" ]; then
        pid=$(cat "$runtime/client.pid")
        verify_process
    fi
}
proc_stat() { actual_start=fixture; }
verify_process() {
    running=0
    if kill -0 "$pid" 2>/dev/null; then running=1; fi
}
"""

    def local_script(script):
        if script.startswith("sh /tmp/voice-script-"):
            script = Path(shlex.split(script)[1]).read_text()
        script = script.replace(station.COMMON, station.COMMON + overrides)
        for original, replacement in (
            ("app=/data/yandex-station-chatgpt-voice", root / "app"),
            ("runtime=/tmp/yandex-station-chatgpt-voice", root / "runtime"),
            ("lock=/data/.yandex-station-chatgpt-voice.lock", root / "lock"),
            ("log=/tmp/yandex-station-chatgpt-voice.log", root / "client.log"),
        ):
            name = original.split("=", 1)[0]
            script = script.replace(
                original, name + "=" + shlex.quote(str(replacement))
            )
        return script.replace('[ "$(id -u)" = 0 ]', "[ 0 = 0 ]")

    def fake_adb(args, *command, quiet=False):
        if command[0] in ("push", "pull"):
            shutil.copyfile(command[1], command[2])
            return b""
        if command[0] != "shell" or len(command) != 2:
            raise AssertionError("Unexpected fake ADB command")
        lexer = shlex.shlex(command[1], posix=True, punctuation_chars=";")
        if [next(lexer), next(lexer)] != ["sh", "-c"]:
            raise AssertionError("Unexpected shell wrapper")
        original = next(lexer)
        outer = command[1].replace(
            shlex.quote(original), shlex.quote(local_script(original)), 1
        )
        result = subprocess.run(
            ["/bin/sh", "-c", outer], capture_output=True, timeout=10
        )
        if result.returncode:
            raise AssertionError(
                "Local fake shell failed\n" + result.stderr.decode(errors="replace")
            )
        if b"Client started.\n" in result.stdout:
            pid = int((root / "runtime/client.pid").read_text())
            deadline = time.monotonic() + 5
            while not (root / "app/alive").exists() or (
                json.loads((root / "process.json").read_text())["pid"] != pid
            ):
                if time.monotonic() >= deadline:
                    raise AssertionError(
                        "Local fake client did not publish its launch record"
                    )
                time.sleep(0.01)
        sys.stderr.write(result.stderr.decode(errors="replace"))
        return result.stdout

    with (
        mock.patch.object(station, "adb_run", fake_adb),
        mock.patch.object(station, "arm_binary", return_value=b"synthetic-arm"),
        mock.patch.object(
            station, "session_input", return_value={"accessToken": "synthetic-token"}
        ),
        mock.patch.object(station, "auth"),
        mock.patch.object(station, "install"),
    ):
        station.main()


class RuntimeDirectoryTest(unittest.TestCase):
    def test_process_requires_private_owned_directory_without_stat(self):
        with tempfile.TemporaryDirectory(prefix="station-runtime-test-") as directory:
            root = Path(directory)
            runtime = root / "runtime"
            runtime.mkdir(mode=0o700)
            private_mode = (
                subprocess.check_output(
                    ["ls", "-ldn", str(runtime)], env={**os.environ, "LC_ALL": "C"}
                )
                .decode()
                .split()[0]
            )
            for name, mode, owner, symlink, expected in (
                ("private", 0o700, os.getuid(), False, 0),
                ("public", 0o755, os.getuid(), False, 1),
                ("wrong owner", 0o700, os.getuid() + 1, False, 1),
                ("symlink", 0o700, os.getuid(), True, 1),
            ):
                with self.subTest(name=name):
                    runtime.chmod(mode)
                    selected = runtime
                    if symlink:
                        selected = root / "link"
                        selected.symlink_to(runtime)
                    script = station.COMMON.replace(
                        '[ "$(id -u)" = 0 ]', f'[ "$(id -u)" = {os.getuid()} ]'
                    ).replace("drwx------:0", f"{private_mode}:{owner}")
                    script += (
                        f"\nruntime={shlex.quote(str(selected))}\n"
                        "stat() { return 127; }\nprocess\n"
                    )
                    result = subprocess.run(
                        ["/bin/sh", "-c", script], capture_output=True, timeout=5
                    )
                    self.assertEqual(result.returncode, expected, result.stderr)


class LaunchSettingsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="station-launch-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        shutil.copyfile(SOURCE / "Makefile", self.root / "Makefile")
        (self.root / "tools").mkdir()
        (self.root / "tools/station.py").write_text(
            "import sys\n"
            f"sys.path.insert(0, {str(SOURCE / 'tools')!r})\n"
            "from test_station import fake_station_main\n"
            "fake_station_main()\n"
        )
        (self.root / "app").mkdir()
        client = self.root / "app/client"
        client.write_text(
            f"#!{sys.executable}\n"
            "import json, os, signal, sys, time\n"
            "from pathlib import Path\n"
            "root = Path(os.environ['STATION_TEST_ROOT'])\n"
            "if sys.argv[1:] == ['auth', 'status']:\n"
            "    print('Saved token expiry (unverified): 2100-01-01T00:00:00Z')\n"
            "    sys.exit(0)\n"
            "alive = root / 'app/alive'\n"
            "def stop(*args):\n"
            "    alive.unlink(missing_ok=True)\n"
            "    sys.exit(0)\n"
            "signal.signal(signal.SIGTERM, stop)\n"
            "(root / 'process.json').write_text(json.dumps({'pid': os.getpid(), 'session': os.getsid(0)}))\n"
            "with (root / 'argv.jsonl').open('a') as output:\n"
            "    output.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "alive.touch()\n"
            "while True: time.sleep(0.1)\n"
        )
        client.chmod(0o700)
        launcher = self.root / "setsid"
        launcher.write_text(
            f"#!{sys.executable}\n"
            "import os, sys\n"
            "os.setsid()\n"
            "os.execvp(sys.argv[1], sys.argv[1:])\n"
        )
        launcher.chmod(0o700)
        self.environment = os.environ.copy()
        for name in (*SETTING_NAMES, "MAKEFLAGS", "MFLAGS", "MAKELEVEL"):
            self.environment.pop(name, None)
        self.environment.update(
            STATION_TEST_ROOT=str(self.root),
            PYTHONDONTWRITEBYTECODE="1",
            PATH=str(self.root) + os.pathsep + self.environment.get("PATH", os.defpath),
        )
        self.addCleanup(self.stop_local_client)

    def stop_local_client(self):
        pidfile = self.root / "runtime/client.pid"
        if pidfile.exists() and (self.root / "app/alive").exists():
            os.kill(int(pidfile.read_text()), 15)
            deadline = time.monotonic() + 3
            while (self.root / "app/alive").exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertFalse(
                (self.root / "app/alive").exists(), "test client did not stop"
            )

    def configure(self, values=None, source=None):
        text = "STATION_IP = 192.0.2.1\n"
        if source is not None:
            text += source
        elif values is not None:
            for name, value in zip(SETTING_NAMES, values):
                text += f"define {name}\n{value.replace('$', '$$')}\nendef\n"
        (self.root / "config.mk").write_text(text)

    def run_make(self, action):
        result = subprocess.run(
            [
                "make",
                "--no-print-directory",
                "-o",
                "build",
                f"PYTHON={sys.executable}",
                "ADB=fake-local-only",
                action,
            ],
            cwd=self.root,
            env=self.environment,
            capture_output=True,
            text=True,
            timeout=15,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return json.loads((self.root / "exports.json").read_text())

    def launches(self):
        path = self.root / "argv.jsonl"
        return (
            [json.loads(line) for line in path.read_text().splitlines()]
            if path.exists()
            else []
        )

    def test_status_reports_expiry_and_tolerates_old_or_missing_client(self):
        self.configure()
        client = self.root / "app/client"
        for name, expiry in (
            ("current", "2100-01-01T00:00:00Z"),
            ("old", "unknown"),
            ("missing", "unknown"),
        ):
            with self.subTest(name=name):
                if name == "old":
                    client.write_text(
                        "#!/bin/sh\necho 'unsupported command' >&2\nexit 1\n"
                    )
                elif name == "missing":
                    client.unlink()
                result = subprocess.run(
                    [
                        "make",
                        "--no-print-directory",
                        f"PYTHON={sys.executable}",
                        "ADB=fake-local-only",
                        "status",
                    ],
                    cwd=self.root,
                    env=self.environment,
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(
                    f"Saved token expiry (unverified): {expiry}\n", result.stdout
                )
                self.assertIn("Client stopped.", result.stdout)
                self.assertNotIn("unsupported command", result.stdout + result.stderr)
        self.assertEqual(self.launches(), [])

    def test_launcher_preserves_argv_and_detaches_session(self):
        marker = self.root / "MUST_NOT_RUN"
        values = (
            f"  голос\t'\"`touch {marker}`; | & <> \\ # $HOME $(touch {marker})\nsecond line  ",
            "-任意/future=model",
            " disabled ",
        )
        self.configure(values)
        expected = [
            "run",
            "--voice=" + values[0],
            "--backend-model=" + values[1],
            "--backend-thinking-effort=" + values[2],
        ]
        for action in ("start", "restart"):
            exports = self.run_make(action)
            self.assertEqual(exports, dict(zip(SETTING_NAMES, values)))
            self.assertEqual(self.launches()[-1], expected)
            identity = json.loads((self.root / "process.json").read_text())
            self.assertEqual(identity["session"], identity["pid"])
            self.assertEqual(
                int((self.root / "runtime/client.pid").read_text()), identity["pid"]
            )
            self.assertFalse(marker.exists(), "a setting executed as shell code")
        self.assertEqual(len(self.launches()), 2)
        self.run_make("start")
        self.assertEqual(len(self.launches()), 2, "start replaced the running snapshot")
        self.configure(("changed", "", "disabled"))
        self.run_make("start")
        self.assertEqual(len(self.launches()), 2, "configuration hot-reloaded")
        self.run_make("restart")
        self.assertEqual(
            self.launches()[-1],
            [
                "run",
                "--voice=changed",
                "--backend-model=",
                "--backend-thinking-effort=disabled",
            ],
        )

    def test_missing_empty_and_make_escaping(self):
        for values in (None, ("", "", ""), (" \t ", "-model", "Disabled")):
            self.configure(values)
            self.run_make("restart")
            actual = values or ("", "", "")
            self.assertEqual(
                self.launches()[-1],
                [
                    "run",
                    "--voice=" + actual[0],
                    "--backend-model=" + actual[1],
                    "--backend-thinking-effort=" + actual[2],
                ],
            )
        self.configure(
            source="VOICE = $()  cash$$hash\\#  \nBACKEND_MODEL = -model\nBACKEND_THINKING_EFFORT = disabled\n"
        )
        self.run_make("restart")
        self.assertEqual(
            self.launches()[-1],
            [
                "run",
                "--voice=  cash$hash#  ",
                "--backend-model=-model",
                "--backend-thinking-effort=disabled",
            ],
        )

    def test_other_actions_do_not_export_or_launch_settings(self):
        self.configure(("private-setting", "model", "effort"))
        for action in ("stop", "auth", "install"):
            self.assertEqual(self.run_make(action), dict.fromkeys(SETTING_NAMES))
            self.assertEqual(self.launches(), [])

    def test_unrepresentable_settings_fail_before_mutation(self):
        for value in ("private\0value", "private\udcffvalue"):
            diagnostics = io.StringIO()
            with (
                mock.patch.object(station.os, "environ", dict(os.environ, VOICE=value)),
                mock.patch.object(
                    sys, "argv", ["station.py", "--ip", "192.0.2.1", "start"]
                ),
                mock.patch.object(station, "acquire") as acquire,
                contextlib.redirect_stderr(diagnostics),
            ):
                with self.assertRaises(ValueError) as rejected:
                    station.main()
                self.assertNotIn("private", str(rejected.exception))
                acquire.assert_not_called()


class AuthInputTest(unittest.TestCase):
    def test_auth_environment_and_explicit_path_precedence(self):
        for cli, expected in (
            ([], "environment-path"),
            (["--session-json", "cli-path"], "cli-path"),
            (["--session-json="], ""),
        ):
            with (
                self.subTest(cli=cli),
                mock.patch.dict(os.environ, SESSION_JSON="environment-path"),
                mock.patch.object(
                    sys, "argv", ["station.py", "--ip", "192.0.2.1", *cli, "auth"]
                ),
                mock.patch.object(
                    station, "session_input", side_effect=ValueError("selected")
                ) as selected,
                mock.patch.object(station, "acquire") as acquire,
            ):
                with self.assertRaisesRegex(ValueError, "selected"):
                    station.main()
                selected.assert_called_once_with(expected)
                acquire.assert_not_called()

    def hidden_input(self, payload, interrupt=False):
        master, slave = pty.openpty()
        program = (
            "import hashlib, sys, termios\n"
            "import station\n"
            "before = termios.tcgetattr(0)\n"
            "try:\n"
            " token = station.session_input('')['accessToken'].encode()\n"
            " print('RESULT', len(token), hashlib.sha256(token).hexdigest(), flush=True)\n"
            "except (ValueError, EOFError, KeyboardInterrupt) as error:\n"
            " print('RESULT', type(error).__name__, str(error), flush=True)\n"
            "print('RESTORED', termios.tcgetattr(0) == before, flush=True)\n"
            "sys.stdin.readline()\n"
        )
        output = bytearray()
        process = None
        try:
            process = subprocess.Popen(
                [sys.executable, "-c", program],
                stdin=slave,
                stdout=slave,
                stderr=slave,
                start_new_session=True,
                env=dict(os.environ, PYTHONPATH=str(SOURCE / "tools")),
            )
            os.set_blocking(master, False)
            offset = 0
            signaled = False
            acknowledged = False
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                pending = (
                    [master]
                    if b"(hidden): " in output and offset < len(payload)
                    else []
                )
                ready, writable, _ = select.select([master], pending, [], 0.01)
                if ready:
                    try:
                        data = os.read(master, 65536)
                        if not data:
                            process.wait(timeout=5)
                            break
                        output.extend(data)
                    except OSError as error:
                        if error.errno != errno.EIO:
                            raise
                        process.wait(timeout=5)
                        break
                if process.poll() is not None and not ready:
                    break
                if b"(hidden): " not in output:
                    continue
                if writable and offset < len(payload):
                    try:
                        offset += os.write(master, payload[offset : offset + 256])
                    except BlockingIOError:
                        pass
                if interrupt and offset == len(payload) and not signaled:
                    process.send_signal(signal.SIGINT)
                    signaled = True
                if b"RESTORED " in output and offset == len(payload) and not acknowledged:
                    os.write(master, b"\n")
                    acknowledged = True
            self.assertIsNotNone(
                process.poll(), "Hidden input did not finish: " + output.decode(errors="replace")
            )
            self.assertEqual(process.returncode, 0, output.decode(errors="replace"))
            self.assertIn(b"RESTORED True", output)
            self.assertNotIn(b"PRIVATE-TOKEN", output)
            self.assertNotIn(b"A" * 32, output)
            return output.decode()
        finally:
            if process is not None and process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            os.close(master)
            os.close(slave)

    def test_hidden_paste_accepts_long_tokens_and_restores_terminal(self):
        for size in (5000, 32768):
            with self.subTest(size=size):
                token = b"PRIVATE-TOKEN" + b"A" * (size - len(b"PRIVATE-TOKEN"))
                payload = (
                    token + b"Z\x7f\r\n" if size < 32768 else token + b"\n"
                )
                output = self.hidden_input(payload)
                self.assertIn(
                    f"RESULT {size} {hashlib.sha256(token).hexdigest()}", output
                )

    def test_hidden_paste_rejection_and_cancel_restore_terminal(self):
        for payload, interrupt, expected in (
            (
                b"PRIVATE-TOKEN" + b"A" * (32769 - len(b"PRIVATE-TOKEN")) + b"\n",
                False,
                "ValueError Access token exceeds 32768 bytes.",
            ),
            (
                b"PRIVATE-TOKEN" + b"A" * (32769 - len(b"PRIVATE-TOKEN")) + b"\x7f\n",
                False,
                "ValueError Access token exceeds 32768 bytes.",
            ),
            (
                b"PRIVATE-TOKEN" + b"A" * 40000 + b"\n",
                False,
                "ValueError Access token exceeds 32768 bytes.",
            ),
            (
                b"PRIVATE-TOKEN\xff\n",
                False,
                "ValueError Paste the ChatGPT web access token as ASCII text.",
            ),
            (b"PRIVATE-TOKEN\x04", False, "EOFError"),
            (b"PRIVATE-TOKEN", True, "KeyboardInterrupt"),
        ):
            with self.subTest(expected=expected):
                self.assertIn("RESULT " + expected, self.hidden_input(payload, interrupt))

    def test_other_actions_ignore_environment_and_reject_explicit_path(self):
        for action in (
            "connect",
            "status",
            "install",
            "start",
            "stop",
            "restart",
            "logs",
            "shell",
        ):
            with (
                self.subTest(action=action),
                mock.patch.dict(os.environ, SESSION_JSON="incidental-path"),
                mock.patch.object(
                    sys, "argv", ["station.py", "--ip", "192.0.2.1", action]
                ),
                mock.patch.object(station, "session_input") as selected,
                mock.patch.object(
                    station, "adb_run", return_value=b"connected to 192.0.2.1:5555"
                ),
                mock.patch.object(station, "status"),
                mock.patch.object(station, "adb_shell"),
                mock.patch.object(station.subprocess, "call", return_value=0),
                mock.patch.object(station, "arm_binary"),
                mock.patch.object(
                    station, "acquire", side_effect=ValueError("stop before mutation")
                ),
            ):
                try:
                    station.main()
                except ValueError as error:
                    self.assertEqual(str(error), "stop before mutation")
                except SystemExit as error:
                    self.assertEqual(error.code, 0)
                selected.assert_not_called()
                for explicit in ("", "explicit-path"):
                    with (
                        mock.patch.object(
                            sys,
                            "argv",
                            [
                                "station.py",
                                "--ip",
                                "192.0.2.1",
                                "--session-json=" + explicit,
                                action,
                            ],
                        ),
                        contextlib.redirect_stderr(io.StringIO()),
                    ):
                        with self.assertRaises(SystemExit) as rejected:
                            station.main()
                        self.assertEqual(rejected.exception.code, 2)

    def test_selected_file_errors_are_fixed_and_hide_paths(self):
        marker = "synthetic-private-path-marker"
        with tempfile.TemporaryDirectory() as directory:
            missing = str(Path(directory) / marker)
            result = subprocess.run(
                [
                    sys.executable,
                    str(SOURCE / "tools/station.py"),
                    "--ip",
                    "192.0.2.1",
                    "--session-json=" + missing,
                    "auth",
                ],
                capture_output=True,
                text=True,
                timeout=5,
            )
            self.assertEqual(result.returncode, 1)
            self.assertEqual(
                result.stderr, "Error: Cannot read selected session JSON file.\n"
            )
            with (
                mock.patch.object(station.os, "open", return_value=123),
                mock.patch.object(station.os, "fdopen") as opened,
            ):
                opened.return_value.__enter__.return_value.read.side_effect = OSError(
                    marker
                )
                with mock.patch.object(
                    station.os,
                    "fstat",
                    return_value=SimpleNamespace(st_mode=0o100600, st_size=10),
                ):
                    with self.assertRaises(ValueError) as rejected:
                        station.session_input(marker)
                self.assertEqual(
                    str(rejected.exception), "Cannot read selected session JSON file."
                )
                self.assertTrue(rejected.exception.__suppress_context__)
                self.assertIsNone(rejected.exception.__cause__)

    def test_make_auth_transports_literal_path_and_exports_only_for_auth(self):
        with tempfile.TemporaryDirectory(prefix="station-auth-test-") as directory:
            root = Path(directory)
            shutil.copyfile(SOURCE / "Makefile", root / "Makefile")
            (root / "tools").mkdir()
            (root / "tools/station.py").write_text(
                "import json, os, sys\nfrom pathlib import Path\nfrom unittest import mock\n"
                f"sys.path.insert(0, {str(SOURCE / 'tools')!r})\nimport station\n"
                f"root = Path({directory!r})\n"
                "(root / 'export.json').write_text(json.dumps(os.environ.get('SESSION_JSON')))\n"
                "read_input = station.session_input\n"
                "def selected(path):\n"
                "    (root / 'selected.json').write_text(json.dumps(path))\n"
                "    return read_input(path)\n"
                "def imported(args, nonce, stage, local):\n"
                "    (root / 'imported.json').write_bytes(local.read_bytes())\n"
                "with mock.patch.object(station, 'session_input', selected), mock.patch.object(station, 'auth', imported), mock.patch.object(station, 'acquire'), mock.patch.object(station, 'release'), mock.patch.object(station, 'adb_shell', return_value=b''):\n"
                "    station.main()\n"
            )
            environment = os.environ.copy()
            for name in ("SESSION_JSON", "MAKEFLAGS", "MFLAGS", "MAKELEVEL"):
                environment.pop(name, None)
            environment["PYTHONDONTWRITEBYTECODE"] = "1"
            for name in (
                'session"; touch SHELL_MARKER; #',
                "  сессия\t'\"`touch BACKTICK_MARKER` $(touch DOLLAR_MARKER) $HOME\nend  ",
            ):
                path = root / name
                path.write_text(
                    '{"accessToken":"synthetic-token","deviceId":"synthetic-device"}'
                )
                (root / "config.mk").write_text(
                    "STATION_IP = 192.0.2.1\ndefine SESSION_JSON\n"
                    + str(path).replace("$", "$$")
                    + "\nendef\n"
                )
                result = subprocess.run(
                    [
                        "make",
                        "--no-print-directory",
                        "auth",
                        f"PYTHON={sys.executable}",
                    ],
                    cwd=root,
                    env=environment,
                    capture_output=True,
                    text=True,
                    timeout=5,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(
                    json.loads((root / "selected.json").read_text()), str(path)
                )
                self.assertEqual(
                    json.loads((root / "export.json").read_text()), str(path)
                )
                self.assertEqual(
                    json.loads((root / "imported.json").read_text()),
                    {"accessToken": "synthetic-token", "deviceId": "synthetic-device"},
                )
                for marker in ("SHELL_MARKER", "BACKTICK_MARKER", "DOLLAR_MARKER"):
                    self.assertFalse(
                        (root / marker).exists(),
                        "session path executed as shell syntax",
                    )
            result = subprocess.run(
                ["make", "--no-print-directory", "stop", f"PYTHON={sys.executable}"],
                cwd=root,
                env=environment,
                capture_output=True,
                text=True,
                timeout=5,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIsNone(json.loads((root / "export.json").read_text()))


class UncertainCleanupTest(unittest.TestCase):
    def test_uncertain_shell_stops_remote_cleanup_and_removes_local_script(self):
        for failure in (
            "push timeout",
            "pull error",
            "missing frame",
            "malformed frame",
            "interrupt",
            "confirmed error",
        ):
            with self.subTest(failure=failure):
                args = SimpleNamespace(
                    adb="fake-adb", serial="synthetic", uncertain=False
                )
                calls, local_scripts = [], []

                def fake_run(command, **kwargs):
                    self.assertFalse(
                        args.uncertain, "another ADB command ran after uncertainty"
                    )
                    calls.append(command[3:])
                    if command[3] == "push":
                        local_scripts.append(Path(command[4]))
                        if failure == "push timeout":
                            raise subprocess.TimeoutExpired(command, 60)
                    if command[3] == "pull":
                        if failure == "pull error":
                            return SimpleNamespace(
                                returncode=1, stdout=b"", stderr=b"synthetic ADB error"
                            )
                        Path(command[5]).write_bytes(b"true\n")
                    output = b""
                    if command[3] == "shell":
                        script = command[4]
                        marker = station.re.search(
                            r"__VOICE_STATUS_[0-9a-f]+__", script
                        )[0]
                        content, code = b"", 0
                        if "mktemp" in script:
                            content = b"/tmp/voice-script-synthetic\n"
                        elif "sh /tmp/voice-script-synthetic" in script:
                            if failure == "missing frame":
                                return SimpleNamespace(
                                    returncode=0, stdout=b"", stderr=b""
                                )
                            if failure == "malformed frame":
                                return SimpleNamespace(
                                    returncode=0,
                                    stdout=b"\n" + marker.encode() + b"bad\n",
                                    stderr=b"",
                                )
                            if failure == "interrupt":
                                raise KeyboardInterrupt
                            if failure == "confirmed error":
                                code = 7
                        output = (
                            content
                            + b"\n"
                            + marker.encode()
                            + str(code).encode()
                            + b"\n"
                        )
                    return SimpleNamespace(returncode=0, stdout=output, stderr=b"")

                diagnostics = io.StringIO()
                with (
                    mock.patch.object(station.subprocess, "run", side_effect=fake_run),
                    contextlib.redirect_stderr(diagnostics),
                ):
                    with self.assertRaises(
                        KeyboardInterrupt if failure == "interrupt" else RuntimeError
                    ) as rejected:
                        station.adb_shell(args, "true\n")
                self.assertTrue(local_scripts)
                self.assertTrue(all(not path.exists() for path in local_scripts))
                cleanup = [
                    call
                    for call in calls
                    if call[0] == "shell"
                    and "rm -f /tmp/voice-script-synthetic" in call[1]
                ]
                self.assertEqual(len(cleanup), int(failure == "confirmed error"))
                self.assertEqual(args.uncertain, failure != "confirmed error")
                if failure == "confirmed error":
                    self.assertEqual(
                        str(rejected.exception), "Station command failed (7)."
                    )
                else:
                    self.assertIn(
                        "temporary remote files are retained", diagnostics.getvalue()
                    )

    def test_uncertain_release_retains_stage_and_lock(self):
        for stage in (None, "/data/synthetic-private-stage"):
            diagnostics = io.StringIO()
            with (
                mock.patch.object(station, "adb_shell") as remote,
                contextlib.redirect_stderr(diagnostics),
            ):
                station.release(
                    SimpleNamespace(uncertain=True), "synthetic-owner", stage
                )
                remote.assert_not_called()
            self.assertIn("retained if present", diagnostics.getvalue())
            self.assertNotIn("private", diagnostics.getvalue())
        with mock.patch.object(station, "adb_shell") as remote:
            station.release(
                SimpleNamespace(uncertain=False),
                "synthetic-owner",
                "/data/synthetic-stage",
            )
            self.assertIn("rm -f /data/synthetic-stage", remote.call_args.args[1])
            self.assertIn('rmdir "$lock"', remote.call_args.args[1])

    def test_cleanup_does_not_replace_original_operation_failure(self):
        original = RuntimeError("synthetic original failure")
        for cleanup_error in (
            RuntimeError("synthetic cleanup failure"),
            KeyboardInterrupt(),
        ):
            with (
                self.subTest(cleanup=type(cleanup_error)),
                mock.patch.object(
                    sys, "argv", ["station.py", "--ip", "192.0.2.1", "stop"]
                ),
                mock.patch.object(station, "acquire"),
                mock.patch.object(station, "adb_shell", side_effect=original),
                mock.patch.object(station, "release", side_effect=cleanup_error),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                with self.assertRaises(RuntimeError) as rejected:
                    station.main()
                self.assertIs(rejected.exception, original)


if __name__ == "__main__":
    unittest.main()
