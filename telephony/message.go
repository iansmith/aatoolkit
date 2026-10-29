package telephony

// TransportType identifies the channel over which a conversation turn arrived.
// It is an open string enum — future transport additions do not require recompilation.
type TransportType string

const (
	TransportVoice    TransportType = "voice"
	TransportSMS      TransportType = "sms"
	TransportOutbound TransportType = "outbound"
)

// Message is a typed turn at the policy seam: the compiled harness produces
// one per user utterance. The interpreted policy reads only Text to generate
// a response; the policy does not condition response style on transport or
// session identity. Transport, SessionID, and From are reply-routing metadata
// only — they tell the harness where and how to deliver the response, but do
// not influence the response content. MessageSID is bookkeeping metadata the
// policy never reads: the provider's id for an inbound SMS, which the harness
// copies from twilio.InboundSMS.MessageSID to give a stable key for counting
// or de-duplicating inbound messages. The harness sets it only on SMS turns.
type Message struct {
	Text       string
	Transport  TransportType
	SessionID  string
	From       string
	MessageSID string
}
