package voice

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sessionServer(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	client := signalingClient()
	transport := client.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(certificate)
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func tokenExpiring(after time.Duration) string {
	return syntheticJWT(fmt.Sprintf(`{"exp":%d,"email":"private-marker@example.invalid"}`, time.Now().Add(after).Unix()))
}

func sessionResponse(w http.ResponseWriter, token string, setCookies ...string) {
	for _, c := range setCookies {
		w.Header().Add("Set-Cookie", c)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"user":{"email":"private-marker@example.invalid"},"expires":"2100-01-01T00:00:00Z","accessToken":%q}`, token)
}

func savedSession(t *testing.T, s webSession) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app", "session.json")
	if err := writeSessionFile(path, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, data
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("saved session changed", err)
	}
}

func assertUnlocked(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	unlock, err := lockSession(ctx, path)
	if err != nil {
		t.Fatal("session lock was not released", err)
	}
	unlock()
}

type lockWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *lockWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func waitingContext(parent context.Context) (*lockWaitContext, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	return &lockWaitContext{Context: ctx, waiting: make(chan struct{})}, cancel
}

func TestRenewalThreshold(t *testing.T) {
	const exp = 4102444800
	expires := time.Unix(exp, 0)
	token := syntheticJWT(fmt.Sprintf(`{"exp":%d}`, exp))
	for _, tc := range []struct {
		name  string
		now   time.Time
		renew bool
	}{
		{"one nanosecond beyond a day", expires.Add(-renewWithin - time.Nanosecond), false},
		{"exactly a day", expires.Add(-renewWithin), true},
		{"one nanosecond inside a day", expires.Add(-renewWithin + time.Nanosecond), true},
		{"expired", expires.Add(time.Second), true},
		{"a week left", expires.Add(-7 * renewWithin), false},
	} {
		if needsRenewal(token, tc.now) != tc.renew {
			t.Errorf("%s: renewal decision wrong", tc.name)
		}
	}
	now := expires.Add(-7 * renewWithin)
	for _, unknown := range []string{
		"opaque-token", syntheticJWT(`{}`), syntheticJWT(`{"exp":null}`), syntheticJWT(`{"exp":"4102444800"}`),
		syntheticJWT(`{"exp":4102444800.0}`), syntheticJWT(`{"exp":4102444800,"exp":4102444800}`),
		syntheticJWT(`{"exp":253402300800}`), "a.%%%.c", syntheticJWT(`{"exp":4102444800`),
	} {
		if !needsRenewal(unknown, now) {
			t.Errorf("unknown expiry %q skipped renewal", unknown)
		}
	}
}

func TestRotateFamily(t *testing.T) {
	now := time.Now()
	base := []sessionCookie{{Name: sessionCookieName, Value: "old-base"}}
	chunks := []sessionCookie{{Name: sessionCookieName + ".0", Value: "old-0"}, {Name: sessionCookieName + ".1", Value: "old-1"}, {Name: sessionCookieName + ".2", Value: "old-2"}}
	family := func(values ...string) []sessionCookie {
		if len(values) == 1 {
			return []sessionCookie{{Name: sessionCookieName, Value: values[0]}}
		}
		result := make([]sessionCookie, len(values))
		for i, v := range values {
			result[i] = sessionCookie{Name: fmt.Sprintf("%s.%d", sessionCookieName, i), Value: v}
		}
		return result
	}
	name := func(suffix string) string { return sessionCookieName + suffix }
	for _, tc := range []struct {
		name   string
		old    []sessionCookie
		header []string
		want   []sessionCookie
		err    error
	}{
		{"no family cookie keeps old", chunks, []string{"cf_clearance=edge; Path=/; HttpOnly", "__Secure-next-auth.callback-url=https%3A%2F%2Fchatgpt.com; Path=/"}, chunks, nil},
		{"base rotates", base, []string{name("=new-base; Path=/; Secure; HttpOnly")}, family("new-base"), nil},
		{"base becomes chunks", base, []string{name(".0=new-0; Path=/"), name(".1=new-1; Path=/"), name("=; Max-Age=0; Path=/")}, family("new-0", "new-1"), nil},
		{"chunks become base", chunks, []string{name("=new-base; Path=/"), name(".0=; Max-Age=0"), name(".1=; Max-Age=0"), name(".2=; Max-Age=0")}, family("new-base"), nil},
		{"chunks shrink", chunks, []string{name(".0=new-0"), name(".1=new-1"), name(".2=gone; Expires=Thu, 01 Jan 1970 00:00:00 GMT")}, family("new-0", "new-1"), nil},
		{"unmentioned old chunk is dropped", chunks, []string{name(".0=new-0"), name(".1=new-1")}, family("new-0", "new-1"), nil},
		{"last header wins", base, []string{name("=; Max-Age=0"), name("=again")}, family("again"), nil},
		{"positive max-age overrides past expires", base, []string{name("=kept; Max-Age=60; Expires=Thu, 01 Jan 1970 00:00:00 GMT")}, family("kept"), nil},
		{"deletions only sign out", chunks, []string{name(".0=; Max-Age=0"), name(".1=; Max-Age=0"), name(".2=; Max-Age=0")}, nil, errSignedOut},
		{"gap", base, []string{name(".0=new-0"), name(".2=new-2")}, nil, errInvalidSessionCookie},
		{"base mixed with chunks", base, []string{name("=new-base"), name(".0=new-0")}, nil, errInvalidSessionCookie},
		{"chunk out of range", base, []string{name(".16=new")}, nil, errInvalidSessionCookie},
		{"non-canonical chunk", base, []string{name(".01=new")}, nil, errInvalidSessionCookie},
		{"named chunk", base, []string{name(".x=new")}, nil, errInvalidSessionCookie},
		{"space in value", base, []string{name("=bad value")}, nil, errInvalidSessionCookie},
		{"comma in value", base, []string{name("=bad,value")}, nil, errInvalidSessionCookie},
		{"control in value", base, []string{name("=bad\x7fvalue")}, nil, errInvalidSessionCookie},
		{"oversized value", base, []string{name("=" + strings.Repeat("v", maxCookieValueBytes+1))}, nil, errInvalidSessionCookie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rotateFamily(tc.old, tc.header, now)
			if err != tc.err || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v", got, err)
			}
		})
	}
}

func TestSessionForCallRenewsOnlyWhenNeeded(t *testing.T) {
	var requests atomic.Int32
	var lifetime atomic.Int64
	lifetime.Store(int64(time.Hour))
	var headers sync.Map
	client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		headers.Store(n, r.Header.Clone())
		if r.Method != http.MethodGet || r.Host != "chatgpt.com" || r.URL.Path != "/api/auth/session" || r.URL.RawQuery != "" {
			t.Error("renewal reached the wrong endpoint")
		}
		sessionResponse(w, tokenExpiring(time.Duration(lifetime.Load())), fmt.Sprintf("%s=rotated-%d; Path=/; Secure; HttpOnly", sessionCookieName, n), "cf_clearance=edge-private-marker; Path=/")
	})
	path, _ := savedSession(t, webSession{AccessToken: tokenExpiring(2 * renewWithin), DeviceID: "offline-device", Cookies: []sessionCookie{{Name: sessionCookieName, Value: "initial"}}})
	var logs bytes.Buffer
	report := log.New(&logs, "", 0)
	call := func(wantRequests int32) credential {
		t.Helper()
		auth, err := sessionForCall(t.Context(), path, client, report)
		if err != nil || !validCredential(auth) || requests.Load() != wantRequests {
			t.Fatalf("call made %d requests, want %d: %v", requests.Load(), wantRequests, err)
		}
		saved, err := readSessionFile(path)
		if err != nil || saved.credential != auth {
			t.Fatal("call did not use the token on disk", err)
		}
		return auth
	}
	call(0)
	stale, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale.AccessToken = tokenExpiring(renewWithin)
	if err := writeSessionFile(path, stale); err != nil {
		t.Fatal(err)
	}
	first := call(1)
	if first.DeviceID != "offline-device" {
		t.Fatal("renewal changed the device identity")
	}
	call(2)
	lifetime.Store(int64(2 * renewWithin))
	call(3)
	call(3)
	saved, err := readSessionFile(path)
	if err != nil || !slices.Equal(saved.Cookies, []sessionCookie{{Name: sessionCookieName, Value: "rotated-3"}}) {
		t.Fatal("rotated cookie was not persisted alone", saved.Cookies)
	}
	for n, wantCookie := range map[int32]string{1: "initial", 2: "rotated-1", 3: "rotated-2"} {
		value, _ := headers.Load(n)
		header := value.(http.Header)
		if got := header.Values("Cookie"); len(got) != 1 || got[0] != sessionCookieName+"="+wantCookie {
			t.Fatalf("request %d sent cookies %q", n, got)
		}
		if header.Get("Authorization") != "" {
			t.Fatal("renewal sent the stale access token")
		}
	}
	if strings.Count(logs.String(), "Renewing ChatGPT web session before the call.\n") != 3 || strings.Contains(logs.String(), "private-marker") || strings.Contains(logs.String(), "eyJ") {
		t.Fatal("renewal log was missing or disclosed private data", logs.String())
	}
}

func TestRenewalResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"signed out", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{}`) }, "ended the saved web session"},
		{"empty token", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"accessToken":""}`) }, "ended the saved web session"},
		{"malformed", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"accessToken":"private-marker`) }, "invalid response"},
		{"duplicate token", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"accessToken":"private-marker-a","accessToken":"private-marker-b"}`)
		}, "invalid response"},
		{"non-string token", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"accessToken":42}`) }, "invalid response"},
		{"API key", func(w http.ResponseWriter, _ *http.Request) { sessionResponse(w, "sk-private-marker") }, "invalid access token"},
		{"expired token", func(w http.ResponseWriter, _ *http.Request) {
			sessionResponse(w, syntheticJWT(`{"exp":1,"email":"private-marker"}`))
		}, "expired access token"},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "private-marker")
		}, "rejected the saved web session (HTTP 401)"},
		{"forbidden", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"detail":"private-marker"}`)
		}, "rejected the saved web session (HTTP 403)"},
		{"edge challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
		}, "edge challenged session renewal from the Station's network (HTTP 403)"},
		{"HTML", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html>private-marker</html>")
		}, "edge challenged session renewal from the Station's network (HTTP 200)"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://chatgpt.com/auth/login?next=private-marker", http.StatusFound)
		}, "unexpected redirect (HTTP 302)"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "HTTP 502"},
		{"oversized", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"accessToken":"`+strings.Repeat("a", maxSessionResponseBytes)+`"}`)
		}, "exceeds 256 KiB"},
		{"oversized headers", func(w http.ResponseWriter, _ *http.Request) {
			for i := range 80 {
				w.Header().Add("X-Private-Marker", fmt.Sprint(i, strings.Repeat("h", 4000)))
			}
		}, "HTTPS request failed"},
		{"truncated body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"accessToken":"private-marker`)
		}, "cannot read ChatGPT session renewal response"},
		{"signed out by cookies", func(w http.ResponseWriter, _ *http.Request) {
			sessionResponse(w, tokenExpiring(2*renewWithin), sessionCookieName+"=; Max-Age=0; Path=/")
		}, "ended the saved web session"},
		{"invalid rotated cookie", func(w http.ResponseWriter, _ *http.Request) {
			sessionResponse(w, tokenExpiring(2*renewWithin), sessionCookieName+"=private marker; Path=/")
		}, "invalid session cookie"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				tc.handler(w, r)
			})
			path, before := savedSession(t, syntheticSession(tokenExpiring(time.Hour)))
			var logs bytes.Buffer
			_, err := sessionForCall(t.Context(), path, client, log.New(&logs, "", 0))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error()+logs.String(), "private") || strings.Contains(err.Error()+logs.String(), "eyJ") {
				t.Fatal("renewal failure disclosed private data", err)
			}
			if requests.Load() != 1 {
				t.Fatal("renewal retried or followed a redirect", requests.Load())
			}
			assertFileBytes(t, path, before)
			assertUnlocked(t, path)
		})
	}
}

func TestRenewalRejectsUntrustedTLS(t *testing.T) {
	var requests atomic.Int32
	trusted := sessionServer(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	untrusted := signalingClient()
	defer untrusted.CloseIdleConnections()
	transport := untrusted.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.DialContext = trusted.Transport.(*http.Transport).DialContext
	path, before := savedSession(t, syntheticSession("synthetic-private-token"))
	_, err := sessionForCall(t.Context(), path, untrusted, log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "HTTPS request failed") || requests.Load() != 0 {
		t.Fatal("untrusted TLS reached the renewal handler", err)
	}
	assertFileBytes(t, path, before)
}

func TestRenewalCancellation(t *testing.T) {
	t.Run("lock wait", func(t *testing.T) {
		var requests atomic.Int32
		client := sessionServer(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
		path, before := savedSession(t, syntheticSession("synthetic-private-token"))
		unlock, err := lockSession(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := waitingContext(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := sessionForCall(ctx, path, client, log.New(io.Discard, "", 0))
			done <- err
		}()
		<-ctx.waiting
		cancel(errAlicePriority)
		if err := <-done; err != errAlicePriority {
			t.Fatal("lock wait did not return the exact Alice cause", err)
		}
		unlock()
		if requests.Load() != 0 {
			t.Fatal("canceled lock wait still renewed")
		}
		assertFileBytes(t, path, before)
		assertUnlocked(t, path)
	})
	t.Run("HTTP request", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		path, before := savedSession(t, syntheticSession("synthetic-private-token"))
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := sessionForCall(ctx, path, client, log.New(io.Discard, "", 0))
			done <- err
		}()
		<-started
		cancel(errAlicePriority)
		if err := <-done; err != errAlicePriority {
			t.Fatal("HTTP renewal did not return the exact Alice cause", err)
		}
		assertFileBytes(t, path, before)
		assertUnlocked(t, path)
	})
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		path, before := savedSession(t, syntheticSession("synthetic-private-token"))
		ctx, cancel := context.WithTimeoutCause(t.Context(), 100*time.Millisecond, errRenewTimeout)
		defer cancel()
		if _, err := sessionForCall(ctx, path, client, log.New(io.Discard, "", 0)); err != errRenewTimeout {
			t.Fatal("renewal timeout lost its cause", err)
		}
		assertFileBytes(t, path, before)
	})
	t.Run("after decoded response", func(t *testing.T) {
		renewed := tokenExpiring(2 * renewWithin)
		client := sessionServer(t, func(w http.ResponseWriter, _ *http.Request) {
			sessionResponse(w, renewed, sessionCookieName+".0=rotated-0; Path=/", sessionCookieName+".1=rotated-1; Path=/", sessionCookieName+"=; Max-Age=0; Path=/")
		})
		ctx, cancel := context.WithCancelCause(t.Context())
		base := client.Transport
		client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := base.RoundTrip(r)
			if err == nil {
				response.Body = eofHook{response.Body, func() { cancel(errAlicePriority) }}
			}
			return response, err
		})
		path, _ := savedSession(t, syntheticSession("synthetic-private-token"))
		if _, err := sessionForCall(ctx, path, client, log.New(io.Discard, "", 0)); err != errAlicePriority {
			t.Fatal("post-response cancellation lost its exact cause", err)
		}
		saved, err := readSessionFile(path)
		want := []sessionCookie{{Name: sessionCookieName + ".0", Value: "rotated-0"}, {Name: sessionCookieName + ".1", Value: "rotated-1"}}
		if err != nil || saved.AccessToken != renewed || !slices.Equal(saved.Cookies, want) {
			t.Fatal("completed rotation was not persisted before reporting cancellation", err)
		}
		assertUnlocked(t, path)
	})
}

type eofHook struct {
	io.ReadCloser
	atEOF func()
}

func (r eofHook) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		r.atEOF()
	}
	return n, err
}

func TestImportBrowserSession(t *testing.T) {
	transfer, err := os.ReadFile(filepath.Join("testdata", "browser-sign-in.json"))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	renewed := tokenExpiring(2 * renewWithin)
	client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Values("Cookie"); len(got) != 1 || got[0] != sessionCookieName+".0=synthetic-golden-chunk-0; "+sessionCookieName+".1=synthetic-golden-chunk-1" {
			t.Errorf("import sent cookies %q", got)
		}
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		sessionResponse(w, renewed)
	})
	dir := filepath.Join(t.TempDir(), "app")
	path := filepath.Join(dir, "session.json")
	if err := importBrowserSession(t.Context(), path, client, bytes.NewReader(transfer)); err != nil {
		t.Fatal(err)
	}
	first, err := readSessionFile(path)
	if err != nil || first.AccessToken != renewed || !validDeviceID(first.DeviceID) || len(first.Cookies) != 2 {
		t.Fatal("first sign-in did not save a renewed session", err)
	}
	for name, mode := range map[string]os.FileMode{dir: 0700, path: 0600} {
		if info, err := os.Stat(name); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s %v", name, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"accessToken":"synthetic-private-token","deviceId":"legacy-device"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := importBrowserSession(t.Context(), path, client, bytes.NewReader(transfer)); err != nil {
		t.Fatal(err)
	}
	upgraded, err := readSessionFile(path)
	if err != nil || upgraded.DeviceID != "legacy-device" || upgraded.AccessToken != renewed {
		t.Fatal("sign-in over a token-only session lost its device identity", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusForbidden)
	if err := importBrowserSession(t.Context(), path, client, bytes.NewReader(transfer)); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("rejected sign-in was accepted", err)
	}
	assertFileBytes(t, path, before)
	accepted := requests.Load()
	for name, input := range map[string]string{
		"access token":     `{"accessToken":"synthetic-private-token","cookies":[{"name":"` + sessionCookieName + `","value":"v"}]}`,
		"extra attribute":  `{"cookies":[{"name":"` + sessionCookieName + `","value":"v","domain":"chatgpt.com"}]}`,
		"no cookies":       `{"cookies":[]}`,
		"foreign cookie":   `{"cookies":[{"name":"cf_clearance","value":"v"}]}`,
		"gap":              `{"cookies":[{"name":"` + sessionCookieName + `.1","value":"v"}]}`,
		"duplicate":        `{"cookies":[{"name":"` + sessionCookieName + `","value":"v"},{"name":"` + sessionCookieName + `","value":"v"}]}`,
		"duplicate member": `{"cookies":[],"cookies":[{"name":"` + sessionCookieName + `","value":"v"}]}`,
		"invalid octet":    `{"cookies":[{"name":"` + sessionCookieName + `","value":"a;b"}]}`,
		"oversized":        strings.Repeat(" ", maxCredentialBytes+1),
	} {
		if err := importBrowserSession(t.Context(), path, client, strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatalf("%s: invalid transfer accepted or disclosed: %v", name, err)
		}
	}
	if requests.Load() != accepted {
		t.Fatal("invalid transfer reached ChatGPT")
	}
	assertFileBytes(t, path, before)
	assertUnlocked(t, path)
}

func TestImportAndRenewalCannotLoseLogin(t *testing.T) {
	transfer, err := os.ReadFile(filepath.Join("testdata", "browser-sign-in.json"))
	if err != nil {
		t.Fatal(err)
	}
	loginToken := tokenExpiring(2 * renewWithin)
	renewalToken := tokenExpiring(2 * renewWithin)
	newLogin := func(r *http.Request) bool { return strings.Contains(r.Header.Get("Cookie"), "synthetic-golden-chunk") }
	assertLogin := func(t *testing.T, path string) {
		t.Helper()
		saved, err := readSessionFile(path)
		if err != nil || saved.AccessToken != loginToken || saved.Cookies[0].Value != "synthetic-golden-chunk-0" || saved.DeviceID != "offline-device" {
			t.Fatal("the new sign-in was lost", err)
		}
	}
	t.Run("renewal first", func(t *testing.T) {
		renewalStarted := make(chan struct{})
		releaseRenewal := make(chan struct{})
		var oldRequests, loginRequests atomic.Int32
		client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
			if newLogin(r) {
				loginRequests.Add(1)
				sessionResponse(w, loginToken)
				return
			}
			oldRequests.Add(1)
			close(renewalStarted)
			<-releaseRenewal
			sessionResponse(w, renewalToken, sessionCookieName+"=renewed-old; Path=/")
		})
		path, _ := savedSession(t, syntheticSession(tokenExpiring(time.Hour)))
		renewal := make(chan error, 1)
		go func() {
			_, err := sessionForCall(t.Context(), path, client, log.New(io.Discard, "", 0))
			renewal <- err
		}()
		<-renewalStarted
		ctx, cancel := waitingContext(t.Context())
		defer cancel(nil)
		login := make(chan error, 1)
		go func() { login <- importBrowserSession(ctx, path, client, bytes.NewReader(transfer)) }()
		<-ctx.waiting
		close(releaseRenewal)
		if err := <-renewal; err != nil {
			t.Fatal(err)
		}
		if err := <-login; err != nil {
			t.Fatal(err)
		}
		if oldRequests.Load() != 1 || loginRequests.Load() != 1 {
			t.Fatal("unexpected renewal requests")
		}
		assertLogin(t, path)
	})
	t.Run("login first", func(t *testing.T) {
		loginStarted := make(chan struct{})
		releaseLogin := make(chan struct{})
		var oldRequests atomic.Int32
		client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
			if !newLogin(r) {
				oldRequests.Add(1)
				sessionResponse(w, renewalToken, sessionCookieName+"=renewed-old; Path=/")
				return
			}
			close(loginStarted)
			<-releaseLogin
			sessionResponse(w, loginToken)
		})
		path, _ := savedSession(t, syntheticSession(tokenExpiring(time.Hour)))
		login := make(chan error, 1)
		go func() { login <- importBrowserSession(t.Context(), path, client, bytes.NewReader(transfer)) }()
		<-loginStarted
		ctx, cancel := waitingContext(t.Context())
		defer cancel(nil)
		type result struct {
			auth credential
			err  error
		}
		renewal := make(chan result, 1)
		go func() {
			auth, err := sessionForCall(ctx, path, client, log.New(io.Discard, "", 0))
			renewal <- result{auth, err}
		}()
		<-ctx.waiting
		close(releaseLogin)
		if err := <-login; err != nil {
			t.Fatal(err)
		}
		got := <-renewal
		if got.err != nil || got.auth.AccessToken != loginToken || oldRequests.Load() != 0 {
			t.Fatal("call renewed stale cookies instead of using the new sign-in", got.err, oldRequests.Load())
		}
		assertLogin(t, path)
	})
}

func TestRunCallRenewsBeforeMedia(t *testing.T) {
	original := callClient
	t.Cleanup(func() { callClient = original })
	t.Run("Alice during renewal", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		client := sessionServer(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		callClient = func() *http.Client { return client }
		path, before := savedSession(t, syntheticSession(tokenExpiring(time.Hour)))
		e := newStationEpoch(t.Context(), 1)
		defer e.revoke(context.Canceled)
		done := make(chan error, 1)
		go func() { done <- runCall(e, path, newStationCapture(e), sessionSettings{}) }()
		<-started
		e.revoke(errAlicePriority)
		if err := <-done; err != errAlicePriority {
			t.Fatal("Alice during renewal was not returned unmarked", err)
		}
		assertFileBytes(t, path, before)
	})
	t.Run("rejected renewal", func(t *testing.T) {
		client := sessionServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		callClient = func() *http.Client { return client }
		path, _ := savedSession(t, syntheticSession(tokenExpiring(time.Hour)))
		var logs bytes.Buffer
		e := newStationEpoch(t.Context(), 1)
		defer e.revoke(context.Canceled)
		e.report = log.New(&logs, "", 0)
		err := runCall(e, path, newStationCapture(e), sessionSettings{})
		if _, marked := errors.AsType[voiceCallError](err); !marked || !strings.Contains(err.Error(), "make login") {
			t.Fatal("rejected renewal was not a marked call failure with sign-in guidance", err)
		}
		if logs.String() != "Renewing ChatGPT web session before the call.\n" {
			t.Fatal("renewal failure continued into media setup", logs.String())
		}
	})
}
