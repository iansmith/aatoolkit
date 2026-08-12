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
// and transcripts are published on Transcripts. It always returns a non-nil
// error describing why it stopped — a backend that goes away is a fact the
// caller must see, never a silent return.
// A second concurrent Run is refused rather than allowed to proceed: two
// readers on one connection is undefined, and the loser would panic closing an
// already-closed transcript channel on the way out.
func (b *Bridge) Run(ctx context.Context) error {
	if !b.running.CompareAndSwap(false, true) {
		return fmt.Errorf("realtime: Run already in progress")
	}
	defer close(b.transcripts)

	for {
		ev, err := b.client.Read(ctx)
		if err != nil {
			return fmt.Errorf("realtime: read loop ended: %w", err)
		}
		b.signalActivity()

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
// not model (see the default: branch below) — the same coverage Activity
// documents, carried as data rather than a signal.
//
// Phase 0 stub for AATK-81: the channel exists but Run does not yet publish
// to it, so a consumer never receives anything. That is the sentinel this
// stub is built to fail against.
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
