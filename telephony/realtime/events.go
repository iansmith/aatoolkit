// Package realtime speaks the subset of the OpenAI-Realtime websocket protocol
// an external voice backend needs: a session handshake, inbound audio append,
// and the server events that carry audio, speech boundaries, and transcripts.
//
// Audio crosses this package as a **base64 string, never decoded**. The carrier
// delivers base64 G.711 mu-law and the protocol's audio fields carry base64
// audio, so the two are the same bytes; decoding to re-encode would spend CPU
// per 20 ms frame to reproduce the input. Nothing here imports a G.711 codec.
package realtime

import "encoding/json"

// Client event types (this package -> backend).
const (
	EventSessionUpdate = "session.update"
	EventAudioAppend   = "input_audio_buffer.append"
)

// Server event types (backend -> this package). Any type not listed here is
// ignored by the read loop rather than treated as an error: the protocol is
// larger than the subset this package needs, and a backend is free to emit
// more of it.
const (
	EventSessionCreated  = "session.created"
	EventSpeechStarted   = "input_audio_buffer.speech_started"
	EventSpeechStopped   = "input_audio_buffer.speech_stopped"
	EventTranscriptDelta = "conversation.item.input_audio_transcription.delta"
	EventTranscriptDone  = "conversation.item.input_audio_transcription.completed"
	EventAudioDelta      = "response.output_audio.delta"
)

// FormatG711ULaw is the session audio format for 8 kHz G.711 mu-law. The
// format identifier carries no sample rate because the codec fixes it.
const FormatG711ULaw = "audio/pcmu"

type audioFormat struct {
	Type string `json:"type"`
}

type audioChannel struct {
	Format audioFormat `json:"format"`
	// Voice is only ever set on the OUTPUT channel — see newSessionUpdate.
	// omitempty on purpose, mirroring sessionSpec.Instructions: a caller who
	// supplies no voice must produce the handshake this engine sent before
	// the field existed, not a near-equivalent carrying "".
	Voice string `json:"voice,omitempty"`
}

type sessionAudio struct {
	Input  audioChannel `json:"input"`
	Output audioChannel `json:"output"`
}

type sessionSpec struct {
	Type string `json:"type"`
	// Instructions is omitempty on purpose: a backend is free to distinguish
	// an absent field from an empty string, so a caller who supplies nothing
	// must produce the handshake this engine sent before the field existed —
	// not a near-equivalent carrying "".
	Instructions string       `json:"instructions,omitempty"`
	Audio        sessionAudio `json:"audio"`
}

type sessionUpdate struct {
	Type    string      `json:"type"`
	Session sessionSpec `json:"session"`
}

type audioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// ServerEvent is one decoded server event, flattened to the fields this
// package reads. Delta carries base64 audio for EventAudioDelta; Transcript
// carries text for the transcription events.
//
// Raw carries the whole frame exactly as it arrived on the wire, for every
// event type — including ones this package does not model into Type/Delta/
// Transcript. It has no json tag mapping any wire field onto it: nothing in
// the protocol names "the whole document", so Client.Read must assign it
// explicitly after decoding, from the same bytes it unmarshalled.
type ServerEvent struct {
	Type       string          `json:"type"`
	Delta      string          `json:"delta,omitempty"`
	Transcript string          `json:"transcript,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// newSessionUpdate builds the handshake declaring G.711 mu-law on BOTH
// directions. Declaring only input leaves output at the backend's default,
// which is the silent half-configuration this constructor exists to prevent.
//
// instructions is the session persona. Empty omits the field entirely.
//
// voice is the backend's OUTPUT voice only — the protocol has no notion of
// an input voice, and the two directions are built as separate audioChannel
// values below (rather than one shared literal assigned to both) precisely
// so setting voice can never leak onto the input channel. Empty omits the
// field entirely, same as instructions.
func newSessionUpdate(instructions, voice string) sessionUpdate {
	format := audioFormat{Type: FormatG711ULaw}
	return sessionUpdate{
		Type: EventSessionUpdate,
		Session: sessionSpec{
			Type:         "realtime",
			Instructions: instructions,
			Audio: sessionAudio{
				Input:  audioChannel{Format: format},
				Output: audioChannel{Format: format, Voice: voice},
			},
		},
	}
}
