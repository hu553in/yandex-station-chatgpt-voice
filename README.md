# ChatGPT Voice for Yandex Station

[![CI](https://github.com/hu553in/yandex-station-chatgpt-voice/actions/workflows/ci.yml/badge.svg)](https://github.com/hu553in/yandex-station-chatgpt-voice/actions/workflows/ci.yml)

Talk to ChatGPT through a rooted Yandex Station's microphones and speaker. The client runs on the
Station and connects directly to ChatGPT. Once it is running, the computer can disconnect.

- **Wake words.** Say "Яндекс" for ChatGPT. "Алиса" still starts the original assistant.
- **Account.** Uses your ChatGPT account's Voice access and limits. No OpenAI API key is needed.
- **Hardware.** Tested only on Station Mini 2. It needs root access and the original Alice services.

[Before you start](#1-check-the-station-and-chatgpt) · [Computer setup](#2-prepare-the-computer) ·
[Install](#3-connect-and-install) · [Sign in](#4-sign-in-to-chatgpt) ·
[First conversation](#5-check-a-conversation) · [Manage](#manage-the-client) ·
[Settings](#change-the-voice-or-model) · [Troubleshooting](#troubleshooting)

## 1. Check the Station and ChatGPT

Complete these checks before building anything:

1. In Chrome on the computer, sign in to your own account at [chatgpt.com](https://chatgpt.com/).
   Start a voice conversation and check that it hears you and answers aloud. Stop that conversation.
   If Voice does not work there, resolve that first. See
   [OpenAI's Voice guide](https://help.openai.com/en/articles/20001274-chatgpt-voice).
2. If you have not finished [yandex-station-root](https://github.com/hu553in/yandex-station-root),
   do that first. The client needs **root ADB on port 5555** and the original Alice services. The
   status check in step 3 verifies access before installation.
3. In the Station's settings in the **Дом с Алисой** phone app, select **Алиса** as its wake word.
   This leaves **Яндекс** available to start ChatGPT.
4. Check the network. The computer must reach the Station on the home network, and the **Station
   itself** must reach ChatGPT. A VPN running only on the computer does not route the Station's
   traffic. If your connection needs a VPN, route the Station's traffic through it, for example
   through the router. Allow LAN access in the computer's VPN and avoid guest Wi-Fi or client
   isolation.

Use the same ChatGPT account for the browser check and the sign-in below. Available Voice access
depends on the account and service. This client uses the website's unofficial voice protocol, which
can change independently of this project.

## 2. Prepare the computer

Use a Mac or Linux computer with internet access. Open **Terminal** on macOS, or a terminal on
Linux. Run one block at a time. If a command fails, fix that error before continuing. Skip tool
installation if you already have the required tools.

<details>
<summary>macOS: install the tools</summary>

Install [Homebrew](https://brew.sh/) if `brew --version` does not work:

```sh
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

Follow the installer's **Next steps** to add `brew` to your shell. Accept the Command Line Tools
installation if prompted. Open a new terminal, then run:

```sh
brew install go uv python pkgconf
brew install --cask android-platform-tools
```

</details>

<details>
<summary>Ubuntu 24.04 or newer: install the tools</summary>

```sh
sudo apt-get update
sudo apt-get install -y git make curl ca-certificates adb golang-go python3 pkg-config nano
curl -LsSf https://astral.sh/uv/install.sh | sh
. "$HOME/.local/bin/env"
```

The last two commands use the
[official uv installer](https://docs.astral.sh/uv/getting-started/installation/). Ubuntu's Go
package can be older than this project requires. With the default
[automatic toolchain selection](https://go.dev/doc/toolchain), `make build` downloads the required
Go version from `go.mod`.

For another Linux distribution, install Git, make, curl, tar, Python 3, pkg-config, `adb`, uv, and
Go at the version declared in [`go.mod`](go.mod) or newer. Native Windows setup is not covered here.

</details>

`make login` opens [Google Chrome](https://www.google.com/chrome/) in a separate, empty window.
Install Chrome if you do not have it. To use Chromium instead, see [sign in](#4-sign-in-to-chatgpt).
uv installs the locked Playwright library declared in `pyproject.toml`. It does not download a
browser.

Download the project into a folder **without spaces in its path**. The Opus build cannot use a path
with whitespace.

```sh
mkdir -p ~/Projects
cd ~/Projects
git clone https://github.com/hu553in/yandex-station-chatgpt-voice.git
cd yandex-station-chatgpt-voice
make build
```

`build` downloads Go modules, the pinned Zig compiler and Opus, then builds the ARM audio library.
It prints compiler feature probes. Judge completion by the build checkpoint below. A host Opus
development package is needed only for [developer checks](#developer-checks).

**Checkpoint:** `make build` finishes without an error and creates
`build/yandex-station-chatgpt-voice`. This is the Linux ARM client for the Station, not a Mac app.

Run the remaining `make` commands from this folder. In a new terminal, return with:

```sh
cd ~/Projects/yandex-station-chatgpt-voice
```

## 3. Connect and install

Find the Station's **current IPv4 address** in the router's connected-device list. Use its
home-network address, for example `192.168.1.50`. A DHCP reservation can keep it from changing.

Each project has its own `config.mk`. Create the file in this Voice project once:

```sh
cp config.example.mk config.mk
nano config.mk
```

Replace the empty `STATION_IP` line with the Station's actual address:

```make
STATION_IP = 192.168.1.50
```

In nano, press **Ctrl+O**, **Enter**, then **Ctrl+X** to save and exit. `config.mk` is ignored by
Git. Keep the file when returning to this guide.

Connect and inspect the Station before uploading anything:

```sh
make connect
make status
```

**Checkpoint:** `Root ADB verified.` appears. Before the first installation, `Client not installed.`
is normal. If `Runtime missing` appears, stop and read [troubleshooting](#troubleshooting). If ADB
cannot connect, fix root access or the address before continuing.

Install the client:

```sh
make install
```

**Checkpoint:** `Client installed and stopped.` appears. The helper verifies the uploaded binary
before replacing the installed client. It installs under `/data/yandex-station-chatgpt-voice/`,
keeps the old binary as `client.previous`, and leaves the new client stopped.

## 4. Sign in to ChatGPT

The Station needs your ChatGPT sign-in. You sign in once in a browser on the computer. After that,
the Station keeps the session fresh by itself.

```sh
make login
```

1. The command checks the Station first. Then a new Chrome window opens on the ChatGPT sign-in page.
   This window uses a fresh, empty profile, so it starts signed out even if your usual Chrome is
   signed in.
2. Sign in there with the account you checked in step 1. Passwords, codes, and Google or Apple
   sign-in all happen in that window. Nothing is typed into the terminal.
3. When your chats appear, return to the terminal and press **Enter**. If the sign-in is not
   finished yet, the command asks you to finish and press Enter again.
4. Wait. The Chrome window closes by itself.

**Checkpoint:** `Signed in. The Station saved a renewable ChatGPT session` appears.

Before submitting your sign-in, cancel with **Ctrl+C** in the terminal, or close Chrome and then
return to the terminal and press **Enter**. The Station stays unchanged. After submission, an
interrupted command may already have saved the session. Run `make status` before retrying.

The helper keeps only ChatGPT's session cookies from that window, then deletes the window's
temporary profile. It uploads the cookies to the Station. The Station then asks ChatGPT for an
access token over its own network, and saves the cookies and token at
`/data/yandex-station-chatgpt-voice/session.json` with private file permissions. **Treat the saved
session like a password.** Keep it out of screenshots, logs, Git, and messages to other people.
**Network ADB has no key authentication on the tested firmware. Anyone who can reach port 5555 on
the LAN can read the saved session. Keep that port inside a trusted LAN.**

<details>
<summary>Use Chromium or Chrome from another location</summary>

Add the browser program's full path to `config.mk`. Spaces in the path need no quotes:

```make
CHROME = /usr/bin/chromium
```

On macOS the program is inside the app, for example
`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`. On Ubuntu, the snap version of
Chromium may fail to start from this tool. If it does, install Google Chrome from its `.deb`
package.

</details>

Start the client:

```sh
make start
```

Wait more than **three seconds**, then run:

```sh
make status
```

**Checkpoint:** `Client running with verified PID` appears. Check that there are no
`Runtime missing` lines. The status command inspects local files and the process. It does not test
ChatGPT access.

## 5. Check a conversation

1. Say **"Яндекс"** on its own. Wait for the short high tone.
2. After the tone, say **"Сосчитай от одного до десяти"**. Check that the whole answer is audible.
3. Say **"Заверши разговор"**. A low tone marks normal completion. The client passes queued audio to
   the speaker first. The last fraction of a second can still be cut by the audio device or by
   ChatGPT ending before sending its tail.
4. Say **"Алиса, сколько будет два плюс два?"**. Check that the original assistant still answers.

**Ready:** both assistants work. You can close the terminal and disconnect the computer. Audio
before the high tone is discarded, so wait for the tone before asking your question.

**After a Station reboot, start the client again from the computer:**

```sh
cd ~/Projects/yandex-station-chatgpt-voice
make connect start
```

Automatic startup is off until you [enable it below](#start-after-a-station-reboot). If the address
changed, update `STATION_IP` first.

## Manage the client

After restarting the computer or changing networks, reconnect before running device commands:

```sh
cd ~/Projects/yandex-station-chatgpt-voice
make connect
```

| Command         | Action                                                                                 |
| --------------- | -------------------------------------------------------------------------------------- |
| `make status`   | Inspect root, files, process, runtime files, and saved token expiry.                   |
| `make start`    | Start the installed client. Leave an already running client alone.                     |
| `make stop`     | Stop ChatGPT and pause boot startup until `make start`. Keep Alice available.          |
| `make restart`  | Stop and start the client. Apply new settings.                                         |
| `make login`    | Sign in again. A running client uses the new session from its next call.               |
| `make rollback` | Restore the saved previous binary and leave it stopped. Keep the session and settings. |
| `make logs`     | Follow the client log. Ctrl+C stops following, not the client.                         |

Logs use **UTC timestamps** and live at `/tmp/yandex-station-chatgpt-voice.log`. Ctrl+C in the log
viewer can print `Canceled`; the client keeps running. For an ADB shell, run `make shell`. For the
full command list, run `make help`.

Worker output rotates between the log and its `.1` file, with 512 KiB per segment. If a running
`make logs` stops showing new entries after rotation, close it with Ctrl+C and run it again.

### Start after a Station reboot

First complete a conversation using `make start`. Then enable boot startup:

```sh
make enable-autostart
make status
```

**Checkpoint:** `Boot scripts recognized.` and `Autostart enabled.` appear. This installs one init
script on the tested Mini 2 boot layout. Other boot layouts are refused. It uses the settings saved
by your last `make start` or `make restart` and starts wake detection, not a conversation.

- `make stop`, `make install`, and `make rollback` pause startup across reboot. Run `make start` to
  resume it.
- `make disable-autostart` removes the boot hook and keeps the current process running.
- A retained mutation lock makes boot startup skip the client. Resolve the interrupted operation
  before removing that lock.

### Return to the previous build

```sh
make rollback start
```

`rollback` keeps `client.previous`, so repeating it leaves you on the same build. Installing an
identical build also keeps the previous binary. To return to this checkout's build, run
`make install restart`. An older build may use the legacy launcher without process recovery.

A build from before browser sign-in still reads the saved access token, but it never renews it. It
works until that token expires. `make login` refuses such a build. Run `make install` first.

### How the session stays fresh

Before each call, right after "Яндекс", the client checks the saved access token. If the token has
24 hours or less remaining, or its expiry cannot be read, the client renews it once from ChatGPT. It
saves the new token and cookies, then continues the call. This adds one short HTTPS request before
the high tone, only on calls that need renewal. The client has no background timer and never retries
a failed renewal. After a failed renewal it does not fall back to the old token. That call ends with
a log message, and wake detection rearms for the next "Яндекс".

`make status` shows the saved token's expiry in UTC, with `expired` when that time has passed. An
expired token is normal between calls; the next call renews it. The expiry is **unverified
metadata**, not proof that ChatGPT accepts the session. An opaque token or unavailable expiry shows
`unknown`. The token itself is not printed.

### Sign in again

Run `make login` again when the log says `run make login`. This happens, for example, after you sign
out of ChatGPT on all devices or change your password. No rebuild, reinstall, or restart is needed:
a running client uses the new session from its next call.

### Update the client

In the project folder, run:

```sh
git pull --ff-only && make install restart
```

This builds, installs, and starts the updated client. The saved session is preserved. If `git pull`
refuses local changes, inspect them before replacing anything.

**Updating from a version that used `make auth`:** its saved token cannot renew, so sign in once
with the browser before starting:

```sh
git pull --ff-only && make install && make login && make start
```

Until you do, `make start` reports `Client exited during startup` and `make logs` shows
`saved session predates browser sign-in; run make login on the computer`.

## Change the voice or model

These settings are optional. Open `config.mk` with `nano config.mk` and add only what you want to
change. These are the defaults:

```make
VOICE = orbit
BACKEND_MODEL = gpt-6-thinking
BACKEND_THINKING_EFFORT = extended
```

Save the file, then run `make restart`. No rebuild, reinstall, or new sign-in is needed.

- Missing or exactly empty values use the defaults.
- `BACKEND_THINKING_EFFORT = disabled` omits the thinking-effort parameter. Only that exact value
  has special meaning.
- Other strings pass unchanged, including case and whitespace. There is no fixed list of names.
  ChatGPT decides which values it accepts. For a rejection, restore the defaults and restart.

<details>
<summary>Make syntax and direct command-line flags</summary>

`config.mk` is Make source. Write a literal dollar as `$$` and a literal hash as `\#`. Quotes become
part of the value. Expressions such as `$(shell ...)` execute as Make code. Evaluated settings are
passed to the client as literal arguments.

The executable also accepts run flags:

```sh
client run --voice=orbit --backend-model=gpt-6-thinking --backend-thinking-effort=disabled
```

Use shell quoting for literal text and `--name=value` for values beginning with a hyphen. Running
without arguments or flags uses defaults. Environment and argument size limits apply. NUL cannot be
transported. See [config.example.mk](config.example.mk) for build and tool overrides.

</details>

## Conversation behavior

- **Alice has priority.** Saying "Алиса" during a GPT call ends that call and lets Alice respond.
  After Alice finishes, say "Яндекс" to start a new call. The old call does not resume.
- **No local time limit.** The client has no idle timeout or call-duration limit. ChatGPT can end a
  call because of account limits. The microphone keeps sending audio while the call is open. End the
  call, say "Алиса", or run `make stop` to stop that stream.
- **Brief connection loss.** The existing WebRTC connection can recover from `disconnected`. A final
  failure or closed control channel ends the call. There is no automatic new call.
- **Completion tone.** Only a decoded ChatGPT hangup produces the low tone. A transport failure ends
  the call without that tone. Say "Яндекс" again when ready for a new call.
- **Process recovery.** A supervisor can restart a failed client twice, waiting five seconds before
  each attempt. Each replacement waits for "Яндекс". A normal hangup or rejected web session does
  not trigger a new call. A missing, invalid, or pre-sign-in saved session stops recovery; run
  `make login`.

## Troubleshooting

Run `make status` and `make logs` first. Logs can contain conversation diagnostics. Review them
before sharing, and never share a session file, sign-in cookies, or a token.

<details>
<summary>ADB cannot connect, or ADB command failed</summary>

Check the Station's current IP in the router and update `STATION_IP` if needed. Confirm root ADB is
enabled, the computer is on the same LAN, and the VPN permits local access. Run `make connect`. The
[root guide](https://github.com/hu553in/yandex-station-root) covers enabling ADB. For
`remote state may be unknown`, run `make status` and inspect any mutation lock before retrying an
operation that might have changed the Station.

</details>

<details>
<summary>No high tone after saying Яндекс</summary>

Check `make status` and `make logs`. Confirm Alice is idle and its wake word is **Алиса**. Confirm
that the Station itself can reach ChatGPT. Browser Voice on the computer does not prove that the
Station has the same network access. `status` is not a ChatGPT connection test.

</details>

<details>
<summary>Client stopped, or Station audio reports i/o timeout</summary>

A native audio fault stops the worker. The supervisor tries at most two replacements, then stops.
Inspect `make logs`, run `make restart`, wait more than three seconds, then run `make status` again.
The audio service can connect without delivering audio. An initial `Yandex wake detection armed.`
message alone does not prove capture works.

</details>

<details>
<summary>Sign-in fails, or the service returns 401, 403, 400, or 422</summary>

| Error                                                       | Next action                                                                                  |
| ----------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `Install the client first` or `cannot use browser sign-in`  | Run `make install`, then `make login` again.                                                 |
| `Cannot start Chrome` or `CHROME must be the full path`     | Install Google Chrome, or set `CHROME` as described in [sign in](#4-sign-in-to-chatgpt).     |
| `No ChatGPT sign-in found yet`                              | Finish signing in until your chats appear in that window, then press Enter again.            |
| Google says the browser or app may not be secure            | Try another sign-in method that ChatGPT offers in that window.                               |
| `edge challenged session renewal` or `HTTPS request failed` | The Station itself cannot reach ChatGPT's sign-in service. Check its network route (step 1). |
| `session renewal timed out`                                 | The Station's network was slow. Say "Яндекс" again, and check the network if it repeats.     |
| `run make login`, `ended the saved web session`, or `401`   | Run `make login`. A running client uses the new session from its next call.                  |
| `403` during a call, or connection setup failure            | Check browser Voice access and the Station's network. This need not mean expiry.             |
| `400`, `422`, or session error                              | Restore the voice settings to defaults, restart, and check service compatibility.            |

`make login` shows the Station's error after `Station command failed (1).` Rejected sessions log a
status or fixed error without the rejected value, cookies, or response body. Detection rearms for a
fresh wake word. The client does not automatically retry renewal or signaling.

</details>

<details>
<summary>Runtime missing, or an operation leaves a mutation lock</summary>

For missing runtime files, stop. The client needs the firmware's audio services, libraries, and
Yandex wake-word model. Other firmware layouts are not supported by the current code.

For a mutation lock, confirm no install, sign-in, start, or stop is running. Inspect filenames and
permissions without printing the saved session:

```sh
make shell
```

Inside that shell:

```sh
ls -ld /data/.yandex-station-chatgpt-voice.lock
ls -la /data/yandex-station-chatgpt-voice/
exit
```

An uncertain operation retains remote scripts, staging files, and the lock. Retained `.auth-*` files
contain ChatGPT sign-in cookies. Keep them private. Check `make status` and `make logs` to confirm
the previous operation's outcome. If that is unclear, keep the files and lock rather than retrying.

Only after confirming the outcome and that no mutation command is still running, open `make shell`
again and clear the leftover staging files and stale lock:

```sh
rm -f /data/yandex-station-chatgpt-voice/.auth-* /data/yandex-station-chatgpt-voice/.candidate-* &&
  rm -f /data/.yandex-station-chatgpt-voice.lock/owner &&
  rmdir /data/.yandex-station-chatgpt-voice.lock
exit
```

This keeps `client`, `client.previous`, and `session.json`. Recheck `make status` before retrying
the interrupted command.

</details>

<details>
<summary>command not found, or Go reports an older version</summary>

Return to [computer setup](#2-prepare-the-computer). Follow the Homebrew or uv installer's PATH
instructions and open a new terminal. If Go is at least 1.21 but automatic toolchain selection was
disabled, run `export GOTOOLCHAIN=auto` in this terminal, then repeat `make build`. For older Go,
install Go at the version declared in [`go.mod`](go.mod) or newer.

</details>

## Remove the client

**This deletes the saved session and previous binary as well as the client.** Stop it and open the
Station shell:

```sh
make disable-autostart stop shell
```

Run the following **inside that shell**, then return to the computer:

```sh
rm -rf /data/yandex-station-chatgpt-voice \
  /tmp/yandex-station-chatgpt-voice /tmp/yandex-station-chatgpt-voice.log \
  /tmp/yandex-station-chatgpt-voice.log.1
exit
```

Root access and the original Alice services remain.

## Developer checks

Normal installation does not need these checks. For development, install
[Bun](https://bun.sh/docs/installation), [Node.js](https://nodejs.org/),
[golangci-lint](https://golangci-lint.run/docs/welcome/install/local/) and
[shfmt](https://github.com/mvdan/sh), plus host Opus and a C compiler:

```sh
# macOS, with Command Line Tools already installed
brew install opus golangci-lint shfmt
```

```sh
# Ubuntu, after installing the tools linked above
sudo apt-get install -y build-essential libopus-dev
```

Developer dependencies can build Rust extensions when a Python wheel is unavailable. Install
[Rust](https://rustup.rs/) if uv reports a missing Rust compiler. Normal installation and sign-in do
not install developer tools.

Then run:

```sh
make install-deps
make check
```

This checks formatting, Go/Python/C/shell lint, types, dependencies, known vulnerabilities, unused
code, Bandit, host tests, the ARM build, Renovate and GitHub Actions. Tests use local fixtures,
virtual Pion packets and a local HTTPS server. They do not contact a Station or ChatGPT.
`make lint-fix` applies supported fixes; `make check-fix` applies them and then repeats all checks.

`make check` does not need a browser. To check installed Chrome against a synthetic local page, with
no account or ChatGPT traffic, run:

```sh
STATION_BROWSER_TEST=1 uv run --locked python -B -m unittest discover -s tools -p 'test_browser_login.py'
```

Add `STATION_BROWSER_HEADED=1` to watch the window, or `CHROME=/path/to/chrome` to test another
browser. This proves browser handling only, not a real ChatGPT sign-in.

Go dependencies and tools live in `go.mod`; Python dependencies, including Playwright, live in
`pyproject.toml` and `uv.lock`. Renovate extends the shared
[Go](https://github.com/hu553in/common-things/blob/main/presets/renovate/go.json) and
[Python](https://github.com/hu553in/common-things/blob/main/presets/renovate/python.json) presets.
The build script declares Zig and Opus once; Renovate updates Opus and its archive checksum
together. The shared presets and uv wait three days for new package releases.

After `make install-deps`, enable the project checks and Conventional Commit hooks once per clone:

```sh
uv run --locked prek install
```

`make build` targets Linux ARMv7 with the hard-float ABI. Its compiler wrappers and static ARM Opus
library live under `.cache/`. `make clean` removes `build/` and keeps that cache, local settings and
the Station's saved session. Running `make` without a target lists commands and does not touch a
Station.

## Credits

The protocol work uses [chatgpt-web-voice](https://github.com/dyhhhhhh/chatgpt-web-voice) as a
reference. Terminal event handling also uses
[Alisa ChatGPT Voice](https://github.com/rison1337/Alisa-ChatGPT-voice-assistant) as a reference.
