package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFixedOriginSignalingAndFailures(t *testing.T) {
	const private = "synthetic-private-token"
	auth := credential{AccessToken: private, DeviceID: "offline-device"}
	session, err := encodeSession(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"ok", 200, "v=0\r\ns=test\r\n", ""},
		{"created", 201, "v=0\ns=test\n", ""},
		{"unauthorized", 401, private, "auth import"},
		{"forbidden", 403, private, "entitlement"},
		{"quota", 429, private, "429"},
		{"invalid SDP", 200, private, "SDP answer"},
		{"large answer", 200, "v=0\r\n" + strings.Repeat("x", maxAnswerBytes), "1 MiB"},
		{"redirect", 307, private, "307"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.String() != signalingURL || req.Method != "POST" {
					t.Fatal("wrong signaling destination")
				}
				if req.Header.Get("Authorization") != "Bearer "+private || req.Header.Get("Origin") != "https://chatgpt.com" || req.Header.Get("Oai-Device-Id") != auth.DeviceID {
					t.Fatal("missing web authorization headers")
				}
				if req.Header.Get("X-Openai-Web-Frontend") != "" {
					t.Fatal("unexpected frontend header")
				}
				_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				form := multipart.NewReader(req.Body, params["boundary"])
				fields := map[string]string{}
				for {
					part, err := form.NextPart()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(part)
					if err != nil {
						t.Fatal(err)
					}
					fields[part.FormName()] = string(data)
				}
				if len(fields) != 2 || fields["sdp"] != "v=0\r\ns=offer\r\n" || fields["session"] != string(session) {
					t.Fatal("incorrect multipart fields")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://example.invalid/steal"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			answer, err := exchangeOffer(t.Context(), client, auth, "v=0\r\ns=offer\r\n", session)
			if tc.want == "" {
				if err != nil || !strings.HasPrefix(answer, "v=0") {
					t.Fatal("valid SDP failed", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), private) {
				t.Fatalf("unsafe or wrong error: %v", err)
			}
			if requests != 1 {
				t.Fatal("redirect or retry performed")
			}
		})
	}
}

func TestSignalingCancellationAndTLSValidation(t *testing.T) {
	auth := credential{AccessToken: "offline-fixture", DeviceID: "offline-device"}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	done := make(chan error, 1)
	go func() { _, err := exchangeOffer(ctx, client, auth, "v=0\r\n", []byte(`{}`)); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("signaling did not cancel")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request passed untrusted TLS") }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	safe := signalingClient()
	defer safe.CloseIdleConnections()
	transport := safe.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	if _, err := exchangeOffer(t.Context(), safe, auth, "v=0\r\n", []byte(`{}`)); err == nil {
		t.Fatal("untrusted TLS was accepted")
	}
}

func localPeer(t *testing.T, ctx context.Context) *peer {
	t.Helper()
	var settings webrtc.SettingEngine
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	p, err := newPeer(ctx, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func connectLocalPeers(t *testing.T, ctx context.Context, a, b *peer) {
	t.Helper()
	offer, err := a.offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer, "a=sendrecv") || !strings.Contains(offer, "opus/48000/2") || strings.Contains(offer, "PCMU/") || !strings.Contains(offer, "a=max-message-size:262144") {
		t.Fatal("incorrect Opus-only offer")
	}
	if a.dc.Label() != "" || a.dc.ID() == nil || *a.dc.ID() != 0 || !a.dc.Ordered() || !a.dc.Negotiated() || a.dc.MaxRetransmits() != nil || a.dc.MaxPacketLifeTime() != nil {
		t.Fatal("incorrect negotiated control channel")
	}
	if err = b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		t.Fatal(err)
	}
	answer, err := b.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(b.pc)
	if err = b.pc.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("answer gathering timed out")
	}
	if err = a.accept(b.pc.LocalDescription().SDP); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*peer{a, b} {
		select {
		case <-p.ready:
		case <-ctx.Done():
			t.Fatal("local channel did not open")
		}
	}
}

func TestCancellationDuringICEGathering(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	var settings webrtc.SettingEngine
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	p, err := newPeer(t.Context(), settings, []webrtc.ICEServer{{URLs: []string{"stun:" + blackhole.LocalAddr().String()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if offer, err := p.offer(ctx); err == nil || offer != "" {
		t.Fatal("unfinished ICE gathering returned a successful offer")
	}
}

func receiveMessage(t *testing.T, ctx context.Context, p *peer) []byte {
	t.Helper()
	select {
	case data := <-p.messages:
		p.messageBytes.Add(-int64(len(data)))
		return data
	case <-ctx.Done():
		t.Fatal("no DataChannel message")
		return nil
	}
}

func TestPeerLimitsAndLateCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		size, count int
	}{
		{"count", 1, maxQueuedEvents + 1}, {"bytes", maxMessageBytes, 5}, {"message", maxMessageBytes + 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := localPeer(t, t.Context())
			for range tc.count {
				p.enqueue(make([]byte, tc.size))
			}
			if p.ctx.Err() == nil {
				t.Fatal("queue limit did not cancel peer")
			}
			if len(p.messages) > maxQueuedEvents || p.messageBytes.Load() > maxQueuedEventBytes {
				t.Fatal("mailbox exceeded budget")
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			var callbacks sync.WaitGroup
			for range 20 {
				callbacks.Go(func() { p.enqueue([]byte(`{"type":"state_update"}`)) })
			}
			callbacks.Wait()
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
		})
	}
	p := localPeer(t, t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if offer, err := p.offer(ctx); err == nil || offer != "" {
		t.Fatal("canceled gather returned an offer")
	}
}

func TestRealDataChannelProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	connectLocalPeers(t, ctx, a, b)
	response := testEnvelope(t, `{"type":"state_update","payload":{"new_state":"listening"}}`)
	if err := b.dc.SendText(string(response)); err != nil {
		t.Fatal(err)
	}
	ev, err := decodeEvent(receiveMessage(t, ctx, a))
	if err != nil || ev != (event{}) {
		t.Fatal("server state affected the Station runtime", err)
	}
	if err := a.dc.SendText(string(testEnvelope(t, `{"type":"tool_update","payload":{"update_type":"hang_up"}}`))); err != nil {
		t.Fatal(err)
	}
	ev, err = decodeEvent(receiveMessage(t, ctx, b))
	if err != nil || !ev.ended {
		t.Fatal("hangup not received", err)
	}
}

func TestNativePriorityCancelsSignalingAndRejectsLateAnswer(t *testing.T) {
	g := newStationGate()
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	e := g.begin(t.Context())
	defer g.finish(e, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		close(canceled)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("v=0\r\ns=late\r\n")), Header: make(http.Header), Request: req}, nil
	})}
	done := make(chan struct{})
	go func() {
		_, _ = exchangeOffer(e.ctx, client, credential{AccessToken: "synthetic-token", DeviceID: "test-device"}, "v=0\r\n", []byte(`{}`))
		close(done)
	}()
	<-started
	g.observe(nativeEvent{stateChanged: true, state: aliceListening})
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Alice revocation waited for HTTP")
	}
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	close(release)
	<-done
	if e.allowed() || g.activate(e) {
		t.Fatal("late HTTP answer revived revoked epoch")
	}
}
