package voice

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func syntheticJWT(claims string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic-private-signature"
}

func syntheticSession(token string) webSession {
	return webSession{
		AccessToken: token,
		DeviceID:    "offline-device",
		Cookies:     []sessionCookie{{Name: sessionCookieName, Value: "synthetic-private-cookie"}},
	}
}

func TestPrivateSessionPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "session.json")
	want := webSession{AccessToken: "offline-fixture-token", DeviceID: "offline-device", Cookies: []sessionCookie{
		{Name: sessionCookieName + ".0", Value: "synthetic-private-chunk-0"},
		{Name: sessionCookieName + ".1", Value: "synthetic-private-chunk-1"},
	}}
	if err := writeSessionFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readSessionFile(path)
	if err != nil || got.credential != want.credential || len(got.Cookies) != 2 || got.Cookies[1] != want.Cookies[1] {
		t.Fatal("saved session did not round-trip", err)
	}
	for name, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600} {
		info, statErr := os.Stat(name)
		if statErr != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s %v", name, statErr)
		}
	}
	for _, invalid := range []webSession{
		{AccessToken: "bad\r\nInjected", DeviceID: "offline-device", Cookies: want.Cookies},
		{AccessToken: "sk-platform-key", DeviceID: "offline-device", Cookies: want.Cookies},
		{AccessToken: "offline-fixture-token", DeviceID: "offline-device"},
		{AccessToken: "offline-fixture-token", DeviceID: "offline-device", Cookies: []sessionCookie{{Name: sessionCookieName + ".1", Value: "gap"}}},
		{AccessToken: "offline-fixture-token", DeviceID: "offline-device", Cookies: []sessionCookie{{Name: sessionCookieName, Value: "bad;value"}}},
	} {
		if writeSessionFile(path, invalid) == nil {
			t.Fatal("invalid session saved")
		}
	}
	unchanged, err := readSessionFile(path)
	if err != nil || unchanged.AccessToken != want.AccessToken {
		t.Fatal("failed save damaged saved session")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary credential files left behind")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readSessionFile(path); err == nil {
		t.Fatal("public session file accepted")
	}
	link := filepath.Join(filepath.Dir(path), "linked.json")
	if chmodErr := os.Chmod(path, 0600); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if symlinkErr := os.Symlink(path, link); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}
	if _, readSessionFileErr := readSessionFile(link); readSessionFileErr == nil {
		t.Fatal("symlinked session file accepted")
	}
	fifo := filepath.Join(filepath.Dir(path), "fifo.json")
	if mkfifoErr := syscall.Mkfifo(fifo, 0600); mkfifoErr != nil {
		t.Fatal(mkfifoErr)
	}
	if _, fifoErr := readSessionFile(fifo); fifoErr == nil {
		t.Fatal("FIFO session file accepted")
	}
	large := filepath.Join(filepath.Dir(path), "large.json")
	if writeFileErr := os.WriteFile(large, bytes.Repeat([]byte(" "), maxCredentialBytes+1), 0600); writeFileErr != nil {
		t.Fatal(writeFileErr)
	}
	if _, oversizedErr := readSessionFile(large); oversizedErr == nil {
		t.Fatal("oversized session file accepted")
	}
}

func TestStartupRequiresBrowserSession(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"missing": "",
		"legacy":  `{"accessToken":"synthetic-private-token","deviceId":"offline-device"}`,
		"corrupt": `{"accessToken":"synthetic-private-token"`,
		"public":  "public",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			switch name {
			case "missing":
			case "public":
				if err := writeSessionFile(path, syntheticSession("synthetic-private-token")); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			err := runStation(t.Context(), path, sessionSettings{}, &output)
			if ExitCode(err) != exitConfig || output.Len() != 0 || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatalf("startup accepted an unusable session or disclosed it: %v %q", err, output.String())
			}
			if name != "public" && !strings.Contains(err.Error(), "run make login") {
				t.Fatal("startup failure lacks sign-in guidance", err)
			}
		})
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
	err := writeSessionFile(path, syntheticSession("synthetic-private-token"))
	if err == nil || !strings.Contains(err.Error(), "cannot replace private credential file") ||
		!strings.Contains(err.Error(), "cannot remove temporary credential file") ||
		strings.Contains(err.Error(), "synthetic") ||
		strings.Contains(err.Error(), "filesystem") {
		t.Fatal("save lost its primary or cleanup failure, or leaked data", err)
	}
	if filepath.Dir(temporary) != dir || !strings.HasPrefix(filepath.Base(temporary), ".session-") {
		t.Fatal("cleanup did not target the write-side temporary file")
	}
	if _, statErr := os.Stat(temporary); statErr != nil {
		t.Fatal("fixture did not leave a temporary file to report", statErr)
	}
}

func TestAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		name, token, want string
	}{
		{"future", syntheticJWT(`{"exp":4102444800,"private":"synthetic-private-claim"}`), "2100-01-01T00:00:00Z"},
		{"expired", syntheticJWT(`{"exp":1}`), "1970-01-01T00:00:01Z (expired)"},
		{"zero", syntheticJWT(`{"exp":0}`), "1970-01-01T00:00:00Z (expired)"},
		{"opaque", "synthetic-private-token", "unknown"},
		{"absent", syntheticJWT(`{"private":"synthetic-private-claim"}`), "unknown"},
		{"null", syntheticJWT(`{"exp":null}`), "unknown"},
		{"string", syntheticJWT(`{"exp":"synthetic-private-claim"}`), "unknown"},
		{"float", syntheticJWT(`{"exp":4102444800.5}`), "unknown"},
		{"duplicate", syntheticJWT(`{"exp":4102444800,"exp":1}`), "unknown"},
		{"negative", syntheticJWT(`{"exp":-1}`), "unknown"},
		{"invalid JSON", syntheticJWT(`{"exp":1`), "unknown"},
		{"invalid base64", "header.%%%.signature", "unknown"},
		{"out of range", syntheticJWT(`{"exp":253402300800}`), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			if err := writeSessionFile(path, syntheticSession(tc.token)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if writeAuthStatusErr := writeAuthStatus(path, &output); writeAuthStatusErr != nil {
				t.Fatal(writeAuthStatusErr)
			}
			if output.String() != "Saved token expiry (unverified): "+tc.want+"\n" {
				t.Fatal("status did not return only expiry metadata", output.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("status changed the saved session")
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
	if writeFileErr := os.WriteFile(
		path,
		[]byte(`{"accessToken":"synthetic-private-token"`),
		0600,
	); writeFileErr != nil {
		t.Fatal(writeFileErr)
	}
	err = writeAuthStatus(path, &output)
	if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("invalid session was accepted or disclosed private details")
	}
}

func TestRollbackClientReadsRenewableSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	saved := syntheticSession(syntheticJWT(`{"exp":4102444800}`))
	if err := writeSessionFile(path, saved); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var bare credential
	if unmarshalErr := json.Unmarshal(
		data,
		&bare,
	); unmarshalErr != nil || bare != saved.credential ||
		!validCredential(bare) {
		t.Fatal("a token-only client could not read the renewable session", unmarshalErr)
	}
	transfer, err := os.ReadFile(filepath.Join("testdata", "browser-sign-in.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old credential
	if innerUnmarshalErr := json.Unmarshal(
		transfer,
		&old,
	); innerUnmarshalErr != nil ||
		validCredential(credential{AccessToken: strings.TrimSpace(old.AccessToken), DeviceID: "offline-device"}) {
		t.Fatal("a token-only client would accept the cookie-only sign-in transfer", innerUnmarshalErr)
	}
}
