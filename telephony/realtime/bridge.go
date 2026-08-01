package realtime

import (
	"context"
	"fmt"
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
type Bridge struct {
	client      *Client
	sink        MediaSink
	transcripts chan Transcript
}

// NewBridge wires a client to a carrier media sink.
func NewBridge(c *Client, sink MediaSink) *Bridge {
	return &Bridge{client: c, sink: sink, transcripts: make(chan Transcript, 16)}
}

// Forward sends one inbound carrier frame's base64 payload to the backend.
func (b *Bridge) Forward(ctx context.Context, payload string) error {
	return fmt.Errorf("realtime: Forward not implemented")
}

// Run reads server events until the connection ends or ctx is cancelled,
// driving the sink: audio deltas become Media, speech starts become Clear,
// and transcripts are published on Transcripts. It always returns a non-nil
// error describing why it stopped — a backend that goes away is a fact the
// caller must see, never a silent return.
func (b *Bridge) Run(ctx context.Context) error {
	return fmt.Errorf("realtime: Run not implemented")
}

// Transcripts yields transcription results in arrival order. The channel is
// closed when Run returns.
func (b *Bridge) Transcripts() <-chan Transcript {
	return b.transcripts
}
