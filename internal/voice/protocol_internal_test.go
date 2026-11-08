package voice

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testEnvelope(t *testing.T, inner string) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{"data_message", inner})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWebsiteProtocol(t *testing.T) {
	session, err := encodeSession(
		time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("Asia/Kolkata", 19800)),
		sessionSettings{},
	)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err = json.Unmarshal(session, &values); err != nil {
		t.Fatal(err)
	}
	if values["timezone_offset_min"] != float64(-330) || values["timezone"] != "Asia/Kolkata" {
		t.Fatal("wrong local timezone", values)
	}
	id := values["voice_session_id"].(string)
	if id != strings.ToUpper(id) || len(id) != 36 || values["voice_status_request_id"] != id {
		t.Fatal("session IDs do not match")
	}
	for key, want := range map[string]any{"voice": "orbit", "voice_mode": "wingman", "backend_model": "gpt-6-thinking", "backend_thinking_effort": "extended", "deprecate_celsius": false, "requested_default_model": "", "model_slug": "", "model_slug_advanced": "", "enable_message_streaming": true} {
		if values[key] != want {
			t.Fatalf("%s=%v", key, values[key])
		}
	}
}

func TestDisplayEventsIgnored(t *testing.T) {
	for _, raw := range []string{
		`{"type":"chat_message_delta","delta":{"v":{"message":{"content":{"parts":[{"text":"private words"}]}}}}}`,
		`{"type":"state_update","payload":{"new_state":"speaking"}}`,
		`{"type":"state_update","payload":{"new_state":{"future":true}}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || ev != (event{}) {
			t.Fatalf("display event affected runtime: %+v %v", ev, err)
		}
	}
	if _, err := decodeEvent(make([]byte, maxMessageBytes+1)); err == nil {
		t.Fatal("unbounded event accepted")
	}
}

func TestUnknownEventDataShapes(t *testing.T) {
	for _, data := range []string{`{"extra":true}`, `[1,"extra"]`, `"extra"`, `42`, `true`, `null`} {
		t.Run(data, func(t *testing.T) {
			raw := `{"type":"future_event","data":` + data + `}`
			for _, input := range [][]byte{[]byte(raw), testEnvelope(t, raw), testEnvelope(t, `{"payload":`+raw+`}`)} {
				ev, err := decodeEvent(input)
				if err != nil || ev != (event{}) {
					t.Fatalf("unknown event returned %+v, %v", ev, err)
				}
			}
		})
	}
}

func TestMalformedDataMessage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"data_message"}`,
		`{"type":"data_message","data":null}`,
		`{"type":"data_message","data":{"extra":true}}`,
		`{"type":"data_message","data":[1,"extra"]}`,
		`{"type":"data_message","data":42}`,
		`{"type":"data_message","data":true}`,
		`{"type":"data_message","data":""}`,
		`{"type":"data_message","data":"invalid JSON"}`,
	} {
		if _, err := decodeEvent([]byte(raw)); err == nil {
			t.Errorf("malformed data_message accepted: %s", raw)
		}
	}
}

func TestRemoteEnd(t *testing.T) {
	for _, raw := range []string{
		`{"type":"usage_update","audio_s":0}`,
		`{"type":"usage_update","audio_s":-1}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":0}}}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":-1}}}`,
		`{"type":"usage_update","rate_limit_message":{"limit_reached":true}}`,
		`{"type":"usage_update","rate_limit_message":{"exceed_limit_message":"limit exceeded"}}`,
		`{"type":"usage_update","rate_limit_message":{"exceeded":true}}`,
		`{"type":"error","payload":{"message":"private server details"}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || ev.err == nil {
			t.Fatalf("end event missed: %s %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"type":"tool_update","payload":{"update_type":"hang_up"}}`,
		`{"type":"usage_update","payload":{"instructions":{"hang_up":true}}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || !ev.ended || ev.err != nil {
			t.Fatal("remote hangup was not normal completion", ev, err)
		}
	}
}

func TestPositiveRemainingQuotaIsNotTerminal(t *testing.T) {
	for _, raw := range []string{
		`{"type":"usage_update","audio_s":1}`,
		`{"type":"usage_update","audio_s":5}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":1}}}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":5}}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || ev != (event{}) {
			t.Errorf("positive remaining quota ended the call: %s %+v %v", raw, ev, err)
		}
	}
}

func TestGoodbyeEvents(t *testing.T) {
	for _, tc := range []struct {
		name, raw, diagnostic string
		normal                bool
	}{
		{"normal", `{"type":"goodbye","reason":"normal"}`, "", true},
		{"requested", `{"type":"goodbye","reason":"client_request"}`, "", true},
		{"quota", `{"type":"goodbye","reason":"cap_reached"}`, "usage limit", false},
		{"failure", `{"type":"goodbye","reason":"error","detail":"internal_error"}`, "reason=error, detail=internal_error", false},
		{"expired", `{"type":"goodbye","reason":"session_expired"}`, "reason=session_expired", false},
		{"timeout", `{"type":"goodbye","reason":"timeout"}`, "reason=timeout", false},
		{"failure overrides normal", `{"type":"goodbye","reason":"normal","detail":"invalid_model"}`, "detail=invalid_model", false},
		{"unknown codes", `{"type":"goodbye","reason":"private-marker","detail":"private-marker"}`, "reason=unknown, detail=unknown", false},
		{"missing reason", `{"type":"goodbye"}`, "reason=unknown", false},
		{"session error", `{"type":"session.error","message":"private-marker"}`, "voice session error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range [][]byte{
				[]byte(tc.raw),
				testEnvelope(t, tc.raw),
				testEnvelope(t, `{"payload":`+tc.raw+`}`),
			} {
				ev, err := decodeEvent(input)
				if err != nil || ev.ended != tc.normal || (ev.err == nil) != tc.normal {
					t.Fatalf("terminal classification: %+v, %v", ev, err)
				}
				if ev.err != nil &&
					(!strings.Contains(ev.err.Error(), tc.diagnostic) || strings.Contains(ev.err.Error(), "private-marker")) {
					t.Fatalf("unsafe or missing terminal diagnostic: %v", ev.err)
				}
			}
		})
	}
	for _, raw := range []string{
		`{"type":"goodbye","reason":42}`,
		`{"type":"goodbye","payload":{"reason":true}}`,
		`{"type":"goodbye","reason":"normal","detail":{}}`,
	} {
		if _, err := decodeEvent(testEnvelope(t, raw)); err == nil {
			t.Errorf("malformed goodbye accepted: %s", raw)
		}
	}
}

func TestRunSettingsPayload(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		args                 []string
		voice, model, effort string
		omit                 bool
	}{
		{"missing", nil, "orbit", "gpt-6-thinking", "extended", false},
		{"empty", []string{"--voice=", "--backend-model=", "--backend-thinking-effort="}, "orbit", "gpt-6-thinking", "extended", false},
		{"empty voice", []string{"--voice=", "--backend-model=future", "--backend-thinking-effort=other"}, "orbit", "future", "other", false},
		{"empty model", []string{"--voice=future", "--backend-model=", "--backend-thinking-effort=other"}, "future", "gpt-6-thinking", "other", false},
		{"empty effort", []string{"--voice=future", "--backend-model=other", "--backend-thinking-effort="}, "future", "other", "extended", false},
		{"whitespace", []string{"--voice= \t ", "--backend-model=\n", "--backend-thinking-effort= "}, " \t ", "\n", " ", false},
		{"literal", []string{"--voice=  голос\t\n$'\"`;&|<>#", "--backend-model=-future/model", "--backend-thinking-effort=任意"}, "  голос\t\n$'\"`;&|<>#", "-future/model", "任意", false},
		{"disabled", []string{"--voice=disabled", "--backend-model=disabled", "--backend-thinking-effort=disabled"}, "disabled", "disabled", "", true},
		{"case", []string{"--backend-thinking-effort=Disabled"}, "orbit", "gpt-6-thinking", "Disabled", false},
		{"leading space", []string{"--backend-thinking-effort= disabled"}, "orbit", "gpt-6-thinking", " disabled", false},
		{"trailing space", []string{"--backend-thinking-effort=disabled "}, "orbit", "gpt-6-thinking", "disabled ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := parseRunSettings(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			before := settings
			data, err := encodeSession(
				time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("Asia/Kolkata", 19800)),
				settings,
			)
			if err != nil {
				t.Fatal(err)
			}
			if settings != before {
				t.Fatal("encoding changed the startup snapshot")
			}
			var got map[string]any
			if unmarshalErr := json.Unmarshal(data, &got); unmarshalErr != nil {
				t.Fatal(unmarshalErr)
			}
			id, ok := got["voice_session_id"].(string)
			if !ok || len(id) != 36 || strings.ToUpper(id) != id {
				t.Fatal("invalid session ID")
			}
			want := map[string]any{
				"backend_reasoning_effort": "instant", "language_code": "auto",
				"requested_default_model": "", "voice": tc.voice, "voice_session_id": id,
				"voice_status_request_id": id, "timezone_offset_min": float64(-330), "timezone": "Asia/Kolkata",
				"voice_mode": "wingman", "model_slug": "", "model_slug_advanced": "",
				"client_tools": []any{}, "history_and_training_disabled": false,
				"conversation_mode":        map[string]any{"kind": "primary_assistant"},
				"enable_message_streaming": true, "backend_model": tc.model, "deprecate_celsius": false,
			}
			if !tc.omit {
				want["backend_thinking_effort"] = tc.effort
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("payload got %#v, want %#v", got, want)
			}
		})
	}
}
