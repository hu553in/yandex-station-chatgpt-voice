package voice

import (
	"encoding/json/v2"
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
	session, err := encodeSession(time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("Asia/Kolkata", 19800)))
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
