package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const maxIPCBytes = 10 << 20
const audioStallTimeout = 3 * time.Second

type aliceState uint8

const (
	aliceUnknown aliceState = iota
	aliceIdle
	aliceListening
	aliceSpeaking
	aliceBusy
	aliceShazam
	aliceNone
)

type nativeEvent struct {
	state        aliceState
	stateChanged bool
	activation   bool
}

type ipcEnvelope struct {
	kind    uint64
	payload []byte
}

type wireField struct {
	number  protowire.Number
	typ     protowire.Type
	integer uint64
	bytes   []byte
}

func wireFields(data []byte, visit func(wireField) error) error {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 || num < 1 || num > protowire.MaxValidNumber {
			return errors.New("invalid IPC protobuf tag")
		}
		data = data[n:]
		f := wireField{number: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.integer, n = protowire.ConsumeVarint(data)
		case protowire.BytesType:
			f.bytes, n = protowire.ConsumeBytes(data)
		default:
			n = protowire.ConsumeFieldValue(num, typ, data)
		}
		if n < 0 {
			return errors.New("invalid IPC protobuf value")
		}
		if err := visit(f); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (f wireField) require(typ protowire.Type, seen *bool) error {
	if f.typ != typ || *seen {
		return errors.New("wrong type or duplicate IPC field")
	}
	*seen = true
	return nil
}

func decodeIPC(data []byte) (ipcEnvelope, error) {
	var e ipcEnvelope
	var kind, payload bool
	err := wireFields(data, func(f wireField) error {
		switch f.number {
		case 252:
			if err := f.require(protowire.BytesType, &payload); err != nil {
				return err
			}
			e.payload = f.bytes
		case 253:
			if err := f.require(protowire.VarintType, &kind); err != nil {
				return err
			}
			e.kind = f.integer
		}
		return nil
	})
	if err != nil {
		return e, err
	}
	if !kind || !payload {
		return e, errors.New("missing IPC envelope fields")
	}
	return e, nil
}

func readIPC(in io.Reader) (ipcEnvelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return ipcEnvelope{}, err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size == 0 || size > maxIPCBytes {
		return ipcEnvelope{}, errors.New("IPC frame exceeds size bound")
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(in, body); err != nil {
		return ipcEnvelope{}, err
	}
	return decodeIPC(body)
}

func audioRequest() []byte {
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, nil)
	body := protowire.AppendTag(nil, 252, protowire.BytesType)
	body = protowire.AppendBytes(body, payload)
	body = protowire.AppendTag(body, 253, protowire.VarintType)
	body = protowire.AppendVarint(body, 159)
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(body)))
	return append(out, body...)
}

func decodeAlice(data []byte) (aliceState, error) {
	state := aliceUnknown
	seen := false
	err := wireFields(data, func(f wireField) error {
		if f.number != 1 {
			return nil
		}
		if err := f.require(protowire.VarintType, &seen); err != nil {
			return err
		}
		switch f.integer {
		case 0:
			state = aliceIdle
		case 1:
			state = aliceListening
		case 2:
			state = aliceSpeaking
		case 3:
			state = aliceBusy
		case 5:
			state = aliceShazam
		case 6:
			state = aliceNone
		}
		return nil
	})
	return state, err
}

func decodeNativeEvent(e ipcEnvelope) (nativeEvent, error) {
	if e.kind == 3 {
		state, err := decodeAlice(e.payload)
		return nativeEvent{state: state, stateChanged: true}, err
	}
	if e.kind != 210 {
		return nativeEvent{}, nil
	}
	var capability, alice, activation []byte
	var haveCapability, haveAlice, haveActivation bool
	err := wireFields(e.payload, func(f wireField) error {
		switch f.number {
		case 7:
			if err := f.require(protowire.BytesType, &haveCapability); err != nil {
				return err
			}
			capability = f.bytes
		case 18:
			if err := f.require(protowire.BytesType, &haveAlice); err != nil {
				return err
			}
			alice = f.bytes
		case 15:
			if err := f.require(protowire.BytesType, &haveActivation); err != nil {
				return err
			}
			activation = f.bytes
		}
		return nil
	})
	if err != nil {
		return nativeEvent{}, err
	}
	var listener []byte
	switch string(capability) {
	case "AliceCapability":
		if !haveAlice {
			return nativeEvent{}, errors.New("missing Alice listener")
		}
		listener = alice
	case "ActivationSpotterCapability":
		if !haveActivation {
			return nativeEvent{}, errors.New("missing activation listener")
		}
		listener = activation
	default:
		return nativeEvent{}, nil
	}
	var method uint64
	var state []byte
	var haveMethod, haveState, havePhrase bool
	err = wireFields(listener, func(f wireField) error {
		switch f.number {
		case 1:
			if err := f.require(protowire.VarintType, &haveMethod); err != nil {
				return err
			}
			method = f.integer
		case 2:
			if string(capability) == "AliceCapability" {
				if err := f.require(protowire.BytesType, &haveState); err != nil {
					return err
				}
				state = f.bytes
			}
		case 3:
			if string(capability) == "ActivationSpotterCapability" {
				return f.require(protowire.BytesType, &havePhrase)
			}
		}
		return nil
	})
	if err != nil {
		return nativeEvent{}, err
	}
	if !haveMethod {
		return nativeEvent{}, errors.New("missing native listener method")
	}
	if string(capability) == "ActivationSpotterCapability" {
		return nativeEvent{activation: method == 2}, nil
	}
	if method != 0 {
		return nativeEvent{}, nil
	}
	if !haveState {
		return nativeEvent{}, errors.New("missing Alice listener state")
	}
	value, err := decodeAlice(state)
	return nativeEvent{state: value, stateChanged: true}, err
}

func decodeAudio(e ipcEnvelope, consume func([]byte) error) error {
	if e.kind != 158 {
		return nil
	}
	return wireFields(e.payload, func(f wireField) error {
		if f.number != 1 {
			return nil
		}
		if f.typ != protowire.BytesType {
			return errors.New("wrong audio channel type")
		}
		var name, pcm []byte
		var rate, format uint64
		var haveName, havePCM, haveRate, haveFormat bool
		err := wireFields(f.bytes, func(c wireField) error {
			switch c.number {
			case 2:
				if err := c.require(protowire.BytesType, &haveName); err != nil {
					return err
				}
				name = c.bytes
			case 3:
				if err := c.require(protowire.BytesType, &havePCM); err != nil {
					return err
				}
				pcm = c.bytes
			case 7:
				if err := c.require(protowire.VarintType, &haveRate); err != nil {
					return err
				}
				rate = c.integer
			case 8:
				if err := c.require(protowire.VarintType, &haveFormat); err != nil {
					return err
				}
				format = c.integer
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !haveName {
			return errors.New("missing audio channel name")
		}
		if string(name) != "vqe_0" {
			return nil
		}
		if !havePCM || !haveRate || !haveFormat || rate != 16000 || format != 1 || len(pcm)%2 != 0 {
			return errors.New("invalid processed audio format")
		}
		return consume(pcm)
	})
}

func dialStation(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", address)
}

func readAlice(ctx context.Context, conn net.Conn, gate *stationGate) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	for {
		e, err := readIPC(conn)
		if err != nil {
			gate.observe(nativeEvent{stateChanged: true})
			return err
		}
		ev, err := decodeNativeEvent(e)
		if err != nil {
			gate.observe(nativeEvent{stateChanged: true})
			return err
		}
		gate.observe(ev)
	}
}

func readCapture(ctx context.Context, conn net.Conn, capture *stationCapture) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(audioStallTimeout)); err != nil {
		return err
	}
	request := audioRequest()
	for len(request) > 0 {
		n, err := conn.Write(request)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		request = request[n:]
	}
	progress := time.Now()
	for {
		if err := conn.SetReadDeadline(progress.Add(audioStallTimeout)); err != nil {
			return err
		}
		e, err := readIPC(conn)
		if err != nil {
			return err
		}
		if err := decodeAudio(e, func(pcm []byte) error {
			if len(pcm) > 0 {
				progress = time.Now()
			}
			if err := capture.pushPCM(pcm); err != nil {
				return err
			}
			return ctx.Err()
		}); err != nil {
			return err
		}
	}
}
