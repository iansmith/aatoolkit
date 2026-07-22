package main

import (
	"context"
	"errors"
	"io"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// sampleRateHz is Twilio's μ-law rate: 8000 samples/sec, 1 byte/sample.
const sampleRateHz = 8000

// muLawFrame20ms is 8000 Hz × 0.020 s × 1 byte/sample.
const muLawFrame20ms = sampleRateHz * 20 / 1000

// mediaFrameEncoder tracks a monotonic chunk counter for one call's outgoing
// media frames, starting at 1. seqNum points at the call's shared per-message
// sequenceNumber counter (owned by dial): each media frame advances it, so the
// wire carries a single unified sequence across start, media, and stop.
type mediaFrameEncoder struct {
	streamSID string
	chunk     int
	seqNum    *int
}

func newMediaFrameEncoder(streamSID string, seqNum *int) *mediaFrameEncoder {
	return &mediaFrameEncoder{streamSID: streamSID, seqNum: seqNum}
}

func (e *mediaFrameEncoder) encode(payload []byte) ([]byte, error) {
	e.chunk++
	// AATK-16 RED: media still emits the placeholder 0; the shared seqNum is not
	// yet advanced or wired into the frame.
	return twilio.EncodeMediaWithMetadata(e.streamSID, payload, e.chunk, 0)
}

// drainFrames reads fixed-size frames from r and calls send for each complete
// frame. A partial trailing frame is dropped. Stops on EOF, send error, or
// context cancellation. frameSize must be positive.
func drainFrames(ctx context.Context, r io.Reader, frameSize int, send func([]byte) error) error {
	if frameSize <= 0 {
		return errors.New("drainFrames: frameSize must be positive")
	}
	buf := make([]byte, frameSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := io.ReadFull(r, buf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := send(buf); err != nil {
			return err
		}
	}
}
