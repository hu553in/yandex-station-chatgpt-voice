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
  yandex-station-chatgpt-voice run
  yandex-station-chatgpt-voice auth import [--session-json FILE]
  yandex-station-chatgpt-voice help

Import your own ChatGPT web access token at the hidden prompt, or select one
session JSON file containing accessToken. The Station starts GPT on Яндекс
only while native Alice explicitly reports IDLE. Алиса stays native.
`

func Run(ctx context.Context, args []string, in *os.File, out, errOut io.Writer) error {
	if len(args) == 0 {
		return runStation(ctx, credentialPath(), out)
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
		fmt.Fprintln(out, "Session imported into your private configuration directory.")
		return nil
	case "run":
		if len(args) != 1 {
			return errors.New("usage: yandex-station-chatgpt-voice run")
		}
		return runStation(ctx, credentialPath(), out)
	default:
		return errors.New("unknown command; run yandex-station-chatgpt-voice help")
	}
}

func readAvailable(ctx context.Context, in *os.File, buf []byte) (int, error) {
	for ctx.Err() == nil {
		fds := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
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
		count, err := unix.Read(int(in.Fd()), buf)
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
	defer term.Restore(fd, state)
	fmt.Fprint(out, "ChatGPT web access token (hidden): ")
	defer fmt.Fprint(out, "\r\n")
	value := make([]byte, 0, 4096)
	var b [1]byte
	for {
		if _, err := readAvailable(ctx, in, b[:]); err != nil {
			clear(value)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("credential import canceled")
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
