package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type answerReadFunc func([]byte) (int, error)

func (f answerReadFunc) Read(data []byte) (int, error) { return f(data) }

func TestFixedOriginSignalingAndFailures(t *testing.T) {
	const private = "synthetic-private-token"
	auth := credential{AccessToken: private, DeviceID: "offline-device"}
	session, err := encodeSession(time.Now(), sessionSettings{Voice: " voice\n$custom ", BackendModel: "-future/модель", BackendThinkingEffort: "disabled"})
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
		{"bad settings", 400, private, "400"},
		{"unprocessable", 422, private, "422"},
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
				var payload map[string]any
				if err := json.Unmarshal([]byte(fields["session"]), &payload); err != nil {
					t.Fatal(err)
				}
				if payload["voice"] != " voice\n$custom " || payload["backend_model"] != "-future/модель" || payload["backend_reasoning_effort"] != "instant" {
					t.Fatal("settings did not reach multipart")
				}
				if _, ok := payload["backend_thinking_effort"]; ok {
					t.Fatal("disabled effort reached multipart")
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
	p, err := newPeer(ctx, settings, nil, log.New(io.Discard, "", 0))
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
	answerLocalOffer(t, ctx, a, b, offer)
}

func answerLocalOffer(t *testing.T, ctx context.Context, a, b *peer, offer string) {
	t.Helper()
	var err error
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
	p, err := newPeer(t.Context(), settings, []webrtc.ICEServer{{URLs: []string{"stun:" + blackhole.LocalAddr().String()}}}, log.New(io.Discard, "", 0))
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

func TestCanceledOffer(t *testing.T) {
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
	if err := b.dc.SendText(string(testEnvelope(t, `{"type":"tool_update","payload":{"update_type":"hang_up"}}`))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.ctx.Done():
	case <-ctx.Done():
		t.Fatal("hangup after state update not received")
	}
	if got := a.outcome(context.Cause(a.ctx)); got != nil {
		t.Fatal("state update or hangup ended abnormally", got)
	}
	if b.ctx.Err() != nil {
		t.Fatal("receiving peer completion canceled the sending peer")
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
	if e.allowed() || g.activate(e) == nil {
		t.Fatal("late HTTP answer revived revoked epoch")
	}
}

func TestNativePacketLossRecoveryAndFailure(t *testing.T) {
	for _, recoverPackets := range []bool{true, false} {
		t.Run(map[bool]string{true: "recover", false: "fail"}[recoverPackets], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			router, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "192.0.2.0/24", LoggerFactory: &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}})
			if err != nil {
				t.Fatal(err)
			}
			var drop atomic.Bool
			router.AddChunkFilter(func(vnet.Chunk) bool { return !drop.Load() })
			var peers []*peer
			for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
				network, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{ip}})
				if err != nil {
					t.Fatal(err)
				}
				if err := router.AddNet(network); err != nil {
					t.Fatal(err)
				}
				var settings webrtc.SettingEngine
				settings.SetNet(network)
				settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
				settings.SetICETimeouts(200*time.Millisecond, 2*time.Second, 30*time.Millisecond)
				p, err := newPeer(ctx, settings, nil, log.New(io.Discard, "", 0))
				if err != nil {
					t.Fatal(err)
				}
				peers = append(peers, p)
			}
			if err := router.Start(); err != nil {
				t.Fatal(err)
			}
			defer router.Stop()
			for _, p := range peers {
				defer p.close()
			}
			a, b := peers[0], peers[1]
			connectLocalPeers(t, ctx, a, b)
			originalPC, originalDC := a.pc, a.dc
			waitState := func(p *peer, want webrtc.PeerConnectionState) {
				t.Helper()
				for p.pc.ConnectionState() != want {
					select {
					case <-ctx.Done():
						t.Fatalf("wanted %s, got %s, cause %v", want, p.pc.ConnectionState(), context.Cause(p.ctx))
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			drop.Store(true)
			for _, p := range peers {
				waitState(p, webrtc.PeerConnectionStateDisconnected)
				if p.ctx.Err() != nil {
					t.Fatal("Disconnected canceled production peer", context.Cause(p.ctx))
				}
			}
			if recoverPackets {
				drop.Store(false)
				for _, p := range peers {
					waitState(p, webrtc.PeerConnectionStateConnected)
				}
				for _, p := range peers {
					if p.ctx.Err() != nil {
						t.Fatal("recovered peer canceled")
					}
					if err := p.dc.SendText(`{"type":"future_event","data":"after loss"}`); err != nil {
						t.Fatal(err)
					}
				}
				for _, p := range peers {
					if err := p.dc.SendText(`{"type":"tool_update","update_type":"hangup"}`); err != nil {
						t.Fatal(err)
					}
				}
				for _, p := range peers {
					select {
					case <-p.ctx.Done():
					case <-ctx.Done():
						t.Fatal("hangup after recovery was not received")
					}
					if got := p.outcome(context.Cause(p.ctx)); got != nil {
						t.Fatal("recovered control stream ended abnormally", got)
					}
				}
				if a.pc != originalPC || a.dc != originalDC {
					t.Fatal("recovery replaced the peer or channel")
				}
				t.Log("Packets dropped; both production peers stayed live through Disconnected; restored Connected and exchanged DC data on the original peers.")
			} else {
				for _, p := range peers {
					waitState(p, webrtc.PeerConnectionStateFailed)
					select {
					case <-p.ctx.Done():
					case <-ctx.Done():
						t.Fatal("native Failed did not cancel")
					}
					if err := p.outcome(context.Cause(p.ctx)); err == nil || !strings.Contains(err.Error(), "connection lost") {
						t.Fatal("failed loss was not reported", err)
					}
				}
				t.Log("Packets remained blocked; native Failed canceled both production peers with a safe connection-loss error.")
			}
		})
	}
}

func TestReceivedErrorSurvivesRemoteClose(t *testing.T) {
	for _, terminal := range []string{`{"type":"error","message":"private-marker"}`, `{"type":"usage_update","audio_s":0}`} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		a, b := localPeer(t, ctx), localPeer(t, ctx)
		connectLocalPeers(t, ctx, a, b)
		if err := b.dc.SendText(terminal); err != nil {
			t.Fatal(err)
		}
		select {
		case <-a.ctx.Done():
		case <-ctx.Done():
			t.Fatal("received terminal event did not cancel")
		}
		if err := b.close(); err != nil {
			t.Fatal(err)
		}
		a.cancel(errRemoteClosed)
		setupCtx, stopSetup := context.WithCancelCause(ctx)
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Request: req, Body: io.NopCloser(answerReadFunc(func([]byte) (int, error) {
				stopSetup(errRemoteClosed)
				return 0, io.ErrUnexpectedEOF
			}))}, nil
		})}
		_, setupExit := exchangeOffer(setupCtx, client, credential{AccessToken: "synthetic-token", DeviceID: "offline-device"}, "v=0\r\n", []byte(`{}`))
		if setupExit != errRemoteClosed {
			t.Fatal("signaling answer read masked the setup close cause", setupExit)
		}
		if _, gatherExit := a.offer(setupCtx); gatherExit != errRemoteClosed {
			t.Fatal("offer gathering masked the setup close cause", gatherExit)
		}
		want := "voice session error"
		if strings.Contains(terminal, "usage_update") {
			want = "usage limit"
		}
		for _, exit := range []error{setupExit, context.Cause(a.ctx)} {
			result := a.outcome(exit)
			if result == nil || !strings.Contains(result.Error(), want) || strings.Contains(result.Error(), "private-marker") {
				t.Fatal("received failure lost or leaked", result)
			}
		}
		if err := a.close(); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	t.Log("A terminal event survived closure; both signaling setup and active-call exit outcomes retained its sanitized failure.")
}

type synchronizedOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (o *synchronizedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.Write(data)
}

func (o *synchronizedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.String()
}

func TestIntentionalPeerShutdownIsQuiet(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var output synchronizedOutput
	var peers []*peer
	for range 2 {
		var settings webrtc.SettingEngine
		settings.SetIncludeLoopbackCandidate(true)
		settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		p, err := newPeer(ctx, settings, nil, log.New(&output, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, p)
		t.Cleanup(func() { _ = p.close() })
	}
	connectLocalPeers(t, ctx, peers[0], peers[1])
	for _, p := range peers {
		if err := p.close(); err != nil {
			t.Fatal(err)
		}
		for _, expected := range []error{io.EOF, io.ErrClosedPipe, net.ErrClosed, context.Canceled} {
			p.channelError(expected)
		}
		if err := p.outcome(nil); err != nil {
			t.Fatal("intentional shutdown became a control or RTCP failure", err)
		}
	}
	if output.String() != "" {
		t.Fatal("intentional shutdown logged a failure", output.String())
	}
}

func TestChannelErrorRemainsVisibleAfterClose(t *testing.T) {
	for _, callbackErr := range []error{errors.New("private transport details"), fmt.Errorf("%w: private transport details", context.DeadlineExceeded)} {
		for _, order := range []string{"error first", "close first", "after cleanup"} {
			t.Run(fmt.Sprintf("%s/deadline=%t", order, errors.Is(callbackErr, context.DeadlineExceeded)), func(t *testing.T) {
				var output bytes.Buffer
				p, err := newPeer(t.Context(), webrtc.SettingEngine{}, nil, log.New(&output, "", 0))
				if err != nil {
					t.Fatal(err)
				}
				defer p.close()
				if order != "error first" {
					p.cancel(errRemoteClosed)
				}
				if order == "after cleanup" {
					if err := p.close(); err != nil {
						t.Fatal(err)
					}
				}
				p.channelError(callbackErr)
				p.cancel(errRemoteClosed)
				expectedDeadline := order == "after cleanup" && errors.Is(callbackErr, context.DeadlineExceeded)
				wantReports := 1
				if expectedDeadline {
					wantReports = 0
				}
				if strings.Count(output.String(), "Voice control channel failed.") != wantReports || strings.Contains(output.String(), "private") {
					t.Fatal("missing or unsafe callback diagnostic", output.String())
				}
				got := p.outcome(context.Cause(p.ctx))
				if got == nil || strings.Contains(got.Error(), "voice control channel failed") == expectedDeadline {
					t.Fatal("unexpected channel failure lost or forced local deadline became fatal", got)
				}
			})
		}
	}
	p := localPeer(t, t.Context())
	p.cancel(errRemoteClosed)
	p.channelError(io.EOF)
	if got := p.outcome(context.Cause(p.ctx)); !errors.Is(got, errRemoteClosed) {
		t.Fatal("ordinary EOF certified normal completion", got)
	}
	cleanup := errors.New("cleanup test")
	if got := p.outcome(errors.Join(errProtocolHangup, cleanup)); !errors.Is(got, cleanup) {
		t.Fatal("normalization erased aggregate")
	}
}

func TestMalformedRTCPStopsDrainWithSafeError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	connectLocalPeers(t, ctx, a, b)
	ssrc := a.pc.GetSenders()[0].GetParameters().Encodings[0].SSRC
	packet := rtcp.RawPacket{0x8f, 205, 0, 2, 0, 0, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(packet[8:], uint32(ssrc))
	if err := b.pc.WriteRTCP([]rtcp.Packet{&packet}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.ctx.Done():
	case <-ctx.Done():
		t.Fatal("malformed RTCP silently stopped drain")
	}
	if got := context.Cause(a.ctx).Error(); got != "microphone RTCP transport failed" {
		t.Fatal("wrong or unsafe RTCP error", got)
	}
}

func TestDataChannelReceiveBoundAndCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	connectLocalPeers(t, ctx, a, b)
	data := `{"type":"future_event"}`
	data += strings.Repeat(" ", 256*1024-len(data))
	if err := b.dc.SendText(data); err != nil {
		t.Fatal(err)
	}
	if err := b.dc.SendText(data + " "); err == nil {
		t.Fatal("Pion sent beyond negotiated receive limit")
	}
	if err := b.dc.SendText(`{"type":"tool_update","update_type":"hangup"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.ctx.Done():
	case <-ctx.Done():
		t.Fatal("hangup after maximum legal message did not propagate")
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	if got := a.outcome(context.Cause(a.ctx)); got != nil {
		t.Fatal("confirmed hangup became error", got)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedDataChannelReceive(t *testing.T) {
	for _, tc := range []struct {
		size int
		want string
	}{{256*1024 + 1, "voice event exceeds 256 KiB"}, {512 * 1024, "voice event exceeds 256 KiB"}} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			offer, err := a.offer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Let the sender exceed the receiver's actual limit without its own admission check.
			advertised := strings.Replace(offer, "a=max-message-size:262144", "a=max-message-size:1048576", 1)
			answerLocalOffer(t, ctx, a, b, advertised)
			data := `{"type":"future_event","message":"private-marker"}`
			data += strings.Repeat(" ", tc.size-len(data))
			if err := b.dc.SendText(data); err != nil {
				t.Fatal("fixture did not send the oversized message", err)
			}
			select {
			case <-a.ctx.Done():
			case <-ctx.Done():
				t.Fatal("oversized receive did not terminate the peer")
			}
			if got := a.outcome(context.Cause(a.ctx)); got == nil || !strings.Contains(got.Error(), tc.want) || strings.Contains(got.Error(), "private-marker") {
				t.Fatal("oversized receive lost its safe failure", got)
			}
		})
	}
}

func TestRunCallMarksGenuineFailuresThroughCancellation(t *testing.T) {
	failure := errors.New("received voice session rejection")
	for _, cause := range []error{
		errAlicePriority, context.Canceled, context.DeadlineExceeded,
		errors.Join(errAlicePriority, failure), errors.Join(context.Canceled, failure),
		fmt.Errorf("%w: native spotter submit failed", errStationAudio),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			parent := t.Context()
			if cause == context.DeadlineExceeded {
				var cancel context.CancelFunc
				parent, cancel = context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer cancel()
			}
			e := newStationEpoch(parent, 1)
			e.revoke(cause)
			err := runCall(e, credential{AccessToken: "synthetic-token", DeviceID: "offline-device"}, newStationCapture(e), sessionSettings{})
			_, marked := errors.AsType[voiceCallError](err)
			if marked != errors.Is(cause, failure) || !errors.Is(err, cause) {
				t.Fatal("call failure marker lost a real failure or marked expected cancellation", err)
			}
			if !marked && err != cause {
				t.Fatal("successful cancellation did not preserve its exact cause", err)
			}
		})
	}
	e := newStationEpoch(t.Context(), 2)
	defer e.revoke(context.Canceled)
	if _, marked := errors.AsType[voiceCallError](runCall(e, credential{}, newStationCapture(e), sessionSettings{})); !marked {
		t.Fatal("invalid call credential was not marked as a call failure")
	}
}
