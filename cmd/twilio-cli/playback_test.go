package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
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

// --- μ-law encoding validation ---

// TestMulawEncoding_RoundTripsThroughRealFfmpeg validates that LinearToMuLaw
// produces correct ITU-T G.711 μ-law encoded bytes by round-tripping through
// the actual ffmpeg binary (the oracle), not a hand-rolled decoder. Encoding a
// sample and decoding it through ffmpeg recovers a value within quantization
// tolerance and with the correct sign.
func TestMulawEncoding_RoundTripsThroughRealFfmpeg(t *testing.T) {
	// Test samples: zero, small pos/neg, large pos/neg, saturation extremes.
	tests := []struct {
		sample int16
		name   string
	}{
		{0, "silence"},
		{100, "small positive"},
		{-100, "small negative"},
		{1000, "large positive"},
		{-1000, "large negative"},
		{0x7fff, "max positive"},
		{-0x8000, "min negative (saturated)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode the sample.
			encoded := telephony.LinearToMuLaw(tt.sample)

			// Write to a temp file and decode via ffmpeg.
			tmpfile, err := os.CreateTemp("", "mulaw-*.raw")
			if err != nil {
				if os.IsNotExist(err) {
					t.Skip("no temp directory")
				}
				t.Fatalf("CreateTemp: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			if _, err := tmpfile.Write([]byte{encoded}); err != nil {
				tmpfile.Close()
				t.Fatalf("write encoded: %v", err)
			}
			tmpfile.Close()

			// Decode via ffmpeg.
			decodedFile, err := os.CreateTemp("", "decoded-*.pcm")
			if err != nil {
				t.Skip("no temp directory")
			}
			defer os.Remove(decodedFile.Name())
			decodedFile.Close()

			cmd := exec.Command("ffmpeg",
				"-f", "mulaw", "-ar", "8000", "-i", tmpfile.Name(),
				"-f", "s16le", "-ar", "8000", "-y", decodedFile.Name())
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard

			if err := cmd.Run(); err != nil {
				// ffmpeg might not be available in test environment; skip gracefully.
				if errors.Is(err, exec.ErrNotFound) {
					t.Skip("ffmpeg not found")
				}
				t.Fatalf("ffmpeg decode: %v", err)
			}

			// Read the decoded PCM sample (little-endian int16).
			decodedBuf, err := os.ReadFile(decodedFile.Name())
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if len(decodedBuf) < 2 {
				t.Fatalf("decoded file too short: %d bytes", len(decodedBuf))
			}

			decoded := int16(decodedBuf[0]) | (int16(decodedBuf[1]) << 8)

			// Check: decoded value is close to original (within μ-law quantization tolerance)
			// and has the correct sign.

			// Determine expected sign.
			expectedSign := tt.sample >= 0
			actualSign := decoded >= 0

			// For small magnitudes, be lenient on the decoded value itself but strict on sign.
			if expectedSign != actualSign {
				t.Errorf("sign mismatch: encoded %d → decoded %d (sign inverted)", tt.sample, decoded)
			}

			// For non-zero samples, check that the magnitude is in the ballpark.
			// μ-law quantization step grows with signal magnitude, so scale tolerance
			// accordingly; tight enough to reject a wrong-mantissa-shift regression.
			if tt.sample != 0 && decoded != 0 {
				diff := tt.sample - decoded
				if diff < 0 {
					diff = -diff
				}
				magnitude := int32(tt.sample)
				if magnitude < 0 {
					magnitude = -magnitude
				}
				// Tolerance: magnitude/50 + 10. Passes correct encoder's diffs
				// (0→0, ±100→4, ±1000→12, 32767→643) but rejects the wrong-mantissa-shift
				// mutant (±100→20, ±1000→116).
				scaledTolerance := int16(magnitude/50 + 10)
				if diff > scaledTolerance {
					t.Errorf("magnitude error: encoded %d → decoded %d (diff %d, tolerance %d)",
						tt.sample, decoded, diff, scaledTolerance)
				}
			}
		})
	}
}

// TestMulawEncoding validates that LinearToMuLaw produces correct ITU-T G.711
// μ-law encoded bytes by checking known reference values. The encoding must
// include the final bitwise complement step. This test's original sign-bit
// assertions (V1) were independently verified backwards by reviewers and
// replaced with TestMulawEncoding_RoundTripsThroughRealFfmpeg, which uses the
// ffmpeg oracle. This test is now minimal and non-critical; the ffmpeg-based
// test is the source of truth.
func TestMulawEncoding(t *testing.T) {
	tests := []struct {
		sample  int16
		name    string
		checkFn func(byte) bool
	}{
		// Test that silence (0) is encoded distinctly.
		{0, "silence", func(b byte) bool {
			// After bias+compress+XOR, 0 encodes to 0xFF (all bits set).
			return b == 0xFF
		}},
		// Test that positive and negative samples encode differently (no sign check;
		// sign correctness is verified by TestMulawEncoding_RoundTripsThroughRealFfmpeg).
		{100, "small positive", func(b byte) bool {
			// Should not be 0xFF (silence).
			return b != 0xFF
		}},
		{-100, "small negative", func(b byte) bool {
			// Should not be 0xFF (silence).
			return b != 0xFF
		}},
		// Test that larger magnitudes compress and encode distinctly.
		{1000, "large positive", func(b byte) bool {
			return b != 0xFF
		}},
		{-1000, "large negative", func(b byte) bool {
			return b != 0xFF
		}},
		// Test saturation at clip value.
		{0x7fff, "max positive", func(b byte) bool {
			return b != 0xFF
		}},
		{-0x8000, "min negative (saturated)", func(b byte) bool {
			return b != 0xFF
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := telephony.LinearToMuLaw(tt.sample)
			if !tt.checkFn(got) {
				t.Errorf("LinearToMuLaw(%d) = 0x%02x, failed validation", tt.sample, got)
			}
		})
	}
}

// --- earcon tests ---

// TestEarcon_FiresOnMicWarmSignalNotBefore verifies that the earcon is played
// exactly once on the mic-warm signal through the real dial() wiring, with zero
// tone bytes before the signal. Uses withFakeMic to exercise the actual
// onMicWarm callback path.
func TestEarcon_FiresOnMicWarmSignalNotBefore(t *testing.T) {
	s := &recordingSink{}

	// Inject a fake player that records earcon writes.
	origNewPlayerFunc := newPlayerFunc
	defer func() { newPlayerFunc = origNewPlayerFunc }()
	newPlayerFunc = func(ctx context.Context) (*audioPlayer, error) {
		return newPlayerWithSink(s), nil
	}

	// Inject a fake mic that fires onMicWarm exactly once (capHit=false).
	var onMicWarmCalled bool
	withFakeMic(t, func(ctx context.Context, _ *websocket.Conn, _ string, _ *int, onMicWarm func(bool)) error {
		// Before calling onMicWarm, the sink should be empty.
		if s.buf.Len() != 0 {
			t.Errorf("before onMicWarm: got %d bytes, want 0", s.buf.Len())
		}

		// Call onMicWarm to trigger the earcon.
		onMicWarm(false)
		onMicWarmCalled = true

		// Signal dial() to stop by returning without error.
		return nil
	})

	// Call dial with a stub server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // consume connected + start
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	if err := dial(context.Background(), newSID("CA"), addr); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Verify onMicWarm was actually called.
	if !onMicWarmCalled {
		t.Error("onMicWarm callback was never called")
	}

	// Verify exactly one earcon tone was written. Sized from the tone's own
	// constant, not a literal: this assertion used to hard-code 160 and had to
	// be edited when the tone was lengthened to be audible at all.
	if want := len(generateEarcon()); s.buf.Len() != want {
		t.Errorf("playback sink: got %d bytes written, want %d (one earcon tone)",
			s.buf.Len(), want)
	}
}

// TestEarcon_ToneWrittenOnlyToPlaybackSink verifies that earcon bytes reach
// only the playback sink through the real dial() wiring and do not leak to
// capture/websocket paths. Uses withFakeMic to exercise the actual onMicWarm
// callback path.
func TestEarcon_ToneWrittenOnlyToPlaybackSink(t *testing.T) {
	s := &recordingSink{}

	// Inject a fake player that records earcon writes.
	origNewPlayerFunc := newPlayerFunc
	defer func() { newPlayerFunc = origNewPlayerFunc }()
	newPlayerFunc = func(ctx context.Context) (*audioPlayer, error) {
		return newPlayerWithSink(s), nil
	}

	// Inject a fake mic that fires onMicWarm.
	withFakeMic(t, func(ctx context.Context, _ *websocket.Conn, _ string, _ *int, onMicWarm func(bool)) error {
		// Call onMicWarm to trigger the earcon.
		onMicWarm(false)
		return nil
	})

	// Call dial with a stub server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // consume connected + start
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	if err := dial(context.Background(), newSID("CA"), addr); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Verify the earcon tone was written to the playback sink.
	if want := len(generateEarcon()); s.buf.Len() != want {
		t.Errorf("playback sink: got %d bytes, want %d",
			s.buf.Len(), want)
	}

	// Verify tone was written exactly once (one Write call).
	if s.writes != 1 {
		t.Errorf("playback sink writes: %d, want 1 (earcon fires once)", s.writes)
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

// TestGenerateEarcon_LongEnoughToHear pins the tone's duration.
//
// It was one 20 ms frame -- 160 bytes, eight cycles of 400 Hz -- which is a
// click, not a cue. An operator running a demo call reported never hearing it
// at all, and the tone being inaudible is why. This asserts a duration a
// person can actually register as a tone rather than a transient.
func TestGenerateEarcon_LongEnoughToHear(t *testing.T) {
	tone := generateEarcon()

	wantBytes := sampleRateHz * earconDurationMS / 1000
	if len(tone) != wantBytes {
		t.Errorf("earcon length: got %d bytes, want %d (%d ms at %d Hz)",
			len(tone), wantBytes, earconDurationMS, sampleRateHz)
	}
	if earconDurationMS < 150 {
		t.Errorf("earconDurationMS = %d, want >= 150: a shorter tone reads as a click", earconDurationMS)
	}
	if len(tone)%muLawFrame20ms != 0 {
		t.Errorf("earcon length %d is not a whole number of %d-byte frames", len(tone), muLawFrame20ms)
	}
}

// TestGenerateEarcon_RampsInAndOut pins the envelope.
//
// A tone that starts and ends at full amplitude puts a step discontinuity into
// the stream, which is heard as a click on either side of the tone -- the exact
// artifact that makes a short tone read as a pop. The ramp is what makes the
// tone sound deliberate rather than like a dropout.
func TestGenerateEarcon_RampsInAndOut(t *testing.T) {
	tone := generateEarcon()
	if len(tone) == 0 {
		t.Fatal("earcon is empty")
	}

	const quiet = 2000 // well under the 32767 peak, well over digital silence

	if got := absSample(tone[0]); got > quiet {
		t.Errorf("first sample amplitude %d, want <= %d: the tone starts with a step", got, quiet)
	}
	if got := absSample(tone[len(tone)-1]); got > quiet {
		t.Errorf("last sample amplitude %d, want <= %d: the tone ends with a step", got, quiet)
	}

	// The middle must still be loud, or "ramps in and out" is satisfied by a
	// tone that is silent throughout.
	var peak int
	for _, b := range tone {
		if a := absSample(b); a > peak {
			peak = a
		}
	}
	if peak < 16000 {
		t.Errorf("peak amplitude %d, want >= 16000: the tone is too quiet to hear", peak)
	}
}

// absSample decodes one mu-law byte and returns its magnitude.
//
// telephony exports LinearToMuLaw but no inverse, and adding one to the
// engine's public API to satisfy a CLI test would be the wrong place for it.
// This is the standard G.711 decode, test-only.
func absSample(b byte) int {
	b = ^b
	mantissa := int(b&0x0F)<<3 + 0x84
	exponent := int(b&0x70) >> 4
	v := (mantissa << exponent) - 0x84
	if b&0x80 != 0 {
		v = -v
	}
	if v < 0 {
		v = -v
	}
	return v
}
