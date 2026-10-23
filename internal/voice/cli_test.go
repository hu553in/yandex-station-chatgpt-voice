package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunOptions(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--help"}, {"run", "--voice=-future", "-h"}, {"help"},
	} {
		var out, diagnostics bytes.Buffer
		if err := Run(t.Context(), args, nil, &out, &diagnostics); err != nil || !strings.Contains(out.String(), "--backend-thinking-effort") {
			t.Fatalf("help failed: %v %q", err, out.String())
		}
	}
	for _, args := range [][]string{
		{"run", "--private-marker=secret"}, {"run", "--voice"}, {"run", "private-marker"},
		{"run", "--", "private-marker"}, {"run", "--voice=valid", "private-marker"},
	} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), args, nil, &out, &diagnostics)
		if err == nil || strings.Contains(err.Error()+out.String()+diagnostics.String(), "private-marker") {
			t.Fatalf("invalid options accepted or disclosed: %v", err)
		}
	}
	settings, err := parseRunSettings([]string{"--voice", "-future", "--backend-model", "  модель  ", "--backend-thinking-effort", ""})
	if err != nil || settings != (sessionSettings{Voice: "-future", BackendModel: "  модель  "}) {
		t.Fatalf("ordinary string flags changed: %#v %v", settings, err)
	}
}

type failingPrompt struct{}

func (failingPrompt) Write([]byte) (int, error) { return 0, errors.New("private writer details") }

func TestHiddenInputRestorationAndFailures(t *testing.T) {
	for _, action := range []string{"success", "restore", "read", "EOF", "cancel", "prompt"} {
		t.Run(action, func(t *testing.T) {
			in, input, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			defer input.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch action {
			case "read":
				in.Close()
			case "EOF":
				input.Close()
			case "cancel":
				cancel()
			default:
				if _, err := io.WriteString(input, "synthetic-token\n"); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if action == "prompt" {
				writer = failingPrompt{}
			}
			restored := 0
			token, err := readHiddenInput(ctx, in, writer, func() error {
				restored++
				if action == "restore" {
					return errors.New("private restore details")
				}
				return nil
			})
			if restored != 1 || strings.Contains(output.String(), "synthetic-token") {
				t.Fatal("input was echoed or restore was skipped")
			}
			if action == "success" {
				if err != nil || string(token) != "synthetic-token" {
					t.Fatal("hidden input failed", err)
				}
				clear(token)
				return
			}
			if token != nil || err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("failed input returned a token or unsafe error", err)
			}
			if wantCancel := action == "EOF" || action == "cancel"; errors.Is(err, context.Canceled) != wantCancel {
				t.Fatal("I/O failure and cancellation were confused", err)
			}
		})
	}
}
