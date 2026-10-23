# ChatGPT Voice for Yandex Station

Talk to ChatGPT through a rooted Yandex Station's microphones and speaker. The voice client runs on
the Station and connects directly to ChatGPT, so you do not need a computer during a conversation.

- **Wake words.** Say "Яндекс" for ChatGPT or "Алиса" for the original assistant.
- **Account.** Uses your ChatGPT account's Voice access and limits. No OpenAI API key is needed.
- **Hardware.** Tested only on Station Mini 2.

[Setup](#setup) · [Token](#add-a-chatgpt-token) · [Settings](#choose-voice-session-settings) ·
[Use](#use) · [Manage](#manage) · [Troubleshooting](#troubleshooting) · [Development](#development)

## Setup

You need:

- A rooted Station with network ADB on port 5555 and its original Alice services running. See
  [yandex-station-root](https://github.com/hu553in/yandex-station-root) for root access.
- Go 1.27.3 or later, [uv](https://docs.astral.sh/uv/getting-started/installation/), Python 3, make,
  pkg-config, curl, tar, and Android platform-tools with `adb`.
- Internet access from the Station to ChatGPT. If your connection needs a VPN, route the Station's
  traffic through it. A VPN on the build computer alone does not cover the Station.

Before starting the client, open the Station's settings in **Дом с Алисой** and select **Алиса** as
its wake word. This setting is required so **Яндекс** can start ChatGPT.

Run `make` commands from the repository root. Its path must not contain whitespace: Opus/libtool
does not support it.

### 1. Set the Station's address

```sh
cp config.example.mk config.mk
```

Edit `config.mk` and set the Station's current IPv4 address:

```make
STATION_IP = 192.168.1.50
```

`config.mk` is ignored by Git. Use the address from your router's device list.

### 2. Build and install

```sh
make setup
make connect
make install
```

`setup` downloads the build tools and builds ARM Opus. `install` builds the client, verifies the
upload, and installs it under `/data/yandex-station-chatgpt-voice/`. It stops an existing client and
leaves the new one stopped. The previous binary is kept as `client.previous`.

## Add a ChatGPT token

Use an account that can already start a voice conversation at [chatgpt.com](https://chatgpt.com/).

1. Open ChatGPT in Chrome and sign in.
2. Open Developer Tools, then **Network**.
3. Start a voice conversation. Select the **POST** request whose URL contains `/realtime/wm`.
4. Under **Headers → Request Headers**, copy the token from `Authorization`, without the `Bearer `
   prefix. Close the browser's voice conversation.

Import the token at the hidden prompt:

```sh
make auth
```

Then start the Station client:

```sh
make start
```

The token is saved in `/data/yandex-station-chatgpt-voice/session.json` with private file
permissions. Treat it as an account credential. Keep it out of shell commands, logs, and Git.

### Replace an expired token

Copy a fresh token from the browser, then run:

```sh
make auth
make restart
```

No rebuild or reinstall is needed. The running client reads the token at startup, so importing alone
does not replace the token in its memory. Tokens are not refreshed automatically.

To import a JSON file instead of using the prompt:

```sh
make auth SESSION_JSON=/absolute/path/to/session.json
make restart
```

The file must contain an `accessToken` string. An OpenAI API key does not work here.

## Choose voice session settings

Save these optional values in your computer's `config.mk`, then run `make restart`:

```make
VOICE = orbit
BACKEND_MODEL = gpt-6-thinking
BACKEND_THINKING_EFFORT = extended
```

Missing or exactly empty values use the defaults shown above. Set
`BACKEND_THINKING_EFFORT = disabled` to omit the thinking-effort parameter. Only that exact value
has special meaning. Names are unrestricted, and all other strings keep their case and whitespace.
ChatGPT determines which values it accepts.

Settings are read at startup. `make restart` applies changes; `make start` leaves a running client
alone. No rebuild, reinstall, or token import is needed.

`config.mk` is Make source. Write a literal dollar as `$$` and a literal hash in an assignment as
`\#`. Quotes become part of the value. Make expressions such as `$(shell ...)` execute as Make code.
The evaluated values pass to the client as literal arguments.

The executable also accepts ordinary run flags, for example
`client run --voice=orbit --backend-model=gpt-6-thinking --backend-thinking-effort=disabled`. Use
shell quoting for literal text and `--name=value` for values beginning with a hyphen. Running
without arguments or flags uses the defaults. Environment and process argument limits apply, and NUL
cannot be transported.

## Use

1. Say **"Яндекс"** on its own.
2. Wait for the short high tone, then speak. Audio from before that tone is discarded.
3. Ask ChatGPT to end the conversation, for example **"Заверши разговор"**. A low tone marks normal
   completion. The goodbye can currently be cut short before that tone.

**Alice has priority.** Saying "Алиса" during a GPT conversation ends the GPT call and lets Alice
respond. After Alice finishes, say "Яндекс" again to start a new GPT call. The old call does not
resume.

The client sets no idle timeout or call-duration limit. ChatGPT can still end a call because of
account limits. A temporarily disconnected transport can recover on the existing peer. If Pion
reports failure or the control channel closes, the call ends. A fresh "Яндекс" starts another call.
Only a decoded ChatGPT hangup permits the normal completion tone. Transport closure alone is a call
failure and produces no completion tone.

## Manage

| Command                | Action                                                                           |
| ---------------------- | -------------------------------------------------------------------------------- |
| `make status`          | Inspect the installed files, client process, and required Station runtime files. |
| `make start`           | Start the installed client.                                                      |
| `make stop`            | Stop the client. Alice remains available.                                        |
| `make restart`         | Stop and start the client.                                                       |
| `make logs`            | Follow the client log. Press Ctrl+C to stop following.                           |
| `make shell`           | Open an ADB shell on the selected Station.                                       |
| `make install restart` | Build, install, and start an updated client. The saved token is preserved.       |

The client does not start automatically after a Station reboot. Run `make connect start` to start it
again. The computer can disconnect once the client is running.

### Remove the client

Stop it and open a Station shell:

```sh
make stop shell
```

In that shell, remove the client, saved token, previous binary, and temporary runtime files:

```sh
rm -rf /data/yandex-station-chatgpt-voice \
  /tmp/yandex-station-chatgpt-voice /tmp/yandex-station-chatgpt-voice.log
exit
```

Root access and the original Alice services are kept.

## Troubleshooting

- **ADB cannot connect.** Check `STATION_IP`, confirm that root ADB is enabled, then run
  `make connect`.
- **No ready tone.** Run `make status`, then `make logs`. Confirm that Alice is idle and that the
  Station itself can reach ChatGPT. `status` checks local files and the process, not ChatGPT access.
- **401 or expired session.** Import a fresh token with `make auth`, then run `make restart`.
- **403 or connection setup failure.** Check that Voice works in the browser and that the Station's
  network can reach ChatGPT. These failures do not necessarily mean the token expired.
- **400, 422, or a voice session error.** Check the configured strings and service compatibility,
  then run `make restart` after changes. Rejections log a status or fixed error without the response
  text or rejected value. Detection rearms for a fresh wake word, with no automatic signaling retry.
- **A required runtime file is missing.** The client uses the firmware's audio services, libraries,
  and Yandex wake-word model. Other firmware layouts are not supported by the current code.
- **An operation leaves a mutation lock.** Check that no install, token import, start, or stop
  command is still running. Inspect `/data/.yandex-station-chatgpt-voice.lock` through `make shell`
  before removing the stale lock. An uncertain result retains remote temporary scripts, staging
  files, and the lock. Inspect the Station's state and retained files before cleanup or retrying.

The client uses the ChatGPT website's unofficial voice protocol. Website changes can break it.

## Development

`make build` creates `build/yandex-station-chatgpt-voice` for Linux ARMv7 with the hard-float ABI.
The build script downloads its pinned Zig compiler and builds a static ARM Opus library under
`.cache/`.

For host checks, install a C compiler, pkg-config, and the host Opus development library, then run:

```sh
make check
```

This checks formatting, helper syntax, local launch behavior, Go race tests, and `go vet` without
contacting a Station. Local Pion tests drop and restore virtual packets with production callbacks.
`make fmt` formats Go files. `make clean` removes `build/`, preserving the toolchain cache, local
configuration, and the Station's saved token. `make help` lists all targets.

[config.example.mk](config.example.mk) lists the session settings and build and tool overrides.

## Credits

The protocol work uses [chatgpt-web-voice](https://github.com/dyhhhhhh/chatgpt-web-voice) as a
reference.
