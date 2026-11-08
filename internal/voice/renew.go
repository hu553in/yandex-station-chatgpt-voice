package voice

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"
)

const sessionURL = "https://chatgpt.com/api/auth/session"
const renewWithin = 86400 * time.Second
const renewTimeout = 10 * time.Second
const maxSessionResponseBytes = 256 << 10

var errRenewTimeout = errors.New("ChatGPT session renewal timed out; check the Station's network")
var errSignedOut = errors.New("ChatGPT ended the saved web session; run make login on the computer")
var errInvalidSessionCookie = errors.New("ChatGPT session renewal returned an invalid session cookie; run make login on the computer")

func needsRenewal(token string, now time.Time) bool {
	expires, ok := tokenExpiry(token)
	return !ok || expires.Sub(now) <= renewWithin
}

func renew(ctx context.Context, client *http.Client, s webSession) (webSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return webSession{}, errors.New("cannot prepare session renewal request")
	}
	pairs := make([]string, len(s.Cookies))
	for i, c := range s.Cookies {
		pairs[i] = c.Name + "=" + c.Value
	}
	req.Header.Set("Cookie", strings.Join(pairs, "; "))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "yandex-station-chatgpt-voice/0.1")
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return webSession{}, context.Cause(ctx)
		}
		return webSession{}, errors.New("ChatGPT session renewal HTTPS request failed; check the Station's network, TLS, and service availability")
	}
	defer response.Body.Close()
	status := response.StatusCode
	switch {
	case status >= 300 && status < 400:
		return webSession{}, fmt.Errorf("ChatGPT session renewal returned an unexpected redirect (HTTP %d)", status)
	case response.Header.Get("Cf-Mitigated") == "challenge" || strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/html"):
		return webSession{}, fmt.Errorf("ChatGPT's edge challenged session renewal from the Station's network (HTTP %d)", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return webSession{}, fmt.Errorf("ChatGPT rejected the saved web session (HTTP %d); run make login on the computer", status)
	case status != http.StatusOK:
		return webSession{}, fmt.Errorf("ChatGPT session renewal returned HTTP %d", status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSessionResponseBytes+1))
	defer clear(data)
	if err != nil {
		if ctx.Err() != nil {
			return webSession{}, context.Cause(ctx)
		}
		return webSession{}, errors.New("cannot read ChatGPT session renewal response")
	}
	if len(data) > maxSessionResponseBytes {
		return webSession{}, errors.New("ChatGPT session renewal response exceeds 256 KiB")
	}
	var body struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(data, &body) != nil {
		return webSession{}, errors.New("ChatGPT session renewal returned an invalid response")
	}
	if body.AccessToken == "" {
		return webSession{}, errSignedOut
	}
	renewed := webSession{AccessToken: body.AccessToken, DeviceID: s.DeviceID}
	if !validCredential(renewed.credential) {
		return webSession{}, errors.New("ChatGPT session renewal returned an invalid access token")
	}
	now := time.Now()
	if expires, ok := tokenExpiry(renewed.AccessToken); ok && !expires.After(now) {
		return webSession{}, errors.New("ChatGPT session renewal returned an expired access token; run make login on the computer")
	}
	renewed.Cookies, err = rotateFamily(s.Cookies, response.Header.Values("Set-Cookie"), now)
	if err != nil {
		return webSession{}, err
	}
	return renewed, nil
}

// rotateFamily replaces the whole session-token family whenever a response touches it,
// because NextAuth rewrites every chunk and expires the leftovers in one response.
func rotateFamily(old []sessionCookie, header []string, now time.Time) ([]sessionCookie, error) {
	live := map[string]string{}
	touched := false
	for _, line := range header {
		name, _, _ := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if suffix, chunk := strings.CutPrefix(name, sessionCookieName+"."); chunk {
			if n, err := strconv.Atoi(suffix); err != nil || n < 0 || n >= maxSessionCookies || strconv.Itoa(n) != suffix {
				return nil, errInvalidSessionCookie
			}
		} else if name != sessionCookieName {
			continue
		}
		cookie, err := http.ParseSetCookie(line)
		if err != nil {
			return nil, errInvalidSessionCookie
		}
		touched = true
		if cookie.Value == "" || cookie.MaxAge < 0 || cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !cookie.Expires.After(now) {
			delete(live, name)
			continue
		}
		if !validCookieValue(cookie.Value) {
			return nil, errInvalidSessionCookie
		}
		live[name] = cookie.Value
	}
	if !touched {
		return old, nil
	}
	if len(live) == 0 {
		return nil, errSignedOut
	}
	if value, ok := live[sessionCookieName]; ok && len(live) == 1 {
		return []sessionCookie{{Name: sessionCookieName, Value: value}}, nil
	}
	rotated := make([]sessionCookie, len(live))
	for i := range rotated {
		name := sessionCookieName + "." + strconv.Itoa(i)
		value, ok := live[name]
		if !ok {
			return nil, errInvalidSessionCookie
		}
		rotated[i] = sessionCookie{Name: name, Value: value}
	}
	return rotated, nil
}

func sessionForCall(ctx context.Context, path string, client *http.Client, report *log.Logger) (credential, error) {
	s, err := readSessionFile(path)
	if err != nil {
		return credential{}, err
	}
	if !needsRenewal(s.AccessToken, time.Now()) {
		return s.credential, nil
	}
	unlock, err := lockSession(ctx, path)
	if err != nil {
		return credential{}, err
	}
	defer unlock()
	if s, err = readSessionFile(path); err != nil {
		return credential{}, err
	}
	if !needsRenewal(s.AccessToken, time.Now()) {
		return s.credential, nil
	}
	report.Print("Renewing ChatGPT web session before the call.")
	renewed, err := renew(ctx, client, s)
	if err != nil {
		return credential{}, err
	}
	if err := writeSessionFile(path, renewed); err != nil {
		return credential{}, err
	}
	if ctx.Err() != nil {
		return credential{}, context.Cause(ctx)
	}
	return renewed.credential, nil
}

func importBrowserSession(ctx context.Context, path string, client *http.Client, in io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(in, maxCredentialBytes+1))
	defer clear(data)
	invalid := errors.New("invalid browser sign-in transfer; run make login again")
	if err != nil || len(data) > maxCredentialBytes {
		return invalid
	}
	var transfer struct {
		Cookies []sessionCookie `json:"cookies"`
	}
	if json.Unmarshal(data, &transfer, json.RejectUnknownMembers(true)) != nil || !validFamily(transfer.Cookies) {
		return invalid
	}
	unlock, err := lockSession(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	deviceID := uuid.New().String()
	if previous, err := readPrivateFile(path); err == nil {
		var old credential
		if json.Unmarshal(previous, &old) == nil && validDeviceID(old.DeviceID) {
			deviceID = old.DeviceID
		}
		clear(previous)
	}
	renewCtx, stop := context.WithTimeoutCause(ctx, renewTimeout, errRenewTimeout)
	defer stop()
	renewed, err := renew(renewCtx, client, webSession{DeviceID: deviceID, Cookies: transfer.Cookies})
	if err != nil {
		return err
	}
	return writeSessionFile(path, renewed)
}
