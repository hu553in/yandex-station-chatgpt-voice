package voice

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const help = `yandex-station-chatgpt-voice: Station ChatGPT website Voice

Usage:
  yandex-station-chatgpt-voice run [--voice=NAME] [--backend-model=NAME] [--backend-thinking-effort=VALUE]
  yandex-station-chatgpt-voice supervise --log=FILE [run options]
  yandex-station-chatgpt-voice auth import --session-json FILE
  yandex-station-chatgpt-voice auth status
  yandex-station-chatgpt-voice help

Sign in with make login on the computer. It passes only ChatGPT session
cookies to auth import, which renews them on the Station and saves the session.
Before each call the client renews the session once when its token expires
within 24 hours or has no readable expiry. The Station starts GPT on Яндекс
only while native Alice explicitly reports IDLE. Алиса stays native.
auth status prints the saved token's unverified expiry in UTC, without the token.
`

//nolint:gocognit // The command dispatch keeps flag errors and credential input on their existing output paths.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return runStation(ctx, credentialPath(), sessionSettings{}, out)
	}
	switch args[0] {
	case "supervise":
		return superviseCommand(ctx, args[1:], out)
	case "help", "-h", "--help":
		_, err := io.WriteString(out, help)
		return err
	case "auth":
		usage := errors.New("usage: yandex-station-chatgpt-voice auth import --session-json FILE | auth status")
		if len(args) == 2 && args[1] == "status" {
			return writeAuthStatus(credentialPath(), out)
		}
		if len(args) < 2 || args[1] != "import" {
			return usage
		}
		flags := flag.NewFlagSet("auth import", flag.ContinueOnError)
		flags.SetOutput(errOut)
		input := flags.String("session-json", "", "sign-in cookie file written by make login")
		if err := flags.Parse(args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return errors.New("invalid auth import options")
		}
		if flags.NArg() != 0 {
			return errors.New("credentials are never accepted as arguments")
		}
		if *input == "" {
			return usage
		}
		return importCommand(ctx, *input, out)
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

func importCommand(ctx context.Context, input string, out io.Writer) error {
	//nolint:gosec // Auth import intentionally opens the path explicitly selected by --session-json.
	f, err := os.Open(input)
	if err != nil {
		return errors.New("cannot open the selected sign-in file")
	}
	defer f.Close()
	client := signalingClient()
	defer client.CloseIdleConnections()
	if importBrowserSessionErr := importBrowserSession(
		ctx,
		credentialPath(),
		client,
		f,
	); importBrowserSessionErr != nil {
		if ctx.Err() != nil {
			return errors.New("sign-in import was interrupted; inspect make status before retrying")
		}
		return importBrowserSessionErr
	}
	_, err = fmt.Fprintln(
		out,
		"Signed in. The Station saved a renewable ChatGPT session; a running client uses it from its next call.",
	)
	return err
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
