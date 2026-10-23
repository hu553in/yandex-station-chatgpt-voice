package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

const signalingURL = "https://chatgpt.com/realtime/wm?dcid=0"
const maxAnswerBytes = 1 << 20
const maxQueuedEventBytes = 1 << 20
const maxQueuedEvents = 64

var errRemoteClosed = errors.New("remote voice connection closed")

type peer struct {
	pc           *webrtc.PeerConnection
	dc           *webrtc.DataChannel
	mic          *webrtc.TrackLocalStaticSample
	ctx          context.Context
	cancel       context.CancelCauseFunc
	ready        chan struct{}
	messages     chan []byte
	messageBytes atomic.Int64
	tracks       chan *webrtc.TrackRemote
	trackSeen    atomic.Bool
	wg           sync.WaitGroup
	close        func() error
}

func newPeer(ctx context.Context, settings webrtc.SettingEngine, servers []webrtc.ICEServer) (*peer, error) {
	settings.SetSCTPMaxMessageSize(maxMessageBytes)
	settings.SetSCTPMaxReceiveBufferSize(maxQueuedEventBytes)
	settings.LoggerFactory = &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	ctx, cancel := context.WithCancelCause(ctx)
	p := &peer{ctx: ctx, cancel: cancel, ready: make(chan struct{}), messages: make(chan []byte, maxQueuedEvents), tracks: make(chan *webrtc.TrackRemote, 1)}
	var err error
	p.pc, err = api.NewPeerConnection(webrtc.Configuration{ICEServers: servers, BundlePolicy: webrtc.BundlePolicyMaxBundle})
	if err != nil {
		cancel(err)
		return nil, errors.New("cannot create WebRTC peer")
	}
	p.close = sync.OnceValue(func() error {
		p.cancel(context.Canceled)
		// Peer closure tears down SCTP before joining its DataChannel callbacks.
		pcErr := p.pc.GracefulClose()
		p.wg.Wait()
		if pcErr != nil {
			return errors.New("WebRTC did not close cleanly")
		}
		return nil
	})
	fail := func(message string) (*peer, error) { _ = p.close(); return nil, errors.New(message) }
	p.mic, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, "microphone", "voice")
	if err != nil {
		return fail("cannot create Opus track")
	}
	tx, err := p.pc.AddTransceiverFromTrack(p.mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		return fail("cannot attach Opus track")
	}
	codecs := tx.Sender().GetParameters().Codecs
	var opusCodecs []webrtc.RTPCodecParameters
	for _, c := range codecs {
		if c.MimeType == webrtc.MimeTypeOpus && c.ClockRate == 48000 && c.Channels == 2 {
			opusCodecs = append(opusCodecs, c)
		}
	}
	if len(opusCodecs) == 0 || tx.SetCodecPreferences(opusCodecs) != nil {
		return fail("cannot select Opus codec")
	}
	p.dc, err = p.pc.CreateDataChannel("", &webrtc.DataChannelInit{Negotiated: new(true), Ordered: new(true), ID: new(uint16(0))})
	if err != nil {
		return fail("cannot create voice control channel")
	}
	p.dc.OnOpen(sync.OnceFunc(func() { close(p.ready) }))
	p.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			p.cancel(errors.New("unexpected binary voice control message"))
			return
		}
		p.enqueue(message.Data)
	})
	p.dc.OnClose(func() { p.cancel(errRemoteClosed) })
	p.dc.OnError(func(error) { p.cancel(errors.New("voice control channel failed")) })
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			p.cancel(errors.New("WebRTC connection lost"))
		case webrtc.PeerConnectionStateClosed:
			p.cancel(errRemoteClosed)
		}
	})
	p.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if p.ctx.Err() != nil {
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeAudio || !p.trackSeen.CompareAndSwap(false, true) {
			p.cancel(errors.New("unexpected additional remote media track"))
			return
		}
		select {
		case p.tracks <- track:
		case <-p.ctx.Done():
		default:
			p.cancel(errors.New("remote track mailbox overflow"))
		}
	})
	p.wg.Go(func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := tx.Sender().Read(buffer); err != nil {
				return
			}
		}
	})
	return p, nil
}

func (p *peer) enqueue(data []byte) {
	if p.ctx.Err() != nil {
		return
	}
	if len(data) > maxMessageBytes {
		p.cancel(errors.New("voice event exceeds 256 KiB"))
		return
	}
	size := int64(len(data))
	if p.messageBytes.Add(size) > maxQueuedEventBytes {
		p.messageBytes.Add(-size)
		p.cancel(errors.New("voice event byte queue overflow"))
		return
	}
	select {
	case p.messages <- bytes.Clone(data):
	default:
		p.messageBytes.Add(-size)
		p.cancel(errors.New("voice event count queue overflow"))
	}
}

func (p *peer) offer(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", errors.New("cannot create voice offer")
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if p.pc.SetLocalDescription(offer) != nil {
		return "", errors.New("cannot set voice offer")
	}
	gatherCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	select {
	case <-gatherCtx.Done():
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("ICE gathering did not complete before timeout")
	case <-p.ctx.Done():
		return "", context.Cause(p.ctx)
	case <-gathered:
	}
	if gatherCtx.Err() != nil {
		return "", errors.New("ICE gathering did not complete before cancellation or timeout")
	}
	description := p.pc.LocalDescription()
	if description == nil || p.pc.ICEGatheringState() != webrtc.ICEGatheringStateComplete {
		return "", errors.New("ICE offer is incomplete")
	}
	return description.SDP, nil
}

func (p *peer) accept(answer string) error {
	if p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}) != nil {
		return errors.New("ChatGPT SDP answer was not accepted by the native peer")
	}
	return nil
}

func signalingClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func exchangeOffer(ctx context.Context, client *http.Client, auth credential, offer string, session []byte) (string, error) {
	if !validCredential(auth) {
		return "", errors.New("invalid web credential")
	}
	if len(offer) > maxAnswerBytes || !strings.HasPrefix(offer, "v=0") {
		return "", errors.New("invalid local SDP offer")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("sdp", offer); err != nil {
		return "", errors.New("cannot encode offer")
	}
	if err := form.WriteField("session", string(session)); err != nil {
		return "", errors.New("cannot encode voice session")
	}
	if form.Close() != nil {
		return "", errors.New("cannot finish offer")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signalingURL, &body)
	if err != nil {
		return "", errors.New("cannot prepare signaling request")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "yandex-station-chatgpt-voice/0.1")
	req.Header.Set("Oai-Device-Id", auth.DeviceID)
	req.Header.Set("Oai-Language", "en-US")
	req.Header.Set("Oai-Client-Version", "prod-fb4a8a2a751dfec391053cfd7b01c52699ccf78c")
	req.Header.Set("Oai-Client-Build-Number", "8370486")
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("voice signaling HTTPS request failed; check network, TLS, and service availability")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return "", errors.New("ChatGPT session expired or rejected (401); run yandex-station-chatgpt-voice auth import with a fresh own web access token")
	case http.StatusForbidden:
		return "", errors.New("ChatGPT rejected voice access (403); account entitlement or edge restrictions may prevent this native client")
	case http.StatusOK, http.StatusCreated:
	default:
		return "", fmt.Errorf("ChatGPT voice signaling returned HTTP %d", response.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes+1))
	if err != nil {
		return "", errors.New("cannot read voice SDP answer")
	}
	if len(answer) > maxAnswerBytes {
		return "", errors.New("voice SDP answer exceeds 1 MiB")
	}
	sdp := strings.TrimSpace(string(answer))
	if !strings.HasPrefix(sdp, "v=0\r\n") && !strings.HasPrefix(sdp, "v=0\n") {
		return "", errors.New("ChatGPT did not return an SDP answer")
	}
	return sdp + "\r\n", nil
}

func runCall(e *stationEpoch, auth credential, capture *stationCapture) (result error) {
	if !validCredential(auth) {
		return errors.New("invalid web credential")
	}
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	ctx := e.ctx
	var workers sync.WaitGroup
	p, err := newPeer(ctx, webrtc.SettingEngine{}, []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302", "stun:stun4.l.google.com:19302"}}})
	if err != nil {
		return err
	}
	shutdown := sync.OnceValue(func() error {
		e.revoke(context.Canceled)
		return errors.Join(e.output.close(), p.close())
	})
	stopShutdown := context.AfterFunc(ctx, func() { _ = shutdown() })
	defer func() {
		e.revoke(context.Canceled)
		result = errors.Join(result, shutdown())
		stopShutdown()
		workers.Wait()
	}()
	stopPeerCancel := context.AfterFunc(p.ctx, func() { e.revoke(context.Cause(p.ctx)) })
	defer stopPeerCancel()
	setupCtx, finishSetup := context.WithTimeout(ctx, setupTimeout)
	defer finishSetup()
	stopSetupTimeout := context.AfterFunc(setupCtx, func() {
		if errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
			e.revoke(errors.New("voice connection setup timed out"))
		}
	})
	defer stopSetupTimeout()
	offer, err := p.offer(setupCtx)
	if err != nil {
		return err
	}
	session, err := encodeSession(time.Now())
	if err != nil {
		return errors.New("cannot encode voice session")
	}
	client := signalingClient()
	defer client.CloseIdleConnections()
	answer, err := exchangeOffer(setupCtx, client, auth, offer, session)
	if err != nil {
		return err
	}
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if err = p.accept(answer); err != nil {
		return err
	}
	handle := func(data []byte) error {
		p.messageBytes.Add(-int64(len(data)))
		ev, err := decodeEvent(data)
		if err != nil {
			return err
		}
		if ev.err != nil {
			return ev.err
		}
		if ev.ended {
			return errRemoteClosed
		}
		return nil
	}
	ready := false
	for !ready {
		select {
		case <-setupCtx.Done():
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return errors.New("voice connection setup timed out")
		case data := <-p.messages:
			if err := handle(data); err != nil {
				if errors.Is(err, errRemoteClosed) {
					return nil
				}
				return err
			}
		case <-p.ready:
			ready = true
		}
	}
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended before media became ready")
	}
	if err = e.output.open(); err != nil {
		return err
	}
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended while opening playback")
	}
	if err = playCue(setupCtx, 880); err != nil {
		return err
	}
	if err = capture.beginStreaming(setupCtx); err != nil {
		return err
	}
	finishSetup()
	workers.Go(func() {
		if err := e.output.send(ctx, p.mic, capture.input); err != nil {
			e.revoke(err)
		}
	})
	for {
		select {
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if errors.Is(cause, errRemoteClosed) {
				return nil
			}
			return cause
		case data := <-p.messages:
			if err := handle(data); err != nil {
				if errors.Is(err, errRemoteClosed) {
					return nil
				}
				return err
			}
		case track := <-p.tracks:
			workers.Go(func() {
				if err := e.output.receive(ctx, track); err != nil {
					e.revoke(err)
				}
			})
		}
	}
}
