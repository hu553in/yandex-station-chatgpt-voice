package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func wireInt(data []byte, number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(data, number, protowire.VarintType), value)
}

func wireBytes(data []byte, number protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(data, number, protowire.BytesType), value)
}

func framedIPC(kind uint64, payload []byte) []byte {
	body := wireInt(wireBytes(nil, 252, payload), 253, kind)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)
}

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(b []byte) (int, error) { return r.Reader.Read(b[:min(len(b), 1)]) }

func TestStationWireValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    aliceState
	}{
		{"missing", nil, aliceUnknown}, {"idle", wireInt(nil, 1, 0), aliceIdle}, {"listening", wireInt(nil, 1, 1), aliceListening},
		{"speaking", wireInt(nil, 1, 2), aliceSpeaking}, {"busy", wireInt(nil, 1, 3), aliceBusy},
		{"shazam", wireInt(nil, 1, 5), aliceShazam}, {"none", wireInt(nil, 1, 6), aliceNone}, {"unknown", wireInt(nil, 1, 42), aliceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := protowire.AppendFixed32(protowire.AppendTag(tc.payload, 99, protowire.Fixed32Type), 123)
			e, err := readIPC(fragmentedReader{bytes.NewReader(framedIPC(3, payload))})
			if err != nil {
				t.Fatal(err)
			}
			ev, err := decodeNativeEvent(e)
			if err != nil || !ev.stateChanged || ev.state != tc.want {
				t.Fatalf("state %+v %v", ev, err)
			}
		})
	}
	for _, payload := range [][]byte{wireBytes(nil, 1, nil), wireInt(wireInt(nil, 1, 0), 1, 0), {0x08, 0x80}, {0}} {
		if _, err := decodeNativeEvent(ipcEnvelope{kind: 3, payload: payload}); err == nil {
			t.Fatal("malformed state accepted")
		}
	}
	for _, data := range [][]byte{nil, {1, 0, 0}, {0, 0, 0, 0}, binary.LittleEndian.AppendUint32(nil, maxIPCBytes+1), framedIPC(3, nil)[:5]} {
		if _, err := readIPC(bytes.NewReader(data)); err == nil {
			t.Fatal("bad frame accepted")
		}
	}
	for _, body := range [][]byte{wireBytes(nil, 252, nil), wireInt(nil, 253, 3), wireBytes(wireBytes(nil, 252, nil), 253, nil)} {
		if _, err := decodeIPC(body); err == nil {
			t.Fatal("bad envelope accepted")
		}
	}
	listener := wireBytes(wireInt(nil, 1, 0), 2, wireInt(nil, 1, 0))
	payload := wireBytes(wireBytes(nil, 7, []byte("AliceCapability")), 18, listener)
	ev, err := decodeNativeEvent(ipcEnvelope{kind: 210, payload: payload})
	if err != nil || ev.state != aliceIdle || !ev.stateChanged {
		t.Fatal("Alice listener missed", err)
	}
	for _, bad := range [][]byte{wireBytes(nil, 2, wireInt(nil, 1, 0)), wireInt(nil, 1, 0), wireInt(wireInt(nil, 1, 0), 2, 0)} {
		payload := wireBytes(wireBytes(nil, 7, []byte("AliceCapability")), 18, bad)
		if _, err := decodeNativeEvent(ipcEnvelope{kind: 210, payload: payload}); err == nil {
			t.Fatal("malformed Alice listener accepted")
		}
	}
	activation := wireBytes(wireInt(nil, 1, 2), 3, []byte("Алиса"))
	payload = wireBytes(wireBytes(nil, 7, []byte("ActivationSpotterCapability")), 15, activation)
	ev, err = decodeNativeEvent(ipcEnvelope{kind: 210, payload: payload})
	if err != nil || !ev.activation || ev.stateChanged {
		t.Fatal("activation did not revoke exclusively", err)
	}
	request, err := readIPC(bytes.NewReader(audioRequest()))
	if err != nil || request.kind != 159 || !bytes.Equal(request.payload, wireBytes(nil, 1, nil)) {
		t.Fatal("wrong processed audio subscription", err)
	}
}

func TestGateRevokesBeforeCleanupAndRejectsLateWake(t *testing.T) {
	g := newStationGate()
	if g.begin(t.Context()) != nil {
		t.Fatal("initial unknown armed")
	}
	for _, state := range []aliceState{aliceUnknown, aliceListening, aliceSpeaking, aliceBusy, aliceShazam, aliceNone} {
		g.observe(nativeEvent{stateChanged: true, state: state})
		if g.begin(t.Context()) != nil {
			t.Fatal("non-IDLE armed")
		}
	}
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	e := g.begin(t.Context())
	if e == nil || g.activate(e) != nil {
		t.Fatal("explicit IDLE did not arm")
	}
	g.observe(nativeEvent{activation: true})
	if e.allowed() || !errors.Is(context.Cause(e.ctx), errAlicePriority) {
		t.Fatal("activation did not directly revoke media and call")
	}
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	if g.activate(e) == nil || g.begin(t.Context()) != nil {
		t.Fatal("late detector or new call escaped before cleanup")
	}
	g.finish(e, nil)
	next := g.begin(t.Context())
	if next == nil || next.number == e.number || g.activate(next) != nil {
		t.Fatal("fresh epoch did not rearm after cleanup")
	}
	defer g.finish(next, nil)
	if g.activate(e) == nil {
		t.Fatal("old epoch activated new call")
	}
	var workers sync.WaitGroup
	workers.Go(func() { g.observe(nativeEvent{stateChanged: true, state: aliceListening}) })
	workers.Go(func() {
		for range 100 {
			_ = g.activate(e)
		}
	})
	workers.Wait()
	if next.allowed() || g.activate(next) == nil {
		t.Fatal("concurrent state revocation lost")
	}
}

func TestNativePrioritySurvivesMaskedSetupError(t *testing.T) {
	g := newStationGate()
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	e := g.begin(t.Context())
	g.observe(nativeEvent{stateChanged: true, state: aliceListening})
	masked := errors.New("cannot read voice SDP answer")
	if err := g.finish(e, masked); !errors.Is(err, errAlicePriority) || !errors.Is(err, masked) {
		t.Fatal("masked setup error turned native takeover into a fatal failure", err)
	}
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	next := g.begin(t.Context())
	if next == nil || g.activate(next) != nil || g.activate(e) == nil {
		t.Fatal("native takeover prevented a fresh wake after cleanup")
	}
	if err := g.finish(next, masked); !errors.Is(err, masked) {
		t.Fatal("ordinary setup failure lost its cause", err)
	}
}

func TestStationRearmsAfterCallFailureAndWaitsForAlice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, server := net.Pipe()
	defer server.Close()
	attempts := make(chan *stationEpoch, 3)
	done := make(chan error, 1)
	returned := make(chan struct{})
	var output bytes.Buffer
	go func() {
		defer close(returned)
		count := 0
		done <- serveStation(ctx, client, credential{}, &output, func(e *stationEpoch, _ *stationGate, _ credential) error {
			count++
			attempts <- e
			if count == 1 {
				return voiceCallError{errors.New("signaling HTTP 503")}
			}
			<-e.ctx.Done()
			return context.Cause(e.ctx)
		}, func(context.Context) error {
			t.Error("failed or canceled attempt produced a completion cue")
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); server.Close(); <-returned })
	state := func(value uint64) {
		t.Helper()
		if _, err := server.Write(framedIPC(3, wireInt(nil, 1, value))); err != nil {
			t.Fatal(err)
		}
	}
	next := func() *stationEpoch {
		t.Helper()
		select {
		case e := <-attempts:
			return e
		case err := <-done:
			t.Fatalf("station exited instead of rearming: %v", err)
		case <-ctx.Done():
			t.Fatal("station did not arm")
		}
		return nil
	}
	state(0)
	first, second := next(), next()
	if first.allowed() || first.number == second.number {
		t.Fatal("failed call reused its epoch")
	}
	state(1)
	select {
	case <-second.ctx.Done():
	case <-ctx.Done():
		t.Fatal("Alice did not revoke the second attempt")
	}
	select {
	case <-attempts:
		t.Fatal("station rearmed while Alice was listening")
	case <-time.After(30 * time.Millisecond):
	}
	state(0)
	third := next()
	if !third.allowed() || third.number == second.number {
		t.Fatal("new Alice IDLE did not allow a fresh attempt")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("station did not stop on cancellation", err)
	}
	if !bytes.Contains(output.Bytes(), []byte("signaling HTTP 503")) {
		t.Fatal("failed call was not reported")
	}
}

func TestStationRearmsAfterSessionRejectionWithoutRetry(t *testing.T) {
	for _, cancellation := range []string{"none", "Alice", "parent", "joined Alice", "joined parent"} {
		for _, status := range []int{400, 422, 200} {
			t.Run(fmt.Sprintf("%d/%s", status, cancellation), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var requests atomic.Int32
				httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)
					body := "private-rejection-body"
					if status == 200 {
						body = "v=0\r\ns=offline\r\n"
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header), Request: req}, nil
				})}
				var controlFailure error
				var controlPeer *peer
				if status == 200 {
					a, b := localPeer(t, ctx), localPeer(t, ctx)
					connectLocalPeers(t, ctx, a, b)
					if err := b.dc.SendText(`{"type":"error","message":"private-rejection-body"}`); err != nil {
						t.Fatal(err)
					}
					select {
					case <-a.ctx.Done():
					case <-ctx.Done():
						t.Fatal("DC rejection was not received")
					}
					if err := b.close(); err != nil {
						t.Fatal(err)
					}
					controlPeer = a
					controlFailure = a.outcome(errRemoteClosed)
					if controlFailure == nil {
						t.Fatal("received rejection became normal closure")
					}
				}
				client, server := net.Pipe()
				defer server.Close()
				armed := make(chan struct{}, 2)
				wake := make(chan struct{}, 1)
				rejected := make(chan struct{})
				done := make(chan error, 1)
				var output bytes.Buffer
				go func() {
					done <- serveStation(ctx, client, credential{AccessToken: "synthetic-token", DeviceID: "offline-device"}, &output, func(e *stationEpoch, gate *stationGate, auth credential) error {
						armed <- struct{}{}
						select {
						case <-e.ctx.Done():
							return context.Cause(e.ctx)
						case <-wake:
						}
						_, err := exchangeOffer(e.ctx, httpClient, auth, "v=0\r\n", []byte(`{}`))
						if err == nil {
							err = controlFailure
						}
						if strings.Contains(cancellation, "Alice") {
							gate.observe(nativeEvent{activation: true})
						} else if strings.Contains(cancellation, "parent") {
							cancel()
						}
						if strings.HasPrefix(cancellation, "joined") {
							if controlPeer != nil {
								err = controlPeer.outcome(context.Cause(e.ctx))
							} else {
								err = errors.Join(context.Cause(e.ctx), err)
							}
						}
						close(rejected)
						return voiceCallError{err}
					}, func(context.Context) error {
						t.Error("rejected session produced a completion cue")
						return nil
					})
				}()
				if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
					t.Fatal(err)
				}
				select {
				case <-armed:
				case <-ctx.Done():
					t.Fatal("station did not arm")
				}
				wake <- struct{}{}
				select {
				case <-rejected:
				case <-time.After(time.Second):
					t.Fatal("session rejection did not complete")
				}
				if strings.Contains(cancellation, "Alice") {
					select {
					case <-armed:
						t.Fatal("station rearmed while Alice had priority")
					case <-time.After(20 * time.Millisecond):
					}
					if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
						t.Fatal(err)
					}
				}
				if !strings.Contains(cancellation, "parent") {
					select {
					case <-armed:
					case <-ctx.Done():
						t.Fatal("rejected call did not rearm under IDLE")
					}
				}
				if requests.Load() != 1 {
					t.Fatal("detection rearm made a new signaling request")
				}
				cancel()
				if err := <-done; err != context.Canceled {
					t.Fatal("successful cleanup did not preserve bare cancellation", err)
				}
				if !bytes.Contains(output.Bytes(), []byte("Voice attempt failed")) || bytes.Contains(output.Bytes(), []byte("private")) || bytes.Contains(output.Bytes(), []byte("ended normally")) {
					t.Fatal("rejection was hidden, classified as success, or disclosed", output.String())
				}
			})
		}
	}
}

func TestStationExpectedCancellationIsQuiet(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		t.Run(fmt.Sprint(parentCanceled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, server := net.Pipe()
			defer server.Close()
			armed := make(chan *stationEpoch, 2)
			done := make(chan error, 1)
			var output bytes.Buffer
			go func() {
				done <- serveStation(ctx, client, credential{}, &output, func(e *stationEpoch, _ *stationGate, _ credential) error {
					armed <- e
					<-e.ctx.Done()
					return runCall(e, credential{AccessToken: "synthetic-token", DeviceID: "offline-device"}, newStationCapture(e), sessionSettings{})
				}, func(context.Context) error {
					t.Error("canceled attempt produced a completion cue")
					return nil
				})
			}()
			t.Cleanup(func() { cancel(); server.Close() })
			if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
				t.Fatal(err)
			}
			first := <-armed
			if !parentCanceled {
				if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 1))); err != nil {
					t.Fatal(err)
				}
				select {
				case <-armed:
					t.Fatal("station rearmed while Alice was listening")
				case <-time.After(20 * time.Millisecond):
				}
				if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
					t.Fatal(err)
				}
				select {
				case next := <-armed:
					if next.number == first.number || !next.allowed() {
						t.Fatal("Alice cancellation did not permit a fresh epoch under IDLE")
					}
				case <-ctx.Done():
					t.Fatal("station did not rearm after expected cancellation")
				}
			}
			cancel()
			if err := <-done; err != context.Canceled {
				t.Fatal("parent cancellation did not remain bare", err)
			}
			if strings.Contains(output.String(), "failed") || strings.Contains(output.String(), "ended normally") {
				t.Fatal("expected cancellation was reported as failure or success", output.String())
			}
		})
	}
}

func TestStationCleanupSurvivesAliceAndParentCancellation(t *testing.T) {
	for _, cleanup := range []error{
		fmt.Errorf("%w: native spotter submit failed", errStationAudio),
		fmt.Errorf("%w: native spotter event read failed", errStationAudio),
		fmt.Errorf("%w: native spotter destroy failed", errCleanup),
		fmt.Errorf("%w: WebRTC did not close cleanly", errCleanup),
		fmt.Errorf("%w: native spotter create failed; native spotter destroy failed", errStationAudio),
	} {
		for _, parentCanceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parent=%t", cleanup, parentCanceled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client, server := net.Pipe()
				defer server.Close()
				done := make(chan error, 1)
				attempts := 0
				go func() {
					done <- serveStation(ctx, client, credential{}, io.Discard, func(e *stationEpoch, gate *stationGate, _ credential) error {
						attempts++
						if parentCanceled {
							cancel()
						} else {
							gate.observe(nativeEvent{activation: true})
						}
						return errors.Join(context.Cause(e.ctx), cleanup)
					}, func(context.Context) error {
						t.Error("cleanup failure produced completion cue")
						return nil
					})
				}()
				if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					cause := errAlicePriority
					if parentCanceled {
						cause = context.Canceled
					}
					if attempts != 1 || !errors.Is(err, cleanup) || !errors.Is(err, cause) {
						t.Fatal("cancellation hid cleanup failure or original cause", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cleanup failure did not end service")
				}
			})
		}
	}
}

func TestCompletionCueDiagnostic(t *testing.T) {
	for _, cueErr := range []error{errors.New("cannot play voice cue: executable missing"), context.DeadlineExceeded, errAlicePriority, context.Canceled} {
		t.Run(cueErr.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client, server := net.Pipe()
			defer server.Close()
			done := make(chan error, 1)
			var output bytes.Buffer
			go func() {
				attempts := 0
				done <- serveStation(ctx, client, credential{}, &output, func(e *stationEpoch, _ *stationGate, _ credential) error {
					attempts++
					if attempts == 1 {
						return nil
					}
					cancel()
					return context.Cause(e.ctx)
				}, func(context.Context) error {
					if cueErr == context.Canceled {
						cancel()
					}
					return cueErr
				})
			}()
			if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != context.Canceled {
				t.Fatal("cue failure made service fatal", err)
			}
			logged := strings.Contains(output.String(), "Completion cue failed")
			wantLog := cueErr != errAlicePriority && cueErr != context.Canceled
			if logged != wantLog || wantLog && !strings.Contains(output.String(), cueErr.Error()) {
				t.Fatal("cue detail lost or expected cancellation reported", output.String())
			}
		})
	}
}

func TestStationNativeAudioFaultIsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- serveStation(ctx, client, credential{}, io.Discard, func(*stationEpoch, *stationGate, credential) error {
			return voiceCallError{fmt.Errorf("%w: capture stalled", errStationAudio)}
		}, func(context.Context) error {
			t.Error("native audio failure produced a completion cue")
			return nil
		})
	}()
	if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errStationAudio) {
			t.Fatal("native fault lost its cause", err)
		}
	case <-ctx.Done():
		t.Fatal("native audio fault was retried")
	}
}

func TestStationAliceTakeoverCleanupFaultIsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	client, server := net.Pipe()
	deadline, _ := ctx.Deadline()
	if err := server.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	cleanupErr := fmt.Errorf("%w: injected playback close", errStationAudio)
	entered := make(chan struct{})
	type result struct {
		err      error
		cause    error
		attempts int
		cues     int
	}
	done := make(chan result, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		var got result
		got.err = serveStation(ctx, client, credential{}, io.Discard, func(e *stationEpoch, gate *stationGate, _ credential) error {
			got.attempts++
			if got.attempts > 1 {
				return errors.New("unexpected retry after native audio cleanup failure")
			}
			close(entered)
			<-e.ctx.Done()
			got.cause = context.Cause(e.ctx)
			gate.observe(nativeEvent{stateChanged: true, state: aliceIdle})
			return errors.Join(context.Canceled, cleanupErr)
		}, func(context.Context) error {
			got.cues++
			return nil
		})
		done <- got
	}()
	t.Cleanup(func() { cancel(); server.Close(); <-returned })
	if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("station did not arm")
	}
	if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 1))); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !errors.Is(got.cause, errAlicePriority) {
			t.Fatal("attempt was not canceled by Alice", got.cause)
		}
		if got.attempts != 1 || got.cues != 0 {
			t.Fatalf("native audio cleanup fault caused %d attempts and %d completion cues", got.attempts, got.cues)
		}
		for _, cause := range []error{cleanupErr, errStationAudio, context.Canceled, errAlicePriority} {
			if !errors.Is(got.err, cause) {
				t.Errorf("native audio cleanup fault lost %v: %v", cause, got.err)
			}
		}
	case <-ctx.Done():
		t.Fatal("native audio cleanup fault did not stop the service")
	}
}

func TestCompletionEpochHandoff(t *testing.T) {
	for _, action := range []string{"protocol hangup", "Alice then IDLE", "parent canceled"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			g := newStationGate()
			g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
			old := g.begin(ctx)
			defer g.finish(old, nil)
			old.revoke(errProtocolHangup)
			switch action {
			case "Alice then IDLE":
				g.observe(nativeEvent{activation: true})
				g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
			case "parent canceled":
				cancel()
			}
			cue := g.completion(ctx, old)
			if action != "protocol hangup" {
				if cue != nil {
					g.finish(cue, nil)
					t.Fatal("completion escaped takeover or cancellation")
				}
				return
			}
			if cue == nil {
				t.Fatal("confirmed hangup prevented completion cue")
			}
			defer g.finish(cue, nil)
			g.finish(old, nil)
			if !cue.allowed() || cue.number == old.number || old.allowed() || g.activate(old) == nil || g.completion(ctx, old) != nil {
				t.Fatal("completion did not transfer ownership exclusively")
			}
			g.observe(nativeEvent{activation: true})
			g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
			if cue.allowed() || !errors.Is(context.Cause(cue.ctx), errAlicePriority) || g.begin(ctx) != nil {
				t.Fatal("Alice did not revoke completion until its cleanup")
			}
		})
	}
}

func TestStationCompletionYieldsToAliceAndRearms(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	client, server := net.Pipe()
	attempts := make(chan *stationEpoch, 2)
	cues := make(chan context.Context, 2)
	done := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		count := 0
		done <- serveStation(ctx, client, credential{}, io.Discard, func(e *stationEpoch, _ *stationGate, _ credential) error {
			count++
			attempts <- e
			if count == 1 {
				e.revoke(errProtocolHangup)
				return nil
			}
			<-e.ctx.Done()
			return context.Cause(e.ctx)
		}, func(cue context.Context) error {
			cues <- cue
			<-cue.Done()
			return context.Cause(cue)
		})
	}()
	t.Cleanup(func() { cancel(); server.Close(); <-returned })
	state := func(value uint64) {
		t.Helper()
		if _, err := server.Write(framedIPC(3, wireInt(nil, 1, value))); err != nil {
			t.Fatal(err)
		}
	}
	next := func() *stationEpoch {
		t.Helper()
		select {
		case e := <-attempts:
			return e
		case <-ctx.Done():
			t.Fatal("station did not rearm")
		}
		return nil
	}
	state(0)
	first := next()
	var cue context.Context
	select {
	case cue = <-cues:
	case <-ctx.Done():
		t.Fatal("completed attempt did not produce a cue")
	}
	if first.allowed() || cue.Err() != nil {
		t.Fatal("completion reused the old media permission")
	}
	state(1)
	select {
	case <-cue.Done():
		if !errors.Is(context.Cause(cue), errAlicePriority) {
			t.Fatal("completion lost Alice priority")
		}
	case <-ctx.Done():
		t.Fatal("Alice did not cancel completion")
	}
	select {
	case <-attempts:
		t.Fatal("station rearmed while Alice was listening")
	default:
	}
	state(0)
	if second := next(); !second.allowed() || second.number == first.number || cue.Err() == nil {
		t.Fatal("station revived an old epoch")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("station did not stop", err)
	}
	if len(cues) != 0 {
		t.Fatal("one completed attempt produced multiple cues")
	}
}

func TestQuietAliceAndTransportEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, server := net.Pipe()
	defer server.Close()
	gate := newStationGate()
	done := make(chan error, 1)
	go func() { done <- readAlice(ctx, client, gate) }()
	if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
		t.Fatal(err)
	}
	<-gate.changed
	e := gate.begin(ctx)
	if e == nil {
		t.Fatal("explicit IDLE missing")
	}
	defer gate.finish(e, nil)
	if err := server.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := server.Read(b[:]); err == nil {
		t.Fatal("state connection sent an unproved init message")
	}
	if !e.allowed() {
		t.Fatal("quiet Alice IDLE was treated as a missing heartbeat")
	}
	server.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("EOF ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("state EOF did not end reader")
	}
	if e.allowed() || gate.activate(e) == nil {
		t.Fatal("state EOF left call permitted")
	}
}

func TestCapturePhasesDiscardWakeAndConnectingAudio(t *testing.T) {
	e := newStationEpoch(t.Context(), 7)
	defer e.revoke(context.Canceled)
	c := newStationCapture(e)
	push := func(samples int, value int16) {
		t.Helper()
		pcm := make([]byte, samples*2)
		for i := range samples {
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(value))
		}
		if err := c.pushPCM(pcm); err != nil {
			t.Fatal(err)
		}
	}
	push(captureSamples+17, 1000)
	if len(c.detector) != 1 || len(c.input) != 0 {
		t.Fatal("wake audio escaped detection")
	}
	if err := c.beginConnecting(); err != nil {
		t.Fatal(err)
	}
	push(captureSamples*(captureQueueFrames+detectorQueueFrames+1)+23, 2000)
	if len(c.detector) != 1 || len(c.input) != 0 {
		t.Fatal("connecting audio was queued")
	}
	if err := c.beginStreaming(t.Context()); err != nil {
		t.Fatal(err)
	}
	push(captureSamples-1, 3000)
	if len(c.input) != 0 {
		t.Fatal("partial wake or connecting audio crossed readiness")
	}
	push(1, 3000)
	if len(c.input) != 1 {
		t.Fatal("first live frame missing")
	}
	frame := <-c.input
	if frame.epoch != 7 || frame.sequence != 1 {
		t.Fatal("wrong first live epoch or sequence")
	}
	for _, sample := range frame.pcm {
		if sample != 3000 {
			t.Fatal("wake or connecting audio reached live capture")
		}
	}
	for range captureQueueFrames {
		push(captureSamples, 0)
	}
	if err := c.pushPCM(make([]byte, captureSamples*2)); !errors.Is(err, errCaptureOverflow) || e.allowed() {
		t.Fatal("live capture overflow did not end attempt", err)
	}
	for i := range captureQueueFrames {
		frame := <-c.input
		if frame.epoch != 7 || frame.sequence != uint64(i+2) || frame.pcm != [captureSamples]int16{} {
			t.Fatal("live capture was overwritten or reordered")
		}
	}
	if len(c.detector) != 1 {
		t.Fatal("live audio reached the detector")
	}
}

func TestCaptureReadinessRejectsCancellation(t *testing.T) {
	for _, action := range []string{"setup canceled", "Alice priority"} {
		t.Run(action, func(t *testing.T) {
			e := newStationEpoch(t.Context(), 7)
			defer e.revoke(context.Canceled)
			c := newStationCapture(e)
			if err := c.beginConnecting(); err != nil {
				t.Fatal(err)
			}
			setupCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := context.Canceled
			if action == "setup canceled" {
				cancel()
			} else {
				want = errAlicePriority
				e.revoke(want)
			}
			if err := c.beginStreaming(setupCtx); !errors.Is(err, want) {
				t.Fatal("canceled readiness was accepted", err)
			}
			if err := c.pushPCM(make([]byte, captureSamples*2)); err != nil && !errors.Is(err, want) {
				t.Fatal(err)
			}
			if len(c.input) != 0 {
				t.Fatal("canceled readiness opened capture")
			}
		})
	}
}

func audioPayload(pcm []byte, rate, format uint64) []byte {
	channel := wireBytes(wireBytes(nil, 2, []byte("vqe_0")), 3, pcm)
	channel = wireInt(wireInt(channel, 7, rate), 8, format)
	return wireBytes(nil, 1, channel)
}

func TestProcessedCaptureAssemblyAndCancellation(t *testing.T) {
	e := newStationEpoch(t.Context(), 9)
	defer e.revoke(context.Canceled)
	capture := newStationCapture(e)
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- readCapture(e.ctx, client, capture) }()
	request, err := readIPC(server)
	if err != nil || request.kind != 159 {
		t.Fatal("audio subscription not sent", err)
	}
	pcm := make([]byte, 1280)
	for i := range len(pcm) / 2 {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i))
	}
	for _, part := range [][]byte{pcm[:26], pcm[26:900], pcm[900:]} {
		frame := framedIPC(158, audioPayload(part, 16000, 1))
		for len(frame) > 0 {
			n := min(len(frame), 7)
			if _, err := server.Write(frame[:n]); err != nil {
				t.Fatal(err)
			}
			frame = frame[n:]
		}
	}
	for i := range 2 {
		select {
		case frame := <-capture.detector:
			if frame.epoch != 9 || frame.sequence != uint64(i) {
				t.Fatal("wrong capture epoch/sequence")
			}
			for j, sample := range frame.pcm {
				if sample != int16(i*captureSamples+j) {
					t.Fatal("PCM fragment assembly corrupted samples")
				}
			}
		case <-time.After(time.Second):
			t.Fatal("capture frame missing")
		}
	}
	e.revoke(errAlicePriority)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("capture connection not closed on cancel")
	}
	for _, tc := range []struct {
		pcm          []byte
		rate, format uint64
	}{{[]byte{0}, 16000, 1}, {nil, 48000, 1}, {nil, 16000, 2}} {
		if err := decodeAudio(ipcEnvelope{kind: 158, payload: audioPayload(tc.pcm, tc.rate, tc.format)}, func([]byte) error { return nil }); err == nil {
			t.Fatal("invalid capture format accepted")
		}
	}
	bad := wireBytes(wireBytes(nil, 2, []byte("vqe_0")), 3, nil)
	bad = wireBytes(wireInt(bad, 8, 1), 7, nil)
	if err := decodeAudio(ipcEnvelope{kind: 158, payload: wireBytes(nil, 1, bad)}, func([]byte) error { return nil }); err == nil {
		t.Fatal("wrong audio field wire type accepted")
	}
}

func TestModelOverlayIsPrivateAndLeavesStockUntouched(t *testing.T) {
	stock := t.TempDir()
	flags := []byte("freq-filter-enabled true\nuse-tts-blocker true\nother true\n")
	if err := os.WriteFile(filepath.Join(stock, "flags.txt"), flags, 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := modelOverlay(stock)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("model overlay not private")
	}
	got, err := os.ReadFile(filepath.Join(dir, "flags.txt"))
	if err != nil || string(got) != "freq-filter-enabled false\nuse-tts-blocker false\nother true\n" {
		t.Fatal("wrong private model flags")
	}
	original, err := os.ReadFile(filepath.Join(stock, "flags.txt"))
	if err != nil || !bytes.Equal(original, flags) {
		t.Fatal("stock flags modified")
	}
	for _, name := range []string{"words.txt", "acoustic_model.nnet"} {
		link, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || link != filepath.Join(stock, name) {
			t.Fatal("wrong stock model link")
		}
	}
}

func TestMalformedStateRevokesAndCaptureStallEnds(t *testing.T) {
	for _, payload := range [][]byte{wireBytes(nil, 1, nil), wireInt(nil, 1, 99), nil} {
		t.Run(string(payload), func(t *testing.T) {
			g := newStationGate()
			g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
			e := g.begin(t.Context())
			defer g.finish(e, nil)
			client, server := net.Pipe()
			defer server.Close()
			done := make(chan error, 1)
			go func() { done <- readAlice(e.ctx, client, g) }()
			if _, err := server.Write(framedIPC(3, payload)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-e.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("unknown or malformed state did not directly revoke")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("state reader did not stop on revoked context")
			}
		})
	}
	e := newStationEpoch(t.Context(), 12)
	defer e.revoke(context.Canceled)
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- readCapture(e.ctx, client, newStationCapture(e)) }()
	if _, err := readIPC(server); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled capture ended without an error")
		}
	case <-time.After(audioStallTimeout + time.Second):
		t.Fatal("stalled audio was not bounded")
	}
}
