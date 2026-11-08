package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func finishPlayback(t *testing.T, e *stationEpoch) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- e.output.finish(e.ctx) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !e.output.finishing.Load() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("playback did not begin finishing")
		}
	}
	return done
}

func TestPlaybackFinishesAcceptedSamplesBeforeAcknowledging(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	for _, value := range []int16{1000, 2000} {
		pcm := make([]int16, samplesPerFrame*2)
		for i := range pcm {
			pcm[i] = value
		}
		e.output.queueDecoded(pcm)
	}
	output := make([]byte, samplesPerFrame*4)
	e.output.callback(output[:480*4], nil, 480)
	done := finishPlayback(t, e)
	e.output.queueDecoded([]int16{3000, 3000})
	for _, tc := range []struct{ frames, sample int }{{480, 1000}, {960, 2000}} {
		//nolint:gosec // Fixture indices and payload lengths are bounded by the literal test cases.
		e.output.callback(output[:tc.frames*4], nil, uint32(tc.frames))
		for i := range tc.frames * 2 {
			//nolint:gosec // PCM uses the same 16 bits for signed samples and little-endian words.
			if got := int16(binary.LittleEndian.Uint16(output[i*2:])); got != int16(tc.sample) {
				t.Fatalf("accepted sample became %d, want %d", got, tc.sample)
			}
		}
		select {
		case err := <-done:
			t.Fatalf("finish acknowledged a callback still carrying PCM: %v", err)
		default:
		}
	}
	e.output.callback(output, nil, samplesPerFrame)
	if !allZero(output) {
		t.Fatal("post-hangup PCM was admitted")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("silent callback did not acknowledge delivered audio")
	}
}

func TestAliceAbortsPlaybackFinish(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	e.output.queueDecoded([]int16{1000, 1000})
	done := finishPlayback(t, e)
	e.revoke(errAlicePriority)
	output := make([]byte, samplesPerFrame*4)
	e.output.callback(output, nil, samplesPerFrame)
	if !allZero(output) {
		t.Fatal("Alice interruption leaked queued PCM")
	}
	if err := <-done; !errors.Is(err, errAlicePriority) {
		t.Fatalf("Alice interruption became %v", err)
	}
}

func TestPlaybackFinishWithoutCallbacksIsAnAudioFault(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	if err := e.output.finish(e.ctx); !errors.Is(err, errStationAudio) {
		t.Fatalf("stalled playback reported completion: %v", err)
	}
}

func TestCaptureDrainingDiscardsMicrophoneFrames(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	c := newStationCapture(e)
	if err := c.beginConnecting(); err != nil {
		t.Fatal(err)
	}
	if err := c.beginStreaming(e.ctx); err != nil {
		t.Fatal(err)
	}
	c.beginDraining()
	if err := c.pushPCM(make([]byte, captureSamples*2*(captureQueueFrames+1))); err != nil {
		t.Fatal(err)
	}
	if len(c.input) != 0 || len(c.detector) != 0 || !e.allowed() {
		t.Fatal("finishing call retained microphone audio or overflowed")
	}
}
