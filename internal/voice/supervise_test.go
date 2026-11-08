package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorFiniteRecoveryAndStop(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code, calls int
		wantError   bool
	}{
		{"clean exit", 0, 1, false},
		{"bad configuration", 78, 1, true},
		{"panic exit", 2, 3, true},
		{"recovery exhausted", 1, 3, true},
		{"signal exit", -1, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, pauses := 0, 0
			s := supervisor{
				report: log.New(io.Discard, "", 0),
				ready:  func(context.Context) error { return nil },
				pause:  func(context.Context) error { pauses++; return nil },
				child:  func(context.Context) (int, error) { calls++; return tc.code, nil },
			}
			err := s.run(t.Context())
			if (err != nil) != tc.wantError || calls != tc.calls || pauses != tc.calls-1 {
				t.Fatalf("calls=%d pauses=%d err=%v", calls, pauses, err)
			}
		})
	}
	for _, duringPause := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		s := supervisor{
			report: log.New(io.Discard, "", 0),
			ready:  func(context.Context) error { return nil },
			pause:  func(context.Context) error { cancel(); return ctx.Err() },
			child:  func(context.Context) (int, error) { calls++; return 1, nil },
		}
		if !duringPause {
			cancel()
		}
		if err := s.run(ctx); !errors.Is(err, context.Canceled) || calls > 1 || (!duringPause && calls != 0) {
			t.Fatalf("cancellation started another worker: calls=%d err=%v", calls, err)
		}
		cancel()
	}
}

func TestCappedLogRetainsOnlyTwoBoundedSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.log")
	output, err := openCappedLog(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("0123456789", 21))
	if n, err := output.Write(payload); err != nil || n != len(payload) {
		t.Fatal(n, err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) != 64 || len(current) != 18 || !bytes.Equal(append(previous, current...), payload[len(payload)-82:]) {
		t.Fatal("rotation lost ordering or exceeded the bound")
	}
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	output, err = openCappedLog(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	output.Close()
	current, _ = os.ReadFile(path)
	if !bytes.Equal(current, payload[len(payload)-64:]) {
		t.Fatal("pre-existing oversized log did not retain its recent tail")
	}
}

func TestCappedLogRefusesSymlinks(t *testing.T) {
	for _, suffix := range []string{"", ".1"} {
		path := filepath.Join(t.TempDir(), "client.log")
		target := filepath.Join(t.TempDir(), "untouched")
		if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+suffix); err != nil {
			t.Fatal(err)
		}
		if output, err := openCappedLog(path, 64); err == nil {
			output.Close()
			t.Fatal("symlink accepted")
		}
		if got, _ := os.ReadFile(target); string(got) != "untouched" {
			t.Fatal("log touched the symlink target")
		}
	}
}

type childOutput struct {
	ready  chan struct{}
	signal func()
}

func (w *childOutput) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("test-ready")) {
		w.signal()
	}
	return len(p), nil
}

func TestSupervisorChildFixture(t *testing.T) {
	if os.Getenv("STATION_SUPERVISOR_FIXTURE") == "panic" {
		panic("synthetic worker panic")
	}
	if os.Getenv("STATION_SUPERVISOR_FIXTURE") != "wait" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	fmt.Println("test-ready")
	<-ctx.Done()
}

func TestSupervisorRecoversActualGoPanics(t *testing.T) {
	t.Setenv("STATION_SUPERVISOR_FIXTURE", "panic")
	lock, err := supervisorLock(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	calls := 0
	s := supervisor{
		report: log.New(io.Discard, "", 0),
		ready:  func(context.Context) error { return nil },
		pause:  func(context.Context) error { return nil },
		child: func(ctx context.Context) (int, error) {
			calls++
			return runChild(ctx, os.Args[0], []string{"-test.run=^TestSupervisorChildFixture$"}, lock, io.Discard, log.New(io.Discard, "", 0))
		},
	}
	if err := s.run(t.Context()); err == nil || calls != 3 || ExitCode(err) != 1 {
		t.Fatalf("panic was treated as clean exit or invalid credentials: calls=%d err=%v", calls, err)
	}
	if ExitCode(configError{errors.New("bad saved session")}) != 78 || ExitCode(errors.Join(errStationAudio, context.Canceled)) != 1 {
		t.Fatal("configuration and canceled audio faults have ambiguous exit codes")
	}
}

func TestLogFailureDropsOutputAndRecoversRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.log")
	output, err := openCappedLog(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	output.Write(bytes.Repeat([]byte("a"), 64))
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	if n, err := output.Write([]byte("dropped")); n != 7 || err != nil {
		t.Fatal("log failure escaped to worker pipe", n, err)
	}
	if _, err := output.f.Stat(); err != nil {
		t.Fatal("failed rotation closed the active descriptor", err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	output.Write([]byte("recovered"))
	previous, _ := os.ReadFile(path + ".1")
	current, _ := os.ReadFile(path)
	if len(previous) > 64 || len(current) > 64 || !bytes.Contains(append(previous, current...), []byte("recovered")) {
		t.Fatal("log did not recover with bounded segments")
	}
	output.f.Close()
	if n, err := output.Write([]byte("unwritable")); n != 10 || err != nil {
		t.Fatal("write fault escaped to worker pipe", n, err)
	}
}

func TestSupervisorOwnsChildAndInheritedLock(t *testing.T) {
	t.Setenv("STATION_SUPERVISOR_FIXTURE", "wait")
	dir := filepath.Join(t.TempDir(), "runtime")
	lock, err := supervisorLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := supervisorLock(dir); err == nil {
		other.Close()
		t.Fatal("a second supervisor acquired the live lock")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{})
	output := &childOutput{ready: ready, signal: sync.OnceFunc(func() { close(ready) })}
	done := make(chan error, 1)
	go func() {
		_, err := runChild(ctx, os.Args[0], []string{"-test.run=^TestSupervisorChildFixture$"}, lock, output, log.New(io.Discard, "", 0))
		done <- err
	}()
	select {
	case <-output.ready:
	case err := <-done:
		t.Fatal("worker exited before readiness", err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	lock.Close()
	if other, err := supervisorLock(dir); err == nil {
		other.Close()
		t.Fatal("worker did not retain its supervisor lock")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop and reap its worker")
	}
	other, err := supervisorLock(dir)
	if err != nil {
		t.Fatal("reaped worker retained its lock", err)
	}
	other.Close()
}
