package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestControlBurstsReachTerminalEvent(t *testing.T) {
	for _, burst := range []struct {
		name        string
		count, size int
	}{{"80 small", 80, 32}, {"over 1 MiB total", 5, maxMessageBytes}} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%t", burst.name, reject), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				a, b := localPeer(t, ctx), localPeer(t, ctx)
				connectLocalPeers(t, ctx, a, b)
				data := `{"type":"future_event"}`
				data += strings.Repeat(" ", burst.size-len(data))
				for range burst.count {
					if err := b.dc.SendText(data); err != nil {
						t.Fatal(err)
					}
				}
				terminal := `{"type":"tool_update","update_type":"hangup"}`
				if reject {
					terminal = `{"type":"error","message":"private-marker"}`
				}
				if err := b.dc.SendText(terminal); err != nil {
					t.Fatal(err)
				}
				select {
				case <-a.ctx.Done():
				case <-ctx.Done():
					t.Fatal("terminal event after burst was not received")
				}
				got := a.outcome(context.Cause(a.ctx))
				if reject {
					if got == nil || got.Error() != "ChatGPT reported a voice session error" {
						t.Fatal("burst lost the sanitized rejection", got)
					}
				} else if got != nil {
					t.Fatal("healthy burst did not reach normal hangup", got)
				}
			})
		}
	}
}

func TestControlRejectsMalformedAndBinaryMessages(t *testing.T) {
	for _, binary := range []bool{false, true} {
		t.Run(fmt.Sprintf("binary=%t", binary), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			connectLocalPeers(t, ctx, a, b)
			var err error
			want := "invalid voice event JSON"
			if binary {
				err = b.dc.Send([]byte(`{"type":"tool_update","update_type":"hangup"}`))
				want = "unexpected binary voice control message"
			} else {
				err = b.dc.SendText(`{"private-marker":`)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-a.ctx.Done():
			case <-ctx.Done():
				t.Fatal("invalid control message did not end the call")
			}
			if got := a.outcome(context.Cause(a.ctx)); got == nil || got.Error() != want {
				t.Fatal("wrong or unsafe control rejection", got)
			}
		})
	}
}

func TestControlTerminationCompletionCue(t *testing.T) {
	for _, hangup := range []bool{false, true} {
		t.Run(fmt.Sprintf("hangup=%t", hangup), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			connectLocalPeers(t, ctx, a, b)
			if hangup {
				if err := b.dc.SendText(`{"type":"tool_update","update_type":"hangup"}`); err != nil {
					t.Fatal(err)
				}
			} else if err := b.close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-a.ctx.Done():
			case <-ctx.Done():
				t.Fatal("remote termination did not propagate")
			}
			if err := a.close(); err != nil {
				t.Fatal(err)
			}
			result := a.outcome(context.Cause(a.ctx))
			if (result == nil) != hangup {
				t.Fatal("transport closure was misclassified before delayed OnError", result)
			}
			client, server := net.Pipe()
			defer server.Close()
			rearmed := make(chan struct{})
			done := make(chan error, 1)
			attempts, cues := 0, 0
			go func() {
				done <- serveStation(ctx, client, credential{}, io.Discard, func(e *stationEpoch, _ *stationGate, _ credential) error {
					attempts++
					if attempts == 1 {
						e.revoke(context.Cause(a.ctx))
						if result != nil {
							return voiceCallError{result}
						}
						return nil
					}
					close(rearmed)
					<-e.ctx.Done()
					return context.Cause(e.ctx)
				}, func(context.Context) error { cues++; return nil })
			}()
			if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-rearmed:
			case <-ctx.Done():
				t.Fatal("station did not rearm after termination")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			wantCues := 0
			if hangup {
				wantCues = 1
			}
			if cues != wantCues || attempts != 2 {
				t.Fatalf("got %d completion cues and %d attempts", cues, attempts)
			}
			if !hangup {
				a.channelError(errors.New("private delayed transport details"))
				if got := a.outcome(context.Cause(a.ctx)); !errors.Is(got, errRemoteClosed) || !strings.Contains(got.Error(), "voice control channel failed") || strings.Contains(got.Error(), "private") {
					t.Fatal("delayed failure lost or leaked", got)
				}
			}
		})
	}
}

type blockedControlLog struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockedControlLog) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(data), nil
}

func TestControlFailurePublishedBeforeLog(t *testing.T) {
	w := blockedControlLog{make(chan struct{}), make(chan struct{})}
	p, err := newPeer(t.Context(), webrtc.SettingEngine{}, nil, log.New(w, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	done := make(chan struct{})
	go func() { p.channelError(errors.New("private transport details")); close(done) }()
	defer func() { close(w.release); <-done }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not reach logger")
	}
	select {
	case <-p.ctx.Done():
	default:
		t.Fatal("blocked logger prevented failure cancellation")
	}
	if got := p.outcome(errProtocolHangup); got == nil || got.Error() != "voice control channel failed" {
		t.Fatal("blocked logger hid the sanitized failure", got)
	}
}

func TestControlOutcomePreservesCauses(t *testing.T) {
	failure := errors.New("earlier genuine failure")
	for _, cause := range []error{errRemoteClosed, errAlicePriority, context.Canceled, failure} {
		t.Run(cause.Error(), func(t *testing.T) {
			p := localPeer(t, t.Context())
			p.cancel(cause)
			p.handleMessage([]byte(`{"type":"tool_update","update_type":"hangup"}`))
			if got := p.outcome(context.Cause(p.ctx)); got != cause {
				t.Fatal("late hangup replaced an earlier cause", got)
			}
		})
	}
	p := localPeer(t, t.Context())
	p.handleMessage([]byte(`{"type":"tool_update","update_type":"hangup"}`))
	if got := p.outcome(context.Cause(p.ctx)); got != nil {
		t.Fatal("confirmed hangup was not normal", got)
	}
	for _, result := range []error{fmt.Errorf("wrapped: %w", errProtocolHangup), errors.Join(errProtocolHangup, errCleanup)} {
		if got := p.outcome(result); got != result {
			t.Fatal("normalization erased a wrapped or aggregate cause", got)
		}
	}
	p.fail(failure)
	p.fail(errors.New("later failure"))
	if got := p.outcome(context.Cause(p.ctx)); !errors.Is(got, failure) || strings.Contains(got.Error(), "later failure") {
		t.Fatal("hangup hid or replaced the first genuine failure", got)
	}
}
