package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunOptions(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--help"}, {"run", "--voice=-future", "-h"}, {"help"},
	} {
		var out, diagnostics bytes.Buffer
		if err := Run(t.Context(), args, &out, &diagnostics); err != nil || !strings.Contains(out.String(), "--backend-thinking-effort") {
			t.Fatalf("help failed: %v %q", err, out.String())
		}
		if !strings.Contains(out.String(), "make login") {
			t.Fatal("help lost the make login marker that the host preflight probes")
		}
	}
	for _, args := range [][]string{
		{"run", "--private-marker=secret"}, {"run", "--voice"}, {"run", "private-marker"},
		{"run", "--", "private-marker"}, {"run", "--voice=valid", "private-marker"},
	} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), args, &out, &diagnostics)
		if err == nil || strings.Contains(err.Error()+out.String()+diagnostics.String(), "private-marker") {
			t.Fatalf("invalid options accepted or disclosed: %v", err)
		}
	}
	settings, err := parseRunSettings([]string{"--voice", "-future", "--backend-model", "  модель  ", "--backend-thinking-effort", ""})
	if err != nil || settings != (sessionSettings{Voice: "-future", BackendModel: "  модель  "}) {
		t.Fatalf("ordinary string flags changed: %#v %v", settings, err)
	}
}

func TestAuthCommandsRejectUnsupportedInput(t *testing.T) {
	if _, err := os.Lstat(credentialPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusing to test auth commands against an existing or inaccessible Station session")
	}
	for _, args := range [][]string{
		{"auth", "status", "private-marker"}, {"auth", "status", "--session-json=private-marker"},
		{"auth", "import"}, {"auth", "import", "--session-json="}, {"auth"}, {"auth", "private-marker"},
	} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), args, &out, &diagnostics)
		if err == nil || !strings.HasPrefix(err.Error(), "usage:") || strings.Contains(err.Error()+out.String()+diagnostics.String(), "private-marker") {
			t.Fatalf("auth accepted unsupported arguments or disclosed them: %v", err)
		}
	}
	for _, args := range [][]string{
		{"auth", "import", "--session-json", "private-marker", "private-marker"},
		{"auth", "import", "--private-marker"},
	} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), args, &out, &diagnostics)
		if err == nil || strings.Contains(err.Error()+out.String(), "private-marker") {
			t.Fatalf("auth import accepted extra input or disclosed it: %v", err)
		}
	}
	var out bytes.Buffer
	err := Run(t.Context(), []string{"auth", "import", "--session-json", "/nonexistent/private-marker.json"}, &out, io.Discard)
	if err == nil || err.Error() != "cannot open the selected sign-in file" || out.Len() != 0 {
		t.Fatalf("missing sign-in file was not rejected with a fixed message: %v", err)
	}
	if err := Run(t.Context(), []string{"auth", "status"}, io.Discard, io.Discard); err != errNoSession {
		t.Fatalf("auth status did not reach the saved session: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	err = Run(canceled, []string{"auth", "import", "--session-json", filepath.Join("testdata", "browser-sign-in.json")}, &out, io.Discard)
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "interrupted") || strings.Contains(err.Error(), "was not changed") || out.Len() != 0 {
		t.Fatalf("interrupted import could look successful to the host: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(credentialPath())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted import created the Station app directory on this host")
	}
}
