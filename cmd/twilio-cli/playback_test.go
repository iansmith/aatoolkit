package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// recordingSink is an in-memory io.WriteCloser standing in for ffplay's stdin.
// It records everything written (in order) and counts Close calls, so tests can
// assert that frames form one continuous stream through a single sink.
type recordingSink struct {
	buf      bytes.Buffer
	writes   int
	closes   int
	writeErr error
}

func (s *recordingSink) Write(p []byte) (int, error) {
	s.writes++
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.buf.Write(p)
}

func (s *recordingSink) Close() error {
	s.closes++
	return nil
}

// mkFrame returns a 160-byte μ-law frame filled with value v.
func mkFrame(v byte) []byte {
	f := make([]byte, muLawFrame20ms)
	for i := range f {
		f[i] = v
	}
	return f
}

// --- core: the property that distinguishes the fix from the SOP-50 bug ---

// The whole point of SOP-65: consecutive frames must land in the single sink
// back-to-back, in order, forming one continuous μ-law stream — not one
// playback per frame.
func TestPlayer_FramesFormOneContinuousStreamInOrder(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	a, b, c := mkFrame(0x01), mkFrame(0x02), mkFrame(0x03)
	for _, f := range [][]byte{a, b, c} {
		if err := p.play(f); err != nil {
			t.Fatalf("play: %v", err)
		}
	}

	want := bytes.Join([][]byte{a, b, c}, nil)
	if !bytes.Equal(s.buf.Bytes(), want) {
		t.Errorf("stream mismatch: got %d bytes, want %d contiguous in-order bytes",
			s.buf.Len(), len(want))
	}
}

// A realistic multi-second clip (500 × 20ms frames = 10s) must arrive as one
// gap-free stream with every frame in its original position.
func TestPlayer_ManyFramesAllContiguous(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	const n = 500
	for i := 0; i < n; i++ {
		if err := p.play(mkFrame(byte(i))); err != nil {
			t.Fatalf("play %d: %v", i, err)
		}
	}

	if s.buf.Len() != n*muLawFrame20ms {
		t.Fatalf("total bytes = %d, want %d (no gaps, no drops)", s.buf.Len(), n*muLawFrame20ms)
	}
	got := s.buf.Bytes()
	for i := 0; i < n; i++ {
		if got[i*muLawFrame20ms] != byte(i) {
			t.Errorf("frame %d out of order at offset %d", i, i*muLawFrame20ms)
		}
	}
}

// One sink for the whole call: however many frames play, the player must close
// exactly one sink — i.e. one ffplay process per call, not one per frame.
func TestPlayer_OneSinkForWholeCall(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	for i := 0; i < 10; i++ {
		if err := p.play(mkFrame(byte(i))); err != nil {
			t.Fatalf("play %d: %v", i, err)
		}
	}
	if err := p.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s.closes != 1 {
		t.Errorf("sink closed %d times, want exactly 1 — one ffplay per call", s.closes)
	}
}

// --- edge / boundary ---

// Empty or nil frames are no-ops: they must not write bytes into the stream.
func TestPlayer_EmptyFrameWritesNothing(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	if err := p.play(nil); err != nil {
		t.Fatalf("play(nil): %v", err)
	}
	if err := p.play([]byte{}); err != nil {
		t.Fatalf("play(empty): %v", err)
	}
	if s.buf.Len() != 0 {
		t.Errorf("empty frames wrote %d bytes, want 0", s.buf.Len())
	}
}

// close must signal end-of-stream to the sink exactly once (the EOF that makes
// ffplay exit) even when no frames were ever played.
func TestPlayer_CloseWithNoFramesClosesSinkOnce(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	if err := p.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s.buf.Len() != 0 {
		t.Errorf("close wrote %d bytes, want 0", s.buf.Len())
	}
	if s.closes != 1 {
		t.Errorf("sink Close called %d times, want 1", s.closes)
	}
}

// --- adversary gap tests ---

// Gap: the served audio file length is rarely a multiple of 160, so the server
// emits a short final frame. play must write it whole, not pad or drop it.
func TestPlayer_ShortFinalFrameWrittenWhole(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	full := mkFrame(0x01)                   // 160 bytes
	short := bytes.Repeat([]byte{0x02}, 80) // partial trailing frame
	if err := p.play(full); err != nil {
		t.Fatalf("play full: %v", err)
	}
	if err := p.play(short); err != nil {
		t.Fatalf("play short: %v", err)
	}

	want := append(append([]byte{}, full...), short...)
	if !bytes.Equal(s.buf.Bytes(), want) {
		t.Errorf("short final frame not written whole: got %d bytes, want %d",
			s.buf.Len(), len(want))
	}
}

// Gap: frames of differing sizes must concatenate byte-exactly — no per-frame
// padding to a fixed size and no truncation.
func TestPlayer_MixedSizeFramesConcatenatedExactly(t *testing.T) {
	s := &recordingSink{}
	p := newPlayerWithSink(s)

	frames := [][]byte{
		bytes.Repeat([]byte{0x01}, 160),
		bytes.Repeat([]byte{0x02}, 40),
		bytes.Repeat([]byte{0x03}, 160),
		bytes.Repeat([]byte{0x04}, 1),
	}
	for i, f := range frames {
		if err := p.play(f); err != nil {
			t.Fatalf("play %d: %v", i, err)
		}
	}

	want := bytes.Join(frames, nil)
	if !bytes.Equal(s.buf.Bytes(), want) {
		t.Errorf("mixed-size concat mismatch: got %d bytes, want %d", s.buf.Len(), len(want))
	}
}

// --- error / rejection ---

// A sink write failure (e.g. ffplay died, broken pipe) must propagate from play,
// not be swallowed.
func TestPlayer_WriteErrorPropagated(t *testing.T) {
	wantErr := errors.New("broken pipe")
	s := &recordingSink{writeErr: wantErr}
	p := newPlayerWithSink(s)

	if err := p.play(mkFrame(0x01)); !errors.Is(err, wantErr) {
		t.Errorf("play error = %v, want %v", err, wantErr)
	}
}

// --- lazyPlayer: fail-once disable behavior ---

// If ffplay fails to start, playback is disabled: newPlayer is not called again
// on later frames.
func TestLazyPlayer_DisablesAfterStartFailure(t *testing.T) {
	calls := 0
	l := newLazyPlayer(context.Background())
	l.newPlayer = func(context.Context) (*audioPlayer, error) {
		calls++
		return nil, errors.New("no ffplay")
	}

	l.play(mkFrame(0x01))
	l.play(mkFrame(0x02))

	if calls != 1 {
		t.Errorf("newPlayer called %d times, want 1 (start failure must be remembered)", calls)
	}
}

// If ffplay dies mid-call (write fails), the dead player is reaped and playback
// is disabled — no further writes are attempted for the rest of the call.
func TestLazyPlayer_DisablesAndReapsAfterMidCallWriteError(t *testing.T) {
	failing := &recordingSink{writeErr: errors.New("broken pipe")}
	calls := 0
	l := newLazyPlayer(context.Background())
	l.newPlayer = func(context.Context) (*audioPlayer, error) {
		calls++
		return &audioPlayer{sink: failing}, nil
	}

	l.play(mkFrame(0x01)) // starts player, write fails → disable + reap
	l.play(mkFrame(0x02)) // must be a no-op

	if calls != 1 {
		t.Errorf("newPlayer called %d times, want 1 (dead player must not be recreated)", calls)
	}
	if failing.writes != 1 {
		t.Errorf("sink written %d times, want 1 (no writes after the player is disabled)", failing.writes)
	}
	if failing.closes != 1 {
		t.Errorf("sink closed %d times, want 1 (dead player must be reaped)", failing.closes)
	}
}
