package voice

import (
	"bytes"
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
	err := runStation(t.Context(), filepath.Join(t.TempDir(), "missing.json"), &output)
	if err == nil || !strings.Contains(err.Error(), "auth import") || output.Len() != 0 {
		t.Fatalf("expected auth failure before setup, got %v %q", err, output.String())
	}
}

func TestStationCredentialPath(t *testing.T) {
	if credentialPath() != "/data/yandex-station-chatgpt-voice/session.json" {
		t.Fatal("credential path is not writable Station data")
	}
}
