package voice

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const help = `yandex-station-chatgpt-voice: Station ChatGPT website Voice

Usage:
  yandex-station-chatgpt-voice run [--voice=NAME] [--backend-model=NAME] [--backend-thinking-effort=VALUE]
  yandex-station-chatgpt-voice auth import [--session-json FILE]
  yandex-station-chatgpt-voice help

Import your own ChatGPT web access token at the hidden prompt, or select one
session JSON file containing accessToken. The Station starts GPT on Яндекс
only while native Alice explicitly reports IDLE. Алиса stays native.
`

func Run(ctx context.Context, args []string, in *os.File, out, errOut io.Writer) error {
	if len(args) == 0 {
		return runStation(ctx, credentialPath(), sessionSettings{}, out)
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := io.WriteString(out, help)
		return err
	case "auth":
		if len(args) < 2 || args[1] != "import" {
			return errors.New("usage: yandex-station-chatgpt-voice auth import [--session-json FILE]")
		}
		flags := flag.NewFlagSet("auth import", flag.ContinueOnError)
		flags.SetOutput(errOut)
		input := flags.String("session-json", "", "explicit session JSON file containing accessToken")
		if err := flags.Parse(args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return errors.New("invalid auth import options")
		}
		if flags.NArg() != 0 {
			return errors.New("credentials are never accepted as arguments")
		}
		path := credentialPath()
		var c credential
		if *input != "" {
			f, err := os.Open(*input)
			if err != nil {
				return errors.New("cannot open the selected session JSON file")
			}
			c, err = readSession(f)
			f.Close()
			if err != nil {
				return err
			}
		} else {
			token, err := readHiddenToken(ctx, in, errOut)
			if err != nil {
				return err
			}
			c.AccessToken = string(token)
			clear(token)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := importCredential(path, c); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Session imported into your private configuration directory.")
		return err
	case "run":
		settings, err := parseRunSettings(args[1:])
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(out, help)
			return err
		}
		if err != nil {
			return err
		}
		return runStation(ctx, credentialPath(), settings, out)
	default:
		return errors.New("unknown command; run yandex-station-chatgpt-voice help")
	}
}

func parseRunSettings(args []string) (sessionSettings, error) {
	var settings sessionSettings
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&settings.Voice, "voice", "", "voice name")
	flags.StringVar(&settings.BackendModel, "backend-model", "", "backend model name")
	flags.StringVar(&settings.BackendThinkingEffort, "backend-thinking-effort", "", "thinking effort or disabled")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return sessionSettings{}, flag.ErrHelp
		}
		return sessionSettings{}, errors.New("invalid run options; see help")
	}
	if flags.NArg() != 0 {
		return sessionSettings{}, errors.New("run does not accept positional arguments")
	}
	return settings, nil
}

func readAvailable(ctx context.Context, in *os.File, buf []byte) (int, error) {
	for ctx.Err() == nil {
		fd := int(in.Fd())
		if fd < 0 {
			return 0, os.ErrClosed
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		count, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, io.EOF
		}
		return count, nil
	}
	return 0, ctx.Err()
}

func readHiddenToken(ctx context.Context, in *os.File, out io.Writer) ([]byte, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("hidden import requires a terminal; use auth import --session-json FILE for an explicit file")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, errors.New("cannot hide terminal input")
	}
	return readHiddenInput(ctx, in, out, func() error { return term.Restore(fd, state) })
}

func readHiddenInput(ctx context.Context, in *os.File, out io.Writer, restore func() error) (token []byte, result error) {
	defer func() {
		if restore() != nil {
			result = errors.Join(result, errors.New("cannot restore terminal input"))
		}
		if _, err := fmt.Fprint(out, "\r\n"); err != nil {
			result = errors.Join(result, errors.New("cannot finish hidden token prompt"))
		}
		if result != nil {
			clear(token)
			token = nil
		}
	}()
	if _, err := fmt.Fprint(out, "ChatGPT web access token (hidden): "); err != nil {
		return nil, errors.New("cannot write hidden token prompt")
	}
	value := make([]byte, 0, 4096)
	var b [1]byte
	for {
		if _, err := readAvailable(ctx, in, b[:]); err != nil {
			clear(value)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil, context.Canceled
			}
			return nil, errors.New("cannot read hidden token input")
		}
		switch b[0] {
		case 3, 4:
			clear(value)
			return nil, context.Canceled
		case '\r', '\n':
			return value, nil
		case 127, 8:
			if len(value) > 0 {
				value[len(value)-1] = 0
				value = value[:len(value)-1]
			}
		default:
			if len(value) >= 32768 {
				clear(value)
				return nil, errors.New("access token is too long")
			}
			value = append(value, b[0])
		}
	}
}
