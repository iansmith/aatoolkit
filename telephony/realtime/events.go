// Package realtime speaks the subset of the OpenAI-Realtime websocket protocol
// an external voice backend needs: a session handshake, inbound audio append,
// and the server events that carry audio, speech boundaries, and transcripts.
//
// Audio crosses this package as a **base64 string, never decoded**. The carrier
// delivers base64 G.711 mu-law and the protocol's audio fields carry base64
// audio, so the two are the same bytes; decoding to re-encode would spend CPU
// per 20 ms frame to reproduce the input. Nothing here imports a G.711 codec.
package realtime

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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
	EventResponseDone    = "response.done"
)

// ItemTypeFunctionCall is the "type" of a response output item that is a tool
// call rather than speech. It is not an event type — it appears INSIDE a
// response.done's output list — and it is named here beside the event types
// because it is read for the same reason they are: to tell whether a turn has
// actually ended.
const ItemTypeFunctionCall = "function_call"

// FormatG711ULaw is the session audio format for 8 kHz G.711 mu-law. The
// format identifier carries no sample rate because the codec fixes it.
const FormatG711ULaw = "audio/pcmu"

// sessionTypeRealtime is the session variant every session.update this package
// sends declares. The backend maps session.update onto a discriminated union
// of session variants and rejects the transcription one explicitly, so an
// update without it does not parse as the accepted variant at all — which is
// true of the dial handshake and of the mid-call voice update alike, hence one
// definition rather than a literal at each site.
const sessionTypeRealtime = "realtime"

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
			Type:         sessionTypeRealtime,
			Instructions: instructions,
			Audio: sessionAudio{
				Input:  audioChannel{Format: format},
				Output: audioChannel{Format: format, Voice: voice},
			},
		},
	}
}

// buildSessionUpdate marshals the session.update handshake and, when tools is
// non-empty, declares it as the session object's last field (after Audio, per
// AATK-85 — see the splice-point discussion below for why declaration order
// is fixed).
//
// tools is NOT a struct field on sessionSpec, and that is deliberate rather
// than an oversight. encoding/json HTML-escapes '<', '>', '&' and compacts
// insignificant whitespace whenever it marshals a value — including a
// json.RawMessage embedded in a larger struct, because the standard encoder
// runs compact() over every Marshaler's output before writing it, regardless
// of field type. Measured: a Tools json.RawMessage struct field with
// `json:"tools,omitempty"` turned `"description":"<desc>&more"` into
// `"description":"<desc>&more"` and collapsed
// `"type":  "function"` to `"type":"function"` — exactly the re-encoding the
// DoD's "unmodified" forbids. The only way tools' bytes survive verbatim is
// to never pass them through json.Marshal at all: this function marshals
// everything else normally, then splices tools in via plain byte
// concatenation.
//
// The splice point relies on one structural invariant of encoding/json: a
// non-empty Go struct always marshals to a single top-level JSON object, so
// its encoded bytes always end in exactly one '}' — the object's own closing
// brace — no matter what is nested inside it. sessionUpdate's last field is
// Session, so marshalling it produces "...<session object>}" where that
// final '}' is sessionUpdate's own close and the '}' immediately before it is
// sessionSpec's own close (sessionSpec's last field, Audio, is not
// omitempty, so that close is always present at a fixed two-byte offset from
// the end). Stripping the trailing "}}" exposes the session object's field
// list with its closing brace removed, tools is appended as its new last
// field, and both closing braces are appended back.
func buildSessionUpdate(instructions, voice string, tools json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(newSessionUpdate(instructions, voice))
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal session.update: %w", err)
	}
	if len(tools) == 0 {
		return base, nil
	}
	// Reject a malformed tools value here rather than splicing it in blind:
	// the byte concatenation below has no parser of its own, so a caller
	// mistake (a truncated fragment, an unbalanced bracket) would otherwise
	// produce an invalid handshake that is never reported as such — it would
	// just be written to the wire, and the failure would surface later and
	// unclearly, as a dial that never receives session.created. This checks
	// only that tools is syntactically valid JSON; what it contains stays
	// entirely the caller's concern, per WithTools' doc comment.
	if !json.Valid(tools) {
		return nil, fmt.Errorf("realtime: declared tools is not valid JSON: %s", tools)
	}
	if !bytes.HasSuffix(base, []byte("}}")) {
		return nil, fmt.Errorf("realtime: session.update did not end in the expected closing braces: %s", base)
	}
	body := base[:len(base)-2]
	out := make([]byte, 0, len(body)+len(`,"tools":`)+len(tools)+2)
	out = append(out, body...)
	out = append(out, `,"tools":`...)
	out = append(out, tools...)
	out = append(out, '}', '}')
	return out, nil
}

// voiceUpdateOutput is the mid-call frame's OUTPUT audio channel: a voice and
// nothing else. It is a separate type from audioChannel, not a re-use of it,
// and that is the whole point of this builder — audioChannel.Format has no
// omitempty, so marshalling one with no format set emits
// "format":{"type":""}. The backend deep-merges exactly the fields an update
// actually sent, so such a frame would overwrite the negotiated G.711 mu-law
// format on a live call. See BuildVoiceUpdate.
type voiceUpdateOutput struct {
	Voice string `json:"voice"`
}

type voiceUpdateAudio struct {
	Output voiceUpdateOutput `json:"output"`
}

// voiceUpdateSession carries Type because the backend maps session.update
// onto a discriminated union of session variants and rejects the
// transcription one explicitly; an update without "type":"realtime" does not
// parse as the accepted variant at all.
type voiceUpdateSession struct {
	Type  string           `json:"type"`
	Audio voiceUpdateAudio `json:"audio"`
}

type voiceUpdate struct {
	Type    string             `json:"type"`
	Session voiceUpdateSession `json:"session"`
}

// BuildVoiceUpdate marshals the minimal session.update that changes the
// backend's OUTPUT voice mid-call and touches nothing else:
//
//	{"type":"session.update","session":{"type":"realtime","audio":{"output":{"voice":"<id>"}}}}
//
// It is deliberately NOT a parameterisation of newSessionUpdate. That
// constructor builds the dial handshake, whose job is to declare the audio
// format on both directions; its audioChannel.Format has no omitempty because
// the handshake always sets it. Reusing it here would send two empty format
// types, and a backend that merges only the fields it was sent would take
// them literally — clearing the negotiated format on a call already carrying
// audio. Hence a separate, minimal shape.
//
// Exported, unlike buildSessionUpdate beside it, because its caller is in
// another package: the handshake is built and sent inside Dial, but the
// mid-call frame is built for HandleStreamRealtime to write through
// Client.Send. Send itself stays a raw client method — this adds a shape the
// engine can hand it, it does not restrict what else may be sent.
//
// The voice reaches the wire exactly as supplied: not validated, trimmed, or
// case-folded. Which names are legal is the backend's to say, the same rule
// WithVoice's doc comment states for the dial voice.
func BuildVoiceUpdate(voice string) ([]byte, error) {
	out, err := json.Marshal(voiceUpdate{
		Type: EventSessionUpdate,
		Session: voiceUpdateSession{
			Type:  sessionTypeRealtime,
			Audio: voiceUpdateAudio{Output: voiceUpdateOutput{Voice: voice}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: marshal voice session.update: %w", err)
	}
	return out, nil
}
