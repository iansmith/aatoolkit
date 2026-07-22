package telephony

import (
	"testing"
)

func TestMessage_FromField(t *testing.T) {
	msg := Message{
		Text:      "hi",
		Transport: TransportSMS,
		From:      "+15105550123",
		SessionID: "CA1",
	}

	if msg.Text != "hi" {
		t.Errorf("expected Text='hi', got '%s'", msg.Text)
	}
	if msg.Transport != TransportSMS {
		t.Errorf("expected Transport=TransportSMS, got %s", msg.Transport)
	}
	if msg.From != "+15105550123" {
		t.Errorf("expected From='+15105550123', got '%s'", msg.From)
	}
	if msg.SessionID != "CA1" {
		t.Errorf("expected SessionID='CA1', got '%s'", msg.SessionID)
	}

	voiceMsg := Message{
		Text:      "hello",
		Transport: TransportVoice,
		From:      "+12025551234",
		SessionID: "CALL-456",
	}

	if voiceMsg.From != "+12025551234" {
		t.Errorf("expected From='+12025551234', got '%s'", voiceMsg.From)
	}

	zeroMsg := Message{}
	if zeroMsg.From != "" {
		t.Errorf("expected zero-value From to be empty string, got '%s'", zeroMsg.From)
	}

	partialMsg := Message{
		Text:      "test",
		Transport: TransportSMS,
		SessionID: "S123",
	}
	if partialMsg.From != "" {
		t.Errorf("expected unset From to default to empty string, got '%s'", partialMsg.From)
	}
}
