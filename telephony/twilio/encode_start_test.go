package twilio_test

import (
	"encoding/json"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// EncodeStart must produce a JSON object with event="start", streamSid, and
// a nested start object containing callSid.
func TestEncodeStart_FieldsPresent(t *testing.T) {
	data, err := twilio.EncodeStart("SS123", "CA456", "AC789", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["event"] != "start" {
		t.Errorf("event: got %v, want start", m["event"])
	}
	if m["streamSid"] != "SS123" {
		t.Errorf("streamSid: got %v, want SS123", m["streamSid"])
	}
	startObj, ok := m["start"].(map[string]any)
	if !ok {
		t.Fatalf("start field: got type %T, want map", m["start"])
	}
	if startObj["callSid"] != "CA456" {
		t.Errorf("start.callSid: got %v, want CA456", startObj["callSid"])
	}
}

// EncodeStart output must round-trip through DecodeFrame and yield the same SIDs.
func TestEncodeStart_RoundTripsViaDecodeFrame(t *testing.T) {
	data, err := twilio.EncodeStart("SSabc", "CAdef", "ACabc", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	f, err := twilio.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != twilio.EventStart {
		t.Errorf("event: got %q, want %q", f.Event, twilio.EventStart)
	}
	if f.StreamSID != "SSabc" {
		t.Errorf("streamSid: got %q, want SSabc", f.StreamSID)
	}
	if f.CallSID != "CAdef" {
		t.Errorf("callSid: got %q, want CAdef", f.CallSID)
	}
}

// EncodeStart must not produce the zero streamSid for a non-empty input.
func TestEncodeStart_NonEmptyStreamSIDPreserved(t *testing.T) {
	data, err := twilio.EncodeStart("SSnonzero", "CA000", "ACnonzero", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["streamSid"] == "" {
		t.Error("streamSid must not be empty for a non-empty input")
	}
}
