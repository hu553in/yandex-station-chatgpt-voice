package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hraban/opus"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func toneFrame(epoch uint64, frame int, hz float64) captureFrame {
	f := captureFrame{epoch: epoch, sequence: uint64(frame)}
	for i := range f.pcm {
		f.pcm[i] = int16(12000 * math.Sin(2*math.Pi*hz*float64(frame*captureSamples+i)/16000))
	}
	return f
}

func tonePower(pcm []byte, hz float64) float64 {
	var sine, cosine float64
	for i := range len(pcm) / 4 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i*4:])))
		angle := 2 * math.Pi * hz * float64(i) / 48000
		sine += sample * math.Sin(angle)
		cosine += sample * math.Cos(angle)
	}
	return sine*sine + cosine*cosine
}

func goodTone(pcm []byte, want, other float64) bool {
	var energy float64
	for i := 0; i < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		energy += v * v
	}
	return energy/float64(len(pcm)/2) > 1e6 && tonePower(pcm, want) > 10*tonePower(pcm, other)
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

type paddingOpusPayloader struct {
	codecs.OpusPayloader
	frames  int
	padding func()
}

func (p *paddingOpusPayloader) Payload(mtu uint16, payload []byte) [][]byte {
	if p.frames == 30 {
		p.padding()
	}
	p.frames++
	return p.OpusPayloader.Payload(mtu, payload)
}

func TestPlaybackBoundsAndRevocation(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	a := e.output
	pcm := make([]int16, samplesPerFrame*2)
	for i := range pcm {
		pcm[i] = 1000
	}
	for range 14 {
		a.queueDecoded(pcm)
	}
	if len(a.playback) != 12 || a.playbackDrops.Load() != 2 {
		t.Fatal("playback queue is not bounded")
	}
	output := make([]byte, 480*4)
	a.callback(output, nil, 480)
	if allZero(output) {
		t.Fatal("consumed PCM produced no output")
	}
	e.revoke(errAlicePriority)
	a.queueDecoded(pcm)
	a.callback(output, nil, 480)
	if !allZero(output) {
		t.Fatal("revoked current or queued PCM escaped")
	}
	for _, state := range []string{"idle", "listening", "speaking", "responding"} {
		if _, err := decodeEvent(testEnvelope(t, `{"type":"state_update","payload":{"new_state":"`+state+`"}}`)); err != nil {
			t.Fatal(err)
		}
		a.callback(output, nil, 480)
		if !allZero(output) {
			t.Fatal("late server state reopened playback")
		}
	}
}

func TestPlaybackSetupCancellationIsNotANativeFault(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	timeout := errors.New("voice connection setup timed out")
	e.revoke(timeout)
	if err := e.output.open(); !errors.Is(err, timeout) || errors.Is(err, errStationAudio) {
		t.Fatal("setup cancellation became a fatal native audio error", err)
	}
}

func TestCuePCMHasDistinctFadedTones(t *testing.T) {
	for _, tc := range []struct {
		name             string
		frequency, other float64
	}{{"ready", 880, 440}, {"completion", 440, 880}} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := cuePCM(tc.frequency)
			if len(pcm) != 7200*2 {
				t.Fatal("cue is not 150 ms of stereo 48 kHz PCM")
			}
			data := make([]byte, len(pcm)*2)
			for i, sample := range pcm {
				if sample > 9831 || sample < -9831 {
					t.Fatal("cue exceeds 0.3 volume")
				}
				if i%2 == 1 && sample != pcm[i-1] {
					t.Fatal("cue stereo channels differ")
				}
				binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
			}
			if !goodTone(data, tc.frequency, tc.other) || !allZero(data[:4]) || !allZero(data[len(data)-4:]) {
				t.Fatal("cue is not the expected distinct faded tone")
			}
		})
	}
}

func TestCanceledCompletionDoesNotStartPlayback(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errAlicePriority)
	if err := playEndCue(ctx); !errors.Is(err, errAlicePriority) {
		t.Fatal("canceled completion attempted playback", err)
	}
}

func TestProductionAudioOverLocalPion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	paddingSent := make(chan error, 2)
	for _, p := range []*peer{a, b} {
		track, err := webrtc.NewTrackLocalStaticSample(p.mic.Codec(), p.mic.ID(), p.mic.StreamID(), webrtc.WithPayloader(func(webrtc.RTPCodecCapability) (rtp.Payloader, error) {
			return &paddingOpusPayloader{padding: func() { paddingSent <- p.mic.GeneratePadding(1) }}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.pc.GetSenders()[0].ReplaceTrack(track); err != nil {
			t.Fatal(err)
		}
		p.mic = track
	}
	connectLocalPeers(t, ctx, a, b)
	epochA, epochB := newStationEpoch(ctx, 1), newStationEpoch(ctx, 2)
	audioA, audioB := epochA.output, epochB.output
	inputA, inputB := make(chan captureFrame, 150), make(chan captureFrame, 150)
	var workers sync.WaitGroup
	failures := make(chan error, 4)
	for _, endpoint := range []struct {
		p     *peer
		audio *audio
		input <-chan captureFrame
	}{{a, audioA, inputA}, {b, audioB, inputB}} {
		workers.Go(func() {
			if err := endpoint.audio.send(ctx, endpoint.p.mic, endpoint.input); err != nil {
				failures <- err
			}
		})
		workers.Go(func() {
			select {
			case track := <-endpoint.p.tracks:
				if err := endpoint.audio.receive(ctx, track); err != nil {
					failures <- err
				}
			case <-ctx.Done():
			}
		})
	}
	t.Cleanup(func() { cancel(); _ = a.close(); _ = b.close(); workers.Wait() })
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	outputA, outputB := make([]byte, samplesPerFrame*4), make([]byte, samplesPerFrame*4)
	goodA, goodB, afterPaddingA, afterPaddingB := 0, 0, 0, 0
	for frame := range 120 {
		select {
		case <-ticker.C:
		case err := <-failures:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("local media timeout")
		}
		inputA <- toneFrame(1, frame, 440)
		inputB <- toneFrame(2, frame, 880)
		audioA.callback(outputA, nil, samplesPerFrame)
		audioB.callback(outputB, nil, samplesPerFrame)
		if frame >= 15 {
			if goodTone(outputA, 880, 440) {
				goodA++
				if frame >= 45 {
					afterPaddingA++
				}
			}
			if goodTone(outputB, 440, 880) {
				goodB++
				if frame >= 45 {
					afterPaddingB++
				}
			}
		}
	}
	for range 2 {
		select {
		case err := <-paddingSent:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("bidirectional padding not sent")
		}
	}
	if goodA < 75 || goodB < 75 || afterPaddingA < 60 || afterPaddingB < 60 {
		t.Fatalf("short decoded tones: A %d/%d B %d/%d", goodA, afterPaddingA, goodB, afterPaddingB)
	}
	epochA.revoke(errAlicePriority)
	for frame := range 3 {
		<-ticker.C
		inputA <- toneFrame(1, frame+120, 440)
		inputB <- toneFrame(2, frame+120, 880)
		audioA.callback(outputA, nil, samplesPerFrame)
		audioB.callback(outputB, nil, samplesPerFrame)
		if !allZero(outputA) {
			t.Fatal("native priority leaked playback")
		}
	}
	cancel()
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	t.Logf("16k mono Opus to 48k stereo playback through local SRTP, A %d tone frames, B %d tone frames; padding and irreversible native priority passed", goodA, goodB)
}

func TestRemoteDecoderReplacedOnTimestampDiscontinuity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	connectLocalPeers(t, ctx, a, b)
	e := newStationEpoch(ctx, 1)
	defer e.revoke(context.Canceled)
	done := make(chan error, 1)
	go func() {
		select {
		case track := <-a.tracks:
			done <- e.output.receive(e.ctx, track)
		case <-ctx.Done():
			done <- ctx.Err()
		}
	}()
	t.Cleanup(func() { cancel(); _ = a.close(); _ = b.close(); <-done })
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		f := toneFrame(1, i, 440)
		var packet [4000]byte
		n, err := encoder.Encode(f.pcm[:], packet[:])
		if err != nil {
			t.Fatal(err)
		}
		duration := frameDuration
		if i == 0 {
			duration = 200 * time.Millisecond
		}
		if err := b.mic.WriteSample(media.Sample{Data: packet[:n], Duration: duration}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(e.output.playback) < 3 {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("decoder did not survive timestamp gap: %v", err)
		case <-deadline.C:
			t.Fatal("decoded audio after gap missing")
		case <-tick.C:
		}
	}
}

func TestCaptureUses48kRTPClockAndStopsOnRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	connectLocalPeers(t, ctx, a, b)
	e := newStationEpoch(ctx, 11)
	defer e.revoke(context.Canceled)
	input := make(chan captureFrame, 20)
	for i := range cap(input) {
		input <- toneFrame(11, i, 440)
	}
	done := make(chan error, 1)
	go func() { done <- e.output.send(e.ctx, a.mic, input) }()
	t.Cleanup(func() { e.revoke(context.Canceled); _ = a.close(); _ = b.close() })
	var track *webrtc.TrackRemote
	select {
	case track = <-b.tracks:
	case <-ctx.Done():
		t.Fatal("no microphone RTP")
	}
	if err := track.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, _, err := track.ReadRTP()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := track.ReadRTP()
	if err != nil {
		t.Fatal(err)
	}
	if second.Timestamp-first.Timestamp != 960 {
		t.Fatalf("wrong RTP clock increment %d", second.Timestamp-first.Timestamp)
	}
	e.revoke(errAlicePriority)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("revoked microphone sender did not finish")
	}
}

func TestRemoteAudioReorderingAndLoss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   uint16
		arrival []int
		want    []int
		padding bool
	}{
		{"reordered", 100, []int{0, 2, 1, 3}, []int{0, 1, 2, 3}, false},
		{"duplicate", 100, []int{0, 2, 1, 1, 3}, []int{0, 1, 2, 3}, false},
		{"sequence wrap", 65534, []int{0, 2, 1, 3}, []int{0, 1, 2, 3}, false},
		{"missing", 100, []int{0, 2, 3}, []int{0, -1, 2, 3}, false},
		{"padding after tail", 100, []int{0}, []int{0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			raw, err := webrtc.NewTrackLocalStaticRTP(b.mic.Codec(), b.mic.ID(), b.mic.StreamID())
			if err != nil {
				t.Fatal(err)
			}
			if err := b.pc.GetSenders()[0].ReplaceTrack(raw); err != nil {
				t.Fatal(err)
			}
			connectLocalPeers(t, ctx, a, b)
			e := newStationEpoch(ctx, 1)
			var paddingWorker sync.WaitGroup
			done := make(chan error, 1)
			go func() {
				select {
				case track := <-a.tracks:
					done <- e.output.receive(e.ctx, track)
				case <-ctx.Done():
					done <- ctx.Err()
				}
			}()
			t.Cleanup(func() {
				e.revoke(context.Canceled)
				cancel()
				_ = a.close()
				_ = b.close()
				paddingWorker.Wait()
				<-done
			})
			encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
			if err != nil {
				t.Fatal(err)
			}
			packets := make([][]byte, 4)
			for i := range packets {
				frame := toneFrame(1, i, 440+float64(i)*220)
				buf := make([]byte, 4000)
				n, err := encoder.Encode(frame.pcm[:], buf)
				if err != nil {
					t.Fatal(err)
				}
				packets[i] = buf[:n]
			}
			for _, i := range tc.arrival {
				if err := raw.WriteRTP(&rtp.Packet{Version: 2, SequenceNumber: tc.start + uint16(i), Timestamp: 1000 + uint32(i)*960, Payload: packets[i]}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.padding {
				paddingWorker.Go(func() {
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for sequence := tc.start + 1; ; sequence++ {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
						}
						if err := raw.WriteRTP(&rtp.Packet{Version: 2, SequenceNumber: sequence, Timestamp: 1960, Padding: true, PaddingSize: 1}); err != nil {
							if ctx.Err() == nil {
								t.Error(err)
							}
							return
						}
					}
				})
			}
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			decoder, err := opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range tc.want {
				pcm := make([]int16, samplesPerFrame*2)
				n := samplesPerFrame
				if i < 0 {
					err = decoder.DecodePLC(pcm)
				} else {
					n, err = decoder.Decode(packets[i], pcm)
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-e.output.playback:
					if got.used != n || !slices.Equal(got.pcm[:n*2], pcm[:n*2]) {
						t.Fatal("RTP order or loss concealment corrupted decoded speech")
					}
				case err := <-done:
					done <- err
					t.Fatal("remote receiver ended", err)
				case <-ctx.Done():
					t.Fatal("reorder window did not release the audio tail")
				case <-deadline.C:
					t.Fatal("background RTP postponed the audio playout deadline")
				}
			}
		})
	}
}

func TestRemoteAudioAfterLongPadding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   uint16
		padding []int
	}{
		{"half range", 100, []int{33000}},
		{"repeated wraps", 65530, []int{33000, 65535, 98536}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			raw, err := webrtc.NewTrackLocalStaticRTP(b.mic.Codec(), b.mic.ID(), b.mic.StreamID())
			if err != nil {
				t.Fatal(err)
			}
			if err := b.pc.GetSenders()[0].ReplaceTrack(raw); err != nil {
				t.Fatal(err)
			}
			connectLocalPeers(t, ctx, a, b)
			e := newStationEpoch(ctx, 1)
			done := make(chan error, 1)
			go func() {
				select {
				case track := <-a.tracks:
					done <- e.output.receive(e.ctx, track)
				case <-ctx.Done():
					done <- ctx.Err()
				}
			}()
			t.Cleanup(func() { e.revoke(context.Canceled); cancel(); _ = a.close(); _ = b.close(); <-done })
			encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
			if err != nil {
				t.Fatal(err)
			}
			decoder, err := opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatal(err)
			}
			packets := make([][]byte, 5)
			for i := range packets {
				frame := toneFrame(1, i, 440+float64(i)*220)
				buf := make([]byte, 4000)
				n, err := encoder.Encode(frame.pcm[:], buf)
				if err != nil {
					t.Fatal(err)
				}
				packets[i] = buf[:n]
			}
			write := func(sequence uint16, timestamp uint32, payload []byte) {
				t.Helper()
				packet := &rtp.Packet{Version: 2, SequenceNumber: sequence, Timestamp: timestamp, Payload: payload}
				if payload == nil {
					packet.Padding, packet.PaddingSize = true, 1
				}
				if err := raw.WriteRTP(packet); err != nil {
					t.Fatal(err)
				}
			}
			read := func(i int) {
				t.Helper()
				pcm := make([]int16, samplesPerFrame*2)
				if i < 0 {
					err = decoder.DecodePLC(pcm)
				} else {
					_, err = decoder.Decode(packets[i], pcm)
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-e.output.playback:
					if got.used != samplesPerFrame || !slices.Equal(got.pcm[:], pcm) {
						t.Fatal("padding changed Opus history, RTP order, or loss concealment")
					}
				case err := <-done:
					done <- err
					t.Fatal("receiver ended", err)
				case <-time.After(time.Second):
					t.Fatal("valid speech after padding was not decoded")
				}
			}
			sequence, timestamp := tc.start, uint32(1000)
			write(sequence, timestamp, packets[0])
			read(0)
			for _, padding := range tc.padding {
				for i := range padding {
					sequence++
					write(sequence, timestamp+samplesPerFrame, nil)
					if i%50 == 49 {
						time.Sleep(time.Millisecond)
					}
				}
				sequence++
				timestamp += samplesPerFrame
				for _, i := range []int{0, 2, 1, 1, 4} {
					write(sequence+uint16(i), timestamp+uint32(i)*samplesPerFrame, packets[i])
				}
				for _, i := range []int{0, 1, 2, -1, 4} {
					read(i)
				}
				write(sequence+3, timestamp+3*samplesPerFrame, packets[3])
				write(sequence+4, timestamp+4*samplesPerFrame, packets[4])
				select {
				case <-e.output.playback:
					t.Fatal("late or duplicate media was played after the reorder deadline")
				case err := <-done:
					done <- err
					t.Fatal("receiver ended", err)
				case <-time.After(2 * reorderDelay):
				}
				sequence += 4
				timestamp += 4 * samplesPerFrame
				t.Logf("speech and decoder history preserved after %d padding packets", padding)
			}
			if err := raw.WriteRTP(&rtp.Packet{Version: 2, SequenceNumber: sequence + 1, Timestamp: timestamp + samplesPerFrame}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				done <- err
				if err == nil || err.Error() != "invalid remote Opus packet" {
					t.Fatal("empty non-padding RTP was not rejected", err)
				}
			case <-ctx.Done():
				t.Fatal("empty non-padding RTP did not end the receiver")
			}
		})
	}
}

func TestRemoteAudioLatePaddingBeforeBufferedSpeech(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	raw, err := webrtc.NewTrackLocalStaticRTP(b.mic.Codec(), b.mic.ID(), b.mic.StreamID())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.GetSenders()[0].ReplaceTrack(raw); err != nil {
		t.Fatal(err)
	}
	connectLocalPeers(t, ctx, a, b)
	e := newStationEpoch(ctx, 1)
	done := make(chan error, 1)
	go func() {
		select {
		case track := <-a.tracks:
			done <- e.output.receive(e.ctx, track)
		case <-ctx.Done():
			done <- ctx.Err()
		}
	}()
	t.Cleanup(func() { e.revoke(context.Canceled); cancel(); _ = a.close(); _ = b.close(); <-done })
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	packets := make([][]byte, 4)
	for i := range packets {
		frame := toneFrame(1, i, 440)
		data := make([]byte, 4000)
		n, err := encoder.Encode(frame.pcm[:], data)
		if err != nil {
			t.Fatal(err)
		}
		packets[i] = data[:n]
	}
	write := func(sequence uint16, timestamp uint32, payload []byte) {
		t.Helper()
		packet := &rtp.Packet{Version: 2, SequenceNumber: sequence, Timestamp: timestamp, Payload: payload}
		if payload == nil {
			packet.Padding, packet.PaddingSize = true, 1
		}
		if err := raw.WriteRTP(packet); err != nil {
			t.Fatal(err)
		}
	}
	write(100, 1000, packets[0])
	write(101, 1960, nil)
	write(102, 1960, packets[1])
	write(104, 12520, packets[2])
	write(105, 13480, packets[3])
	write(103, 2920, nil)

	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	var blocks []playbackBlock
	for len(blocks) < len(packets) {
		select {
		case block := <-e.output.playback:
			blocks = append(blocks, block)
		case err := <-done:
			done <- err
			t.Fatal("receiver ended", err)
		case <-deadline.C:
			t.Fatalf("decoded %d of 4 valid media packets after reordered padding", len(blocks))
		}
	}
	decoder, err := opus.NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, block := range blocks {
		if i == 2 {
			decoder, err = opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatal(err)
			}
		}
		pcm := make([]int16, samplesPerFrame*2)
		n, err := decoder.Decode(packets[i], pcm)
		if err != nil {
			t.Fatal(err)
		}
		if block.used != n || !slices.Equal(block.pcm[:n*2], pcm[:n*2]) {
			t.Fatalf("reordered padding changed decoded PCM for media packet %d", i)
		}
	}
	t.Log("decoded all 4 media packets with exact PCM after reordered padding and timestamp discontinuity")
}
