import contextlib
import io
import json
import os
from pathlib import Path
import shlex
import shutil
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
SIGN_IN = (SOURCE / "internal/voice/testdata/browser-sign-in.json").read_bytes()


def fake_station_main():
    root = Path(os.environ["STATION_TEST_ROOT"])
    (root / "exports.json").write_text(
        json.dumps({name: os.environ.get(name) for name in SETTING_NAMES})
    )
    (root / "chrome-export.json").write_text(json.dumps(os.environ.get("CHROME")))
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
        if "cleanup_boot" in script:
            script = script.replace(
                station.COMMON + overrides,
                station.COMMON + overrides
                + f"\nawk() {{ echo ro; }}\nmount() {{ echo \"$*\" >> {shlex.quote(str(root / 'mounts'))}; }}\nsync() {{ :; }}\n",
            )
        if (root / "init-supported").exists():
            script = script.replace(station.boot_check(), "boot_scripts_ok() { return 0; }\n")
        script = script.replace(
            "/etc/init.d/.station-voice.", str(root / "hook-tmp.")
        )
        for original, replacement in (
            ("app=/data/yandex-station-chatgpt-voice", root / "app"),
            ("runtime=/tmp/yandex-station-chatgpt-voice", root / "runtime"),
            ("lock=/data/.yandex-station-chatgpt-voice.lock", root / "lock"),
            ("log=/tmp/yandex-station-chatgpt-voice.log", root / "client.log"),
            ("hook=/etc/init.d/S99zchatgptvoice", root / "hook"),
            ("system_lock=/data/local/tmp/station-root.lock", root / "system-lock"),
        ):
            name = original.split("=", 1)[0]
            script = script.replace(
                original, name + "=" + shlex.quote(str(replacement))
            )
        return script.replace(station.APP + "/", str(root / "app") + "/").replace(
            '[ "$(id -u)" = 0 ]', "[ 0 = 0 ]"
        )

    def fake_adb(args, *command, quiet=False):
        if command[0] in ("push", "pull"):
            paths = [
                str(root / "app") + path[len(station.APP):]
                if path.startswith(station.APP + "/")
                else path
                for path in command[1:]
            ]
            shutil.copyfile(*paths)
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

    real_install = station.install
    install = real_install if (root / "real-install").exists() else lambda *args: None
    candidate = root / "candidate" if (root / "real-install").exists() else station.BINARY
    with (
        mock.patch.object(station, "adb_run", fake_adb),
        mock.patch.object(station, "arm_binary", side_effect=lambda p: p.read_bytes() if (root / "real-install").exists() else b"synthetic-arm"),
        mock.patch.object(station, "BINARY", candidate),
        mock.patch.object(station, "install", side_effect=install),
    ):
        station.main()


class BootScriptTest(unittest.TestCase):
    def test_saved_startup_respects_pause_and_mutation_lock(self):
        subprocess.run(["sh", "-n"], input=station.BOOT_STUB, check=True)
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            app = root / "app"
            app.mkdir()
            lock = root / "lock"
            launched = root / "launched"
            script = station.start_script(["--voice=custom voice"]).decode().replace(
                station.COMMON,
                "set -eu\n"
                + f"app={shlex.quote(str(app))}\nlock={shlex.quote(str(lock))}\n"
                + f"start_client() {{ printf '%s\\n' \"$@\" > {shlex.quote(str(launched))}; }}\n",
            )
            (app / "stopped").touch()
            subprocess.run(["sh"], input=script, text=True, capture_output=True, check=True)
            self.assertFalse(launched.exists())
            (app / "stopped").unlink()
            lock.mkdir()
            subprocess.run(["sh"], input=script, text=True, capture_output=True, check=True)
            self.assertFalse(launched.exists())
            lock.rmdir()
            subprocess.run(["sh"], input=script, text=True, capture_output=True, check=True)
            self.assertEqual(launched.read_text(), "--voice=custom voice\n")
            self.assertFalse(lock.exists())
            launched.unlink()
            racing_script = script.replace(
                'mkdir "$lock"',
                'mkdir() { : > "$app/stopped"; command mkdir "$@"; }\nmkdir "$lock"',
            )
            subprocess.run(["sh"], input=racing_script, text=True, capture_output=True, check=True)
            self.assertFalse(launched.exists(), "boot ignored a stop completed before lock admission")
            self.assertFalse(lock.exists())


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


class FakeStationTest(unittest.TestCase):
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
            "if sys.argv[1:] == ['help']:\n"
            "    print('Import a token.' if (root / 'old-client').exists() else 'Sign in with make login.')\n"
            "    sys.exit(0)\n"
            "if sys.argv[1:3] == ['auth', 'import'] and sys.argv[3] == '--session-json':\n"
            "    if (root / 'import-fails').exists():\n"
            "        print('2026/11/08 00:00:00 Error: ChatGPT rejected the saved web session (HTTP 403); run make login on the computer', file=sys.stderr)\n"
            "        sys.exit(1)\n"
            "    (root / 'imported.json').write_bytes(Path(sys.argv[4]).read_bytes())\n"
            "    print('Signed in. The Station saved a renewable ChatGPT session.')\n"
            "    sys.exit(0)\n"
            "if sys.argv[1:] == ['supervise', '--help']:\n"
            "    sys.exit(0 if (root / 'supports-supervise').exists() else 1)\n"
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
        uv = self.root / "uv"
        uv.write_text(
            f"#!{sys.executable}\n"
            "import json, os, stat, sys\n"
            "from pathlib import Path\n"
            "root = Path(os.environ['STATION_TEST_ROOT'])\n"
            "output = Path(sys.argv[-1])\n"
            "(root / 'uv-call.json').write_text(json.dumps({'argv': sys.argv[1:], 'directory_mode': stat.S_IMODE(output.parent.stat().st_mode)}))\n"
            "if (root / 'uv-cancel').exists(): sys.exit(130)\n"
            "if (root / 'uv-fail').exists(): sys.exit(1)\n"
            "fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)\n"
            "os.write(fd, (root / 'sign-in.json').read_bytes())\n"
            "os.close(fd)\n"
        )
        uv.chmod(0o700)
        (self.root / "sign-in.json").write_bytes(SIGN_IN)
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

    def leftovers(self):
        return sorted(path.name for path in (self.root / "app").glob(".auth-*")) + (
            ["lock"] if (self.root / "lock").exists() else []
        )

    def run_make(self, action, succeeds=True):
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
        if not succeeds:
            self.assertNotEqual(result.returncode, 0)
            return result
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return json.loads((self.root / "exports.json").read_text())

    def launches(self):
        path = self.root / "argv.jsonl"
        return (
            [json.loads(line) for line in path.read_text().splitlines()]
            if path.exists()
            else []
        )


class LaunchSettingsTest(FakeStationTest):
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

    def test_supervisor_launch_and_stop_marker(self):
        self.configure(("custom voice", "model", "disabled"))
        (self.root / "supports-supervise").touch()
        self.run_make("start")
        self.assertEqual(
            self.launches()[-1],
            [
                "supervise",
                "--log=" + str(self.root / "client.log"),
                "--voice=custom voice",
                "--backend-model=model",
                "--backend-thinking-effort=disabled",
            ],
        )
        startup = self.root / "app/start.sh"
        self.assertEqual(startup.stat().st_mode & 0o777, 0o700)
        subprocess.run(["sh", "-n", str(startup)], check=True)
        self.run_make("stop")
        self.assertTrue((self.root / "app/stopped").is_file())
        self.run_make("start")
        self.assertFalse((self.root / "app/stopped").exists())

    def test_rollback_retains_previous_and_repeated_rollback_is_stopped(self):
        self.configure()
        client = self.root / "app/client"
        previous = self.root / "app/client.previous"
        previous.write_bytes(client.read_bytes() + b"\n# previous build\n")
        previous.chmod(0o700)
        expected = previous.read_bytes()
        self.run_make("start")
        for _ in range(2):
            self.run_make("rollback")
            self.assertEqual(client.read_bytes(), expected)
            self.assertEqual(previous.read_bytes(), expected)
            self.assertTrue((self.root / "app/stopped").exists())
            self.assertFalse((self.root / "app/alive").exists())

    def test_identical_install_keeps_the_previous_build(self):
        self.configure()
        (self.root / "real-install").touch()
        client = self.root / "app/client"
        original = client.read_bytes()
        candidate = self.root / "candidate"
        candidate.write_bytes(original + b"\n# new build\n")
        for _ in range(2):
            self.run_make("install")
            self.assertEqual(client.read_bytes(), candidate.read_bytes())
            self.assertEqual((self.root / "app/client.previous").read_bytes(), original)
            self.assertTrue((self.root / "app/stopped").exists())

    def test_autostart_owns_only_its_hook_and_restores_mount_mode(self):
        self.configure()
        (self.root / "supports-supervise").touch()
        (self.root / "init-supported").touch()
        self.run_make("start")
        for _ in range(2):
            self.run_make("enable-autostart")
            self.assertEqual((self.root / "hook").read_bytes(), station.BOOT_STUB)
        self.assertEqual((self.root / "mounts").read_text().splitlines(), ["-o remount,rw /", "-o remount,ro /"])
        self.run_make("disable-autostart")
        self.assertFalse((self.root / "hook").exists())
        self.assertTrue((self.root / "app/alive").exists())
        self.assertFalse((self.root / "system-lock").exists())

    def test_autostart_refuses_unknown_boot_missing_settings_and_foreign_hook(self):
        self.configure()
        (self.root / "supports-supervise").touch()
        (self.root / "init-supported").touch()
        result = self.run_make("enable-autostart", succeeds=False)
        self.assertIn("Run make start once first", result.stdout + result.stderr)
        self.assertFalse((self.root / "hook").exists())
        self.assertFalse((self.root / "mounts").exists())
        self.run_make("start")
        (self.root / "init-supported").unlink()
        result = self.run_make("enable-autostart", succeeds=False)
        self.assertIn("Boot scripts not recognized", result.stdout + result.stderr)
        (self.root / "init-supported").touch()
        (self.root / "hook").write_text("foreign hook")
        result = self.run_make("enable-autostart", succeeds=False)
        self.assertIn("Boot hook is not ours", result.stdout + result.stderr)
        self.assertEqual((self.root / "hook").read_text(), "foreign hook")
        self.assertFalse((self.root / "mounts").exists())

    def test_other_actions_do_not_export_or_launch_settings(self):
        self.configure(("private-setting", "model", "effort"))
        for action in ("stop", "login", "install"):
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


class LoginTest(FakeStationTest):
    def test_login_passes_chrome_literally_and_imports_on_the_station(self):
        marker = self.root / "MUST_NOT_RUN"
        chrome = f"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome; touch {marker} `touch {marker}` $(touch {marker}) $HOME"
        self.configure(source=f"define CHROME\n{chrome.replace('$', '$$')}\nendef\n")
        result = subprocess.run(
            ["make", "--no-print-directory", f"PYTHON={sys.executable}", "ADB=fake-local-only", "login"],
            cwd=self.root,
            env=self.environment,
            capture_output=True,
            text=True,
            timeout=15,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        call = json.loads((self.root / "uv-call.json").read_text())
        output = Path(call["argv"][-1])
        self.assertEqual(
            call["argv"],
            ["run", "--script", str(SOURCE / "tools/browser_login.py"), "--chrome=" + chrome, str(output)],
        )
        self.assertEqual(call["directory_mode"], 0o700)
        self.assertFalse(output.parent.exists(), "local sign-in file survived")
        self.assertEqual((self.root / "imported.json").read_bytes(), SIGN_IN)
        self.assertIn("Signed in. The Station saved a renewable ChatGPT session.", result.stdout)
        self.assertEqual(json.loads((self.root / "chrome-export.json").read_text()), chrome)
        self.assertEqual(self.leftovers(), [])
        self.assertFalse(marker.exists(), "CHROME executed as shell code")
        self.run_make("stop")
        self.assertIsNone(json.loads((self.root / "chrome-export.json").read_text()))

    def test_login_refuses_old_or_missing_client_before_opening_a_browser(self):
        self.configure()
        (self.root / "old-client").touch()
        result = self.run_make("login", succeeds=False)
        self.assertIn("cannot use browser sign-in. Run make install first.", result.stderr)
        (self.root / "app/client").unlink()
        result = self.run_make("login", succeeds=False)
        self.assertIn("Install the client first: make install.", result.stderr)
        self.assertFalse((self.root / "uv-call.json").exists(), "browser opened before preflight")
        self.assertEqual(self.leftovers(), [])

    def test_canceled_or_failed_sign_in_leaves_the_station_untouched(self):
        self.configure()
        for flag, code, message in (
            ("uv-cancel", 130, "Sign-in canceled; the Station was not changed."),
            ("uv-fail", 1, "Browser sign-in did not finish; the Station was not changed."),
        ):
            with self.subTest(flag=flag):
                (self.root / flag).touch()
                result = subprocess.run(
                    [sys.executable, str(SOURCE / "tools/test_station.py"), "--fake-station", "--ip", "192.0.2.1", "login"],
                    cwd=self.root,
                    env=self.environment,
                    capture_output=True,
                    text=True,
                    timeout=15,
                )
                (self.root / flag).unlink()
                self.assertEqual(result.returncode, code, result.stderr)
                self.assertIn(message, result.stderr)
                self.assertNotIn("inspect remote state", result.stderr)
                self.assertFalse((self.root / "imported.json").exists())
                self.assertEqual(self.leftovers(), [])

    def test_station_import_failure_is_shown_and_cleaned_up(self):
        self.configure()
        (self.root / "import-fails").touch()
        result = self.run_make("login", succeeds=False)
        self.assertIn("Station command failed (1).", result.stderr)
        self.assertIn(
            "Error: ChatGPT rejected the saved web session (HTTP 403); run make login on the computer",
            result.stderr,
        )
        self.assertNotIn("synthetic-golden", result.stdout + result.stderr)
        self.assertEqual(self.leftovers(), [])

    def test_sign_in_is_captured_before_the_mutation_lock(self):
        events = []
        with (
            mock.patch.object(sys, "argv", ["station.py", "--ip", "192.0.2.1", "login"]),
            mock.patch.object(station, "login_preflight", side_effect=lambda args: events.append("preflight")),
            mock.patch.object(
                station, "browser_sign_in", side_effect=lambda local: events.append("capture") or SIGN_IN
            ),
            mock.patch.object(station, "acquire", side_effect=lambda args, nonce: events.append("lock")),
            mock.patch.object(station, "import_sign_in", side_effect=lambda *args: events.append("import")),
            mock.patch.object(station, "release", side_effect=lambda *args: events.append("release")),
        ):
            station.main()
        self.assertEqual(events, ["preflight", "capture", "lock", "import", "release"])

    def test_upload_is_quiet_and_verified_before_import(self):
        args = SimpleNamespace(adb="fake-adb", serial="synthetic", uncertain=False)
        with tempfile.TemporaryDirectory() as directory:
            local = Path(directory) / "sign-in.json"
            local.write_bytes(SIGN_IN)
            with (
                mock.patch.object(station, "adb_run") as run,
                mock.patch.object(station, "adb_read", return_value=b"altered") as read,
                mock.patch.object(station, "adb_shell") as shell,
            ):
                with self.assertRaisesRegex(RuntimeError, "Uploaded sign-in differs"):
                    station.import_sign_in(args, "owner", "/data/synthetic-stage", local, SIGN_IN)
            run.assert_called_once_with(args, "push", str(local), "/data/synthetic-stage", quiet=True)
            read.assert_called_once_with(args, "/data/synthetic-stage", quiet=True)
            shell.assert_not_called()

    def test_manual_token_surface_is_gone(self):
        for argv in (
            ["station.py", "--ip", "192.0.2.1", "auth"],
            ["station.py", "--ip", "192.0.2.1", "--session-json=synthetic-path", "login"],
        ):
            with (
                self.subTest(argv=argv),
                mock.patch.object(sys, "argv", argv),
                mock.patch.object(station, "acquire") as acquire,
                contextlib.redirect_stderr(io.StringIO()),
            ):
                with self.assertRaises(SystemExit) as rejected:
                    station.main()
                self.assertEqual(rejected.exception.code, 2)
                acquire.assert_not_called()
        self.configure()
        result = self.run_make("auth", succeeds=False)
        self.assertIn("No rule to make target", result.stderr)


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
    if sys.argv[1:2] == ["--fake-station"]:
        sys.argv = ["station.py", *sys.argv[2:]]
        fake_station_main()
    else:
        unittest.main()
