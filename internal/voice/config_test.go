package voice

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateCredentialImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "session.json")
	c, err := readSession(strings.NewReader(`{"accessToken":"offline-fixture-token","user":{"email":"ignored@example.invalid"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = importCredential(path, c); err != nil {
		t.Fatal(err)
	}
	first, err := loadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s %v", name, err)
		}
	}
	if err = importCredential(path, credential{AccessToken: "replacement-fixture"}); err != nil {
		t.Fatal(err)
	}
	second, err := loadCredential(path)
	if err != nil || second.DeviceID != first.DeviceID || second.AccessToken != "replacement-fixture" {
		t.Fatal("replacement lost device identity")
	}
	for _, token := range []string{"", "bad\r\nInjected", "sk-platform-key"} {
		if importCredential(path, credential{AccessToken: token}) == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	unchanged, err := loadCredential(path)
	if err != nil || unchanged != second {
		t.Fatal("failed import damaged saved session")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary credential files left behind")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadCredential(path); err == nil {
		t.Fatal("public credential file accepted")
	}
}

func TestMissingAuthBeforeSetup(t *testing.T) {
	var output bytes.Buffer
	err := runStation(t.Context(), filepath.Join(t.TempDir(), "missing.json"), sessionSettings{}, &output)
	if err == nil || !strings.Contains(err.Error(), "auth import") || output.Len() != 0 {
		t.Fatalf("expected auth failure before setup, got %v %q", err, output.String())
	}
}

func TestStationCredentialPath(t *testing.T) {
	if credentialPath() != "/data/yandex-station-chatgpt-voice/session.json" {
		t.Fatal("credential path is not writable Station data")
	}
}

func TestCredentialRenameAndCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupied"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	remove := removeCredentialTemp
	t.Cleanup(func() { removeCredentialTemp = remove })
	var temporary string
	removeCredentialTemp = func(name string) error {
		temporary = name
		return errors.New("private filesystem details")
	}
	err := importCredential(path, credential{AccessToken: "synthetic-private-token", DeviceID: "offline-device"})
	if err == nil || !strings.Contains(err.Error(), "cannot replace private credential file") || !strings.Contains(err.Error(), "cannot remove temporary credential file") || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "filesystem") {
		t.Fatal("import lost its primary or cleanup failure, or leaked data", err)
	}
	if filepath.Dir(temporary) != dir || !strings.HasPrefix(filepath.Base(temporary), ".session-") {
		t.Fatal("cleanup did not target the write-side temporary file")
	}
	if _, err := os.Stat(temporary); err != nil {
		t.Fatal("fixture did not leave a temporary file to report", err)
	}
}

func TestAuthStatus(t *testing.T) {
	jwt := func(claims string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic-private-signature"
	}
	for _, tc := range []struct {
		name, token, want string
	}{
		{"future", jwt(`{"exp":4102444800,"private":"synthetic-private-claim"}`), "2100-01-01T00:00:00Z"},
		{"expired", jwt(`{"exp":1}`), "1970-01-01T00:00:01Z (expired)"},
		{"zero", jwt(`{"exp":0}`), "1970-01-01T00:00:00Z (expired)"},
		{"opaque", "synthetic-private-token", "unknown"},
		{"absent", jwt(`{"private":"synthetic-private-claim"}`), "unknown"},
		{"null", jwt(`{"exp":null}`), "unknown"},
		{"string", jwt(`{"exp":"synthetic-private-claim"}`), "unknown"},
		{"invalid JSON", jwt(`{"exp":1`), "unknown"},
		{"invalid base64", "header.%%%.signature", "unknown"},
		{"out of range", jwt(`{"exp":253402300800}`), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			if err := importCredential(path, credential{AccessToken: tc.token, DeviceID: "offline-device"}); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := writeAuthStatus(path, &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != "Saved token expiry (unverified): "+tc.want+"\n" {
				t.Fatal("status did not return only expiry metadata")
			}
			loaded, err := loadCredential(path)
			if err != nil || loaded.AccessToken != tc.token {
				t.Fatal("status changed credential acceptance or contents")
			}
		})
	}
}

func TestAuthStatusCredentialFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic-private-path.json")
	var output bytes.Buffer
	err := writeAuthStatus(path, &output)
	if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("missing session was accepted or disclosed private details")
	}
	if err := os.WriteFile(path, []byte(`{"accessToken":"synthetic-private-token"`), 0600); err != nil {
		t.Fatal(err)
	}
	err = writeAuthStatus(path, &output)
	if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("invalid session was accepted or disclosed private details")
	}
}
