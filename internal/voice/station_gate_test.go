package voice

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestEpochRevocationPublishesCause(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	cancel := e.cancel
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.cancel = func(cause error) {
		close(entered)
		<-release
		cancel(cause)
	}
	go func() {
		defer close(done)
		e.revoke(errCaptureOverflow)
	}()
	<-entered
	missingCause := !e.allowed() && context.Cause(e.ctx) == nil
	close(release)
	<-done
	if missingCause {
		t.Fatal("revocation was visible before its cancellation cause")
	}
	if e.allowed() || !errors.Is(context.Cause(e.ctx), errCaptureOverflow) {
		t.Fatal("revocation lost the native capture failure")
	}
}

func TestWakeAdmissionPreservesFatalCaptureFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	client, server := net.Pipe()
	done := make(chan error, 1)
	returned := make(chan struct{})
	attempts, cues := 0, 0
	go func() {
		defer close(returned)
		done <- serveStation(ctx, client, credential{}, io.Discard, func(e *stationEpoch, gate *stationGate, _ credential) error {
			attempts++
			if attempts > 1 {
				return errors.New("unexpected rearm after native capture failure")
			}
			e.revoke(errCaptureOverflow)
			return gate.activate(e)
		}, func(context.Context) error {
			cues++
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); server.Close(); <-returned })
	if err := server.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write(framedIPC(3, wireInt(nil, 1, 0))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errCaptureOverflow) {
			t.Fatal("wake admission discarded the native failure", err)
		}
		if attempts != 1 || cues != 0 {
			t.Fatalf("native failure caused %d attempts and %d completion cues", attempts, cues)
		}
	case <-ctx.Done():
		t.Fatal("native failure did not stop the service")
	}
}
