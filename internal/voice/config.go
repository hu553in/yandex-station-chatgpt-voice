package voice

import (
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"uuid"
)

const maxCredentialBytes = 1 << 20

var removeCredentialTemp = os.Remove

type credential struct {
	AccessToken string `json:"accessToken"`
	DeviceID    string `json:"deviceId"`
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
	if len(c.DeviceID) == 0 || len(c.DeviceID) > 128 {
		return false
	}
	for _, r := range c.DeviceID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func readSession(in io.Reader) (credential, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxCredentialBytes+1))
	if err != nil || len(data) > maxCredentialBytes {
		return credential{}, errors.New("cannot read session JSON or file exceeds 1 MiB")
	}
	defer clear(data)
	var c credential
	if json.Unmarshal(data, &c) != nil {
		return credential{}, errors.New("invalid session JSON; expected an accessToken field")
	}
	return c, nil
}

func importCredential(path string, c credential) (result error) {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	if c.DeviceID == "" {
		if old, err := loadCredential(path); err == nil {
			c.DeviceID = old.DeviceID
		} else {
			c.DeviceID = uuid.New().String()
		}
	}
	if !validCredential(c) {
		return errors.New("invalid web session credential; import your own ChatGPT web access token, not an API key")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create private credential directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential directory must be a real directory")
	}
	if os.Chmod(dir, 0700) != nil {
		return errors.New("cannot protect credential directory")
	}
	data, err := json.Marshal(c)
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
	return nil
}

func loadCredential(path string) (credential, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return credential{}, errors.New("no imported session; run yandex-station-chatgpt-voice auth import first")
	}
	if err != nil || !info.Mode().IsRegular() {
		return credential{}, errors.New("cannot read private session file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return credential{}, errors.New("session file must be private (chmod 600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return credential{}, errors.New("cannot open private session file")
	}
	defer f.Close()
	c, err := readSession(f)
	if err != nil {
		return credential{}, err
	}
	if !validCredential(c) {
		return credential{}, errors.New("invalid saved session; run yandex-station-chatgpt-voice auth import again")
	}
	return c, nil
}
