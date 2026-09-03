package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// realtimeDialTimeout bounds the backend handshake. A backend that neither
// accepts nor refuses would otherwise hold the call open with silence on the
// line for as long as the carrier tolerates it.
const realtimeDialTimeout = 10 * time.Second

// realtimeClientEventSendTimeout bounds one consumer-event write to the
// backend, for the same reason realtimeDialTimeout bounds the handshake: a
// backend that neither accepts nor refuses would otherwise hold the call open
// indefinitely.
//
// It matters more here than anywhere else on this path, and the reason is
// which goroutine does the writing. Carrier audio is written from
// pumpCarrierToBridge's goroutine, so a write that parks there still leaves
// HandleStreamRealtime's select loop free to observe backendDone,
// carrierDone, and the idle timer — the call still ends. A consumer event is
// written from that select loop ITSELF, so a write that parks there parks the
// loop, and every one of those endings stops being observed: a backend that
// has stopped reading its socket (buffers fill, the write blocks) is exactly
// the case WithIdleTimeout exists to catch, and it is exactly the case that
// would prevent the timer from ever being selected on. Measured before this
// bound existed: with a 200 ms idle timeout and a write that blocks, the call
// was still hung 3 s later.
//
// It bounds one more write, on the other socket: the mark handleMarkRequest
// writes to the CARRIER. The reason is the same one stated above and so is the
// value, so this stays the single definition rather than growing a near-identical
// twin (CLAUDE.md #5) — that write also happens on the select loop, and the
// write slot it queues behind is held by carrier audio written on the call's own
// unbounded context.
//
// Generous by orders of magnitude for what it bounds — a client event is one
// small frame, which a backend that is reading at all accepts in
// microseconds. Exceeding it means the write half is wedged, and the call
// then ends the same way any other failed send does: logged, with a non-nil
// error.
const realtimeClientEventSendTimeout = 5 * time.Second

// dialRealtime is a seam so tests can substitute a custom dial (e.g. to
// inject a transport-level fault). Production code never overrides it.
var dialRealtime = realtime.Dial

// NewStreamHandler selects the transport for a call.
//
// realtimeURL == "" is the default and returns DefaultHandleStream unchanged:
// the existing VAD/STT/TTS sidecar path, with no realtime client constructed
// and no dial attempted. Non-empty routes the call to an external realtime
// voice backend instead.
//
// The realtime path changes turn-taking, which is the reason this defaults off:
// see HandleStreamRealtime for what is bypassed and why the replay corpus does
// not transfer. Which turn-taking is better is a question for measurement, in
// its own ticket; this switch exists so both stacks can be run and compared
// without a redeploy.
//
// Options apply to the realtime path only. Supplying one alongside an empty
// realtimeURL is a no-op, because the default path returns DefaultHandleStream
// and never reaches the realtime transport at all.
func NewStreamHandler(realtimeURL string, opts ...RealtimeOption) StreamHandler {
	if realtimeURL == "" {
		return DefaultHandleStream
	}
	return func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, realtimeURL, opts...)
	}
}

// RealtimeOption configures one call on the realtime transport. It mirrors
// telephony.SessionOption, which configures the default path the same way, so
// the two transports are extended with one vocabulary rather than two.
//
// The zero set of options is today's behavior exactly. Every option is
// therefore additive: a caller that supplies none gets what a caller before
// that option existed got.
type RealtimeOption func(*realtimeConfig)

// realtimeConfig is the resolved option set for one call. Unexported: the
// options are the API, and a struct literal would be a second way to build the
// same value.
type realtimeConfig struct {
	idleTimeout time.Duration

	// instructionsFor is resolved per call rather than stored as a string, and
	// that is the whole point. NewStreamHandler binds its options once at
	// construction and reuses them for every call it serves, so a plain string
	// here would be per-process on that path — a consumer could never vary the
	// persona by caller, which is the case this exists for. A function of the
	// call is resolved when the call arrives, on both entry points.
	instructionsFor func(start Frame) string

	// voice is the constant case, like instructionsFor's non-"For" sibling
	// WithInstructions: a plain string rather than a per-call resolver.
	// Before this field, Instructions was the only session field this engine
	// exposed; a general session-config passthrough is explicitly out of
	// scope (it would let a consumer reconfigure turn-taking, which this
	// engine has opinions about), so voice is added as its own field rather
	// than opening that passthrough. Voice does not touch turn-taking. With
	// no stated need to vary it per call, there is no WithVoiceFor to go
	// with it.
	//
	// This is the voice the call OPENS with, not the voice for its whole
	// life: voiceUpdateChanFor below carries mid-call changes, taking the
	// identical shape for the identical reason.
	voice string

	// tools mirrors voice: a plain value rather than a per-call resolver.
	// AATK-85's behaviours ask for one declaration at handshake time, not a
	// per-call resolver, so there is no WithToolsFor.
	tools json.RawMessage

	transcriptChanFor func(start Frame) chan<- Transcript

	// serverEventChanFor mirrors transcriptChanFor: resolved per call rather
	// than stored as a plain channel, for the same reason.
	serverEventChanFor func(start Frame) chan<- ServerEvent

	// clientEventChanFor mirrors serverEventChanFor, direction reversed: the
	// consumer writes, the engine reads. Resolved per call for the same
	// reason as the others — NewStreamHandler binds its options once and
	// reuses them for every call it serves.
	clientEventChanFor func(start Frame) <-chan json.RawMessage

	// voiceUpdateChanFor mirrors clientEventChanFor — the consumer writes,
	// the engine reads — but carries voice IDs rather than event bytes.
	// AATK-104: it is the mid-call half of the same policy voice's field
	// above states for the dial half. A general session-config passthrough
	// stays out of scope, so voice gets its own seam here too, and because
	// the engine marshals the frame a consumer cannot reach turn detection,
	// instructions or tools through it even by accident.
	voiceUpdateChanFor func(start Frame) <-chan string

	// carrierAudioChanFor mirrors serverEventChanFor: resolved per call
	// rather than stored as a plain channel, for the same reason.
	carrierAudioChanFor func(start Frame) chan<- CarrierAudio

	// markRequestChanFor mirrors voiceUpdateChanFor — the consumer writes,
	// the engine reads — but carries the name of a Twilio mark to place after
	// the audio written so far. AATK-105: it is the request half of the
	// playout-position seam this path had no answer for, the echo half being
	// markEchoChanFor below.
	markRequestChanFor func(start Frame) <-chan string

	// markEchoChanFor mirrors carrierAudioChanFor: the engine writes, the
	// consumer reads, resolved per call for the same reason.
	markEchoChanFor func(start Frame) chan<- MarkEcho
}

// CarrierAudio is one record of what the engine sent to the carrier —
// mirrors Transcript's shape (telephony/realtime/bridge.go): a payload field
// plus a bool distinguishing the two record kinds this ticket names, rather
// than a struct-per-kind or a separate enum.
//
// Clear == true means this record is a barge-in signal (carrierMediaSink.Clear)
// and Payload is empty. Clear == false means this record is one chunk of
// synthesized audio (carrierMediaSink.Media), carried as the same base64
// string the carrier received — never decoded, never re-encoded.
type CarrierAudio struct {
	Payload string
	Clear   bool
}

// Transcript is one transcription result from the backend, re-exported so a
// consumer does not have to import telephony/realtime to name the type its
// channel carries.
type Transcript = realtime.Transcript

// ServerEvent is one backend server event, re-exported so a consumer does not
// have to import telephony/realtime to name the type its channel carries. It
// mirrors Transcript's re-export.
type ServerEvent = realtime.ServerEvent

// transcriptChan resolves the destination for one call, or nil when the
// consumer asked for none.
func (c realtimeConfig) transcriptChan(start Frame) chan<- Transcript {
	if c.transcriptChanFor == nil {
		return nil
	}
	return c.transcriptChanFor(start)
}

// serverEventChan resolves the destination for one call, or nil when the
// consumer asked for none. Mirrors transcriptChan.
func (c realtimeConfig) serverEventChan(start Frame) chan<- ServerEvent {
	if c.serverEventChanFor == nil {
		return nil
	}
	return c.serverEventChanFor(start)
}

// clientEventChan resolves the source for one call, or nil when the consumer
// supplied none. Mirrors serverEventChan, direction reversed.
func (c realtimeConfig) clientEventChan(start Frame) <-chan json.RawMessage {
	if c.clientEventChanFor == nil {
		return nil
	}
	return c.clientEventChanFor(start)
}

// voiceUpdateChan resolves the source for one call, or nil when the consumer
// supplied none. Mirrors clientEventChan.
func (c realtimeConfig) voiceUpdateChan(start Frame) <-chan string {
	if c.voiceUpdateChanFor == nil {
		return nil
	}
	return c.voiceUpdateChanFor(start)
}

// carrierAudioChan resolves the destination for one call, or nil when the
// consumer asked for none. Mirrors serverEventChan.
func (c realtimeConfig) carrierAudioChan(start Frame) chan<- CarrierAudio {
	if c.carrierAudioChanFor == nil {
		return nil
	}
	return c.carrierAudioChanFor(start)
}

// markRequestChan resolves the source for one call, or nil when the consumer
// supplied none. Mirrors voiceUpdateChan.
func (c realtimeConfig) markRequestChan(start Frame) <-chan string {
	if c.markRequestChanFor == nil {
		return nil
	}
	return c.markRequestChanFor(start)
}

// markEchoChan resolves the destination for one call, or nil when the consumer
// asked for none. Mirrors carrierAudioChan.
func (c realtimeConfig) markEchoChan(start Frame) chan<- MarkEcho {
	if c.markEchoChanFor == nil {
		return nil
	}
	return c.markEchoChanFor(start)
}

func resolveRealtimeConfig(opts []RealtimeOption) realtimeConfig {
	var cfg realtimeConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithIdleTimeout bounds how long the call may run with the backend producing
// no event. Unset, or zero, arms no timer: unbounded, exactly the behavior of a
// call with no options at all.
//
// A positive value is reset on every backend event bridge.Run observes, of any
// type. Carrier frames do NOT reset it, so a carrier that is streaming
// continuously cannot mask a backend that has gone silent. If it elapses with
// no backend activity, the call ends with an error naming "idle timeout".
//
// A hung backend and a legitimately quiet one look identical on this path —
// there is no local VAD or turn state to tell them apart (see
// telephony.MaxSilenceMS for the order of magnitude the default transport uses
// to make that call). Set this longer than the longest backend-quiet interval
// the deployment tolerates, or a normal pause in conversation will end the
// call.
func WithIdleTimeout(d time.Duration) RealtimeOption {
	return func(c *realtimeConfig) { c.idleTimeout = d }
}

// WithInstructions sets one session persona for every call this option is
// applied to. It is the constant case, and it is sugar for WithInstructionsFor
// with a function that ignores the call.
//
// Empty text sends no instructions field at all, which is byte-for-byte the
// handshake a caller supplying no option gets.
func WithInstructions(text string) RealtimeOption {
	return WithInstructionsFor(func(Frame) string { return text })
}

// WithVoice sets the backend's output voice for every call this option is
// applied to. A general session-config passthrough is out of scope — it would
// let a consumer reconfigure turn-taking, which this engine has opinions
// about — so this adds voice as its own field rather than opening that
// passthrough. Voice does not touch turn-taking.
//
// This engine does not validate, trim, or case-fold the name — the backend
// owns which names it accepts, so whatever is supplied here reaches the wire
// unmodified.
//
// Empty omits the field entirely, which is byte-for-byte the handshake a
// caller supplying no option gets.
//
// This sets the voice the call OPENS with. It is no longer the only chance to
// set one: WithVoiceUpdateChan changes the backend's output voice mid-call,
// through a seam of the same shape and for the same reason — voice gets its
// own channel rather than the session-config passthrough this comment rules
// out.
func WithVoice(name string) RealtimeOption {
	return func(c *realtimeConfig) { c.voice = name }
}

// WithTools declares the consumer's tool definitions for every call this
// option is applied to (AATK-85). This engine models nothing about what a
// tool is, or how one is dispatched — that is entirely the consumer's — so
// tools is carried as json.RawMessage and reaches the handshake exactly as
// supplied, the same reasoning WithVoice's doc comment gives for staying out
// of session-config modelling generally.
//
// Nil (the default) omits the field entirely, byte-identical to the
// handshake a caller supplying no option gets. A tools value that is not
// syntactically valid JSON makes HandleStreamRealtime return an error
// rather than sending a malformed handshake.
func WithTools(tools json.RawMessage) RealtimeOption {
	return func(c *realtimeConfig) { c.tools = tools }
}

// WithTranscriptChan delivers each transcript the backend produces, partial and
// final alike, to ch. Without it the engine drains transcripts and discards
// them, which is what it did before this existed.
//
// The consumer owns ch: its buffer, its reader, and its lifetime. The engine
// NEVER closes it, so it may be reused across calls and a reader is never handed
// a spurious zero value.
//
// Delivery is non-blocking. When ch is full the transcript is DROPPED. Every
// drop is counted and the count is logged at the bounded rate realtime.LogDrop
// defines — the first drop of a call and periodically after it, each line
// carrying the running total. That is deliberate rather than a limitation:
// Bridge.publish
// parks the backend read loop when nobody reads, and that loop also drives audio
// to the carrier, so waiting on a slow consumer would break the call. A late
// transcript is worth less than unbroken audio.
//
// The engine runs no consumer code, which is the point of the shape. Every
// goroutine it creates blocks only on things it owns — a channel it closes, a
// context it cancels, a select with a default — so a consumer that never reads
// costs nothing once the call ends.
func WithTranscriptChan(ch chan<- Transcript) RealtimeOption {
	return WithTranscriptChanFor(func(Frame) chan<- Transcript { return ch })
}

// WithTranscriptChanFor resolves the destination when the call arrives, from the
// start frame — so a consumer can route calls to different channels.
//
// Prefer this over WithTranscriptChan wherever one channel will not do. Options
// given to NewStreamHandler are bound once, at construction, and reused for
// every call that handler serves; passing a function is what makes the choice
// per-call there rather than per-process. Mirrors WithInstructionsFor.
//
// A nil function, or one returning nil, means no consumer: the engine drains and
// discards, exactly as with no option at all.
func WithTranscriptChanFor(fn func(start Frame) chan<- Transcript) RealtimeOption {
	return func(c *realtimeConfig) { c.transcriptChanFor = fn }
}

// WithServerEventChan delivers every backend server event to ch, mirroring
// WithTranscriptChan — including event types this engine does not model into
// a transcript or a media/clear call, carried with Raw set to the whole frame
// as it arrived. Without it those events are observed only by Bridge.Activity
// and otherwise dropped, which is what happens before this existed.
//
// The consumer owns ch: its buffer, its reader, and its lifetime. The engine
// NEVER closes it, so it may be reused across calls and a reader is never
// handed a spurious zero value.
//
// Delivery is non-blocking. When ch is full the event is DROPPED, counted, and
// the count logged at realtime.LogDrop's bounded rate as above — but not for
// WithTranscriptChan's reason, and the difference is
// worth stating. That one drops because Bridge.publish parks the backend read
// loop when nobody reads, and that loop also drives carrier audio.
// Bridge.publishEvent already drops rather than parking, so a blocking send
// here could not stall that loop; what it would do instead is stop draining
// Bridge.Events() and leave this engine goroutine parked past the end of the
// call — a bounded loss turned into a total one, plus a leak.
func WithServerEventChan(ch chan<- ServerEvent) RealtimeOption {
	return WithServerEventChanFor(func(Frame) chan<- ServerEvent { return ch })
}

// WithServerEventChanFor resolves the destination when the call arrives, from
// the start frame — so a consumer can route calls to different channels.
// Mirrors WithTranscriptChanFor.
//
// A nil function, or one returning nil, means no consumer.
func WithServerEventChanFor(fn func(start Frame) chan<- ServerEvent) RealtimeOption {
	return func(c *realtimeConfig) { c.serverEventChanFor = fn }
}

// WithCarrierAudioChan delivers a record of every payload the engine sends to
// the carrier — media and clears alike — to ch, mirroring WithServerEventChan:
// the consumer owns ch (its buffer, its reader, its lifetime), the engine
// NEVER closes it, and delivery is non-blocking — a full ch drops the record,
// counted and logged, rather than parking the goroutine that also drives
// carrier audio.
//
// Without this option the engine sends carrier audio exactly as it does
// today, with no observer.
func WithCarrierAudioChan(ch chan<- CarrierAudio) RealtimeOption {
	return WithCarrierAudioChanFor(func(Frame) chan<- CarrierAudio { return ch })
}

// WithCarrierAudioChanFor resolves the destination when the call arrives,
// from the start frame — so a consumer can route different calls to
// different channels. Mirrors WithServerEventChanFor.
//
// A nil function, or one returning nil, means no consumer.
func WithCarrierAudioChanFor(fn func(start Frame) chan<- CarrierAudio) RealtimeOption {
	return func(c *realtimeConfig) { c.carrierAudioChanFor = fn }
}

// WithClientEventChan lets a consumer send its own events to the backend for
// the whole life of one call: ch is read, and every value read from it is
// forwarded to the backend via realtime.Client.Send, byte-for-byte, exactly
// as the consumer supplied it, with one exception. This package does not
// model, validate, or interpret what a consumer puts on ch — a
// conversation.item.create, a response.create, function-call output, or
// anything else the backend's protocol accepts — except that a session.update
// is refused: it is never forwarded, and is logged instead. It would
// permanently rewrite the session config the handshake already established;
// set that through this package's own options instead.
//
// The exception is only as good as the "type" field is readable. An event
// whose type cannot be read at all — malformed JSON, or a "type" that is
// absent or not a string — is forwarded unchanged and logs nothing, exactly
// as it did before the exception existed: a parse failure never refuses and
// never ends the call, leaving the backend the judge of what it accepts. So
// bytes spelling a session.update can still leave this process — in a form
// the backend will reject, which is what makes leaving it the judge safe.
//
// Nor is the read quite the protocol's. The probe decodes "type" through
// encoding/json, which matches field names case-insensitively where the wire
// format does not: an event whose only such key is "Type" or "TYPE" is
// refused as though it carried "type", and one carrying more than one is
// judged on whichever decodes last. Read the refusal as policy against the
// session.update a consumer sends by mistake, not as a gate against one
// spelled to get past it.
//
// The arrows reverse here relative to WithTranscriptChan and
// WithServerEventChan: the consumer writes, the engine reads. The engine
// never blocks waiting for a value, survives ch never being written to for
// the whole call, and never requires ch to be closed — a consumer that never
// sends is exactly today's behavior, byte-for-byte. The read lives in the
// same select loop HandleStreamRealtime already runs, so no goroutine is ever
// spawned to service ch and none can outlive the call.
//
// That leak-freedom is the ENGINE's, not the consumer's, and the distinction
// is another consequence of the reversal. Once the call ends nothing reads ch
// again — the select loop is gone, and the engine never drains what it does
// not own — so a consumer parked in a send on an unbuffered or full ch stays
// parked forever: one leaked goroutine per call that ended while it was
// sending. Buffer ch, or select the send against a done signal the consumer
// closes when its own call returns. The engine cannot raise that signal,
// precisely because ch is the consumer's.
//
// ONE ch SERVES ONE CALL AT A TIME, and that is the consequence of the
// reversal that bites. On the engine-writes options a single ch shared by
// concurrent calls fans IN, and is safe; here it fans OUT, and a channel
// value goes to exactly ONE receiver. Two calls running concurrently off the
// same ch therefore SPLIT the stream between them at random: an event the
// consumer meant for one call is forwarded to the other call's backend, and
// nothing reports it. Reuse across SEQUENTIAL calls is fine — the engine
// never closes ch. For calls that can overlap, which is precisely what
// NewStreamHandler serves (it binds this option once and reuses it for every
// call), use WithClientEventChanFor and give each call its own channel.
//
// A value read from ch does NOT reset the idle timer armed by
// WithIdleTimeout: only backend activity does. A consumer that sends steadily
// against a backend that has gone silent must not mask that silence.
//
// A send that fails is logged, and the call ends with a non-nil error — the
// same ending path as the backend going away. This transport's underlying
// writer poisons itself after one write error (every subsequent write to
// that connection fails forever), so a failed send here is never transient;
// continuing the call would mean running it against a permanently dead
// backend link.
//
// A send that never completes is bounded rather than waited on: the write is
// given realtimeClientEventSendTimeout, and a backend that has stopped
// reading its socket ends the call on that bound instead of wedging it. The
// forwarding read shares HandleStreamRealtime's select loop with every other
// way the call can end, so an unbounded write here would silence the idle
// timer and the carrier hangup along with itself.
func WithClientEventChan(ch <-chan json.RawMessage) RealtimeOption {
	return WithClientEventChanFor(func(Frame) <-chan json.RawMessage { return ch })
}

// WithClientEventChanFor resolves the source when the call arrives, from the
// start frame — so a consumer can route different calls to different
// channels. Mirrors WithServerEventChanFor, direction reversed.
//
// A nil function, or one returning nil, means no consumer: the engine never
// reads anything beyond what it already reads, exactly as with no option at
// all.
func WithClientEventChanFor(fn func(start Frame) <-chan json.RawMessage) RealtimeOption {
	return func(c *realtimeConfig) { c.clientEventChanFor = fn }
}

// WithVoiceUpdateChan lets a consumer change the backend's OUTPUT voice
// during a call (AATK-104): every non-empty value read from ch is turned into
// a minimal session.update and sent to the backend, and the call carries on.
//
// The channel carries a voice ID — a string — not event bytes, and that is
// the point of it. This engine builds the frame itself, so what crosses the
// wire is exactly {"type":"session.update","session":{"type":"realtime",
// "audio":{"output":{"voice":"<id>"}}}} and nothing else: no instructions, no
// tools, no turn_detection, and no audio format on either direction. A
// consumer cannot reach the rest of the session config through this seam even
// by mistake, which is what lets the seam exist at all — the general
// session-config passthrough WithVoice's doc rules out stays shut, and
// WithClientEventChan's refusal of a consumer-supplied session.update is
// untouched. The two are halves of one policy: session config changes through
// this package's own options, never through consumer-supplied event bytes.
//
// The voice ID is not validated, trimmed, or case-folded — which names a
// backend accepts is the backend's to say, the same rule WithVoice states for
// the dial voice. An EMPTY string is the one value that is not forwarded: an
// empty voice is what WithVoice means by "no voice", so sending one would ask
// the backend to unset a field rather than to change it, and this package has
// nothing to say about what unsetting a voice mid-call means.
//
// A voice update is a consumer event, not backend activity: it does not reset
// the idle timeout. A chatty consumer against a silent backend must not mask
// that silence.
//
// Closing ch is not an error and does not end the call — the engine simply
// stops reading it. A send that never completes is bounded rather than waited
// on, exactly as WithClientEventChan's is and for the same reason: the read
// shares HandleStreamRealtime's select loop with every other way the call can
// end, so an unbounded write here would silence the idle timer and the
// carrier hangup along with itself. A send that FAILS is logged, and the call
// ends with a non-nil error — the same ending path as the backend going away,
// because this transport's writer poisons itself after one write error.
//
// Everything WithClientEventChan's doc says about the reversed arrow — the
// consumer writes, the engine reads — holds here unchanged, because it is the
// same reversal, and the two consequences that bite are stated there in full
// rather than restated here. In short: the engine's leak-freedom is not the
// CONSUMER's, so a consumer parked in a send on an unbuffered or full ch when
// the call ends stays parked forever; and ONE ch SERVES ONE CALL AT A TIME,
// so two calls running concurrently off the same ch split the stream between
// them at random — a voice meant for one caller silently changes the other
// caller's instead. NewStreamHandler binds this option once and reuses it for
// every call it serves, so for calls that can overlap use
// WithVoiceUpdateChanFor and give each call its own channel.
func WithVoiceUpdateChan(ch <-chan string) RealtimeOption {
	return WithVoiceUpdateChanFor(func(Frame) <-chan string { return ch })
}

// WithVoiceUpdateChanFor resolves the source when the call arrives, from the
// start frame — so a consumer can route different calls to different
// channels. Mirrors WithClientEventChanFor.
//
// A nil function, or one returning nil, means no consumer: the engine never
// sends a mid-call session.update at all, and the frames the backend receives
// are byte-for-byte what it receives with no option supplied.
func WithVoiceUpdateChanFor(fn func(start Frame) <-chan string) RealtimeOption {
	return func(c *realtimeConfig) { c.voiceUpdateChanFor = fn }
}

// WithMarkRequestChan lets a consumer ask "has everything I sent to the
// carrier actually played yet?" (AATK-105): every non-empty name read from ch
// is written to the carrier as a Twilio mark, placed AFTER every media frame
// the engine had already written, and the carrier echoes it back once that
// audio has finished playing. WithMarkEchoChan is where the echo arrives.
//
// The classic path has answered that question since SOP-125 and this path
// could not: the backend owns playback here, so nothing local consumed marks
// and an inbound one was dropped. A consumer can still have a reason to care
// about the carrier's playout position — ending a call after a spoken line,
// without cutting the line mid-word — and the alternative was for it to model
// the carrier as a real-time player and add up the audio deltas it observed.
// That estimate is short by however many deltas it missed, because they reach
// a consumer through a seam that drops when the consumer is behind.
//
// Ordering is the property being bought, and it comes from WHERE the write
// happens: through carrierMediaSink's own write slot, never from this
// package's select loop directly. See handleMarkRequest, which states the
// hazard in full — a second writer on the carrier connection, and a mark able
// to land ahead of audio the bridge has not yet flushed, so the echo would
// report a playout position that has not happened.
//
// NAME EVERY MARK DISTINCTLY. A consumer may have more than one outstanding,
// the wire carries nothing but the name, and an echo is matched on it: reusing
// a name while the first is still in flight collapses the two, and the engine
// logs that and re-arms rather than tracking both. An empty name is refused
// outright — its echo could never be matched to the request that caused it.
//
// A mark request does NOT reset the idle timer armed by WithIdleTimeout: only
// backend activity does. A consumer marking steadily against a backend that
// has gone silent must not mask that silence.
//
// A write that fails is logged and ends the call with a non-nil error, and one
// that never completes is bounded rather than waited on — both exactly as
// WithClientEventChan's are, and for the reasons stated there.
//
// Everything WithClientEventChan's doc says about the reversed arrow — the
// consumer writes, the engine reads — holds here unchanged. In short: the
// engine's leak-freedom is not the CONSUMER's, so a consumer parked in a send
// on an unbuffered or full ch when the call ends stays parked forever; and ONE
// ch SERVES ONE CALL AT A TIME, so two concurrent calls off the same ch split
// the stream between them at random — a mark meant for one caller is written
// to the other caller's carrier instead. NewStreamHandler binds this option
// once and reuses it for every call it serves, so for calls that can overlap
// use WithMarkRequestChanFor and give each call its own channel.
//
// Supplying neither this nor WithMarkEchoChan is byte-for-byte today's
// behavior: no mark is written, and an inbound mark frame is still ignored.
func WithMarkRequestChan(ch <-chan string) RealtimeOption {
	return WithMarkRequestChanFor(func(Frame) <-chan string { return ch })
}

// WithMarkRequestChanFor resolves the source when the call arrives, from the
// start frame — so a consumer can route different calls to different channels.
// Mirrors WithVoiceUpdateChanFor.
//
// A nil function, or one returning nil, means no consumer: the engine writes
// no mark at all.
func WithMarkRequestChanFor(fn func(start Frame) <-chan string) RealtimeOption {
	return func(c *realtimeConfig) { c.markRequestChanFor = fn }
}

// WithMarkEchoChan delivers each resolved mark to ch: the carrier has finished
// playing everything written ahead of that mark. It is the answer half of
// WithMarkRequestChan, and it mirrors WithCarrierAudioChan — the consumer owns
// ch (its buffer, its reader, its lifetime), the engine NEVER closes it, and
// delivery is non-blocking, so a full ch drops the record (counted and logged)
// rather than parking an engine goroutine.
//
// A record arrives for a requested mark in exactly two cases, distinguished by
// MarkEcho.TimedOut: the carrier echoed it, or it did not and the engine's own
// bound elapsed. THE BOUND LIVES HERE, not in the consumer — see
// carrierMediaSink.markEchoBound, which derives it from the playout still
// queued ahead of the mark plus telephony.MarkEchoGraceMS, the same grace the
// classic path's MarkEchoTimeout adds atop a clip. So a carrier that never
// honors the mark protocol cannot leave a consumer waiting, and a carrier that
// is honoring it perfectly is not cut short for still playing.
//
// An echo whose name matches nothing outstanding is logged and NOT delivered:
// it is not the mark anything is waiting on, and delivering it as one is the
// mistake a consumer with two marks in flight cannot recover from.
//
// Nothing is delivered once the call has ended. A consumer must not treat this
// channel as the only way its wait can finish — the call ending is the other,
// and it is the consumer's own context that ends it.
func WithMarkEchoChan(ch chan<- MarkEcho) RealtimeOption {
	return WithMarkEchoChanFor(func(Frame) chan<- MarkEcho { return ch })
}

// WithMarkEchoChanFor resolves the destination when the call arrives, from the
// start frame — so a consumer can route different calls to different channels.
// Mirrors WithCarrierAudioChanFor.
//
// A nil function, or one returning nil, means no consumer: a requested mark is
// still written and still tracked, but its resolution goes nowhere.
func WithMarkEchoChanFor(fn func(start Frame) chan<- MarkEcho) RealtimeOption {
	return func(c *realtimeConfig) { c.markEchoChanFor = fn }
}

// WithInstructionsFor resolves the session persona when the call arrives, from
// the start frame — which carries the caller identity, so a consumer can vary
// the persona per caller.
//
// Prefer this over WithInstructions wherever the text is not a constant.
// Options given to NewStreamHandler are bound once, at construction, and reused
// for every call that handler serves; passing a function is what makes the text
// per-call there rather than per-process. On a direct HandleStreamRealtime call
// the distinction does not arise, because the options are supplied per call
// already.
//
// A nil function, or one returning empty, sends no instructions field.
func WithInstructionsFor(fn func(start Frame) string) RealtimeOption {
	return func(c *realtimeConfig) { c.instructionsFor = fn }
}

// instructions resolves the persona for one call. Kept off the option
// constructors so the nil-function case has exactly one home.
func (c realtimeConfig) instructions(start Frame) string {
	if c.instructionsFor == nil {
		return ""
	}
	return c.instructionsFor(start)
}

// HandleStreamRealtime drives one call over the realtime voice backend. It is
// the realtime peer of HandleStreamWithOpts: same entry shape, called with the
// (ctx, conn, start) a StreamHandler receives, so a consumer that owns its own
// handler body — because its per-call setup and teardown are bound to that
// scope — can select this transport by branching on its own configuration
// rather than going through NewStreamHandler.
//
// Carrier audio crosses to the backend as the base64 string it arrived as, and
// the backend's audio comes back the same way — no transcoding in either
// direction.
//
// TURN-TAKING CONSEQUENCE, stated here rather than at NewStreamHandler because
// a consumer calling this directly never reads that doc: on this path the
// *backend's* VAD and turn logic govern the conversation, so
// telephony/decision.go — this engine's own turn-taking decision function — is
// bypassed entirely for those turns, along with the VAD tuning that feeds it.
// That is a real behavioral change, not a transport swap, and the replay corpus
// was captured against the current decision function, so its labels do NOT
// transfer to calls run this way and cannot be used to judge them.
//
// The call ends when either side does: the carrier hangs up (a stop frame or a
// read error) or the backend goes away. A backend that fails to dial, or drops
// mid-call, ends the call with a logged error rather than leaving the session
// hung.
//
// Options configure one call; see RealtimeOption and WithIdleTimeout. Supplying
// none is unbounded, exactly the behavior this function had before options
// existed.
func HandleStreamRealtime(ctx context.Context, conn *websocket.Conn, start Frame, url string, opts ...RealtimeOption) error {
	cfg := resolveRealtimeConfig(opts)

	// CloseNow on every exit path, not Close: there is no local session to
	// drain, and closing the carrier is also what unblocks the carrier pump
	// below if it is still parked in Read, so no goroutine outlives this
	// function however the call ends.
	defer func() { _ = conn.CloseNow() }()

	dialCtx, cancelDial := context.WithTimeout(ctx, realtimeDialTimeout)
	defer cancelDial()

	client, err := dialRealtime(dialCtx, url,
		realtime.WithInstructions(cfg.instructions(start)),
		realtime.WithVoice(cfg.voice),
		realtime.WithTools(cfg.tools),
	)
	if err != nil {
		log.Printf("twilio: realtime: dial: %v", err)
		return err
	}
	defer client.Close()

	// carrierAudioChan is resolved ONCE, here, before bridge.Run is spawned —
	// not inside carrierMediaSink.Media/Clear. Those are called once per 20 ms
	// audio frame from bridge.Run's own read-loop goroutine, and re-invoking
	// consumer code there on every frame is exactly the hazard this file's
	// drain-ordering comment above warns about for clientEventChan.
	//
	// Both mark channels are resolved here too, earlier than clientEventChan
	// and voiceUpdateChan below, and for a reason of construction rather than
	// preference: the sink needs the tracker and the tracker needs the echo
	// destination, and whether a tracker exists at all depends on whether
	// EITHER mark option was supplied. Resolving them here runs consumer code
	// on this goroutine before bridge.Run is spawned, the same cost
	// carrierAudioChan already pays and for the same reason; it is far ahead
	// of the idle guard being armed, which is the ordering that comment below
	// protects.
	markRequestCh := cfg.markRequestChan(start)
	markEchoCh := cfg.markEchoChan(start)

	// nil tracker when the consumer asked for no marks, which is what keeps a
	// call with neither option byte-identical to today: no mark is written,
	// and an inbound mark frame is ignored without even a log line.
	var marks *markTracker
	if markRequestCh != nil || markEchoCh != nil {
		marks = newMarkTracker(markEchoCh)
	}
	// Before conn.CloseNow's defer runs, so a bound firing during teardown
	// cannot deliver on a channel the consumer may already be reusing — while
	// pumpCarrierToBridge, which outlives this until the conn closes, is still
	// free to call marks.echo and be ignored.
	defer marks.stop()

	sink := newCarrierMediaSink(conn, start.StreamSID, cfg.carrierAudioChan(start), marks)
	bridge := realtime.NewBridge(client, sink)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	backendDone := make(chan error, 1)
	go func() { backendDone <- bridge.Run(runCtx) }()

	// Drain transcripts FIRST, ahead of the server-event drain, so a full
	// channel can never wedge the read loop. The channel closes when Run
	// returns, which ends this goroutine.
	//
	// The order is load-bearing, and the axis is what each source does when its
	// buffer fills — not how fast it fills. Both destinations are resolved by
	// consumer code on this goroutine (WithTranscriptChanFor and
	// WithServerEventChanFor exist to let a consumer route per call) and a go
	// statement evaluates its arguments before it spawns, so whichever is
	// resolved first delays the drain spawned after it by however long that
	// consumer takes. Bridge.publish PARKS Run's read loop once its 16-slot
	// buffer fills, and that loop is also what drives the MediaSink: a
	// transcript drain that starts late costs the caller AUDIO, for the rest of
	// the call. Bridge.publishEvent drops instead of parking, so an events drain
	// that starts late costs EVENTS and nothing else. Events fill their buffer
	// far faster — one audio delta per 20 ms frame, so roughly 320 ms — but
	// losing events is the cheaper outcome, and it is the one this order picks
	// to risk. Measured, with a serverEventChanFor that sleeps 3 s: resolved
	// first, no media frame reaches the carrier for 3 s; resolved second, audio
	// flows immediately and events drop instead.
	go deliver(bridge.Transcripts(), cfg.transcriptChan(start), "transcript")

	go deliver(bridge.Events(), cfg.serverEventChan(start), "server event")

	carrierDone := make(chan error, 1)
	go func() { carrierDone <- pumpCarrierToBridge(ctx, conn, bridge, marks) }()

	// Resolved once, before the loop, mirroring how transcriptChan/
	// serverEventChan are resolved once via cfg.transcriptChan(start) /
	// cfg.serverEventChan(start) above rather than per-iteration. nil (no
	// option, or the resolver returned nil) means the select case below
	// simply never fires — a nil channel blocks forever in a select, which is
	// exactly "never blocks waiting, survives never being written to".
	//
	// It resolves BEFORE the idle guard is armed, and that order is
	// load-bearing for the same reason the drain order above is: this line
	// runs CONSUMER code on this goroutine, and a consumer resolver is free to
	// be slow (the transcript/server-event resolvers are measured at 3 s in
	// the comment above). Armed first, the guard would spend its whole first
	// window inside the resolver and fire the instant the loop is entered — on
	// a backend that had been streaming the entire time. Measured, with a
	// 500 ms idle timeout and a resolver sleeping 1.2 s: 6 of 10 calls ended
	// with "idle timeout" against a continuously active backend, the coin flip
	// being whether the select picked the stale timer or the pending
	// bridge.Activity() signal that would have reset it.
	clientEventCh := cfg.clientEventChan(start)
	// Resolved here, beside clientEventCh and before the idle guard is armed,
	// for the reason stated immediately above: this runs consumer code on
	// this goroutine, and an armed guard would spend its first window inside
	// the resolver.
	voiceUpdateCh := cfg.voiceUpdateChan(start)

	idle := newIdleGuard(cfg.idleTimeout)
	defer idle.stop()

	// One goroutine, one select: bridge.Activity() resets the guard without any
	// Reset call crossing a goroutine boundary, which is what keeps this
	// race-free under -race. Only backendDone, carrierDone, the guard firing,
	// or a consumer send failure — client event, voice update or mark alike —
	// ends the loop; an activity signal, a forwarded client event, a forwarded
	// voice update or a written mark just re-arms/loops again.
	for {
		select {
		case err := <-backendDone:
			log.Printf("twilio: realtime: backend ended the call: %v", err)
			return err
		case err := <-carrierDone:
			return err
		case <-idle.fired():
			return fmt.Errorf("twilio: realtime: idle timeout: no backend activity for %s", cfg.idleTimeout)
		case <-bridge.Activity():
			idle.reset()
		case ev, ok := <-clientEventCh:
			var err error
			clientEventCh, err = handleClientEvent(ctx, client, clientEventCh, ev, ok)
			if err != nil {
				return err
			}
		case voice, ok := <-voiceUpdateCh:
			var err error
			voiceUpdateCh, err = handleVoiceUpdate(ctx, client, voiceUpdateCh, voice, ok)
			if err != nil {
				return err
			}
		case name, ok := <-markRequestCh:
			var err error
			markRequestCh, err = handleMarkRequest(ctx, sink, markRequestCh, name, ok)
			if err != nil {
				return err
			}
		}
	}
}

// sendBounded writes one frame to the backend from HandleStreamRealtime's
// select loop and reports what went wrong in terms of what was being sent.
// It is the single definition of that write: handleClientEvent and
// handleVoiceUpdate both go through it, and what differs between them is one
// label.
//
// Bounded, because this runs on HandleStreamRealtime's select loop: an
// unbounded write that parks here parks every other way the call can end. See
// realtimeClientEventSendTimeout, whose bound both senders share — each is one
// small frame on the same socket, sent from the same loop, for the same
// reason.
//
// Wrapped, not returned bare: the transport's write error reads identically
// whether it came from forwarding carrier audio, a consumer event, or a voice
// update, so an unwrapped return would leave the caller unable to tell them
// apart from the error alone. The idle timeout names itself on the way out for
// the same reason. This transport's writer poisons itself after one write
// error, so a failure here is never transient — hence logged, and fatal to the
// call, rather than swallowed.
//
// what names the sender in both the log line and the wrapped error, so those
// strings stay one definition rather than two that can drift apart.
func sendBounded(ctx context.Context, client *realtime.Client, ev json.RawMessage, what string) error {
	sendCtx, cancelSend := context.WithTimeout(ctx, realtimeClientEventSendTimeout)
	defer cancelSend()
	if err := client.Send(sendCtx, ev); err != nil {
		log.Printf("twilio: realtime: %s send failed: %v", what, err)
		return fmt.Errorf("twilio: realtime: %s send: %w", what, err)
	}
	return nil
}

// handleClientEvent processes one receive from the client-event select case,
// returning the channel HandleStreamRealtime's loop should keep selecting on
// next (nil once the consumer has closed it) and any error that should end
// the call. Extracted from the select loop itself, mirroring deliver's role
// for the transcript/server-event drains above — a named unit for one
// select case's branching, not the whole loop's.
//
// Deliberately does NOT call idle.reset(): a consumer event is not backend
// activity, and a chatty consumer against a silent backend must not mask
// that silence. That includes a refused session.update: it is still a
// consumer event, not backend activity.
//
// A session.update on this channel is refused rather than forwarded: it
// would permanently rewrite the session config the handshake established,
// which belongs to the options a consumer sets through, not to whatever it
// puts on this channel mid-call. The refusal is an equality test on the
// decoded "type" field, not a prefix or substring match on the raw bytes —
// a type that merely mentions "session.update" in its payload, or that
// starts with "session." without being equal to it, still forwards
// byte-for-byte. When the type cannot be read at all — malformed JSON, or
// valid JSON whose "type" is absent or not a string — the event is forwarded
// unchanged; a parse failure never refuses and never ends the call. "Absent"
// here means absent to encoding/json, which matches field names
// case-insensitively: a lone "Type" or "TYPE" is read as the type and
// refused, though the wire format is case-sensitive.
func handleClientEvent(ctx context.Context, client *realtime.Client, ch <-chan json.RawMessage, ev json.RawMessage, ok bool) (<-chan json.RawMessage, error) {
	if !ok {
		// The consumer closed its channel: return nil so the caller's select
		// case never fires again, rather than busy-looping on a closed
		// channel that is always ready to receive its zero value.
		return nil, nil
	}

	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(ev, &probe) == nil && probe.Type == realtime.EventSessionUpdate {
		log.Printf("twilio: realtime: client-event channel: refusing session.update; set session config through options")
		return ch, nil
	}

	if err := sendBounded(ctx, client, ev, "client event"); err != nil {
		return ch, err
	}
	return ch, nil
}

// handleVoiceUpdate processes one receive from the voice-update select case,
// returning the channel HandleStreamRealtime's loop should keep selecting on
// next (nil once the consumer has closed it) and any error that should end
// the call. Mirrors handleClientEvent, which is the shape it is deliberately
// modelled on — same extraction, same bound, same failure path.
//
// The engine marshals the frame; the consumer supplies a voice ID and nothing
// else. realtime.BuildVoiceUpdate emits the minimal shape rather than the
// dial handshake's, and that distinction is load-bearing rather than tidy:
// the handshake's audio channels carry a non-omitempty format, and a backend
// that merges exactly the fields an update sent would read an empty one as an
// instruction to clear the negotiated G.711 mu-law format on a live call.
//
// Deliberately does NOT call idle.reset(), for the reason handleClientEvent
// gives: a voice update is a consumer event, not backend activity, and a
// chatty consumer against a silent backend must not mask that silence.
//
// An empty voice is dropped rather than sent. Empty is what WithVoice means
// by "no voice" (the field is omitempty at the handshake), so forwarding one
// would ask the backend to unset a field rather than to change it, and this
// package has no way to say what unsetting a voice mid-call means.
func handleVoiceUpdate(ctx context.Context, client *realtime.Client, ch <-chan string, voice string, ok bool) (<-chan string, error) {
	if !ok {
		// The consumer closed its channel: return nil so the caller's select
		// case never fires again, rather than busy-looping on a closed
		// channel that is always ready to receive its zero value.
		return nil, nil
	}
	if voice == "" {
		return ch, nil
	}

	ev, err := realtime.BuildVoiceUpdate(voice)
	if err != nil {
		log.Printf("twilio: realtime: build voice update: %v", err)
		return ch, fmt.Errorf("twilio: realtime: build voice update: %w", err)
	}

	if err := sendBounded(ctx, client, ev, "voice update"); err != nil {
		return ch, err
	}
	return ch, nil
}

// idleGuard is the idle timeout as a single object, so HandleStreamRealtime's
// loop reads as six things that can end or extend a call rather than as timer
// bookkeeping. A zero duration produces a guard that never fires: fired()
// returns a nil channel, which blocks forever in a select, so the loop reduces
// to the original two-case shape — unbounded, exactly today's behavior.
//
// Not safe for concurrent use, and deliberately so: every method is called from
// the one goroutine that owns the select above.
type idleGuard struct {
	timer *time.Timer
	after time.Duration
}

func newIdleGuard(after time.Duration) *idleGuard {
	if after <= 0 {
		return &idleGuard{}
	}
	return &idleGuard{timer: time.NewTimer(after), after: after}
}

// fired is the channel the guard signals on, or nil when it is disarmed.
func (g *idleGuard) fired() <-chan time.Time {
	if g.timer == nil {
		return nil
	}
	return g.timer.C
}

// reset restarts the guard. Stop-then-drain before Reset is the required idiom:
// a timer that already fired has a value parked in its channel, and resetting
// without draining it would fire again immediately on stale time.
func (g *idleGuard) reset() {
	if g.timer == nil {
		return
	}
	if !g.timer.Stop() {
		select {
		case <-g.timer.C:
		default:
		}
	}
	g.timer.Reset(g.after)
}

func (g *idleGuard) stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
}

// sendOrDrop attempts a non-blocking send of v to out, and on a full channel
// increments *dropped and logs at LogDrop's bounded rate. One definition for
// the drop-and-log idiom deliver's stream drain and carrierMediaSink's inline
// delivery both need: the log line and the counting policy were identical
// between the two hand-copies this replaces, so a fix to either now lands
// once (CLAUDE.md #4/#5 — dedupe, one definition per value).
//
// The what parameter names the payload in the drop log ("transcript", "server
// event", "carrier audio").
func sendOrDrop[T any](out chan<- T, v T, dropped *int, what string) {
	select {
	case out <- v:
	default:
		// The consumer is behind. Drop, and say so: a silently lossy seam is
		// worse than a lossy one, because the consumer cannot tell an absent
		// value from one that never happened. Rate-bounded for the reason
		// realtime.LogDrop documents — every drop is counted and every line
		// carries the running total, so nothing is silent in aggregate.
		*dropped++
		if realtime.LogDrop(*dropped) {
			log.Printf("twilio: realtime: %s dropped, consumer is behind (%d dropped on this call)", what, *dropped)
		}
	}
}

// deliver drains src, forwarding each value to out if a consumer asked for
// one. It never blocks and it never runs consumer code, which is the whole
// design. One function serves both consumer-facing channels on this path —
// transcripts and server events — because the discipline is the same one, and
// two copies of it would be two places for a fix to have to land.
//
// src is fed from Bridge.Run's read loop, which also drives audio to the
// carrier. The two sources differ in what they do when nobody reads: publish
// parks for transcripts, publishEvent drops for events. Either way this drain
// must never add a second place to block, hence the non-blocking send and the
// drop.
//
// The goroutine running this blocks only on src, which Run closes, and on a
// select that always has a default. It therefore exits on every call ending, no
// matter what the consumer does or fails to do.
//
// With no consumer this is a bare drain: byte for byte the behaviour of a call
// supplying no channel option at all.
func deliver[T any](src <-chan T, out chan<- T, what string) {
	if out == nil {
		for range src {
		}
		return
	}

	var dropped int
	for v := range src {
		sendOrDrop(out, v, &dropped, what)
	}
	// out is deliberately NOT closed: the engine did not create it, the consumer
	// may reuse it across calls, and closing a channel you do not own hands its
	// receiver a zero value it cannot distinguish from a real one.
}

// pumpCarrierToBridge reads carrier frames and forwards media to the backend.
// It forwards Frame.EncodedPayload — the base64 exactly as it arrived — never
// re-encoding Frame.Payload, which would spend a decode and an encode per 20 ms
// frame reproducing bytes the carrier already sent.
func pumpCarrierToBridge(ctx context.Context, conn *websocket.Conn, bridge *realtime.Bridge, marks *markTracker) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			// The carrier went away: a hangup, not a fault.
			return nil
		}

		f, err := DecodeFrame(raw)
		if err != nil {
			log.Printf("twilio: realtime: %v", err)
			continue
		}

		switch f.Event {
		case EventMedia:
			if err := bridge.Forward(ctx, f.EncodedPayload); err != nil {
				log.Printf("twilio: realtime: forward to backend: %v", err)
				return err
			}
		case EventStop:
			return nil
		case EventMark:
			// The carrier has finished playing everything written ahead of
			// this mark. A nil tracker — the consumer asked for no marks — is
			// today's behavior: dropped, silently.
			marks.echo(f.MarkName)
		default:
			// clear/connected carry no meaning on this path: the backend owns
			// playback and barge-in, so there is no local clear handling for
			// them to drive. Marks are the exception, handled above, because a
			// consumer can have a reason to care about the carrier's playout
			// position even when it does not own playback (AATK-105).
		}
	}
}

// carrierMediaSink is the carrier-facing side of the bridge: it writes the
// backend's audio and barge-in signals to the Twilio Media Streams WebSocket.
// Audio arrives base64 and is placed on the wire unchanged.
type carrierMediaSink struct {
	conn      wsWriter
	streamSID string

	// carrierAudioCh is the consumer's observation channel for what this sink
	// sends to the carrier, resolved once at construction time by
	// HandleStreamRealtime (see the comment at the NewBridge call site) and
	// nil when no consumer asked for one. Instrumenting Media/Clear directly,
	// rather than deriving records from bridge.Events(), is deliberate: that
	// event stream drops once its 16-slot buffer fills while Bridge.Run calls
	// this sink unconditionally, so an Events()-sourced observer would
	// silently under-report what actually shipped to the carrier.
	carrierAudioCh chan<- CarrierAudio

	// carrierAudioDropped counts records dropped because carrierAudioCh was
	// full. No lock: Bridge.Run calls Media and Clear from a single read
	// loop, never concurrently — the same reasoning Bridge records for its
	// own eventsDropped. Mark does NOT deliver a carrier-audio record, so the
	// second writer this sink acquired in AATK-105 does not reach this field.
	carrierAudioDropped int

	// writeSem serialises every write to the carrier connection through one
	// chokepoint. It exists because this sink has TWO writers: Bridge.Run's
	// read loop calls Media and Clear, while HandleStreamRealtime's select
	// loop calls Mark. Ordering is what a mark is for, so the two cannot
	// simply race — a mark that overtook a media frame would report a playout
	// position that has not happened.
	//
	// A one-slot channel rather than a sync.Mutex, and THAT part is
	// load-bearing for the reason realtime.Client.writeSem states in full:
	// acquisition has to honour ctx, because the two writers do not carry the
	// same kind of deadline. Media and Clear are written on the call's own
	// context, which has none; Mark is written under a bounded one, from the
	// select loop that observes every way the call can end. sync.Mutex.Lock
	// cannot be cancelled, so a plain mutex would put an unbounded wait in
	// front of a bounded write and void its deadline — wedging that loop.
	writeSem chan struct{}

	// playout is how much written audio has not been heard yet, which is what
	// a mark's echo bound is derived from. Guarded by writeSem: every method
	// on it runs inside the slot, beside the write it accounts for, so the
	// figure a mark's bound is computed from is the queue that mark actually
	// sits behind.
	playout playoutClock

	// marks tracks the marks written and not yet echoed, nil when the consumer
	// asked for none — see markTracker, whose methods all tolerate nil.
	marks *markTracker
}

// newCarrierMediaSink builds the sink with its write slot open. A constructor
// rather than a struct literal because writeSem must exist: a nil channel
// blocks forever in a send, so a sink assembled without it would hang on its
// first write rather than fail visibly.
func newCarrierMediaSink(conn wsWriter, streamSID string, carrierAudioCh chan<- CarrierAudio, marks *markTracker) *carrierMediaSink {
	return &carrierMediaSink{
		conn:           conn,
		streamSID:      streamSID,
		carrierAudioCh: carrierAudioCh,
		marks:          marks,
		writeSem:       make(chan struct{}, 1),
	}
}

var _ realtime.MediaSink = (*carrierMediaSink)(nil)

// write is the single chokepoint every carrier write funnels through, and
// afterWrite is whatever bookkeeping belongs to that write and must not be
// separable from it — the playout clock, and a mark's bound.
//
// Inside the slot rather than after it, which is the point of taking a
// closure at all: the playout figure a mark's bound is derived from is the
// queue as of that mark's own write, so a media frame slipping in between the
// write and the accounting would change the answer. Holding the slot across
// both is what makes them one step.
//
// The wait for the slot is taken under ctx, so a caller's deadline covers the
// whole write — the queueing behind the other writer as well as the write
// itself. See writeSem for why that is not optional.
func (s *carrierMediaSink) write(ctx context.Context, msg []byte, afterWrite func()) error {
	select {
	case s.writeSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("twilio: realtime: awaiting carrier write slot: %w", ctx.Err())
	}
	defer func() { <-s.writeSem }()

	if err := s.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return err
	}
	if afterWrite != nil {
		afterWrite()
	}
	return nil
}

func (s *carrierMediaSink) Media(ctx context.Context, payload string) error {
	msg, err := EncodeMediaB64(s.streamSID, payload)
	if err != nil {
		return err
	}
	if err := s.write(ctx, msg, func() {
		s.playout.fed(muLawBytesInB64(payload), time.Now())
	}); err != nil {
		return err
	}
	s.deliverCarrierAudio(CarrierAudio{Payload: payload})
	return nil
}

func (s *carrierMediaSink) Clear(ctx context.Context) error {
	msg, err := EncodeClear(s.streamSID)
	if err != nil {
		return err
	}
	if err := s.write(ctx, msg, func() {
		// Barge-in: the carrier discards what it has buffered, so every
		// quantity derived from the playout clock must stop describing audio
		// nobody will hear. A mark written after this is owed the grace and
		// nothing more, instead of waiting out an abandoned reply.
		s.playout.flush(time.Now())
	}); err != nil {
		return err
	}
	s.deliverCarrierAudio(CarrierAudio{Clear: true})
	return nil
}

// Mark writes a Twilio mark named name to the carrier, after every media frame
// written before it, and arms the bound for its echo.
//
// Called only from HandleStreamRealtime's select loop, via handleMarkRequest,
// which is where the reason the write belongs to the sink is stated. Nothing
// is delivered to the carrier-audio channel: a mark is not audio, and
// CarrierAudio's two record kinds are what shipped as sound.
func (s *carrierMediaSink) Mark(ctx context.Context, name string) error {
	msg, err := EncodeMark(s.streamSID, name)
	if err != nil {
		return err
	}
	return s.write(ctx, msg, func() {
		s.marks.arm(name, s.markEchoBound(time.Now()))
	})
}

// markEchoBound is how long the carrier is given to echo a mark written now:
// the playout still queued ahead of it, plus telephony.MarkEchoGraceMS.
//
// Derived, not a constant, for the reason telephony.MarkEchoTimeout is derived
// from its clip — a mark written behind two seconds of audio cannot be judged
// late until that audio has had two seconds to play, or a carrier honoring the
// protocol perfectly is reported as having timed out simply for still playing.
// The grace is the classic path's own, reused rather than restated: same
// protocol, same peer, same purpose (CLAUDE.md #5).
//
// The playout term comes from telephony.MuLawDuration, the module's one
// definition of the μ-law byte-to-duration conversion. Its result is exact,
// keeping the sub-millisecond remainder; the truncating whole-millisecond
// spelling is a different function and must not be substituted.
//
// Called under writeSem, beside the write whose queue it describes.
func (s *carrierMediaSink) markEchoBound(now time.Time) time.Duration {
	return s.playout.outstanding(now) + telephony.MarkEchoGraceMS*time.Millisecond
}

// deliverCarrierAudio hands rec to carrierAudioCh without ever blocking, via
// the same sendOrDrop idiom deliver's stream drain uses. deliver drains a
// source channel from a goroutine it spawns; there is no source channel here,
// because rec originates inline inside Media/Clear on Bridge.Run's own
// read-loop goroutine. Inventing a source channel to route through deliver
// would reintroduce a per-call goroutine to drain it — the leak this ticket's
// non-blocking, inline design exists to avoid.
func (s *carrierMediaSink) deliverCarrierAudio(rec CarrierAudio) {
	if s.carrierAudioCh == nil {
		return
	}
	sendOrDrop(s.carrierAudioCh, rec, &s.carrierAudioDropped, "carrier audio")
}
