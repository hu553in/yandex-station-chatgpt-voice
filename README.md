# ChatGPT Voice for Yandex Station

Talk to ChatGPT through a rooted Yandex Station's microphones and speaker. The client runs on the
Station and connects directly to ChatGPT. Once it is running, the computer can disconnect.

- **Wake words.** Say "Яндекс" for ChatGPT. "Алиса" still starts the original assistant.
- **Account.** Uses your ChatGPT account's Voice access and limits. No OpenAI API key is needed.
- **Hardware.** Tested only on Station Mini 2. It needs root access and the original Alice services.

[Before you start](#1-check-the-station-and-chatgpt) · [Computer setup](#2-prepare-the-computer) ·
[Install](#3-connect-and-install) · [Token](#4-add-your-chatgpt-token) ·
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

Use the same ChatGPT account for the browser check and token below. Available Voice access depends
on the account and service. This client uses the website's unofficial voice protocol, which can
change independently of this project.

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
[automatic toolchain selection](https://go.dev/doc/toolchain), `make setup` downloads the required
Go version from `go.mod`.

For another Linux distribution, install Git, make, curl, tar, Python 3, pkg-config, `adb`, uv, and
Go 1.27.3 or later. Native Windows setup is not covered here.

</details>

Download the project into a folder **without spaces in its path**. The Opus build cannot use a path
with whitespace.

```sh
mkdir -p ~/Projects
cd ~/Projects
git clone https://github.com/hu553in/yandex-station-chatgpt-voice.git
cd yandex-station-chatgpt-voice
make setup && make build
```

`setup` downloads Go dependencies, the pinned Zig compiler, and Opus, then builds the ARM audio
library. It prints compiler feature probes. Judge completion by the build checkpoint below. A host
Opus development package is needed only for [developer checks](#developer-checks).

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

## 4. Add your ChatGPT token

The token lets the Station use your signed-in ChatGPT account. **Treat it like a password.** Keep it
out of shell commands, screenshots, logs, Git, and messages to other people. An OpenAI API key does
not work here.

### Copy the token from Chrome

1. Open [chatgpt.com](https://chatgpt.com/) and sign in to the account checked in step 1.
2. Open Developer Tools with **Cmd+Option+I** on Mac or **Ctrl+Shift+I** on Linux. Select
   **Network**. Leave it recording before the next step.
3. Start a voice conversation in ChatGPT. In Network's filter field, enter `realtime/wm`.
4. Select a **POST** request containing `/realtime/wm`. Under **Headers → Request Headers**, find
   `Authorization`. Copy the complete value **after** `Bearer `, without that prefix.
5. Stop the browser's voice conversation. Keep the token private while importing it below.

If the request is missing, clear the filter, confirm that Network is recording, and start a new
voice conversation with Developer Tools already open. Header inspection is described in
[Chrome's Network reference](https://developer.chrome.com/docs/devtools/network/reference#headers).

### Import the token and start the client

In the project terminal, **type this command by hand**. Copying it would replace the token on your
clipboard:

```sh
make auth
```

At `ChatGPT web access token (hidden):`, paste the token and press **Enter**. No characters appear
while you paste. Wait for `Session imported.` before continuing. The token is saved at
`/data/yandex-station-chatgpt-voice/session.json` with private file permissions. **Network ADB has
no key authentication on the tested firmware. Anyone who can reach port 5555 on the LAN can read the
token. Keep that port inside a trusted LAN.**

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
3. Say **"Заверши разговор"**. A low tone marks normal completion. The goodbye can currently be cut
   short before that tone.
4. Say **"Алиса, сколько будет два плюс два?"**. Check that the original assistant still answers.

**Ready:** both assistants work. You can close the terminal and disconnect the computer. Audio
before the high tone is discarded, so wait for the tone before asking your question.

**After a Station reboot, start the client again from the computer:**

```sh
cd ~/Projects/yandex-station-chatgpt-voice
make connect start
```

There is no automatic startup after a reboot. If the address changed, update `STATION_IP` first.

## Manage the client

After restarting the computer or changing networks, reconnect before running device commands:

```sh
cd ~/Projects/yandex-station-chatgpt-voice
make connect
```

| Command        | Action                                                               |
| -------------- | -------------------------------------------------------------------- |
| `make status`  | Inspect root, files, process, runtime files, and saved token expiry. |
| `make start`   | Start the installed client. Leave an already running client alone.   |
| `make stop`    | Stop ChatGPT. Keep Alice available.                                  |
| `make restart` | Stop and start the client. Apply a new token or settings.            |
| `make logs`    | Follow the client log. Ctrl+C stops following, not the client.       |

Logs use **UTC timestamps** and live at `/tmp/yandex-station-chatgpt-voice.log`. Ctrl+C in the log
viewer can print `Canceled`; the client keeps running. For an ADB shell, run `make shell`. For the
full command list, run `make help`.

### Replace an expired token

Copy a fresh token using [the Chrome steps above](#copy-the-token-from-chrome). Type the following
command by hand to keep the token on your clipboard:

```sh
make auth && make restart
```

No rebuild or reinstall is needed. The client reads the token at startup, so importing alone does
not change the token in a running process. Tokens are not refreshed automatically.

`make status` shows the saved token's expiry in UTC, with `expired` when that time has passed. This
is **unverified metadata**, not proof that ChatGPT accepts the token. An opaque token or unavailable
expiry shows `unknown`. The token itself is not printed.

<details>
<summary>Import a session JSON file instead of pasting a token</summary>

The file must contain an `accessToken` string. Keep it private. Use its absolute path:

```sh
make auth SESSION_JSON=/absolute/path/to/session.json && make restart
```

</details>

### Update the client

In the project folder, run:

```sh
git pull --ff-only && make install restart
```

This builds, installs, and starts the updated client. The saved token is preserved. If `git pull`
refuses local changes, inspect them before replacing anything.

## Change the voice or model

These settings are optional. Open `config.mk` with `nano config.mk` and add only what you want to
change. These are the defaults:

```make
VOICE = orbit
BACKEND_MODEL = gpt-6-thinking
BACKEND_THINKING_EFFORT = extended
```

Save the file, then run `make restart`. No rebuild, reinstall, or token import is needed.

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

## Troubleshooting

Run `make status` and `make logs` first. Logs can contain conversation diagnostics. Review them
before sharing, and never share a token or a session file.

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

A native audio fault stops the client. It does not restart automatically. Inspect `make logs`, run
`make restart`, wait more than three seconds, then run `make status` again. The audio service can
connect without delivering audio. An initial `Yandex wake detection armed.` message alone does not
prove capture works.

</details>

<details>
<summary>Token import fails, or the service returns 401, 403, 400, or 422</summary>

| Error                                                       | Next action                                                                                         |
| ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `make auth` reports `Station command failed (1).`           | Check `make status` for an installed client. Copy the token again, without `Bearer ` or an API key. |
| `Access token exceeds 32768 bytes.` or the ASCII-text error | Copy only the token after `Bearer ` from the Authorization header.                                  |
| `401` or expired session                                    | Run `make auth` with a fresh browser token, then `make restart`.                                    |
| `403` or connection setup failure                           | Check browser Voice access and the Station's network. This need not mean expiry.                    |
| `400`, `422`, or session error                              | Restore the voice settings to defaults, restart, and check service compatibility.                   |

Rejected sessions log a status or fixed error without the rejected value or response body. Detection
rearms for a fresh wake word. The client does not automatically retry signaling.

</details>

<details>
<summary>Runtime missing, or an operation leaves a mutation lock</summary>

For missing runtime files, stop. The client needs the firmware's audio services, libraries, and
Yandex wake-word model. Other firmware layouts are not supported by the current code.

For a mutation lock, confirm no install, token import, start, or stop is running. Inspect filenames
and permissions without printing the token:

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
contain a token. Keep them private. Check `make status` and `make logs` to confirm the previous
operation's outcome. If that is unclear, keep the files and lock rather than retrying.

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
disabled, run `export GOTOOLCHAIN=auto` in this terminal, then repeat `make setup && make build`.
For older Go, install Go 1.27.3 or later.

</details>

## Remove the client

**This deletes the saved token and previous binary as well as the client.** Stop it and open the
Station shell:

```sh
make stop shell
```

Run the following **inside that shell**, then return to the computer:

```sh
rm -rf /data/yandex-station-chatgpt-voice \
  /tmp/yandex-station-chatgpt-voice /tmp/yandex-station-chatgpt-voice.log
exit
```

Root access and the original Alice services remain.

## Developer checks

Normal installation does not need these checks. To work on the code, install host Opus and a C
compiler first:

```sh
# macOS, with Command Line Tools already installed
brew install opus
```

```sh
# Ubuntu
sudo apt-get install -y build-essential libopus-dev
```

Then run `make check`. It checks formatting, helper syntax, local launch behavior, Go race tests,
and `go vet` without contacting a Station. Local Pion tests drop and restore virtual packets with
production callbacks.

- `make build` targets Linux ARMv7 with the hard-float ABI. The pinned Zig compiler and static ARM
  Opus library live under `.cache/`.
- `make fmt` formats Go files. `make clean` removes `build/` and keeps the toolchain cache, local
  configuration, and the Station's saved token.

## Credits

The protocol work uses [chatgpt-web-voice](https://github.com/dyhhhhhh/chatgpt-web-voice) as a
reference.
