package realtime

import (
	"context"
	"fmt"
	"sync/atomic"
)

// MediaSink is the carrier-facing side of the bridge: where synthesized audio
// and barge-in signals go. Payloads are base64 G.711, forwarded unchanged.
type MediaSink interface {
	// Media delivers one chunk of outbound audio, base64-encoded.
	Media(ctx context.Context, payload string) error
	// Clear discards audio already buffered toward the caller. It is the
	// barge-in signal.
	Clear(ctx context.Context) error
}

// Transcript is one transcription result for caller speech. Final marks the
// completed transcript for an utterance; partials arrive with Final false.
type Transcript struct {
	Text  string
	Final bool
}

// Bridge pumps between a carrier and a Client. It performs no audio
// conversion in either direction — see the package comment.
//
// One goroutine may call Forward and another may call Run concurrently; that
// is the intended shape, and it is safe because the underlying connection
// permits one concurrent reader and one concurrent writer. Concurrent calls to
// Forward are NOT safe — serialise them in the caller, which owns the carrier's
// frame ordering anyway.
type Bridge struct {
	client      *Client
	sink        MediaSink
	transcripts chan Transcript
	events      chan ServerEvent
	running     atomic.Bool
	activity    chan struct{}
}

// NewBridge wires a client to a carrier media sink.
func NewBridge(c *Client, sink MediaSink) *Bridge {
	return &Bridge{
		client:      c,
		sink:        sink,
		transcripts: make(chan Transcript, 16),
		events:      make(chan ServerEvent, 16),
		activity:    make(chan struct{}, 1),
	}
}

// Forward sends one inbound carrier frame's base64 payload to the backend.
// One frame in, one append out: no batching, no reordering, no rebuffering.
func (b *Bridge) Forward(ctx context.Context, payload string) error {
	return b.client.AppendAudio(ctx, payload)
}

// Run reads server events until the connection ends or ctx is cancelled,
// driving the sink: audio deltas become Media, speech starts become Clear,
// and transcripts are published on Transcripts. Every event, of every type, is
// also published on Events — which the caller must drain, or this loop parks
// and the sink goes quiet; see Events. It always returns a non-nil error
// describing why it stopped — a backend that goes away is a fact the caller
// must see, never a silent return.
// A second concurrent Run is refused rather than allowed to proceed: two
// readers on one connection is undefined, and the loser would panic closing an
// already-closed channel on the way out — both Transcripts and Events are
// closed here, so there are now two ways for that to go wrong and one guard
// covering both.
func (b *Bridge) Run(ctx context.Context) error {
	if !b.running.CompareAndSwap(false, true) {
		return fmt.Errorf("realtime: Run already in progress")
	}
	defer close(b.transcripts)
	// Guarded by the same CAS as the transcripts close above: the loser
	// returns before either defer is registered, so this can never be a
	// second close racing the first — see the doc comment above Run.
	defer close(b.events)

	for {
		ev, err := b.client.Read(ctx)
		if err != nil {
			return fmt.Errorf("realtime: read loop ended: %w", err)
		}
		b.signalActivity()
		b.publishEvent(ctx, ev)

		switch ev.Type {
		case EventAudioDelta:
			// Verbatim: the delta is already base64 G.711, which is what the
			// carrier wants. Decoding to re-encode would spend CPU per frame
			// reproducing the input.
			if err := b.sink.Media(ctx, ev.Delta); err != nil {
				return fmt.Errorf("realtime: media sink: %w", err)
			}
		case EventSpeechStarted:
			// One clear per event. Collapsing repeats would be a behavior
			// change, not an optimisation.
			if err := b.sink.Clear(ctx); err != nil {
				return fmt.Errorf("realtime: clear sink: %w", err)
			}
		case EventTranscriptDelta:
			b.publish(ctx, Transcript{Text: ev.Transcript})
		case EventTranscriptDone:
			b.publish(ctx, Transcript{Text: ev.Transcript, Final: true})
		default:
			// Unmodelled event type: ignored, and the loop continues. The
			// protocol is larger than the subset this package reads.
		}
	}
}

// Transcripts yields transcription results in arrival order. The channel is
// closed when Run returns.
func (b *Bridge) Transcripts() <-chan Transcript {
	return b.transcripts
}

// Events yields every server event Run reads, including ones its switch does
// not model (see the default: branch in Run above) — the same coverage Activity
// documents, carried as data rather than a signal. Events are published in
// arrival order, before the event is dispatched into Run's switch, so a
// modelled event reaches both Events() and its existing destination.
//
// A CALLER THAT STARTS Run MUST DRAIN THIS CHANNEL. Unlike Activity, which
// coalesces and never blocks, the publish behind Events blocks once the
// 16-event buffer is full and only abandons the event when ctx ends — see
// publishEvent. Because every event is published, including one audio delta
// per 20 ms frame, an undrained Events parks Run's read loop after roughly
// 320 ms, and that loop is also what drives the MediaSink: audio to the caller
// stops, silently, with Run neither returning nor erroring. The channel is
// closed when Run returns.
func (b *Bridge) Events() <-chan ServerEvent {
	return b.events
}

// Activity signals once per successful backend read, before that event is
// dispatched to any sink or the transcript channel — so it covers every event
// type Run observes, including ones its switch does not model and drops into
// default:. A caller wanting a liveness bound on the backend (as opposed to
// specific event types) should watch this rather than the sink or
// Transcripts, both of which only see a subset.
//
// The channel is buffered by one and every send is non-blocking: a caller
// that misses a tick sees the next one, and Run's read loop can never stall
// waiting on a consumer that is not currently receiving.
func (b *Bridge) Activity() <-chan struct{} {
	return b.activity
}

// signalActivity is the non-blocking producer side of Activity. Coalescing
// multiple unread signals into one pending signal is correct here: a consumer
// only needs to know that activity happened since it last checked, not how
// many times.
func (b *Bridge) signalActivity() {
	select {
	case b.activity <- struct{}{}:
	default:
	}
}

// publish hands a transcript to the consumer, abandoning it if the context ends
// first so a caller that stops reading cannot wedge the read loop.
func (b *Bridge) publish(ctx context.Context, tr Transcript) {
	select {
	case b.transcripts <- tr:
	case <-ctx.Done():
	}
}

// publishEvent hands ev to Events(), abandoning it only if the context ends
// first — the same blocking-with-context-abandon discipline as publish.
//
// Note what that does and does not buy. It bounds the wedge by the call's
// lifetime rather than preventing it: until ctx ends, a full b.events parks
// this loop, and this loop is the one driving the MediaSink. b.events is NOT
// engine-internal — Events() exports it, and telephony/realtime is public API
// a consumer may drive directly — so "the caller drains it" is a real
// obligation on that consumer, documented on Events. What is engine-internal
// is the twilio package's drain of it, which is where the non-blocking
// drop-and-log lives; a consumer going through telephony/twilio never sees
// this channel and inherits no obligation.
func (b *Bridge) publishEvent(ctx context.Context, ev ServerEvent) {
	select {
	case b.events <- ev:
	case <-ctx.Done():
	}
}
