#!/usr/bin/env python3
"""Manage the fixed Station client through explicitly selected root ADB."""

import argparse
import hashlib
import ipaddress
import os
import re
import secrets
import shlex
import struct
import subprocess  # nosec B404 # explicit local argv, no host shell
import sys
import tempfile
from pathlib import Path

APP = "/data/yandex-station-chatgpt-voice"
LOG = "/tmp/yandex-station-chatgpt-voice.log"  # nosec B108 # device log guarded for symlinks and mode 0600
LIBS = "/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib"
BINARY = Path(__file__).resolve().parent.parent / "build/yandex-station-chatgpt-voice"
BROWSER_LOGIN = Path(__file__).resolve().parent / "browser_login.py"
LIMIT = 1 << 20
BOOT_STUB = b"""#!/bin/sh
[ "$1" = start ] || exit 0
s=/data/yandex-station-chatgpt-voice/start.sh
[ -f "$s" ] && [ ! -L "$s" ] || exit 0
sh "$s" </dev/null >/dev/null 2>&1 &
"""
BOOT_HASHES = {
    "/etc/inittab": "7885a8c224a0840777dcf24553191fe6206bb49bec8a6c97dd4b785dee904db3",
    "/etc/init.d/rcS": "dc1615df9f2012b20b81ffad8e07e16293039ba7fd897854ca3646d6cfea0c0f",
}

COMMON = r"""
set -eu
umask 077
app=/data/yandex-station-chatgpt-voice
runtime=/tmp/yandex-station-chatgpt-voice
lock=/data/.yandex-station-chatgpt-voice.lock
log=/tmp/yandex-station-chatgpt-voice.log
hook=/etc/init.d/S99zchatgptvoice
system_lock=/data/local/tmp/station-root.lock
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
    if [ -e "$runtime/supervisor.lock" ] || [ -L "$runtime/supervisor.lock" ]; then
        [ -f "$runtime/supervisor.lock" ] && [ ! -L "$runtime/supervisor.lock" ] || die 'Supervisor lock must be regular.'
        flock -n "$runtime/supervisor.lock" true || die 'A client worker is still shutting down; inspect before retrying.'
    fi
    rm -f "$runtime/client.pid"
}
pause_client() {
    [ ! -L "$app/stopped" ] || die 'Stop marker must not be a symlink.'
    [ ! -e "$app/stopped" ] || [ -f "$app/stopped" ] || die 'Stop marker must be regular.'
    : > "$app/stopped"
    chmod 600 "$app/stopped"
    stop_client
}
start_client() {
    process
    if [ "$running" = 1 ]; then
        echo 'Client already running.'
        return 0
    fi
    [ -f "$app/client" ] && [ ! -L "$app/client" ] && [ -x "$app/client" ] || die 'Install the client first.'
    if [ -e "$runtime/supervisor.lock" ] || [ -L "$runtime/supervisor.lock" ]; then
        [ -f "$runtime/supervisor.lock" ] && [ ! -L "$runtime/supervisor.lock" ] || die 'Supervisor lock must be regular.'
        flock -n "$runtime/supervisor.lock" true || die 'Another client instance is still running.'
    fi
    [ ! -L "$log" ] || die 'Log must not be a symlink.'
    if [ -e "$log" ]; then
        [ -f "$log" ] || die 'Log must be a regular file.'
    fi
    if [ -e "$log" ]; then chmod 600 "$log"; fi
    : >> "$log"
    chmod 600 "$log"
    rm -f "$runtime/client.pid"
    expected=$(readlink -f "$app/client") || die 'Cannot resolve installed executable.'
    mode=run
    if LD_LIBRARY_PATH=__LIBS__ "$app/client" supervise --help >/dev/null 2>&1; then mode=supervise; fi
    LD_LIBRARY_PATH=__LIBS__ setsid nohup sh -c '
        set -eu
        runtime=$1
        app=$2
        mode=$3
        log=$4
        shift 4
        [ ! -L "$runtime/.pid-new" ] || exit 1
        printf "%s\n" "$$" > "$runtime/.pid-new"
        mv -f "$runtime/.pid-new" "$runtime/client.pid"
        if [ "$mode" = supervise ]; then exec "$app/client" supervise "--log=$log" "$@"; fi
        exec "$app/client" run "$@"
    ' sh "$runtime" "$app" "$mode" "$log" "$@" < /dev/null >> "$log" 2>&1 &
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
""".replace("__LIBS__", shlex.quote(LIBS))  # noqa: E501


def boot_check():
    checks = "\n".join(
        f"[ ! -L {shlex.quote(path)} ] && [ -f {shlex.quote(path)} ] || return 1\n"
        f"sum=$(sha256sum {shlex.quote(path)}) || return 1\n"
        "set -- $sum\n"
        f'[ "$1" = {shlex.quote(digest)} ] || return 1'
        for path, digest in BOOT_HASHES.items()
    )
    return (
        "boot_scripts_ok() {\n"
        '[ "$(readlink -f /proc/1/exe)" = /bin/busybox ] || return 1\n'
        '[ "$(readlink -f /data)" = /bkupgrade/data ] || return 1\n' + checks + "\n}\n"
    )


def start_script(options):
    return (
        "#!/bin/sh\n"
        + COMMON
        + """
mkdir "$lock" || exit 0
printf '%s\\n' boot > "$lock/owner"
trap 'rm -f "$lock/owner"; rmdir "$lock"' EXIT
trap 'exit 1' HUP INT TERM
[ ! -e "$app/stopped" ] && [ ! -L "$app/stopped" ] || exit 0
"""
        + "start_client "
        + shlex.join(options)
        + "\n"
    ).encode()


def put_start_script(args, nonce, stage, options):
    data = start_script(options)
    with tempfile.TemporaryDirectory(prefix="voice-start-") as directory:
        local = Path(directory) / "start.sh"
        local.write_bytes(data)
        adb_run(args, "push", str(local), stage)
    if adb_read(args, stage) != data:
        raise RuntimeError("Uploaded startup script differs; startup settings were not changed.")
    adb_shell(
        args,
        owned(nonce)
        + f"""
[ ! -L "$app/start.sh" ] || die 'Startup script must not be a symlink.'
[ ! -e "$app/start.sh" ] || [ -f "$app/start.sh" ] || die 'Startup script must be regular.'
sh -n {shlex.quote(stage)}
chmod 700 {shlex.quote(stage)}
mv -f {shlex.quote(stage)} "$app/start.sh"
""",
    )


def rollback(args, nonce, stage):
    output = adb_shell(
        args,
        owned(nonce)
        + f"""
[ -f "$app/client.previous" ] && [ ! -L "$app/client.previous" ] || die 'No regular previous client is available.'
[ ! -L "$app/client" ] || die 'Installed client must not be a symlink.'
[ ! -e "$app/client" ] || [ -f "$app/client" ] || die 'Installed client must be regular.'
cp -p "$app/client.previous" {shlex.quote(stage)}
cmp -s "$app/client.previous" {shlex.quote(stage)} || die 'Rollback staging differs from the previous client.'
chmod 700 {shlex.quote(stage)}
LD_LIBRARY_PATH={shlex.quote(LIBS)} {shlex.quote(stage)} help
pause_client
if [ -f "$app/client" ] && cmp -s "$app/client" {shlex.quote(stage)}; then
    echo 'Already on the previous client; left stopped.'
else
    mv -f {shlex.quote(stage)} "$app/client"
    echo 'Previous client restored and stopped. Use make start to activate.'
fi
""",  # noqa: E501
    )
    print(output.decode(errors="replace").strip())


def autostart(args, nonce, stage, enable):
    with tempfile.TemporaryDirectory(prefix="voice-boot-") as directory:
        local = Path(directory) / "hook"
        local.write_bytes(BOOT_STUB)
        adb_run(args, "push", str(local), stage)
    if adb_read(args, stage) != BOOT_STUB:
        raise RuntimeError("Uploaded boot hook differs; autostart was not changed.")
    operation = (
        """
boot_scripts_ok || die 'Boot scripts not recognized; autostart was not installed.'
[ -f "$app/start.sh" ] && [ ! -L "$app/start.sh" ] || die 'Run make start once first.'
LD_LIBRARY_PATH=__LIBS__ "$app/client" supervise --help >/dev/null 2>&1 || die 'Install the supervised client first.'
[ "$present" = 0 ] || { echo 'Autostart already enabled.'; exit 0; }
""".replace("__LIBS__", shlex.quote(LIBS))  # noqa: E501
        if enable
        else """
[ "$present" = 1 ] || { echo 'Autostart already disabled.'; exit 0; }
"""
    )
    change = (
        f"""
new=$(mktemp /etc/init.d/.station-voice.XXXXXX)
cat {shlex.quote(stage)} > "$new"
cmp -s "$new" {shlex.quote(stage)}
chmod 755 "$new"
mv -f "$new" "$hook"
new=''
echo 'Autostart enabled; current process was not changed.'
"""
        if enable
        else """
rm "$hook"
echo 'Autostart disabled; current process was not changed.'
"""
    )
    output = adb_shell(
        args,
        owned(nonce)
        + boot_check()
        + f"""
present=0
if [ -e "$hook" ] || [ -L "$hook" ]; then
    [ -f "$hook" ] && [ ! -L "$hook" ] && cmp -s "$hook" {shlex.quote(stage)} || die 'Boot hook is not ours; inspect before changing it.'
    present=1
fi
"""  # noqa: E501
        + operation
        + """
mkdir "$system_lock" || die 'Persistence lock exists; inspect before retrying.'
remounted=false
new=''
cleanup_boot() {
    result=$?
    trap '' HUP INT TERM
    trap - EXIT
    if [ -n "$new" ]; then rm -f "$new" || result=1; fi
    if [ "$remounted" = true ]; then sync || result=1; mount -o remount,ro / || result=1; fi
    rmdir "$system_lock" || result=1
    exit "$result"
}
trap cleanup_boot EXIT
trap 'exit 1' HUP INT TERM
root_options=$(awk '$2 == "/" {print $4}' /proc/mounts)
case ",$root_options," in
    *,ro,*) remounted=true; mount -o remount,rw / ;;
    *,rw,*) : ;;
    *) die 'Cannot determine root mount mode.' ;;
esac
"""
        + change
        + "sync\n",
    )
    print(output.decode(errors="replace").strip())


def adb_run(args, *command, quiet=False):
    try:
        result = subprocess.run(  # nosec B603 # explicitly selected ADB argv, no host shell
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
            "ADB command failed; remote state may be unknown." + ("\n" + detail if detail else "")
        )
    return result.stdout


def _adb_shell_inline(args, script, quiet=False):
    marker = "__VOICE_STATUS_" + secrets.token_hex(16) + "__"
    command = (
        f"sh -c {shlex.quote(script)}; voice_status=$?; printf '\\n{marker}%s\\n' \"$voice_status\""
    )
    output = adb_run(args, "shell", command, quiet=quiet)
    frame = re.search(rb"\r?\n" + marker.encode() + rb"([0-9]+)\r?\n\Z", output)
    if frame is None or output.count(marker.encode()) != 1:
        args.uncertain = True
        raise RuntimeError(
            "ADB shell completion frame is missing or malformed; remote state "
            "is unknown. Inspect before retrying."
        )
    content = output[: frame.start()].replace(b"\r\n", b"\n")
    code = int(frame[1])
    if code:
        detail = "" if quiet else content.decode(errors="replace").strip()
        raise RuntimeError(f"Station command failed ({code})." + ("\n" + detail if detail else ""))
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
                "ADB returned an invalid temporary script path; remote state is "
                "unknown. Inspect before retrying."
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
                "Remote state is unknown; temporary remote files are retained if "
                "present. Inspect before retrying.",
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


def adb_read(args, remote, quiet=False):
    with tempfile.TemporaryDirectory(prefix="voice-pull-") as directory:
        local = Path(directory) / "candidate"
        adb_run(args, "pull", remote, str(local), quiet=quiet)
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
"""  # noqa: E501
    )


def release(args, nonce, stage):
    cleanup = f"rm -f {shlex.quote(stage)}\n" if stage else ""
    if args.uncertain:
        print(
            "Remote state is unknown; staging file and mutation lock are "
            "retained if present. Inspect before retrying.",
            file=sys.stderr,
        )
    else:
        adb_shell(args, owned(nonce) + cleanup + 'rm -f "$lock/owner"\nrmdir "$lock"\n', quiet=True)


def arm_binary(path):
    data = path.read_bytes()
    if len(data) < 52 or data[:7] != b"\x7fELF\x01\x01\x01":
        raise ValueError("Build output must be a 32-bit little-endian ARM ELF.")
    kind, machine, version = struct.unpack_from("<HHI", data, 16)
    flags = struct.unpack_from("<I", data, 36)[0]
    if kind not in (2, 3) or machine != 40 or version != 1 or flags >> 24 != 5 or not flags & 0x400:
        raise ValueError("Build output must use ARM EABI5 and the hard-float ABI.")
    return data


def install(args, nonce, stage, data):
    adb_run(args, "push", str(BINARY), stage)
    if adb_read(args, stage) != data:
        raise RuntimeError(
            "Uploaded candidate differs from the local ARM binary; installed "
            "client was not touched."
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
pause_client
if [ -f "$app/client" ] && ! cmp -s "$app/client" {shlex.quote(stage)}; then
    cp -p "$app/client" "$app/.previous-{nonce}"
    mv -f "$app/.previous-{nonce}" "$app/client.previous"
fi
mv -f {shlex.quote(stage)} "$app/client"
echo 'Client installed and stopped. Use make start or make install restart to activate.'
""",  # noqa: E501
    )
    print(output.decode(errors="replace").strip())


def login_preflight(args):
    adb_shell(
        args,
        COMMON
        + f"""
[ -d "$app" ] && [ ! -L "$app" ] || die 'Install the client first: make install.'
[ -f "$app/client" ] && [ ! -L "$app/client" ] && [ -x "$app/client" ] || die 'Install the client first: make install.'
case "$(LD_LIBRARY_PATH={shlex.quote(LIBS)} "$app/client" help 2>/dev/null)" in
*'make login'*) ;;
*) die 'The installed client cannot use browser sign-in. Run make install first.' ;;
esac
""",  # noqa: E501
    )


def browser_sign_in(local):
    command = [
        os.environ.get("UV") or "uv",
        "run",
        "--locked",
        "--no-dev",
        "--project",
        str(BROWSER_LOGIN.parent.parent),
        "python",
        str(BROWSER_LOGIN),
        "--chrome=" + os.environ.get("CHROME", ""),
        str(local),
    ]
    try:
        process = subprocess.Popen(command)  # nosec B603 # fixed uv subcommands and project paths, no shell
    except OSError:
        raise RuntimeError("Cannot run uv; install it as described in the README.") from None
    try:
        code = process.wait()
    except KeyboardInterrupt:
        try:
            process.wait(timeout=30)
        except (subprocess.TimeoutExpired, KeyboardInterrupt):
            process.kill()
        code = 130
    if code == 130:
        print("Sign-in canceled; the Station was not changed.", file=sys.stderr)
        raise SystemExit(130) from None
    if code:
        raise RuntimeError("Browser sign-in did not finish; the Station was not changed.")
    try:
        data = local.read_bytes()
    except OSError:
        data = b""
    if not data or len(data) > LIMIT:
        raise RuntimeError(
            "Browser sign-in produced no usable result; the Station was not changed."
        )
    return data


def import_sign_in(args, nonce, stage, local, data):
    adb_run(args, "push", str(local), stage, quiet=True)
    if adb_read(args, stage, quiet=True) != data:
        raise RuntimeError(
            "Uploaded sign-in differs from the local copy; the saved session was not changed."
        )
    output = adb_shell(
        args,
        owned(nonce)
        + f"""
chmod 600 {shlex.quote(stage)}
[ -f "$app/client" ] && [ ! -L "$app/client" ] && [ -x "$app/client" ] || die 'Install the client first: make install.'
LD_LIBRARY_PATH={shlex.quote(LIBS)} "$app/client" auth import --session-json {shlex.quote(stage)} 2>&1
""",  # noqa: E501
    )
    print(output.decode(errors="replace").strip())


def status(args):
    output = adb_shell(
        args,
        COMMON
        + f"""
echo 'Root ADB verified.'
if [ -e "$app" ]; then
    [ -d "$app" ] && [ ! -L "$app" ] || die 'App directory is not a real directory.'
    ls -ld "$app"
    for name in client client.previous session.json start.sh stopped; do
        if [ -e "$app/$name" ] || [ -L "$app/$name" ]; then
            [ ! -L "$app/$name" ] || die 'Installed file is a symlink.'
            ls -ld "$app/$name"
        else
            echo "$name missing"
        fi
    done
    if ! LD_LIBRARY_PATH={shlex.quote(LIBS)} "$app/client" auth status 2>/dev/null; then
        echo 'Saved token expiry (unverified): unknown'
    fi
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
""",  # noqa: E501
    )
    print(output.decode(errors="replace").strip())
    output = adb_shell(
        args,
        COMMON
        + boot_check()
        + f"""
if boot_scripts_ok; then echo 'Boot scripts recognized.'; else echo 'Boot scripts not recognized.'; fi
if [ -f "$hook" ] && [ ! -L "$hook" ]; then
    set -- $(sha256sum "$hook")
    if [ "$1" = {hashlib.sha256(BOOT_STUB).hexdigest()} ]; then echo 'Autostart enabled.'; else echo 'Boot hook is not recognized.'; fi
elif [ -e "$hook" ] || [ -L "$hook" ]; then
    echo 'Boot hook is not recognized.'
else
    echo 'Autostart disabled.'
fi
if [ -e "$app/stopped" ] || [ -L "$app/stopped" ]; then echo 'Autostart paused by stop, install or rollback.'; fi
""",  # noqa: E501
    )
    print(output.decode(errors="replace").strip())


# Keep the command dispatch and its single lock/cleanup lifecycle together.
def main():  # noqa: PLR0912, PLR0915
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adb", default="adb")
    parser.add_argument("--ip", required=True, type=ipaddress.IPv4Address)
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
            "login",
            "rollback",
            "enable-autostart",
            "disable-autostart",
        ),
    )
    args = parser.parse_args()
    args.serial = f"{args.ip}:5555"
    args.uncertain = False
    if args.action == "connect":
        output = adb_run(args, "connect", args.serial).decode(errors="replace").strip()
        if output not in (f"connected to {args.serial}", f"already connected to {args.serial}"):
            raise RuntimeError("ADB did not confirm connection to the selected Station.")
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
        # Run the explicitly selected ADB argv without a host shell.
        exit_code = subprocess.call(command)  # nosec B603
        raise SystemExit(exit_code)
    data = arm_binary(BINARY) if args.action == "install" else None
    if args.action == "login":
        login_preflight(args)
    operation = "pause_client"
    if args.action in ("start", "restart"):
        options = [
            "--voice=" + os.environ.get("VOICE", ""),
            "--backend-model=" + os.environ.get("BACKEND_MODEL", ""),
            "--backend-thinking-effort=" + os.environ.get("BACKEND_THINKING_EFFORT", ""),
        ]
        try:
            for option in options:
                option.encode("utf-8")
                if "\0" in option:
                    raise ValueError("Session settings cannot contain NUL.")
        except UnicodeError:
            raise ValueError("Session settings must be representable as UTF-8.") from None
        launch = "start_client " + shlex.join(options)
        operation = 'rm -f "$app/stopped"\n' + launch
        if args.action == "restart":
            operation = "pause_client\n" + operation
    nonce = secrets.token_hex(16)
    stage = (
        f"{APP}/.{'auth' if args.action == 'login' else 'candidate'}-{nonce}"
        if args.action
        in (
            "install",
            "login",
            "start",
            "restart",
            "rollback",
            "enable-autostart",
            "disable-autostart",
        )
        else None
    )
    with tempfile.TemporaryDirectory(prefix="voice-login-") as directory:
        local = Path(directory) / "sign-in.json"
        if args.action == "login":
            data = browser_sign_in(local)
        acquire(args, nonce)
        try:
            if args.action == "install":
                install(args, nonce, stage, data)
            elif args.action == "login":
                import_sign_in(args, nonce, stage, local, data)
            elif args.action == "rollback":
                rollback(args, nonce, stage)
            elif args.action in ("enable-autostart", "disable-autostart"):
                autostart(args, nonce, stage, args.action == "enable-autostart")
            else:
                if args.action in ("start", "restart"):
                    put_start_script(args, nonce, stage, options)
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
                    "Remote cleanup could not be confirmed; inspect the mutation lock "
                    "and staging file.",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        print(f"Error: {error}", file=sys.stderr)
        raise SystemExit(1) from None
    except (KeyboardInterrupt, EOFError):
        print("Canceled; inspect remote state before retrying.", file=sys.stderr)
        raise SystemExit(130) from None
