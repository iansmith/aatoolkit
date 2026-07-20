package twilio_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- signature adversary gaps ---

// Boundary: empty auth token must be rejected regardless of signature value.
// An empty HMAC key is a misconfiguration and must never validate.
func TestValidateSignature_EmptyAuthTokenRejected(t *testing.T) {
	sig := computeSig("", "https://example.com/webhook", nil)
	if twilio.ValidateSignature("", "https://example.com/webhook", nil, sig) {
		t.Fatal("empty auth token must always reject — it indicates misconfiguration")
	}
}

// --- stream adversary gaps ---

// Error path: DecodeFrame with an unknown event type must return an error.
// The implementation must not silently drop or misroute unknown Twilio events.
func TestDecodeFrame_UnknownEventTypeReturnsError(t *testing.T) {
	raw := []byte(`{"event":"disconnected","streamSid":"MZ999"}`)
	_, err := twilio.DecodeFrame(raw)
	if err == nil {
		t.Fatal("DecodeFrame with unknown event type must return an error")
	}
}

// Coverage: start event is the first message Twilio sends on every WebSocket
// connection. Failing to decode it prevents the entire session from starting.
func TestDecodeFrame_StartEventDecodesCallSID(t *testing.T) {
	raw := []byte(`{
		"event":     "start",
		"streamSid": "MZ123",
		"start": {
			"accountSid": "AC123",
			"callSid":    "CA456",
			"tracks":     ["inbound"],
			"streamSid":  "MZ123"
		}
	}`)
	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame start event: %v", err)
	}
	if f.Event != twilio.EventStart {
		t.Fatalf("event: got %q, want %q", f.Event, twilio.EventStart)
	}
	if f.StreamSID != "MZ123" {
		t.Fatalf("streamSid: got %q, want %q", f.StreamSID, "MZ123")
	}
	if f.CallSID != "CA456" {
		t.Fatalf("callSid: got %q, want %q", f.CallSID, "CA456")
	}
}

// --- server adversary gaps ---

// Behavioral: Server must reject requests whose X-Twilio-Signature is absent
// or wrong with HTTP 403. The compile-time interface check in server_test.go
// does not test this — it is possible to implement ServeHTTP and never check
// the signature at all.
func TestServer_RejectsRequestWithoutSignature(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/webhook", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing signature: got HTTP %d, want 403", w.Code)
	}
}

func TestServer_RejectsRequestWithWrongSignature(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/webhook", nil)
	req.Header.Set("X-Twilio-Signature", "bm90YXJlYWxzaWc=")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong signature: got HTTP %d, want 403", w.Code)
	}
}
