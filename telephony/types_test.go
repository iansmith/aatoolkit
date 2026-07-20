package telephony_test

import (
	"testing"

	"github.com/iansmith/aatoolkit/telephony"
)

// Compile-time: TransportType constants exist and are the declared type.
var (
	_ telephony.TransportType = telephony.TransportVoice
	_ telephony.TransportType = telephony.TransportSMS
	_ telephony.TransportType = telephony.TransportOutbound
)

func TestMessage_FieldsRoundTrip(t *testing.T) {
	m := telephony.Message{
		Text:      "hello",
		Transport: telephony.TransportVoice,
		SessionID: "sess-abc",
	}
	if m.Text != "hello" {
		t.Fatalf("Text: got %q want %q", m.Text, "hello")
	}
	if m.Transport != telephony.TransportVoice {
		t.Fatalf("Transport: got %q want %q", m.Transport, telephony.TransportVoice)
	}
	if m.SessionID != "sess-abc" {
		t.Fatalf("SessionID: got %q want %q", m.SessionID, "sess-abc")
	}
}

func TestTransportType_IsStringBased(t *testing.T) {
	// Must accept arbitrary string values — open set for future transports.
	var tt telephony.TransportType = "experimental"
	if string(tt) != "experimental" {
		t.Fatal("TransportType is not string-based")
	}
}

func TestTransportType_BuiltinValuesDistinct(t *testing.T) {
	if telephony.TransportVoice == telephony.TransportSMS {
		t.Fatal("TransportVoice and TransportSMS must be distinct")
	}
	if telephony.TransportVoice == telephony.TransportOutbound {
		t.Fatal("TransportVoice and TransportOutbound must be distinct")
	}
	if telephony.TransportSMS == telephony.TransportOutbound {
		t.Fatal("TransportSMS and TransportOutbound must be distinct")
	}
}

func TestVADEvent_KindDistinguishesThreeStates(t *testing.T) {
	s := telephony.VADEvent{Kind: telephony.VADSpeech}
	p := telephony.VADEvent{Kind: telephony.VADSilence}
	e := telephony.VADEvent{Kind: telephony.VADEndOfUtterance}

	if s.Kind == p.Kind {
		t.Fatal("VADSpeech and VADSilence must be distinct")
	}
	if s.Kind == e.Kind {
		t.Fatal("VADSpeech and VADEndOfUtterance must be distinct")
	}
	if p.Kind == e.Kind {
		t.Fatal("VADSilence and VADEndOfUtterance must be distinct")
	}
}

// Gap 4: TransportType named constants must not be the zero/empty string.
func TestTransportType_NamedConstantsAreNonEmpty(t *testing.T) {
	for name, tt := range map[string]telephony.TransportType{
		"TransportVoice":    telephony.TransportVoice,
		"TransportSMS":      telephony.TransportSMS,
		"TransportOutbound": telephony.TransportOutbound,
	} {
		if string(tt) == "" {
			t.Errorf("%s must not be the empty string", name)
		}
	}
}

func TestTransportType_ZeroValueDistinctFromAllConstants(t *testing.T) {
	var zero telephony.TransportType
	if zero == telephony.TransportVoice {
		t.Error("zero TransportType must not equal TransportVoice")
	}
	if zero == telephony.TransportSMS {
		t.Error("zero TransportType must not equal TransportSMS")
	}
	if zero == telephony.TransportOutbound {
		t.Error("zero TransportType must not equal TransportOutbound")
	}
}

// Gap 5: VADKind named constants must not be the zero/empty string.
func TestVADKind_NamedConstantsAreNonEmpty(t *testing.T) {
	for name, k := range map[string]telephony.VADKind{
		"VADSpeech":         telephony.VADSpeech,
		"VADSilence":        telephony.VADSilence,
		"VADEndOfUtterance": telephony.VADEndOfUtterance,
	} {
		if string(k) == "" {
			t.Errorf("%s must not be the empty string", name)
		}
	}
}

// Gap 7: Message zero value and partial init must be representable.
func TestMessage_ZeroValue(t *testing.T) {
	var m telephony.Message
	if m.Text != "" {
		t.Errorf("zero Message.Text: got %q", m.Text)
	}
	if m.SessionID != "" {
		t.Errorf("zero Message.SessionID: got %q", m.SessionID)
	}
	var zeroTransport telephony.TransportType
	if m.Transport != zeroTransport {
		t.Errorf("zero Message.Transport: got %q want zero TransportType", m.Transport)
	}
}
