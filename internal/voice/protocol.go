package voice

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"
)

const localTimezoneName = "Local"
const unknownValue = "unknown"

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
	//nolint:nestif // Preserve timezone fallback order from TZ through zoneinfo to the zone abbreviation.
	if zone == localTimezoneName {
		if tz := os.Getenv("TZ"); tz != "" {
			zone = strings.TrimPrefix(tz, ":")
		}
		if zone == localTimezoneName {
			if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
				if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
					zone = name
				}
			}
		}
		if zone == localTimezoneName {
			zone, _ = now.Zone()
		}
	}
	id := strings.ToUpper(uuid.New().String())
	payload := map[string]any{
		"backend_reasoning_effort": "instant", "language_code": "auto",
		"requested_default_model": "", "voice": cmp.Or(settings.Voice, "orbit"), "voice_session_id": id,
		"voice_status_request_id": id, "timezone_offset_min": -east / int(time.Minute/time.Second), "timezone": zone,
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

//nolint:cyclop,funlen,gocognit,gocyclo // Terminal-event precedence and bounded nested-envelope decoding share one wire contract.
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
		exceeded := len(p.Rate.Exceed) > 0 && string(p.Rate.Exceed) != "null" && string(p.Rate.Exceed) != "false" &&
			string(p.Rate.Exceed) != `""`
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
		if json.Unmarshal(payload, &p) == nil &&
			(strings.EqualFold(p.Update, "hangup") || strings.EqualFold(p.Update, "hang_up")) {
			return event{ended: true}, nil
		}
	case "goodbye":
		var p struct {
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return event{}, errors.New("invalid goodbye event")
		}
		switch p.Reason {
		case "error", "cap_reached", "normal", "client_request", "timeout", "session_expired":
		default:
			p.Reason = unknownValue
		}
		switch p.Detail {
		case "internal_error",
			"upstream_error",
			"server_error",
			"unauthorized",
			"quota_exceeded",
			"rate_limit_exceeded",
			"invalid_session",
			"invalid_model",
			"unsupported_country":
		default:
			p.Detail = unknownValue
		}
		if p.Reason == "cap_reached" {
			return event{err: errors.New("ChatGPT ended this call because of its usage limit")}, nil
		}
		if (p.Reason == "normal" || p.Reason == "client_request") && p.Detail == unknownValue {
			return event{ended: true}, nil
		}
		return event{
			err: fmt.Errorf("ChatGPT ended this voice session (reason=%s, detail=%s)", p.Reason, p.Detail),
		}, nil
	case "error", "session.error":
		return event{err: errors.New("ChatGPT reported a voice session error")}, nil
	}
	return event{}, nil
}
