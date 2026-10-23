package voice

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"
)

const maxMessageBytes = 256 << 10

type event struct {
	ended bool
	err   error
}

type sessionSettings struct {
	Voice                 string
	BackendModel          string
	BackendThinkingEffort string
}

func encodeSession(now time.Time, settings sessionSettings) ([]byte, error) {
	_, east := now.Zone()
	zone := now.Location().String()
	if zone == "Local" {
		if tz := os.Getenv("TZ"); tz != "" {
			zone = strings.TrimPrefix(tz, ":")
		}
		if zone == "Local" {
			if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
				if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
					zone = name
				}
			}
		}
		if zone == "Local" {
			zone, _ = now.Zone()
		}
	}
	id := strings.ToUpper(uuid.New().String())
	payload := map[string]any{
		"backend_reasoning_effort": "instant", "language_code": "auto",
		"requested_default_model": "", "voice": cmp.Or(settings.Voice, "orbit"), "voice_session_id": id,
		"voice_status_request_id": id, "timezone_offset_min": -east / 60, "timezone": zone,
		"voice_mode": "wingman", "model_slug": "", "model_slug_advanced": "",
		"client_tools": []string{}, "history_and_training_disabled": false,
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_streaming": true, "backend_model": cmp.Or(settings.BackendModel, "gpt-6-thinking"),
		"deprecate_celsius": false,
	}
	if effort := cmp.Or(settings.BackendThinkingEffort, "extended"); effort != "disabled" {
		payload["backend_thinking_effort"] = effort
	}
	return json.Marshal(payload)
}

type wireEvent struct {
	Type    string         `json:"type"`
	Data    jsontext.Value `json:"data"`
	Payload jsontext.Value `json:"payload"`
}

func decodeEvent(data []byte) (event, error) {
	if len(data) > maxMessageBytes {
		return event{}, errors.New("voice event exceeds 256 KiB")
	}
	var wire wireEvent
	if json.Unmarshal(data, &wire) != nil {
		return event{}, errors.New("invalid voice event JSON")
	}
	if wire.Type == "data_message" {
		var inner string
		if json.Unmarshal(wire.Data, &inner) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
		data = []byte(inner)
		wire = wireEvent{}
		if json.Unmarshal(data, &wire) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
	}
	if len(wire.Payload) > 0 {
		var nested wireEvent
		if json.Unmarshal(wire.Payload, &nested) == nil && nested.Type != "" {
			data = wire.Payload
			wire = nested
		}
	}
	payload := wire.Payload
	if len(payload) == 0 {
		payload = data
	}
	switch wire.Type {
	case "usage_update":
		var p struct {
			Instructions struct {
				HangUp bool `json:"hang_up"`
			} `json:"instructions"`
			Rate struct {
				Exceed   jsontext.Value `json:"exceed_limit_message"`
				Exceeded bool           `json:"exceeded"`
				Reached  bool           `json:"limit_reached"`
			} `json:"rate_limit_message"`
			Audio  *float64 `json:"audio_s"`
			Limits struct {
				Audio struct {
					Remaining *float64 `json:"remaining_seconds"`
				} `json:"audio"`
			} `json:"limits"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return event{}, errors.New("invalid usage event")
		}
		remaining := p.Audio
		if remaining == nil {
			remaining = p.Limits.Audio.Remaining
		}
		exceeded := len(p.Rate.Exceed) > 0 && string(p.Rate.Exceed) != "null" && string(p.Rate.Exceed) != "false" && string(p.Rate.Exceed) != `""`
		if p.Instructions.HangUp {
			return event{ended: true}, nil
		}
		if exceeded || p.Rate.Exceeded || p.Rate.Reached || (remaining != nil && *remaining <= 0) {
			return event{err: errors.New("ChatGPT ended this call because of its usage limit")}, nil
		}
	case "tool_update":
		var p struct {
			Update string `json:"update_type"`
		}
		if json.Unmarshal(payload, &p) == nil && (strings.EqualFold(p.Update, "hangup") || strings.EqualFold(p.Update, "hang_up")) {
			return event{ended: true}, nil
		}
	case "error":
		return event{err: errors.New("ChatGPT reported a voice session error")}, nil
	}
	return event{}, nil
}
