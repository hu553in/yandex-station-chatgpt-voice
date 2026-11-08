package voice

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxCredentialBytes = 1 << 20
const sessionCookieName = "__Secure-next-auth.session-token"
const maxSessionCookies = 16
const maxCookieValueBytes = 4096
const maxTokenExpiry = 253402300799

var removeCredentialTemp = os.Remove

var errNoSession = errors.New("no saved ChatGPT session; run make login on the computer")
var errInvalidSession = errors.New("invalid saved session; run make login on the computer")

type credential struct {
	AccessToken string `json:"accessToken"`
	DeviceID    string `json:"deviceId"`
}

type sessionCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type webSession struct {
	credential
	Cookies []sessionCookie `json:"cookies"`
}

func credentialPath() string {
	return "/data/yandex-station-chatgpt-voice/session.json"
}

func validCredential(c credential) bool {
	if len(c.AccessToken) == 0 || len(c.AccessToken) > 32768 || strings.HasPrefix(c.AccessToken, "sk-") {
		return false
	}
	for _, r := range c.AccessToken {
		if r < 33 || r > 126 {
			return false
		}
	}
	return validDeviceID(c.DeviceID)
}

func validDeviceID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func validCookieValue(value string) bool {
	if len(value) == 0 || len(value) > maxCookieValueBytes {
		return false
	}
	for i := range len(value) {
		if b := value[i]; b < 0x21 || b > 0x7e || b == '"' || b == ',' || b == ';' || b == '\\' {
			return false
		}
	}
	return true
}

func validFamily(cookies []sessionCookie) bool {
	if len(cookies) == 0 || len(cookies) > maxSessionCookies {
		return false
	}
	for i, c := range cookies {
		name := sessionCookieName + "." + strconv.Itoa(i)
		if len(cookies) == 1 && c.Name == sessionCookieName {
			name = sessionCookieName
		}
		if c.Name != name || !validCookieValue(c.Value) {
			return false
		}
	}
	return true
}

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create private credential directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("credential directory must be a real directory")
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("credential directory has another owner")
	}
	if os.Chmod(dir, 0700) != nil {
		return errors.New("cannot protect credential directory")
	}
	return nil
}

func readPrivateFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, errors.New("cannot open private session file")
	}
	f := os.NewFile(uintptr(fd), filepath.Base(path))
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("session file must be a regular file owned by the client user")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session file must be private (chmod 600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil || len(data) > maxCredentialBytes {
		clear(data)
		return nil, errors.New("cannot read session file or file exceeds 1 MiB")
	}
	return data, nil
}

func readSessionFile(path string) (webSession, error) {
	data, err := readPrivateFile(path)
	if err != nil {
		return webSession{}, err
	}
	defer clear(data)
	var s webSession
	if json.Unmarshal(data, &s) != nil || !validCredential(s.credential) {
		return webSession{}, errInvalidSession
	}
	if len(s.Cookies) == 0 {
		return webSession{}, errors.New("saved session predates browser sign-in; run make login on the computer")
	}
	if !validFamily(s.Cookies) {
		return webSession{}, errInvalidSession
	}
	return s, nil
}

func writeSessionFile(path string, s webSession) (result error) {
	if !validCredential(s.credential) || !validFamily(s.Cookies) {
		return errors.New("refusing to save an invalid web session")
	}
	dir := filepath.Dir(path)
	if err := privateDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("cannot encode credential")
	}
	defer clear(data)
	f, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return errors.New("cannot create private credential file")
	}
	defer func() {
		if err := removeCredentialTemp(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, errors.New("cannot remove temporary credential file"))
		}
	}()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return errors.New("cannot write private credential file")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("cannot replace private credential file")
	}
	d, err := os.Open(dir)
	if err == nil {
		err = errors.Join(d.Sync(), d.Close())
	}
	if err != nil {
		return errors.New("cannot persist private credential file")
	}
	return nil
}

func lockSession(ctx context.Context, path string) (unlock func(), err error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	dir := filepath.Dir(path)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	f, err := openLockFile(filepath.Join(dir, "session.lock"), "session lock")
	if err != nil {
		return nil, err
	}
	retry := time.Tick(50 * time.Millisecond)
	for {
		if cause := context.Cause(ctx); cause != nil {
			f.Close()
			return nil, cause
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, errors.New("cannot lock saved session")
		}
		select {
		case <-ctx.Done():
		case <-retry:
		}
	}
}

func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	defer clear(data)
	var claims struct {
		Expires *int64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(data, &claims) != nil || claims.Expires == nil || *claims.Expires < 0 || *claims.Expires > maxTokenExpiry {
		return time.Time{}, false
	}
	return time.Unix(*claims.Expires, 0).UTC(), true
}

func writeAuthStatus(path string, out io.Writer) error {
	s, err := readSessionFile(path)
	if err != nil {
		return err
	}
	expiry := "unknown"
	if expires, ok := tokenExpiry(s.AccessToken); ok {
		expiry = expires.Format(time.RFC3339)
		if !time.Now().Before(expires) {
			expiry += " (expired)"
		}
	}
	_, err = fmt.Fprintln(out, "Saved token expiry (unverified):", expiry)
	return err
}
