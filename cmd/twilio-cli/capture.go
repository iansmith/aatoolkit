package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
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
	*e.seqNum++
	return twilio.EncodeMediaWithMetadata(e.streamSID, payload, e.chunk, *e.seqNum)
}

// connFrameWriter returns the func that puts one encoded media frame on conn.
// Every write gets a fresh short-lived context rather than the call's: a write
// must not inherit an unbounded deadline, and on teardown the call's context is
// already cancelled while the trailing frames still have to go out (the same
// reason sendStop and echoMark build their own in dial.go).
func connFrameWriter(conn *websocket.Conn) func([]byte) error {
	return func(msg []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, msg)
	}
}

// mediaFrameSender builds the send func a frame source hands to its drain loop:
// encode the raw μ-law payload as a Twilio media frame, write it, and -- only
// once the write has succeeded -- tee the payload to rec.
//
// This is the one place an outbound frame leaves the process, which is why
// -record-sent tees here rather than in either frame source. The mic and the
// -audio file both send through it, so the recording is of the bytes sent and
// cannot drift from one source's idea of them. Two details are load-bearing:
// the payload is teed pre-encode, so the file is a plain μ-law stream that
// -audio replays directly rather than the framed JSON that went on the wire;
// and the tee runs after the write, so a frame that never reached the server is
// not in the file that claims to be what the server heard.
func mediaFrameSender(enc *mediaFrameEncoder, rec *streamRecorder, write func([]byte) error) func([]byte) error {
	return func(payload []byte) error {
		msg, err := enc.encode(payload)
		if err != nil {
			return err
		}
		if err := write(msg); err != nil {
			return err
		}
		rec.write(payload, time.Now())
		return nil
	}
}

// drainFramesWithDiscard reads frames from r, discarding leading all-0xFF frames
// (μ-law silence), bounded by a cap of 75 frames (1500 ms). Once the first real
// (non-0xFF) frame is encountered or the cap is reached, onMicWarm fires exactly once
// with a bool indicating whether the cap was hit (true) or a real frame was found (false).
// frameSize must be positive. Returns a flag indicating whether the cap was hit.
func drainFramesWithDiscard(ctx context.Context, r io.Reader, frameSize int, send func([]byte) error, onMicWarm func(bool)) (bool, error) {
	if frameSize <= 0 {
		return false, errors.New("drainFramesWithDiscard: frameSize must be positive")
	}

	const discardCap = 75 // frames, 1500 ms at 20 ms/frame
	buf := make([]byte, frameSize)
	discarded := 0
	warmupFired := false
	capHit := false

	for {
		if err := ctx.Err(); err != nil {
			return capHit, err
		}
		_, err := io.ReadFull(r, buf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return capHit, nil
		}
		if err != nil {
			return capHit, err
		}

		// Check if this frame is pure silence (all 0xFF)
		isSilence := true
		for _, b := range buf {
			if b != telephony.MuLawSilence {
				isSilence = false
				break
			}
		}

		if !warmupFired && isSilence && discarded < discardCap {
			// Discard this silence frame
			discarded++
			if discarded >= discardCap {
				warmupFired = true
				capHit = true
				onMicWarm(capHit)
			}
			continue
		}

		// Either this is a real frame or we've hit the cap
		if !warmupFired {
			warmupFired = true
			onMicWarm(capHit)
		}

		// Emit the frame (real frame or post-cap silence frame)
		if err := send(buf); err != nil {
			return capHit, err
		}
	}
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
