package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const detectorQueueFrames = 40
const captureQueueFrames = 600
const setupTimeout = 8 * time.Second

var errAlicePriority = errors.New("native Alice has priority")
var errStationAudio = errors.New("Station audio")
var errCaptureOverflow = fmt.Errorf("%w: processed capture queue overflow", errStationAudio)

type voiceCallError struct{ error }

func (e voiceCallError) Unwrap() error { return e.error }

type stationEpoch struct {
	number  uint64
	ctx     context.Context
	cancel  context.CancelCauseFunc
	revoked atomic.Bool
	output  *audio
}

func newStationEpoch(parent context.Context, number uint64) *stationEpoch {
	ctx, cancel := context.WithCancelCause(parent)
	e := &stationEpoch{number: number, ctx: ctx, cancel: cancel}
	e.output = newAudio(e)
	return e
}

func (e *stationEpoch) revoke(cause error) {
	e.revoked.Store(true)
	e.cancel(cause)
}

func (e *stationEpoch) allowed() bool {
	return !e.revoked.Load() && e.ctx.Err() == nil
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

func (g *stationGate) activate(e *stationEpoch) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state == aliceIdle && g.current == e && g.number == e.number && e.allowed()
}

func (g *stationGate) completion(parent context.Context, old *stationEpoch) *stationEpoch {
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
	if cause := context.Cause(e.ctx); errors.Is(cause, errAlicePriority) {
		result = errors.Join(result, cause)
	}
	e.revoke(context.Canceled)
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
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase == captureConnecting {
		return nil
	}
	output := c.detector
	if c.phase == captureStreaming {
		output = c.input
	}
	for len(pcm) > 0 {
		c.partial.pcm[c.used] = int16(binary.LittleEndian.Uint16(pcm))
		pcm = pcm[2:]
		c.used++
		if c.used != captureSamples {
			continue
		}
		if !c.epoch.allowed() {
			return context.Cause(c.epoch.ctx)
		}
		select {
		case output <- c.partial:
			c.partial.sequence++
			c.used = 0
		default:
			c.epoch.revoke(errCaptureOverflow)
			return errCaptureOverflow
		}
	}
	return nil
}

func runStation(parent context.Context, credentialFile string, out io.Writer) error {
	auth, err := loadCredential(credentialFile)
	if err != nil {
		return err
	}
	conn, err := dialStation(parent, "127.0.0.1:9875")
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("cannot connect to native Alice state")
	}
	return serveStation(parent, conn, auth, out, runStationEpoch, playEndCue)
}

func serveStation(parent context.Context, conn net.Conn, auth credential, out io.Writer, attempt func(*stationEpoch, *stationGate, credential) error, endCue func(context.Context) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gate := newStationGate()
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
			err := attempt(epoch, gate, auth)
			var cue *stationEpoch
			if err == nil {
				cue = gate.completion(ctx, epoch)
			}
			err = gate.finish(epoch, err)
			if cue != nil {
				cueErr := gate.finish(cue, endCue(cue.ctx))
				if cueErr != nil && !errors.Is(cueErr, errAlicePriority) && ctx.Err() == nil {
					fmt.Fprintln(out, "Completion cue failed.")
				}
			}
			if dropped := epoch.output.playbackDrops.Load(); dropped > 0 {
				fmt.Fprintf(out, "Playback queue overflow: %d audio frames dropped.\n", dropped)
			}
			if errors.Is(err, errStationAudio) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !errors.Is(err, errAlicePriority) && !errors.Is(err, context.Canceled) {
				if _, ok := errors.AsType[voiceCallError](err); !ok {
					return err
				}
				fmt.Fprintln(out, "Voice attempt failed:", err)
			}
			fmt.Fprintln(out, "Voice attempt closed; waiting for native Alice IDLE.")
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

func runStationEpoch(e *stationEpoch, gate *stationGate, auth credential) (result error) {
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
		if !e.allowed() {
			return context.Cause(e.ctx)
		}
		return errors.New("cannot connect to native processed audio")
	}
	capture := newStationCapture(e)
	var reader sync.WaitGroup
	reader.Go(func() {
		if err := readCapture(e.ctx, conn, capture); err != nil {
			e.revoke(fmt.Errorf("%w: %w", errStationAudio, err))
		}
	})
	defer func() { e.revoke(context.Canceled); reader.Wait() }()
	for {
		select {
		case <-e.ctx.Done():
			return context.Cause(e.ctx)
		case frame := <-capture.detector:
			if !e.allowed() {
				return context.Cause(e.ctx)
			}
			confirmed, err := detector.feed(frame.pcm[:])
			if err != nil {
				return err
			}
			if !confirmed {
				continue
			}
			if !gate.activate(e) {
				return errAlicePriority
			}
			if err := capture.beginConnecting(); err != nil {
				return err
			}
			err = detector.close()
			detector = nil
			if err != nil {
				return err
			}
			err = runCall(e, auth, capture)
			if cause := context.Cause(e.ctx); errors.Is(cause, errStationAudio) && !errors.Is(err, errStationAudio) {
				return errors.Join(cause, err)
			}
			if err != nil && !errors.Is(err, errStationAudio) {
				return voiceCallError{err}
			}
			return err
		}
	}
}
