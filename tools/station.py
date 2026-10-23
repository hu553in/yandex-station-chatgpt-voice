#!/usr/bin/env python3
"""Manage the fixed Station client through explicitly selected root ADB."""

import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import struct
import subprocess
import sys
import tempfile
import warnings

APP = "/data/yandex-station-chatgpt-voice"
LOG = "/tmp/yandex-station-chatgpt-voice.log"
LIBS = "/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib"
BINARY = Path(__file__).resolve().parent.parent / "build/yandex-station-chatgpt-voice"
LIMIT = 1 << 20

COMMON = r"""
set -eu
umask 077
app=/data/yandex-station-chatgpt-voice
runtime=/tmp/yandex-station-chatgpt-voice
lock=/data/.yandex-station-chatgpt-voice.lock
log=/tmp/yandex-station-chatgpt-voice.log
die() { echo "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die 'Root ADB is required; enable it explicitly.'
private_dir() {
    [ ! -L "$app" ] || die 'App directory must not be a symlink.'
    mkdir -p "$app"
    chmod 700 "$app"
}
proc_stat() {
    line=$(cat "/proc/$pid/stat") || die 'Cannot read process identity.'
    fields=${line##*) }
    set -- $fields
    [ "$#" -ge 20 ] || die 'Malformed process identity.'
    state=$1
    shift 19
    actual_start=$1
    case "$actual_start" in ''|*[!0-9]*) die 'Malformed process start time.' ;; esac
}
verify_process() {
    running=0
    [ -d "/proc/$pid" ] || return 0
    proc_stat
    [ "$actual_start" = "$started" ] || die 'PID was reused; client was not signalled or replaced.'
    [ "$state" != Z ] || return 0
    actual=$(readlink -f "/proc/$pid/exe") || die 'Cannot resolve process executable.'
    [ "$actual" = "$expected" ] || die 'PID executable is not the installed client.'
    running=1
}
process() {
    running=0
    [ ! -L "$runtime" ] || die 'Runtime directory must not be a symlink.'
    mkdir -p "$runtime"
    set -- $(LC_ALL=C ls -ldn "$runtime")
    [ "$1:$3" = drwx------:0 ] || die 'Runtime directory must be root-owned and private.'
    [ ! -L "$runtime/client.pid" ] || die 'PID file must not be a symlink.'
    [ -e "$runtime/client.pid" ] || return 0
    [ -f "$runtime/client.pid" ] || die 'PID file must be regular.'
    pid=$(cat "$runtime/client.pid") || die 'Cannot read PID.'
    case "$pid" in ''|*[!0-9]*) die 'PID file must contain one numeric PID.' ;; esac
    [ "$pid" -gt 1 ] || die 'Refusing PID less than two.'
    [ -d "/proc/$pid" ] || return 0
    proc_stat
    started=$actual_start
    [ ! -L "$app/client" ] || die 'Installed client must not be a symlink.'
    expected=$(readlink -f "$app/client") || die 'Cannot resolve installed executable.'
    verify_process
}
stop_client() {
    process
    if [ "$running" = 1 ]; then
        verify_process
        [ "$running" = 1 ] || die 'Process exited before TERM; inspect before retrying.'
        kill -TERM "$pid" || die 'Cannot send TERM; client was not replaced.'
        tries=0
        while [ "$tries" -lt 15 ]; do
            sleep 1
            verify_process
            [ "$running" = 1 ] || break
            tries=$((tries + 1))
        done
        [ "$running" = 0 ] || die 'Client did not stop within 15 seconds; client was not replaced.'
    fi
    rm -f "$runtime/client.pid"
}
start_client() {
    process
    if [ "$running" = 1 ]; then
        echo 'Client already running.'
        return 0
    fi
    [ -f "$app/client" ] && [ ! -L "$app/client" ] && [ -x "$app/client" ] || die 'Install the client first.'
    [ ! -L "$log" ] || die 'Log must not be a symlink.'
    if [ -e "$log" ]; then
        [ -f "$log" ] || die 'Log must be a regular file.'
    fi
    if [ -e "$log" ]; then chmod 600 "$log"; fi
    : >> "$log"
    chmod 600 "$log"
    rm -f "$runtime/client.pid"
    expected=$(readlink -f "$app/client") || die 'Cannot resolve installed executable.'
    LD_LIBRARY_PATH=__LIBS__ setsid nohup sh -c '
        set -eu
        runtime=$1
        app=$2
        shift 2
        [ ! -L "$runtime/.pid-new" ] || exit 1
        printf "%s\n" "$$" > "$runtime/.pid-new"
        mv -f "$runtime/.pid-new" "$runtime/client.pid"
        exec "$app/client" run "$@"
    ' sh "$runtime" "$app" "$@" < /dev/null >> "$log" 2>&1 &
    pid=$!
    case "$pid" in ''|*[!0-9]*) die 'Startup did not return a PID.' ;; esac
    [ "$pid" -gt 1 ] || die 'Invalid startup PID.'
    while [ ! -f "$runtime/client.pid" ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" || die 'Client startup wrapper failed; inspect its log.'
            die 'Client exited before publishing its PID; inspect its log.'
        fi
        sleep 1
    done
    proc_stat
    started=$actual_start
    sleep 1
    verify_process
    [ "$running" = 1 ] || die 'Client exited during startup; inspect its log.'
    echo 'Client started.'
}
""".replace("__LIBS__", shlex.quote(LIBS))


def adb_run(args, *command, quiet=False):
    try:
        result = subprocess.run(
            [args.adb, "-s", args.serial, *command],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            timeout=60,
            check=False,
        )
    except KeyboardInterrupt:
        args.uncertain = True
        raise
    except subprocess.TimeoutExpired:
        args.uncertain = True
        raise RuntimeError(
            "ADB timed out; remote state is unknown. Inspect before retrying."
        ) from None
    if result.returncode:
        args.uncertain = True
        detail = "" if quiet else result.stderr.decode(errors="replace").strip()
        raise RuntimeError(
            "ADB command failed; remote state may be unknown."
            + ("\n" + detail if detail else "")
        )
    return result.stdout


def _adb_shell_inline(args, script, quiet=False):
    marker = "__VOICE_STATUS_" + secrets.token_hex(16) + "__"
    command = f"sh -c {shlex.quote(script)}; voice_status=$?; printf '\\n{marker}%s\\n' \"$voice_status\""
    output = adb_run(args, "shell", command, quiet=quiet)
    frame = re.search(rb"\r?\n" + marker.encode() + rb"([0-9]+)\r?\n\Z", output)
    if frame is None or output.count(marker.encode()) != 1:
        args.uncertain = True
        raise RuntimeError(
            "ADB shell completion frame is missing or malformed; remote state is unknown. Inspect before retrying."
        )
    content = output[: frame.start()].replace(b"\r\n", b"\n")
    code = int(frame[1])
    if code:
        detail = "" if quiet else content.decode(errors="replace").strip()
        raise RuntimeError(
            f"Station command failed ({code})." + ("\n" + detail if detail else "")
        )
    return content


def adb_shell(args, script, quiet=False):
    data = script.encode()
    local = None
    remote = None
    try:
        with tempfile.NamedTemporaryFile(
            prefix="voice-shell-", suffix=".sh", delete=False
        ) as stream:
            local = Path(stream.name)
            stream.write(data)
        output = _adb_shell_inline(
            args, "umask 077; mktemp /tmp/voice-script-XXXXXXXXXXXX", quiet=True
        )
        if re.fullmatch(rb"/tmp/voice-script-[A-Za-z0-9]+\n", output) is None:
            args.uncertain = True
            raise RuntimeError(
                "ADB returned an invalid temporary script path; remote state is unknown. Inspect before retrying."
            )
        remote = output.decode().strip()
        adb_run(args, "push", str(local), remote, quiet=quiet)
        if adb_read(args, remote) != data:
            raise RuntimeError(
                "Uploaded script differs from the local script; command was not executed."
            )
        return _adb_shell_inline(args, f"sh {shlex.quote(remote)}", quiet=quiet)
    finally:
        failed = sys.exc_info()[0] is not None
        cleanup_error = None
        if args.uncertain:
            print(
                "Remote state is unknown; temporary remote files are retained if present. Inspect before retrying.",
                file=sys.stderr,
            )
        elif remote is not None:
            try:
                _adb_shell_inline(args, f"rm -f {shlex.quote(remote)}", quiet=True)
            except (OSError, RuntimeError, KeyboardInterrupt) as error:
                cleanup_error = error
        if local is not None:
            try:
                local.unlink()
            except OSError as error:
                if cleanup_error is None:
                    cleanup_error = error
        if cleanup_error is not None:
            if not failed:
                raise cleanup_error
            print(
                "Temporary script cleanup could not be confirmed; inspect /tmp before retrying.",
                file=sys.stderr,
            )


def adb_read(args, remote):
    with tempfile.TemporaryDirectory(prefix="voice-pull-") as directory:
        local = Path(directory) / "candidate"
        adb_run(args, "pull", remote, str(local))
        return local.read_bytes()


def acquire(args, nonce):
    adb_shell(
        args,
        COMMON
        + f"""
mkdir "$lock" || die 'Another mutation holds the lock; inspect before retrying.'
trap 'rm -f "$lock/owner"; rmdir "$lock"' EXIT
printf '%s\\n' {shlex.quote(nonce)} > "$lock/owner"
private_dir
trap - EXIT
""",
    )


def owned(nonce):
    return (
        COMMON
        + f"""
[ ! -L "$lock" ] && [ "$(cat "$lock/owner")" = {shlex.quote(nonce)} ] || die 'Mutation lock ownership changed.'
"""
    )


def release(args, nonce, stage):
    cleanup = f"rm -f {shlex.quote(stage)}\n" if stage else ""
    if args.uncertain:
        print(
            "Remote state is unknown; staging file and mutation lock are retained if present. Inspect before retrying.",
            file=sys.stderr,
        )
    else:
        adb_shell(
            args,
            owned(nonce) + cleanup + 'rm -f "$lock/owner"\nrmdir "$lock"\n',
            quiet=True,
        )


def arm_binary(path):
    data = path.read_bytes()
    if len(data) < 52 or data[:7] != b"\x7fELF\x01\x01\x01":
        raise ValueError("Build output must be a 32-bit little-endian ARM ELF.")
    kind, machine, version = struct.unpack_from("<HHI", data, 16)
    flags = struct.unpack_from("<I", data, 36)[0]
    if (
        kind not in (2, 3)
        or machine != 40
        or version != 1
        or flags >> 24 != 5
        or not flags & 0x400
    ):
        raise ValueError("Build output must use ARM EABI5 and the hard-float ABI.")
    return data


def install(args, nonce, stage, data):
    adb_run(args, "push", str(BINARY), stage)
    if adb_read(args, stage) != data:
        raise RuntimeError(
            "Uploaded candidate differs from the local ARM binary; installed client was not touched."
        )
    adb_shell(
        args,
        owned(nonce)
        + f"""
chmod 700 {shlex.quote(stage)}
LD_LIBRARY_PATH={shlex.quote(LIBS)} {shlex.quote(stage)} help
""",
    )
    output = adb_shell(
        args,
        owned(nonce)
        + f"""
[ ! -L "$app/client" ] && [ ! -L "$app/client.previous" ] || die 'Client paths must not be symlinks.'
[ ! -e "$app/client" ] || [ -f "$app/client" ] || die 'Client must be regular.'
[ ! -e "$app/client.previous" ] || [ -f "$app/client.previous" ] || die 'Client backup must be regular.'
stop_client
if [ -f "$app/client" ]; then
    cp -p "$app/client" "$app/.previous-{nonce}"
    mv -f "$app/.previous-{nonce}" "$app/client.previous"
fi
mv -f {shlex.quote(stage)} "$app/client"
echo 'Client installed and stopped. Use make start or make install restart to activate.'
""",
    )
    print(output.decode(errors="replace").strip())


def session_input(path):
    if path:
        try:
            fd = os.open(
                path, os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0)
            )
            with os.fdopen(fd, "rb") as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_size > LIMIT:
                    raise ValueError(
                        "Selected session JSON must be a regular file no larger than 1 MiB."
                    )
                data = stream.read(LIMIT + 1)
        except OSError:
            raise ValueError("Cannot read selected session JSON file.") from None
        try:
            value = json.loads(data) if len(data) <= LIMIT else None
        except (ValueError, UnicodeError):
            raise ValueError(
                "Invalid session JSON; expected an accessToken field."
            ) from None
        if not isinstance(value, dict) or not isinstance(value.get("accessToken"), str):
            raise ValueError("Session JSON must contain an accessToken string.")
        result = {"accessToken": value["accessToken"]}
        if "deviceId" in value:
            if not isinstance(value["deviceId"], str):
                raise ValueError("Session deviceId must be a string.")
            result["deviceId"] = value["deviceId"]
        return result
    if not sys.stdin.isatty() or not sys.stderr.isatty():
        raise ValueError(
            "Hidden auth requires a terminal; use SESSION_JSON=/path/to/file for an explicit file."
        )
    with warnings.catch_warnings():
        warnings.simplefilter("error", getpass.GetPassWarning)
        token = getpass.getpass("ChatGPT web access token (hidden): ")
    if len(token.encode()) > LIMIT:
        raise ValueError("Session input exceeds 1 MiB.")
    return {"accessToken": token}


def auth(args, nonce, stage, local):
    adb_run(args, "push", str(local), stage, quiet=True)
    adb_shell(
        args,
        owned(nonce)
        + f"""
chmod 600 {shlex.quote(stage)}
[ -f "$app/client" ] && [ ! -L "$app/client" ] && [ -x "$app/client" ] || die 'Install the client first.'
LD_LIBRARY_PATH={shlex.quote(LIBS)} "$app/client" auth import --session-json {shlex.quote(stage)}
""",
        quiet=True,
    )
    print("Session imported. Restart explicitly to use it in a running client.")


def status(args):
    output = adb_shell(
        args,
        COMMON
        + r"""
echo 'Root ADB verified.'
if [ -e "$app" ]; then
    [ -d "$app" ] && [ ! -L "$app" ] || die 'App directory is not a real directory.'
    ls -ld "$app"
    for name in client client.previous session.json; do
        if [ -e "$app/$name" ] || [ -L "$app/$name" ]; then
            [ ! -L "$app/$name" ] || die 'Installed file is a symlink.'
            ls -ld "$app/$name"
        else
            echo "$name missing"
        fi
    done
    process
    if [ "$running" = 1 ]; then echo "Client running with verified PID $pid."; else echo 'Client stopped.'; fi
else
    echo 'Client not installed.'
fi
if [ -d "$lock" ]; then echo 'Mutation lock present; inspect before retrying a failed operation.'; fi
for path in /lib/ld-linux-armhf.so.3 /usr/bin/aplay /dev/snd \
    /system/vendor/quasar/libs/libiconv.so /system/vendor/quasar/libs/libtflite.so \
    /system/vendor/quasar/libs/libyandex_evil.so /system/vendor/quasar/spotter_models/activation/yandex; do
    if [ -r "$path" ]; then echo "Runtime present $path"; else echo "Runtime missing $path"; fi
done
""",
    )
    print(output.decode(errors="replace").strip())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adb", default="adb")
    parser.add_argument("--ip", required=True, type=ipaddress.IPv4Address)
    parser.add_argument("--session-json")
    parser.add_argument(
        "action",
        choices=(
            "connect",
            "status",
            "install",
            "start",
            "stop",
            "restart",
            "logs",
            "shell",
            "auth",
        ),
    )
    args = parser.parse_args()
    args.serial = f"{args.ip}:5555"
    args.uncertain = False
    if args.session_json is not None and args.action != "auth":
        parser.error("--session-json is only valid with auth")
    if args.action == "auth" and args.session_json is None:
        args.session_json = os.environ.get("SESSION_JSON")
    if args.action == "connect":
        output = adb_run(args, "connect", args.serial).decode(errors="replace").strip()
        if output not in (
            f"connected to {args.serial}",
            f"already connected to {args.serial}",
        ):
            raise RuntimeError(
                "ADB did not confirm connection to the selected Station."
            )
        print(output)
        return
    if args.action == "status":
        status(args)
        return
    if args.action in ("logs", "shell"):
        adb_shell(args, COMMON + "true\n")
        command = [args.adb, "-s", args.serial, "shell"]
        if args.action == "logs":
            command.append(f"tail -n 100 -f {shlex.quote(LOG)}")
        raise SystemExit(subprocess.call(command))
    data = arm_binary(BINARY) if args.action == "install" else None
    session = session_input(args.session_json) if args.action == "auth" else None
    operation = "stop_client"
    if args.action in ("start", "restart"):
        options = [
            "--voice=" + os.environ.get("VOICE", ""),
            "--backend-model=" + os.environ.get("BACKEND_MODEL", ""),
            "--backend-thinking-effort="
            + os.environ.get("BACKEND_THINKING_EFFORT", ""),
        ]
        try:
            for option in options:
                option.encode("utf-8")
                if "\0" in option:
                    raise ValueError("Session settings cannot contain NUL.")
        except UnicodeError:
            raise ValueError(
                "Session settings must be representable as UTF-8."
            ) from None
        launch = "start_client " + shlex.join(options)
        operation = launch if args.action == "start" else "stop_client\n" + launch
    nonce = secrets.token_hex(16)
    stage = (
        f"{APP}/.{'auth' if session is not None else 'candidate'}-{nonce}"
        if args.action in ("install", "auth")
        else None
    )
    with tempfile.TemporaryDirectory(prefix="voice-auth-") as directory:
        local = Path(directory) / "input.json"
        if session is not None:
            with local.open("xb") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(json.dumps(session).encode())
            session = None
        acquire(args, nonce)
        try:
            if args.action == "install":
                install(args, nonce, stage, data)
            elif args.action == "auth":
                auth(args, nonce, stage, local)
            else:
                output = adb_shell(args, owned(nonce) + operation + "\n")
                print(output.decode(errors="replace").strip() or "Client stopped.")
        finally:
            failed = sys.exc_info()[0] is not None
            try:
                release(args, nonce, stage)
            except (OSError, RuntimeError, KeyboardInterrupt):
                if not failed:
                    raise
                print(
                    "Remote cleanup could not be confirmed; inspect the mutation lock and staging file.",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, getpass.GetPassWarning) as error:
        print(f"Error: {error}", file=sys.stderr)
        raise SystemExit(1)
    except (KeyboardInterrupt, EOFError):
        print("Canceled; inspect remote state before retrying.", file=sys.stderr)
        raise SystemExit(130)
