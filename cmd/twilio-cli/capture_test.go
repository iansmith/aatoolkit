package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- edge / boundary ---

// Edge: an empty reader must produce zero sends and return nil.
func TestDrainFrames_EmptyReader_NoSends(t *testing.T) {
	var calls int
	err := drainFrames(context.Background(), bytes.NewReader(nil), muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames on empty reader: %v", err)
	}
	if calls != 0 {
		t.Errorf("got %d sends, want 0", calls)
	}
}

// Boundary: fewer bytes than one frame — the partial must be dropped, 0 sends.
func TestDrainFrames_PartialOnlyInput_NoSends(t *testing.T) {
	data := bytes.Repeat([]byte{0x7f}, muLawFrame20ms-1)
	var calls int
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames partial-only: %v", err)
	}
	if calls != 0 {
		t.Errorf("got %d sends, want 0 (partial-only dropped)", calls)
	}
}

// Boundary: exactly one frame worth of bytes → exactly one send with that content.
func TestDrainFrames_ExactlyOneFrame_OneSend(t *testing.T) {
	frame := make([]byte, muLawFrame20ms)
	for i := range frame {
		frame[i] = byte(i)
	}
	var got []byte
	err := drainFrames(context.Background(), bytes.NewReader(frame), muLawFrame20ms, func(f []byte) error {
		got = append([]byte(nil), f...)
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames one frame: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Errorf("frame content mismatch: got %d bytes, want %d", len(got), len(frame))
	}
}

// Boundary: three complete frames, no partial tail — exactly three sends.
func TestDrainFrames_ThreeCompleteFrames_ThreeSends(t *testing.T) {
	data := bytes.Repeat([]byte{0x80}, 3*muLawFrame20ms)
	var calls int
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames three frames: %v", err)
	}
	if calls != 3 {
		t.Errorf("got %d sends, want 3", calls)
	}
}

// Boundary: data is one frame + partial tail — only the complete frame is sent.
func TestDrainFrames_PartialTrailingFrame_Dropped(t *testing.T) {
	data := bytes.Repeat([]byte{0x7f}, muLawFrame20ms+90) // 250 bytes → 1 full + 90 partial
	var calls int
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames trailing partial: %v", err)
	}
	if calls != 1 {
		t.Errorf("got %d sends, want 1 (partial tail dropped)", calls)
	}
}

// Boundary: frameSize=1 (minimum) — each byte is its own frame.
func TestDrainFrames_FrameSizeOne_EachByteIsSent(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	var frames [][]byte
	err := drainFrames(context.Background(), bytes.NewReader(data), 1, func(f []byte) error {
		frames = append(frames, append([]byte(nil), f...))
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames frameSize=1: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		if f[0] != data[i] {
			t.Errorf("frame[%d] = %#x, want %#x", i, f[0], data[i])
		}
	}
}

// Boundary: verify sends receive exactly frameSize bytes, not a shared buffer slice.
func TestDrainFrames_EachSendReceivesFrameSizeBytes(t *testing.T) {
	data := bytes.Repeat([]byte{0xff}, 2*muLawFrame20ms)
	var sizes []int
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func(f []byte) error {
		sizes = append(sizes, len(f))
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames frame sizes: %v", err)
	}
	for i, s := range sizes {
		if s != muLawFrame20ms {
			t.Errorf("send[%d]: got %d bytes, want %d", i, s, muLawFrame20ms)
		}
	}
}

// --- error / rejection ---

// Error: send returns an error → drainFrames propagates it and stops after the first failure.
func TestDrainFrames_SendError_PropagatedAndLoopAborted(t *testing.T) {
	data := bytes.Repeat([]byte{0x7f}, 3*muLawFrame20ms)
	wantErr := errors.New("send failed")
	var calls int
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func([]byte) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("got %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("send called %d times after error, want exactly 1", calls)
	}
}

// Error: a pre-cancelled context causes drainFrames to return context.Canceled immediately.
func TestDrainFrames_PreCancelledContext_ReturnsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	err := drainFrames(ctx, &infiniteReader{}, muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	// May be 0 sends (checked ctx before read) or a small number if ctx checked after; either is fine.
	// What must NOT happen is looping indefinitely.
	_ = calls
}

// infiniteReader returns 0x7f bytes indefinitely without blocking.
// Used to verify that drainFrames can be interrupted by context cancellation
// even when the reader always has data ready.
type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x7f
	}
	return len(p), nil
}

// --- cross-feature interaction ---

// Cross-feature: frames must arrive in order (the i-th send gets the i-th frame bytes).
func TestDrainFrames_FramesArrivedInOrder(t *testing.T) {
	const n = 4
	data := make([]byte, n*muLawFrame20ms)
	for i := range data {
		data[i] = byte(i / muLawFrame20ms) // frame 0 = 0x00, frame 1 = 0x01, etc.
	}
	var received []byte
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func(f []byte) error {
		received = append(received, f[0]) // first byte identifies which frame
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames ordered: %v", err)
	}
	for i, b := range received {
		if b != byte(i) {
			t.Errorf("frame[%d] first byte = %d, want %d (frames out of order)", i, b, i)
		}
	}
}

// Cross-feature: io.Reader that delivers data in small chunks (not frame-aligned reads)
// must still produce complete frames — drainFrames must buffer across short reads.
func TestDrainFrames_SmallChunkReader_AssemblesCompleteFrames(t *testing.T) {
	// slowReader delivers data 10 bytes at a time; drainFrames must gather 160 bytes/frame.
	data := bytes.Repeat([]byte{0xAB}, 2*muLawFrame20ms)
	var calls int
	err := drainFrames(context.Background(), &slowReader{data: data, chunkSize: 10}, muLawFrame20ms, func(f []byte) error {
		calls++
		if len(f) != muLawFrame20ms {
			return errors.New("incomplete frame delivered")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames slow reader: %v", err)
	}
	if calls != 2 {
		t.Errorf("got %d frames, want 2", calls)
	}
}

// slowReader delivers at most chunkSize bytes per Read call.
type slowReader struct {
	data      []byte
	chunkSize int
	pos       int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	end := r.pos + r.chunkSize
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}

// --- happy path ---

// Happy: verify the round-trip from drainFrames through twilio.EncodeMedia —
// the send callback encodes each frame as a Twilio media event and decodes it
// back; the recovered payload must match the original μ-law bytes.
func TestDrainFrames_PayloadSurvivesTwilioEncodingRoundTrip(t *testing.T) {
	frame := make([]byte, muLawFrame20ms)
	for i := range frame {
		frame[i] = byte(i % 256)
	}
	const streamSID = "MZ_test_123"

	err := drainFrames(context.Background(), bytes.NewReader(frame), muLawFrame20ms, func(f []byte) error {
		msg, encErr := twilio.EncodeMedia(streamSID, f)
		if encErr != nil {
			return fmt.Errorf("EncodeMedia: %w", encErr)
		}
		dec, decErr := twilio.DecodeFrame(msg)
		if decErr != nil {
			return fmt.Errorf("DecodeFrame: %w", decErr)
		}
		if dec.Event != twilio.EventMedia {
			return fmt.Errorf("event: got %q, want %q", dec.Event, twilio.EventMedia)
		}
		if !bytes.Equal(dec.Payload, frame) {
			return fmt.Errorf("payload mismatch: %d bytes recovered, want %d", len(dec.Payload), len(frame))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Twilio encoding round-trip: %v", err)
	}
}

// --- SOP-126: chunk/timestamp on outgoing media frames ---

// TestCLI_ChunkTimestamp captures 3 outgoing frames through drainFrames +
// mediaFrameEncoder and asserts the OUTGOING wire JSON carries monotonic
// chunk (1, 2, 3) and timestamp ("0", "20", "40") fields — the ticket's
// worked example: chunk 1 starts playing at t=0ms, chunk 2 at t=20ms, etc.
func TestCLI_ChunkTimestamp(t *testing.T) {
	data := bytes.Repeat([]byte{0x7f}, 3*muLawFrame20ms)
	seqNum := 1
	enc := newMediaFrameEncoder("MZ_chunktest", &seqNum)

	var frames [][]byte
	err := drainFrames(context.Background(), bytes.NewReader(data), muLawFrame20ms, func(f []byte) error {
		msg, encErr := enc.encode(f)
		if encErr != nil {
			return encErr
		}
		frames = append(frames, msg)
		return nil
	})
	if err != nil {
		t.Fatalf("drainFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}

	wantChunk := []string{"1", "2", "3"}
	wantTimestamp := []string{"0", "20", "40"}
	for i, raw := range frames {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("frame[%d]: not JSON: %v", i, err)
		}
		media, ok := m["media"].(map[string]any)
		if !ok {
			t.Fatalf("frame[%d]: media field missing or wrong type", i)
		}
		gotChunk, ok := media["chunk"].(string)
		if !ok || gotChunk != wantChunk[i] {
			t.Errorf("frame[%d]: chunk = %v, want %q", i, media["chunk"], wantChunk[i])
		}
		gotTimestamp, ok := media["timestamp"].(string)
		if !ok || gotTimestamp != wantTimestamp[i] {
			t.Errorf("frame[%d]: timestamp = %v, want %q", i, media["timestamp"], wantTimestamp[i])
		}
	}
}

// TestMediaFrameEncoder_SequenceNumberIncrements pins AATK-16 observable
// behavior 4: the shared per-call sequenceNumber counter advances by one for
// every media frame. Seeded at 1 (the start frame's number, as dial does), the
// first media frame carries 2, then 3, 4, ... and the shared counter reflects
// the last value — proving the media path no longer emits the placeholder 0.
func TestMediaFrameEncoder_SequenceNumberIncrements(t *testing.T) {
	seqNum := 1
	enc := newMediaFrameEncoder("MZ_seq", &seqNum)

	var got []string
	for i := 0; i < 3; i++ {
		raw, err := enc.encode([]byte{0x7f})
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("frame[%d]: not JSON: %v", i, err)
		}
		seq, ok := m["sequenceNumber"].(string)
		if !ok {
			t.Fatalf("frame[%d]: sequenceNumber missing or not a string: %v", i, m["sequenceNumber"])
		}
		got = append(got, seq)
	}

	want := []string{"2", "3", "4"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("media[%d] sequenceNumber = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	if seqNum != 4 {
		t.Errorf("shared seqNum after 3 media frames = %d, want 4", seqNum)
	}
}

// --- adversary gap tests ---

// Gap #1: reader returns a non-EOF error (e.g., broken pipe from crashed ffmpeg).
// drainFrames must propagate the error, not swallow it or treat it as EOF.
func TestDrainFrames_ReaderError_Propagated(t *testing.T) {
	wantErr := errors.New("pipe broken")
	var calls int
	err := drainFrames(context.Background(), &errorAfterReader{err: wantErr, afterBytes: 0}, muLawFrame20ms, func([]byte) error {
		calls++
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("got %v, want reader error %v", err, wantErr)
	}
	if calls != 0 {
		t.Errorf("send called %d times after reader error, want 0", calls)
	}
}

// errorAfterReader delivers afterBytes bytes of 0x00, then returns err.
type errorAfterReader struct {
	err        error
	afterBytes int
	pos        int
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.afterBytes {
		return 0, r.err
	}
	n := len(p)
	if r.pos+n > r.afterBytes {
		n = r.afterBytes - r.pos
	}
	for i := 0; i < n; i++ {
		p[i] = 0x00
	}
	r.pos += n
	return n, nil
}

// Gap #2: context cancelled mid-stream (after the first frame is sent, while
// blocked waiting for the second). drainFrames must exit promptly and not hang.
func TestDrainFrames_ContextCancelledMidStream_Stops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// blocksAfterOneFrame delivers one complete frame then blocks until ctx is done.
	r := &blocksAfterFrameReader{frameSize: muLawFrame20ms, ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- drainFrames(ctx, r, muLawFrame20ms, func([]byte) error { return nil })
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("drainFrames did not stop within 2s after context cancellation")
	}
}

// blocksAfterFrameReader delivers exactly frameSize bytes then blocks until ctx is done.
type blocksAfterFrameReader struct {
	frameSize int
	ctx       context.Context
	pos       int
}

func (r *blocksAfterFrameReader) Read(p []byte) (int, error) {
	if r.pos < r.frameSize {
		n := len(p)
		if r.pos+n > r.frameSize {
			n = r.frameSize - r.pos
		}
		for i := 0; i < n; i++ {
			p[i] = 0x7f
		}
		r.pos += n
		return n, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// Gap #4: frameSize=0 is a programming error — drainFrames must return an
// error rather than loop infinitely.
func TestDrainFrames_FrameSizeZero_ReturnsError(t *testing.T) {
	err := drainFrames(context.Background(), bytes.NewReader(nil), 0, func([]byte) error { return nil })
	if err == nil {
		t.Error("drainFrames with frameSize=0 must return an error")
	}
}
