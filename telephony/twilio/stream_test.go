package twilio_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// Edge: decoding a stop event (no media payload) yields the right event type and empty payload.
func TestDecodeFrame_StopEventHasNoPayload(t *testing.T) {
	raw := []byte(`{"event":"stop","streamSid":"MZ999","stop":{}}`)
	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != twilio.EventStop {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventStop)
	}
	if len(f.Payload) != 0 {
		t.Fatalf("stop frame must have no payload, got %d bytes", len(f.Payload))
	}
}

// Edge: mark event carries the mark name, not a media payload.
func TestDecodeFrame_MarkEventName(t *testing.T) {
	raw := []byte(`{"event":"mark","streamSid":"MZ999","mark":{"name":"eot"}}`)
	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != twilio.EventMark {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventMark)
	}
	if f.MarkName != "eot" {
		t.Fatalf("mark name: got %q, want %q", f.MarkName, "eot")
	}
}

// Error: malformed JSON returns a non-nil error and a zero Frame.
func TestDecodeFrame_MalformedJSONReturnsError(t *testing.T) {
	_, err := twilio.DecodeFrame([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("DecodeFrame with malformed JSON must return an error")
	}
}

// Happy: inbound media event decodes streamSid and payload correctly.
func TestDecodeFrame_MediaEventDecodesPayload(t *testing.T) {
	mulaw := []byte{0x7f, 0xff, 0x00, 0x01}
	encoded := base64.StdEncoding.EncodeToString(mulaw)
	raw := []byte(`{"event":"media","streamSid":"MZ123","media":{"track":"inbound","chunk":"1","timestamp":"5","payload":"` + encoded + `"}}`)

	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != twilio.EventMedia {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventMedia)
	}
	if f.StreamSID != "MZ123" {
		t.Fatalf("streamSid: got %q, want %q", f.StreamSID, "MZ123")
	}
	if string(f.Payload) != string(mulaw) {
		t.Fatalf("payload: got %v, want %v", f.Payload, mulaw)
	}
}

// Happy: DecodeFrame captures chunk (int) and timestamp (string) from a media event.
func TestDecodeFrame_ChunkTimestamp(t *testing.T) {
	raw := []byte(`{"event":"media","streamSid":"MZ123","media":{"chunk":42,"timestamp":"1234","payload":""}}`)
	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Chunk != 42 {
		t.Fatalf("chunk: got %d, want 42", f.Chunk)
	}
	if f.Timestamp != "1234" {
		t.Fatalf("timestamp: got %q, want %q", f.Timestamp, "1234")
	}
}

// Happy: EncodeMedia produces a valid Twilio outbound media event with base64 payload.
func TestEncodeMedia_RoundTripPayload(t *testing.T) {
	mulaw := []byte{0x7f, 0xff, 0x00, 0x01}
	data, err := twilio.EncodeMedia("MZ123", mulaw)
	if err != nil {
		t.Fatalf("EncodeMedia: %v", err)
	}
	// Round-trip: decoded frame should recover the original payload.
	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame after EncodeMedia: %v", err)
	}
	if f.Event != twilio.EventMedia {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventMedia)
	}
	if string(f.Payload) != string(mulaw) {
		t.Fatalf("payload round-trip: got %v, want %v", f.Payload, mulaw)
	}
}

// Happy: EncodeMark produces a mark event with the given name.
func TestEncodeMark_NamePreserved(t *testing.T) {
	data, err := twilio.EncodeMark("MZ123", "eot")
	if err != nil {
		t.Fatalf("EncodeMark: %v", err)
	}
	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame after EncodeMark: %v", err)
	}
	if f.Event != twilio.EventMark {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventMark)
	}
	if f.MarkName != "eot" {
		t.Fatalf("mark name: got %q, want %q", f.MarkName, "eot")
	}
}

// Happy: EncodeStop round-trips through DecodeFrame to EventStop with the given StreamSID.
func TestEncodeStop_RoundTrips(t *testing.T) {
	data, err := twilio.EncodeStop("MZ123", "CA123", "AC123", 1)
	if err != nil {
		t.Fatalf("EncodeStop: %v", err)
	}
	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame after EncodeStop: %v", err)
	}
	if f.Event != twilio.EventStop {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventStop)
	}
	if f.StreamSID != "MZ123" {
		t.Fatalf("streamSid: got %q, want %q", f.StreamSID, "MZ123")
	}
}

// Happy: EncodeClear produces a clear event.
func TestEncodeClear_EventType(t *testing.T) {
	data, err := twilio.EncodeClear("MZ123")
	if err != nil {
		t.Fatalf("EncodeClear: %v", err)
	}
	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame after EncodeClear: %v", err)
	}
	if f.Event != twilio.EventClear {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventClear)
	}
}

// Happy: EncodeConnected produces a connected event with correct shape.
func TestEncodeConnected_Shape(t *testing.T) {
	data, err := twilio.EncodeConnected()
	if err != nil {
		t.Fatalf("EncodeConnected: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal connected: %v", err)
	}

	// Check required fields
	if event, ok := result["event"]; !ok || event != "connected" {
		t.Fatalf("event: got %v, want \"connected\"", event)
	}
	if protocol, ok := result["protocol"]; !ok || protocol != "Call" {
		t.Fatalf("protocol: got %v, want \"Call\"", protocol)
	}
	if version, ok := result["version"]; !ok || version != "1.0.0" {
		t.Fatalf("version: got %v, want \"1.0.0\"", version)
	}

	// Check forbidden fields
	if _, ok := result["streamSid"]; ok {
		t.Fatalf("streamSid must not be present in connected event")
	}
	if _, ok := result["sequenceNumber"]; ok {
		t.Fatalf("sequenceNumber must not be present in connected event")
	}
}

// Happy: DecodeFrame on EncodeConnected output yields EventConnected.
func TestDecodeFrame_Connected(t *testing.T) {
	data, err := twilio.EncodeConnected()
	if err != nil {
		t.Fatalf("EncodeConnected: %v", err)
	}

	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame after EncodeConnected: %v", err)
	}

	if f.Event != twilio.EventConnected {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventConnected)
	}
	if f.StreamSID != "" {
		t.Fatalf("StreamSID should be empty for connected event, got %q", f.StreamSID)
	}
	if f.CallSID != "" {
		t.Fatalf("CallSID should be empty for connected event, got %q", f.CallSID)
	}
}

// Error: EncodeDTMF panics with documented message.
func TestEncodeDTMF_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("EncodeDTMF must panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "dtmf encoding not implemented yet") {
			t.Fatalf("panic message: got %v, want string containing \"dtmf encoding not implemented yet\"", r)
		}
	}()

	twilio.EncodeDTMF("MZ123", "5")
}

// assertJSONString checks that m[key] unmarshaled to the JSON string want.
func assertJSONString(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	got, ok := m[key].(string)
	if !ok || got != want {
		t.Errorf("%s: got %#v, want string %q", key, m[key], want)
	}
}

// assertJSONNumber checks that m[key] unmarshaled to the JSON number want.
func assertJSONNumber(t *testing.T, m map[string]any, key string, want float64) {
	t.Helper()
	got, ok := m[key].(float64)
	if !ok || got != want {
		t.Errorf("%s: got %#v, want %v (number)", key, m[key], want)
	}
}

// mustJSONObject asserts m[key] unmarshaled to a JSON object and returns it.
func mustJSONObject(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s: got type %T, want map", key, m[key])
	}
	return v
}

// assertJSONStringArray checks that m[key] unmarshaled to the given string array.
func assertJSONStringArray(t *testing.T, m map[string]any, key string, want []string) {
	t.Helper()
	got, ok := m[key].([]any)
	if !ok || len(got) != len(want) {
		t.Errorf("%s: got %#v, want %v", key, m[key], want)
		return
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("%s: got %#v, want %v", key, m[key], want)
			return
		}
	}
}

// assertEmptyJSONObject checks that m[key] unmarshaled to an empty JSON object.
func assertEmptyJSONObject(t *testing.T, m map[string]any, key string) {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok || len(v) != 0 {
		t.Errorf("%s: got %#v, want {}", key, m[key])
	}
}

// Golden: EncodeStart must match the full Twilio start-frame spec — string
// sequenceNumber, nested start object with streamSid/accountSid/tracks/
// customParameters/mediaFormat, and numeric mediaFormat fields.
func TestEncodeStart_GoldenSpec(t *testing.T) {
	data, err := twilio.EncodeStart("SSgolden01", "CAgolden01", "ACgolden01", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	assertJSONString(t, m, "sequenceNumber", "1")

	startObj := mustJSONObject(t, m, "start")
	if startObj["streamSid"] != m["streamSid"] {
		t.Errorf("start.streamSid: got %v, want top-level streamSid %v", startObj["streamSid"], m["streamSid"])
	}
	assertJSONString(t, startObj, "accountSid", "ACgolden01")
	assertJSONStringArray(t, startObj, "tracks", []string{"inbound"})
	assertEmptyJSONObject(t, startObj, "customParameters")

	mediaFormat := mustJSONObject(t, startObj, "mediaFormat")
	assertJSONString(t, mediaFormat, "encoding", "audio/x-mulaw")
	assertJSONNumber(t, mediaFormat, "sampleRate", 8000)
	assertJSONNumber(t, mediaFormat, "channels", 1)
}

// Golden: EncodeMediaWithMetadata must match the full Twilio media-frame
// spec — string sequenceNumber, media.track "inbound", and media.chunk /
// media.timestamp emitted as JSON strings (not numbers).
func TestEncodeMediaWithMetadata_GoldenSpec(t *testing.T) {
	data, err := twilio.EncodeMediaWithMetadata("SSgolden02", []byte{0x01, 0x02}, 3, 5)
	if err != nil {
		t.Fatalf("EncodeMediaWithMetadata: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if seq, ok := m["sequenceNumber"].(string); !ok || seq != "5" {
		t.Errorf("sequenceNumber: got %#v, want string \"5\"", m["sequenceNumber"])
	}

	mediaObj, ok := m["media"].(map[string]any)
	if !ok {
		t.Fatalf("media field: got type %T, want map", m["media"])
	}
	if mediaObj["track"] != "inbound" {
		t.Errorf("media.track: got %v, want inbound", mediaObj["track"])
	}
	if chunk, ok := mediaObj["chunk"].(string); !ok || chunk != "3" {
		t.Errorf("media.chunk: got %#v, want string \"3\"", mediaObj["chunk"])
	}
	if ts, ok := mediaObj["timestamp"].(string); !ok || ts != "40" {
		t.Errorf("media.timestamp: got %#v, want string \"40\"", mediaObj["timestamp"])
	}
}

// Golden: EncodeStop must match the full Twilio stop-frame spec — string
// sequenceNumber and nested stop object with accountSid/callSid.
func TestEncodeStop_GoldenSpec(t *testing.T) {
	data, err := twilio.EncodeStop("SSgolden03", "CAgolden03", "ACgolden03", 7)
	if err != nil {
		t.Fatalf("EncodeStop: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if seq, ok := m["sequenceNumber"].(string); !ok || seq != "7" {
		t.Errorf("sequenceNumber: got %#v, want string \"7\"", m["sequenceNumber"])
	}

	stopObj, ok := m["stop"].(map[string]any)
	if !ok {
		t.Fatalf("stop field: got type %T, want map", m["stop"])
	}
	if stopObj["accountSid"] != "ACgolden03" {
		t.Errorf("stop.accountSid: got %v, want ACgolden03", stopObj["accountSid"])
	}
	if stopObj["callSid"] != "CAgolden03" {
		t.Errorf("stop.callSid: got %v, want CAgolden03", stopObj["callSid"])
	}
}
