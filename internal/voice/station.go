package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const detectorQueueFrames = 40
const captureQueueFrames = 600
const setupTimeout = 8 * time.Second

var errAlicePriority = errors.New("native Alice has priority")
var errCleanup = errors.New("resource cleanup failed")

//nolint:staticcheck // Station and Opus are proper names in established user-visible diagnostics.
var errStationAudio = errors.New("Station audio")
var errCaptureOverflow = fmt.Errorf("%w: processed capture queue overflow", errStationAudio)

type voiceCallError struct{ error }

func (e voiceCallError) Unwrap() error { return e.error }

type stationEpoch struct {
	number             uint64
	ctx                context.Context
	cancel             context.CancelCauseFunc
	output             *audio
	report             *log.Logger
	readyPlayerStarted bool
	faultMu            sync.Mutex
	audioFaults        []error
}

func newStationEpoch(parent context.Context, number uint64) *stationEpoch {
	ctx, cancel := context.WithCancelCause(parent)
	e := &stationEpoch{number: number, ctx: ctx, cancel: cancel, report: log.New(io.Discard, "", 0)}
	e.output = newAudio(e)
	return e
}

func (e *stationEpoch) revoke(cause error) {
	if errors.Is(cause, errStationAudio) {
		e.faultMu.Lock()
		e.audioFaults = append(e.audioFaults, cause)
		e.faultMu.Unlock()
	}
	e.cancel(cause)
}

func (e *stationEpoch) preserveAudioFaults(result error) error {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	for _, fault := range e.audioFaults {
		if !errors.Is(result, fault) {
			result = errors.Join(result, fault)
		}
	}
	return result
}

func (e *stationEpoch) allowed() bool {
	return e.ctx.Err() == nil
}

type stationGate struct {
	mu      sync.Mutex
	state   aliceState
	number  uint64
	current *stationEpoch
	changed chan struct{}
}

func newStationGate() *stationGate { return &stationGate{changed: make(chan struct{}, 1)} }

func (g *stationGate) observe(ev nativeEvent) {
	if !ev.activation && !ev.stateChanged {
		return
	}
	g.mu.Lock()
	if ev.activation {
		g.state = aliceUnknown
	} else {
		g.state = ev.state
	}
	if g.state != aliceIdle {
		g.number++
		if g.current != nil {
			g.current.revoke(errAlicePriority)
		}
	}
	g.mu.Unlock()
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

func (g *stationGate) begin(ctx context.Context) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != aliceIdle || g.current != nil || ctx.Err() != nil {
		return nil
	}
	g.number++
	g.current = newStationEpoch(ctx, g.number)
	return g.current
}

func (g *stationGate) activate(e *stationEpoch) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cause := context.Cause(e.ctx); cause != nil {
		return cause
	}
	if g.state != aliceIdle || g.current != e || g.number != e.number {
		return errAlicePriority
	}
	return nil
}

func (g *stationGate) beginCue(parent context.Context, old *stationEpoch) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if parent.Err() != nil || g.current != old || g.number != old.number || g.state != aliceIdle {
		return nil
	}
	old.revoke(context.Canceled)
	g.number++
	g.current = newStationEpoch(parent, g.number)
	return g.current
}

func (g *stationGate) finish(e *stationEpoch, result error) error {
	e.revoke(context.Canceled)
	if cause := context.Cause(e.ctx); errors.Is(cause, errAlicePriority) && !errors.Is(result, cause) {
		result = errors.Join(result, cause)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == e {
		g.current = nil
		g.number++
	}
	return result
}

type capturePhase uint8

const (
	captureDetecting capturePhase = iota
	captureConnecting
	captureStreaming
	captureDraining
)

type stationCapture struct {
	mu       sync.Mutex
	epoch    *stationEpoch
	phase    capturePhase
	partial  captureFrame
	used     int
	detector chan captureFrame
	input    chan captureFrame
}

func newStationCapture(e *stationEpoch) *stationCapture {
	return &stationCapture{
		epoch:    e,
		partial:  captureFrame{epoch: e.number},
		detector: make(chan captureFrame, detectorQueueFrames),
		input:    make(chan captureFrame, captureQueueFrames),
	}
}

func (c *stationCapture) beginConnecting() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureDetecting {
		return errors.New("processed capture is not detecting")
	}
	c.phase = captureConnecting
	c.used = 0
	return nil
}

func (c *stationCapture) beginStreaming(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureConnecting {
		return errors.New("processed capture is not connecting")
	}
	c.phase = captureStreaming
	c.used = 0
	return nil
}

func (c *stationCapture) pushPCM(pcm []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() || c.phase == captureConnecting || c.phase == captureDraining {
		return nil
	}
	output := c.detector
	if c.phase == captureStreaming {
		output = c.input
	}
	for len(pcm) > 0 {
		//nolint:gosec // PCM uses the same 16 bits for signed samples and little-endian words.
		c.partial.pcm[c.used] = int16(binary.LittleEndian.Uint16(pcm))
		pcm = pcm[2:]
		c.used++
		if c.used != captureSamples {
			continue
		}
		if !c.epoch.allowed() {
			return nil
		}
		c.used = 0
		select {
		case output <- c.partial:
			c.partial.sequence++
		default:
			c.epoch.revoke(errCaptureOverflow)
			return errCaptureOverflow
		}
	}
	return nil
}

func (c *stationCapture) beginDraining() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phase = captureDraining
	c.used = 0
}

func (c *stationCapture) read(conn net.Conn) error {
	err := c.readPCM(conn)
	if errors.Is(err, errCaptureOverflow) {
		return err
	}
	if stopped := c.epoch.ctx.Err(); stopped != nil &&
		(errors.Is(err, stopped) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)) {
		return nil
	}
	err = fmt.Errorf("%w: %w", errStationAudio, err)
	c.epoch.revoke(err)
	return err
}

func runStation(parent context.Context, sessionFile string, settings sessionSettings, out io.Writer) error {
	if _, err := readSessionFile(sessionFile); err != nil {
		return configError{err}
	}
	conn, err := dialStation(parent, "127.0.0.1:9875")
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("cannot connect to native Alice state")
	}
	return serveStation(parent, conn, out, func(e *stationEpoch, gate *stationGate) error {
		return runStationEpoch(e, gate, sessionFile, settings)
	}, func(ctx context.Context, frequency cueFrequency) error {
		_, playErr := playCue(ctx, frequency)
		return playErr
	})
}

func serveStation(
	parent context.Context,
	conn net.Conn,
	out io.Writer,
	attempt func(*stationEpoch, *stationGate) error,
	play func(context.Context, cueFrequency) error,
) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gate := newStationGate()
	report := log.New(out, "", log.LstdFlags|log.LUTC)
	defer report.Print("Station voice stopped.")
	stateDone := make(chan error, 1)
	var stateWorker sync.WaitGroup
	stateWorker.Go(func() { stateDone <- readAlice(ctx, conn, gate) })
	defer func() { cancel(); stateWorker.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		default:
		}
		if epoch := gate.begin(ctx); epoch != nil {
			epoch.report = report
			if err := finishStationAttempt(ctx, gate, epoch, attempt(epoch, gate), play); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		case <-gate.changed:
		}
	}
}

func finishStationAttempt(
	ctx context.Context,
	gate *stationGate,
	epoch *stationEpoch,
	err error,
	play func(context.Context, cueFrequency) error,
) error {
	report := epoch.report
	if dropped := epoch.output.playbackDrops.Load(); dropped > 0 {
		report.Printf("Playback queue overflow: %d audio frames dropped.", dropped)
	}
	err = epoch.preserveAudioFaults(err)
	if errors.Is(err, errStationAudio) || errors.Is(err, errCleanup) {
		return gate.finish(epoch, err)
	}
	_, callFailed := errors.AsType[voiceCallError](err)
	var frequency cueFrequency
	switch {
	case err == nil:
		frequency = endCueFrequency
	case callFailed && !epoch.readyPlayerStarted && !errors.Is(err, errAlicePriority):
		frequency = failureCueFrequency
	}
	var cue *stationEpoch
	if frequency != 0 {
		cue = gate.beginCue(ctx, epoch)
	}
	err = gate.finish(epoch, err)
	if callFailed {
		report.Print("Voice attempt failed: ", err)
	}
	if cue != nil {
		cueErr := play(cue.ctx, frequency)
		_ = gate.finish(cue, nil)
		if cueErr != nil && !errors.Is(cueErr, errAlicePriority) && !errors.Is(cueErr, context.Cause(ctx)) {
			report.Print("Voice cue failed: ", cueErr)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case errors.Is(err, errAlicePriority):
		report.Print("Voice yielded to native Alice.")
	case err == nil:
		report.Print("Voice call ended normally.")
	case callFailed, errors.Is(err, context.Canceled):
	default:
		return err
	}
	return nil
}

//nolint:gocognit // Wake admission and detector/capture teardown must stay ordered against Alice revocation.
func runStationEpoch(e *stationEpoch, gate *stationGate, sessionFile string, settings sessionSettings) (result error) {
	detector, err := newNativeSpotter()
	if err != nil {
		return err
	}
	defer func() {
		if detector != nil {
			result = errors.Join(result, detector.close())
		}
	}()
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	conn, err := dialStation(e.ctx, "127.0.0.1:9850")
	if err != nil {
		if stopped := e.ctx.Err(); stopped != nil && errors.Is(err, stopped) {
			return context.Cause(e.ctx)
		}
		return fmt.Errorf("%w: cannot connect to native processed audio: %w", errStationAudio, err)
	}
	capture := newStationCapture(e)
	var reader sync.WaitGroup
	reader.Go(func() { _ = capture.read(conn) })
	defer func() { e.revoke(context.Canceled); reader.Wait() }()
	e.report.Print("Yandex wake detection armed.")
	for {
		select {
		case <-e.ctx.Done():
			return context.Cause(e.ctx)
		case frame := <-capture.detector:
			if !e.allowed() {
				return context.Cause(e.ctx)
			}
			confirmed, attemptErr := detector.feed(frame.pcm[:])
			if attemptErr != nil {
				return attemptErr
			}
			if !confirmed {
				continue
			}
			if activateErr := gate.activate(e); activateErr != nil {
				return activateErr
			}
			if beginConnectingErr := capture.beginConnecting(); beginConnectingErr != nil {
				return beginConnectingErr
			}
			attemptErr = detector.close()
			detector = nil
			if attemptErr != nil {
				return attemptErr
			}
			e.report.Print("Yandex wake confirmed; connecting voice.")
			return runCall(e, sessionFile, capture, settings)
		}
	}
}
