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

// ResponseEndedInFunctionCall reports whether a response.done frame's output
// items include a function call — the case where the turn is NOT over and the
// caller's wait continues into the tool round trip plus whatever the model
// says afterwards.
//
// "Include", not "end with": a response carrying speech AND a function call
// answers true. The question is whether anything more is coming, and a tool
// call means it is, whatever else the model said on the way.
//
// Exported so that more than one thing can ask it and get the same answer.
// Inside this module the relay's filler audio reads it to decide whether to
// keep a hold loop running. It is exported for a consumer that has to place
// the same boundary from the outside — one recording what the relay did, say.
// A second implementation of this shape would drift silently: both answers
// look plausible in isolation, and a disagreement surfaces only as one wait
// reported as two, or a loop stopped in the middle of one.
//
// It lives here rather than in the transport package that first needed it
// because it is protocol knowledge, not transport knowledge: it reads the
// realtime response object, and ItemTypeFunctionCall is declared above.
//
// IT CHECKS THE FRAME'S OWN TYPE, so it is safe to hand every server event
// rather than only the ones already known to be a response.done. Without that
// check the exported contract would carry an unstated precondition, and the
// caller most likely to miss it is exactly the one this is exported for: a
// consumer piping every frame through it would get true for anything that
// happened to nest response.output[].type == "function_call".
//
// Read from the raw frame rather than from a modelled field because this
// package models only the subset of the protocol it acts on, and one bool
// about one event type is not a reason to grow a decoded shape for the
// response object. The depth is load-bearing and not incidental: the item
// type is read at response.output[].type and nowhere else, so a frame merely
// containing the token — inside a transcript, or at the wrong nesting —
// answers false.
//
// A frame that does not parse, or that carries no output items, reports false.
// The conservative answer is "the turn ended", which leaves a filler loop
// stopped rather than playing over whatever comes next — a caller hearing
// silence a moment early is a smaller fault than one hearing music over the
// reply.
func ResponseEndedInFunctionCall(raw json.RawMessage) bool {
	var probe struct {
		Type     string `json:"type"`
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Type != EventResponseDone {
		return false
	}
	for _, item := range probe.Response.Output {
		if item.Type == ItemTypeFunctionCall {
			return true
		}
	}
	return false
}

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
	Instructions string `json:"instructions,omitempty"`
	// ClientSessionID is the consumer's OWN identifier for this session
	// (AATK-136), carried so a backend can correlate the work it does for
	// this session with whatever the consumer knows it by. This engine mints
	// nothing and reads nothing back: the value is opaque here.
	//
	// This tag is the wire name's only appearance in prose — both
	// WithSessionID doc comments point here rather than spelling it again.
	// The tests do spell it, because they assert on wire bytes, so a rename
	// touches them too.
	//
	// It is an ENGINE EXTENSION, not a field of the protocol this package
	// speaks. instructions, voice and tools are all defined by the backend's
	// own session object; this one is a convention between a consumer and
	// whatever reads its handshake, so a backend has to be taught it. A
	// backend that refuses the field fails the dial: if it closes the socket
	// that arrives promptly as a read error, and if it answers with an error
	// frame and waits, Dial surfaces that frame in its own error (see the
	// handshake loop) once the caller's context ends. Either way the field's
	// absence from the protocol is worth knowing before turning it on.
	//
	// It is named for the client because the server's own `id` on
	// session.created is a different value with a different owner, and a
	// backend reading both must not have to guess which it has.
	//
	// omitempty for the reason Instructions gives above, and the regression
	// guard for it is the byte-for-byte baseline test at the twilio layer.
	//
	// Unlike tools this is an ordinary struct field, marshalled by
	// encoding/json like any other. buildSessionUpdate's doc comment explains
	// why tools cannot be: the encoder rewrites raw JSON bytes, reordering
	// and dropping what it does not understand. A string has no structure to
	// lose — '<', '>' and '&' become <, > and &, and decode
	// back to the identical string — so copying the splice here would defend
	// against nothing.
	//
	// One value does NOT survive: a string carrying invalid UTF-8 is silently
	// rewritten to U+FFFD, with no error at any layer, and the splice would
	// not help (raw invalid UTF-8 is not valid JSON either). A consumer
	// minting an identifier from a byte slice rather than text should make it
	// valid UTF-8 first. This is encoding/json's behaviour for every string
	// field here, Instructions and Voice included; it is written down at this
	// one because this field is the one a backend keys state on.
	ClientSessionID string       `json:"client_session_id,omitempty"`
	Audio           sessionAudio `json:"audio"`
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
//
// sessionID is the consumer's own identifier for the session; see
// sessionSpec.ClientSessionID. Empty omits the field entirely, same as the
// other two.
func newSessionUpdate(instructions, voice, sessionID string) sessionUpdate {
	format := audioFormat{Type: FormatG711ULaw}
	return sessionUpdate{
		Type: EventSessionUpdate,
		Session: sessionSpec{
			Type:            sessionTypeRealtime,
			Instructions:    instructions,
			ClientSessionID: sessionID,
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
//
// Stated precisely, because AATK-136 added a sessionSpec field and the
// question "where may it go?" came up: the splice needs (a) Session to remain
// sessionUpdate's LAST field, and (b) sessionSpec to be incapable of
// marshalling to an empty object. Nothing else — in particular, field ORDER
// within sessionSpec is irrelevant, and so is whether a new field carries
// omitempty. Put one anywhere.
//
// (b) is the one that can be broken, and Audio is what holds it: a struct
// field is emitted whatever its tag says, because omitempty has no effect on
// a struct. Type helps only while it stays a non-omitempty string. So the
// ways to break (b) are to remove Audio, to change it to something omittable,
// or to reach for Go 1.24's omitzero, which unlike omitempty DOES drop a zero
// struct. Then base ends `"session":{}}`, the suffix check still passes, and
// the splice emits `"session":{,"tools":…` — invalid JSON, reported by
// nothing here, surfacing as a dial that never completes.
//
// TestBuildSessionUpdate_SessionSpecCannotMarshalEmpty is the guard; this
// paragraph is not. Two earlier versions of it stated the rule wrongly in
// opposite directions, and the second was written specifically to correct the
// first, so the invariant now lives in a test that marshals the real type.
func buildSessionUpdate(instructions, voice, sessionID string, tools json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(newSessionUpdate(instructions, voice, sessionID))
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
