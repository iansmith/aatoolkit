// Package host defines the surface that an interpreted chat policy needs from
// its compiled driver. The policy code depends only on this interface; the
// driver constructs a concrete implementation and passes it into the policy's
// entrypoint, keeping transport details out of the policy.
package host

// Host is the capability set the chat policy calls into. Today that's just LLM
// transport by tier — add methods here as the policy needs more from the driver.
type Host interface {
	// Send takes the already-serialized `messages` array and a tier
	// ("fast" | "deep") and returns (content, reasoning, error). reasoning is
	// empty when the served model doesn't emit it.
	Send(contextWindow []byte, tier string) ([]byte, []byte, error)

	// SendStream streams the response, calling onSegment whenever a boundary
	// is reached (sentence/paragraph punctuation with enough accumulated
	// text). Returns the fully assembled (content, reasoning) when the stream
	// completes. onSegment is called on the goroutine of the caller —
	// implementations must not block it for long.
	SendStream(contextWindow []byte, tier string, onSegment func(string)) ([]byte, []byte, error)

	// SystemPrompt returns the current system prompt. The driver reloads it from
	// its backing file only when that file's modification time changes, so the
	// policy can call this every turn cheaply.
	SystemPrompt() string

	// Speak renders text to speech via the driver's TTS server and plays it,
	// using the given voice (e.g. "M1") and speed (0.7..2.0). The policy decides
	// whether to call it and with what settings; the driver owns synthesis and
	// playback. Best-effort: errors are the driver's to report and must not
	// break a turn.
	Speak(text []byte, voice string, speed float64) error

	// SpeakSync is like Speak but blocks until playback finishes, so callers can
	// play several clips in sequence without overlap (e.g. the /voicetest voice
	// comparison).
	SpeakSync(text []byte, voice string, speed float64) error

	// Remember appends a message (role "user" or "assistant") to the running
	// conversation history the driver keeps, so later turns can refer back to
	// earlier ones. This history is durable host state — it survives /reload.
	Remember(role string, content []byte)

	// Context returns the messages array to send: the current system prompt
	// followed by the remembered history, as a JSON array ready for Send.
	Context() []byte

	// Forget drops the most recently remembered message. The policy uses it to
	// roll back a user turn whose send failed, so history stays consistent (and
	// an over-large context doesn't wedge every subsequent turn).
	Forget()

	// CancelQueued drops all clips waiting in the speech queue without rendering
	// them. Any clip already being rendered plays to completion. Call alongside
	// Forget() so orphaned TTS segments from a failed streaming turn don't play
	// after the turn has been rolled back.
	CancelQueued()

	// LastAnswer returns the content of the most recent assistant message, or
	// empty if there is none. Used by "/repeat" with no modifier.
	LastAnswer() []byte

	// Clear discards all remembered history (the /clear command; also used by
	// /compact to swap the full history for a summary).
	Clear()
}
