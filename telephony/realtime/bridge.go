package realtime

import (
	"context"
	"fmt"
	"log"
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
	// eventsDropped counts events dropped because Events() was not being
	// drained. Only Run touches it, on one goroutine, so it needs no lock.
	eventsDropped int
	running       atomic.Bool
	activity      chan struct{}
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
// also offered to Events, whose delivery never blocks this loop: a caller that
// stops draining loses events rather than stalling the call; see Events. It
// always returns a non-nil error describing why it stopped — a backend that
// goes away is a fact the caller must see, never a silent return.
//
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
		b.publishEvent(ev)

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
// modelled event that is not dropped reaches both Events() and its existing
// destination. Dropping is one-sided: it costs the event its place here and
// never its place in the switch, so a dropped transcript event still reaches
// Transcripts and a dropped audio delta still reaches the MediaSink.
//
// Delivery never blocks Run. A caller that stops draining loses events rather
// than stalling the call: once the 16-event buffer is full the publish drops,
// and logs each drop. That is deliberate — every event is published, including
// one audio delta per 20 ms frame, and Run's read loop is also what drives the
// MediaSink, so a blocking publish would silence audio within ~320 ms of a
// consumer looking away. The channel is closed when Run returns.
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

// publishEvent hands ev to Events() without ever blocking Run's read loop.
//
// Non-blocking-with-drop, matching signalActivity rather than publish, and the
// difference between the two is the point. Events carries EVERY event Run
// reads, including one audio delta per 20 ms frame, so a consumer that looks
// away fills the 16-slot buffer in roughly 320 ms. Blocking there parks the
// loop that also drives the MediaSink, and audio to the caller stops silently
// with Run neither returning nor erroring. Measured before this was changed:
// 40 audio deltas with Events() undrained delivered 16 of 40 media frames.
//
// publish keeps the blocking discipline for Transcripts, and correctly so:
// transcripts arrive at conversation rate and each one carries meaning that is
// lost if dropped. An event nobody is reading loses nothing that was being
// read, so the trade runs the other way here.
//
// Only Run calls this, on one goroutine.
func (b *Bridge) publishEvent(ev ServerEvent) {
	select {
	case b.events <- ev:
	default:
		b.eventsDropped++
		log.Printf("realtime: server event dropped, consumer is behind (%d dropped on this call)",
			b.eventsDropped)
	}
}
