package voice

// #include <stdlib.h>
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/hraban/opus"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const captureSamples = 320
const samplesPerFrame = 960
const frameDuration = 20 * time.Millisecond
const reorderDelay = 3 * frameDuration

type captureFrame struct {
	epoch, sequence uint64
	pcm             [captureSamples]int16
}

type playbackBlock struct {
	epoch uint64
	used  int
	pcm   [samplesPerFrame * 2]int16
}

type audio struct {
	epoch         *stationEpoch
	mu            sync.Mutex
	native        *malgo.Device
	nativeContext *malgo.AllocatedContext
	playback      chan playbackBlock
	playbackDrops atomic.Uint64
	stopping      atomic.Bool
	current       playbackBlock
	currentAt     int
}

func newAudio(epoch *stationEpoch) *audio {
	return &audio{epoch: epoch, playback: make(chan playbackBlock, 12)}
}

func (a *audio) open() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.epoch.allowed() {
		return context.Cause(a.epoch.ctx)
	}
	c, err := malgo.InitContext([]malgo.Backend{malgo.BackendAlsa}, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("%w: cannot initialize ALSA playback: %w", errStationAudio, err)
	}
	a.nativeContext = c
	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 2
	cfg.SampleRate = 48000
	cfg.PeriodSizeInFrames = samplesPerFrame
	var id malgo.DeviceID
	copy(id[:], "default")
	cfg.Playback.DeviceID = id.Pointer()
	defer C.free(cfg.Playback.DeviceID)
	a.native, err = malgo.InitDevice(c.Context, cfg, malgo.DeviceCallbacks{Data: a.callback, Stop: func() {
		if !a.stopping.Load() {
			a.epoch.revoke(fmt.Errorf("%w: playback device stopped", errStationAudio))
		}
	}})
	if err != nil {
		return fmt.Errorf("%w: cannot open ALSA default PCM: %w", errStationAudio, err)
	}
	if !a.epoch.allowed() {
		return context.Cause(a.epoch.ctx)
	}
	if err := a.native.Start(); err != nil {
		return fmt.Errorf("%w: cannot start playback: %w", errStationAudio, err)
	}
	return nil
}

func (a *audio) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping.Store(true)
	var err error
	if a.native != nil {
		err = a.native.Stop()
		a.native.Uninit()
		a.native = nil
	}
	if a.nativeContext != nil {
		err = errors.Join(err, a.nativeContext.Uninit())
		a.nativeContext.Free()
		a.nativeContext = nil
	}
	if err != nil {
		return fmt.Errorf("%w: ALSA playback did not close cleanly: %w", errStationAudio, err)
	}
	return nil
}

func (a *audio) callback(output, _ []byte, frames uint32) {
	clear(output)
	if !a.epoch.allowed() {
		return
	}
	for i := 0; i < min(int(frames), len(output)/4); i++ {
		if !a.epoch.allowed() {
			clear(output)
			return
		}
		for attempt := 0; a.currentAt >= a.current.used && attempt < cap(a.playback); attempt++ {
			select {
			case block := <-a.playback:
				if block.epoch != a.epoch.number {
					continue
				}
				a.current = block
				a.currentAt = 0
			default:
				attempt = cap(a.playback)
			}
		}
		if a.currentAt >= a.current.used {
			continue
		}
		left, right := a.current.pcm[a.currentAt*2], a.current.pcm[a.currentAt*2+1]
		binary.LittleEndian.PutUint16(output[i*4:], uint16(left))
		binary.LittleEndian.PutUint16(output[i*4+2:], uint16(right))
		a.currentAt++
	}
	if !a.epoch.allowed() {
		clear(output)
		return
	}
}

func (a *audio) send(ctx context.Context, track *webrtc.TrackLocalStaticSample, input <-chan captureFrame) error {
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("%w: cannot initialize Opus encoder", errStationAudio)
	}
	if encoder.SetBitrate(24000) != nil {
		return fmt.Errorf("%w: cannot configure Opus encoder", errStationAudio)
	}
	var packet [4000]byte
	var expected uint64
	haveFrame := false
	for {
		var frame captureFrame
		select {
		case <-ctx.Done():
			return nil
		case next, ok := <-input:
			if !ok {
				return nil
			}
			frame = next
		}
		if frame.epoch != a.epoch.number || !a.epoch.allowed() {
			return nil
		}
		if haveFrame && frame.sequence != expected {
			return fmt.Errorf("%w: processed capture discontinuity", errStationAudio)
		}
		expected = frame.sequence + 1
		haveFrame = true
		n, err := encoder.Encode(frame.pcm[:], packet[:])
		if err != nil {
			return fmt.Errorf("%w: Opus capture encoding failed", errStationAudio)
		}
		if !a.epoch.allowed() {
			return nil
		}
		if err := track.WriteSample(media.Sample{Data: packet[:n], Duration: frameDuration}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("audio transport send failed")
		}
	}
}

func (a *audio) queueDecoded(pcm []int16) {
	if !a.epoch.allowed() {
		return
	}
	for len(pcm) > 0 {
		n := min(len(pcm), samplesPerFrame*2)
		block := playbackBlock{epoch: a.epoch.number, used: n / 2}
		copy(block.pcm[:], pcm[:n])
		pcm = pcm[n:]
		select {
		case a.playback <- block:
		default:
			a.playbackDrops.Add(1)
		}
	}
}

func cuePCM(frequency float64) []int16 {
	const frames = 48000 * 150 / 1000
	const fade = 48000 * 10 / 1000
	pcm := make([]int16, frames*2)
	for i := range frames {
		gain := 0.3 * min(1, float64(i)/fade, float64(frames-1-i)/fade)
		sample := int16(gain * math.Sin(2*math.Pi*frequency*float64(i)/48000) * math.MaxInt16)
		pcm[i*2], pcm[i*2+1] = sample, sample
	}
	return pcm
}

func playEndCue(ctx context.Context) error {
	return playCue(ctx, 440)
}

func playCue(parent context.Context, frequency float64) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	pcm := cuePCM(frequency)
	data := make([]byte, len(pcm)*2)
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/aplay", "-q", "-D", "default", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "2")
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Run(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return fmt.Errorf("cannot play voice cue: %w", err)
	}
	return nil
}

type paddingOpusDepacketizer struct {
	codecs.OpusPacket
}

func (p *paddingOpusDepacketizer) Unmarshal(payload []byte) ([]byte, error) {
	// Accept padding so a depacketizer error cannot skip buffered media during purge.
	if len(payload) == 0 {
		return nil, nil
	}
	return p.OpusPacket.Unmarshal(payload)
}

func (a *audio) receive(ctx context.Context, track *webrtc.TrackRemote) error {
	codec := track.Codec()
	if codec.MimeType != webrtc.MimeTypeOpus || codec.ClockRate != 48000 || codec.Channels != 2 {
		return errors.New("unsupported remote audio codec")
	}
	decoder, err := opus.NewDecoder(48000, 2)
	if err != nil {
		return errors.New("cannot initialize Opus decoder")
	}
	pcm := make([]int16, 5760*2)
	var lastReleased uint16
	haveReleased := false
	var nextTimestamp uint32
	havePacket := false
	arrivals := make(map[uint16]time.Time)
	buffer := samplebuilder.New(3, &paddingOpusDepacketizer{}, 48000,
		samplebuilder.WithMaxTimeDelay(reorderDelay),
		samplebuilder.WithPacketReleaseHandler(func(packet *rtp.Packet) {
			delete(arrivals, packet.SequenceNumber)
			lastReleased = packet.SequenceNumber
			haveReleased = true
		}))
	drain := func() error {
		for sample := buffer.Pop(); sample != nil; sample = buffer.Pop() {
			if len(sample.Data) == 0 {
				continue
			}
			if havePacket {
				gap := int32(sample.PacketTimestamp - nextTimestamp)
				if gap > 0 && gap <= 3*samplesPerFrame && gap%samplesPerFrame == 0 {
					for i := int32(0); i < gap; i += samplesPerFrame {
						if decoder.DecodePLC(pcm[:samplesPerFrame*2:samplesPerFrame*2]) != nil {
							return errors.New("Opus loss concealment failed")
						}
						a.queueDecoded(pcm[:samplesPerFrame*2])
					}
				} else if gap != 0 {
					decoder, err = opus.NewDecoder(48000, 2)
					if err != nil {
						return errors.New("cannot replace Opus decoder after discontinuity")
					}
				}
			}
			n, err := decoder.Decode(sample.Data, pcm)
			if err != nil {
				return errors.New("invalid remote Opus packet")
			}
			nextTimestamp = sample.PacketTimestamp + uint32(n)
			havePacket = true
			a.queueDecoded(pcm[:n*2])
		}
		return nil
	}
	stop := context.AfterFunc(ctx, func() { _ = track.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		var deadline time.Time
		for _, arrived := range arrivals {
			if deadline.IsZero() || arrived.Add(reorderDelay).Before(deadline) {
				deadline = arrived.Add(reorderDelay)
			}
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			buffer.Flush()
			if err := drain(); err != nil {
				return err
			}
			continue
		}
		if err := track.SetReadDeadline(deadline); err != nil {
			return errors.New("cannot set remote audio deadline")
		}
		if !a.epoch.allowed() || ctx.Err() != nil {
			return nil
		}
		packet, _, err := track.ReadRTP()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
				continue
			}
			return errors.New("remote audio stream ended")
		}
		if haveReleased && int16(packet.SequenceNumber-lastReleased) <= 0 {
			continue
		}
		if len(packet.Payload) == 0 && !packet.Padding {
			return errors.New("invalid remote Opus packet")
		}
		if _, pending := arrivals[packet.SequenceNumber]; !pending {
			arrivals[packet.SequenceNumber] = time.Now()
		}
		buffer.Push(packet)
		if err := drain(); err != nil {
			return err
		}
	}
}
