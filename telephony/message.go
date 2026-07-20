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
// one per user utterance; the interpreted policy reads it and adapts
// response style to the transport.
type Message struct {
	Text      string
	Transport TransportType
	SessionID string
}

// VADKind is the kind of voice-activity event emitted by the VAD goroutine.
type VADKind string

const (
	VADSpeech         VADKind = "speech"
	VADSilence        VADKind = "silence"
	VADEndOfUtterance VADKind = "end-of-utterance"
	VADTurnEnd        VADKind = "turn-end"
)

// VADEvent is a voice-activity boundary emitted on a vadService's VADOutput.
// It is a lossless record of the vadMachine's full state at the moment of
// emission (charter R9) and carries SessionID on every result (charter R10).
type VADEvent struct {
	Kind         VADKind
	Prob         float32 // detector probability that produced this event
	VoicedCount  int     // voiced windows accumulated since speech-start
	SilenceCount int     // consecutive sub-SilenceThresh windows since speech
	WindowIndex  int     // monotonic per-utterance window counter (resets at each speech onset)
	// StreamWindowIndex is a monotonic, never-reset window counter over the
	// whole stream (0 at the first window, +1 per window processed). Unlike
	// WindowIndex it does not reset at speech onset, so AudioMS =
	// StreamWindowIndex * windowMS gives a stable position in the input audio
	// for a DecisionEvent -- see decision.go.
	StreamWindowIndex int
	SessionID         string // set by the vadService wrapper at construction time
}
