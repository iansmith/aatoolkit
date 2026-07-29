package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// AATK-56 Phase 0. These tests are transcribed from the ticket's Test expectations;
// the expected values are the contract and must not be adjusted to match the
// implementation.
//
// They bind to streamFileFramesFrom (the io.Reader + send seam), never to
// streamFileFrames, which needs a live *websocket.Conn.

// collectFrames runs streamFileFramesFrom over input and returns the frames sent,
// each defensively copied (the implementation is free to reuse its read buffer).
func collectFrames(t *testing.T, input []byte) ([][]byte, error) {
	t.Helper()
	var got [][]byte
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func(f []byte) error {
			got = append(got, append([]byte(nil), f...))
			return nil
		},
		func(bool) {},
	)
	return got, err
}

// TestStreamFileFramesFrom_FramesMatchInput pins the framing contract: 160-byte
// frames in file order, with a partial trailing frame dropped.
func TestStreamFileFramesFrom_FramesMatchInput(t *testing.T) {
	// 500 bytes = 3 whole 160-byte frames (480) + a 20-byte partial tail.
	input := make([]byte, 500)
	for i := range input {
		input[i] = byte(i % 251) // distinguishable, non-repeating within a frame
	}

	got, err := collectFrames(t, input)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("frame count: got %d, want 3 — 500 bytes is 3 whole 160-byte frames plus a 20-byte partial tail that must be dropped", len(got))
	}
	for i, f := range got {
		if len(f) != muLawFrame20ms {
			t.Errorf("frame %d length: got %d, want %d", i, len(f), muLawFrame20ms)
		}
	}
	if joined := bytes.Join(got, nil); !bytes.Equal(joined, input[:480]) {
		t.Errorf("frames do not concatenate to the input's first 480 bytes (got %d bytes)", len(joined))
	}
}

// TestStreamFileFramesFrom_RealTimePacing pins that frames are paced, not burst.
// Lower bound only — no upper bound, so CI scheduling jitter cannot make it flaky.
func TestStreamFileFramesFrom_RealTimePacing(t *testing.T) {
	input := make([]byte, 10*muLawFrame20ms) // 1600 bytes = 10 frames

	start := time.Now()
	got, err := collectFrames(t, input)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("frame count: got %d, want 10", len(got))
	}

	const wantAtLeast = 180 * time.Millisecond // 9 gaps × 20 ms
	if elapsed < wantAtLeast {
		t.Errorf("elapsed: got %v, want >= %v — a 10-frame file must stream in real time, not burst", elapsed, wantAtLeast)
	}
}

// TestStreamFileFramesFrom_MicWarmFiresOnceBeforeFirstFrame pins the warm signal's
// arity and ordering, and that leading silence is NOT discarded for a file source.
func TestStreamFileFramesFrom_MicWarmFiresOnceBeforeFirstFrame(t *testing.T) {
	// Frame 1 is pure μ-law silence; frame 2 is not. The mic path would discard
	// frame 1; the file path must emit it verbatim.
	input := make([]byte, 2*muLawFrame20ms)
	for i := 0; i < muLawFrame20ms; i++ {
		input[i] = telephony.MuLawSilence // 0xFF
	}
	for i := muLawFrame20ms; i < 2*muLawFrame20ms; i++ {
		input[i] = 0x7F
	}

	var (
		warmCalls  int
		warmCapHit bool
		sends      int
		warmBefore bool
	)
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func([]byte) error {
			sends++
			return nil
		},
		func(capHit bool) {
			warmCalls++
			warmCapHit = capHit
			warmBefore = sends == 0
		},
	)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}

	if warmCalls != 1 {
		t.Errorf("onMicWarm call count: got %d, want exactly 1", warmCalls)
	}
	if warmCapHit {
		t.Errorf("onMicWarm capHit: got true, want false — the file source has no discard cap to hit")
	}
	if !warmBefore {
		t.Errorf("onMicWarm fired after the first frame was sent; it must fire strictly before")
	}
	if sends != 2 {
		t.Errorf("frame count: got %d, want 2 — the leading all-0xFF frame must NOT be discarded", sends)
	}
}

// TestStreamFileFramesFrom_EOFReturnsNil pins the value dial's naturalEnd branch
// depends on: a clean EOF is a caller hangup, not an error.
func TestStreamFileFramesFrom_EOFReturnsNil(t *testing.T) {
	input := make([]byte, 2*muLawFrame20ms) // exact frame multiple, no partial tail

	if _, err := collectFrames(t, input); err != nil {
		t.Errorf("streamFileFramesFrom at EOF: got %v, want nil — dial's naturalEnd branch sends the stop frame only on a nil return", err)
	}
}

// TestResolveAudioPath pins the validator's accept/reject contract.
func TestResolveAudioPath(t *testing.T) {
	dir := t.TempDir()

	readable := filepath.Join(dir, "clip.ulaw")
	if err := os.WriteFile(readable, make([]byte, muLawFrame20ms), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	missing := filepath.Join(dir, "nope.ulaw")

	t.Run("readable file accepted", func(t *testing.T) {
		if _, err := resolveAudioPath(readable); err != nil {
			t.Errorf("resolveAudioPath(%q): got %v, want nil", readable, err)
		}
	})

	t.Run("missing path rejected naming the path", func(t *testing.T) {
		_, err := resolveAudioPath(missing)
		if err == nil {
			t.Fatalf("resolveAudioPath(%q): got nil error, want a rejection", missing)
		}
		if !bytes.Contains([]byte(err.Error()), []byte(missing)) {
			t.Errorf("error %q does not contain the path as given (%q)", err.Error(), missing)
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		if _, err := resolveAudioPath(dir); err == nil {
			t.Errorf("resolveAudioPath(%q): got nil error, want a rejection — a directory is not a μ-law clip", dir)
		}
	})

	t.Run("relative path resolves against the process working directory", func(t *testing.T) {
		// The test process's cwd is the package dir. A relative name that exists
		// there must be accepted; the same name is not expected to exist at the
		// repo root, which is what the out-of-root subprocess test pins.
		rel := "aatk56_relcheck.ulaw"
		if err := os.WriteFile(rel, make([]byte, muLawFrame20ms), 0o644); err != nil {
			t.Fatalf("write relative fixture: %v", err)
		}
		t.Cleanup(func() { os.Remove(rel) })

		if _, err := resolveAudioPath(rel); err != nil {
			t.Errorf("resolveAudioPath(%q): got %v, want nil — a relative path must resolve against the process working directory", rel, err)
		}
	})
}

// ---------------------------------------------------------------------------
// AATK-56 Phase 0 — adversary gap tests (Step 0f).
//
// The Phase 0 suite above transcribes the ticket's Test expectations. These add
// the cases the gap finder identified as missing, concentrated on the three
// risks pacing introduces that the ticket's own expectations do not reach:
// error propagation, context cancellation, and the fact that a lower bound on
// *total* elapsed time does not actually pin per-frame pacing.
// ---------------------------------------------------------------------------

// TestStreamFileFramesFrom_PerFrameSpacing closes a vacuity in
// TestStreamFileFramesFrom_RealTimePacing: a total-elapsed lower bound is also
// satisfied by an implementation that bursts every frame and then sleeps once.
// Delivering a whole utterance in an instant is exactly what the pacing
// requirement exists to prevent, so pin the arrival time of each frame.
func TestStreamFileFramesFrom_PerFrameSpacing(t *testing.T) {
	const frames = 5
	input := make([]byte, frames*muLawFrame20ms)

	start := time.Now()
	var arrivals []time.Duration
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func([]byte) error {
			arrivals = append(arrivals, time.Since(start))
			return nil
		},
		func(bool) {},
	)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(arrivals) != frames {
		t.Fatalf("frame count: got %d, want %d", len(arrivals), frames)
	}

	// Half-nominal floor per frame: tolerant of scheduler jitter, but a burst
	// (every arrival ~0) cannot satisfy it.
	for k, at := range arrivals {
		floor := time.Duration(k) * 10 * time.Millisecond
		if at < floor {
			t.Errorf("frame %d arrived at %v, want >= %v — frames must be spaced, not burst then padded", k, at, floor)
		}
	}
	for k := 1; k < len(arrivals); k++ {
		if arrivals[k] <= arrivals[k-1] {
			t.Errorf("frame %d arrived at %v, not strictly after frame %d at %v", k, arrivals[k], k-1, arrivals[k-1])
		}
	}
}

// TestStreamFileFramesFrom_SendError_PropagatedAndLoopAborted mirrors
// TestDrainFrames_SendError_PropagatedAndLoopAborted for the file source.
// dial.go's naturalEnd treats any non-cancel return as a caller hangup and
// sends the stop frame, so swallowing a websocket write failure and returning
// nil would report a clean hangup on a broken socket.
func TestStreamFileFramesFrom_SendError_PropagatedAndLoopAborted(t *testing.T) {
	wantErr := errors.New("send failed")
	input := make([]byte, 3*muLawFrame20ms)

	sends := 0
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func([]byte) error {
			sends++
			return wantErr
		},
		func(bool) {},
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("error: got %v, want %v", err, wantErr)
	}
	if sends != 1 {
		t.Errorf("send call count: got %d, want 1 — the loop must abort on the first send error", sends)
	}
}

// TestStreamFileFramesFrom_ReaderError_Propagated pins that a non-EOF read
// error is NOT collapsed to nil. Only io.EOF / io.ErrUnexpectedEOF mean "clip
// finished"; a disk error mid-clip must not be reported to dial as a hangup.
func TestStreamFileFramesFrom_ReaderError_Propagated(t *testing.T) {
	wantErr := errors.New("disk failure")

	sends := 0
	err := streamFileFramesFrom(
		context.Background(),
		&errorAfterReader{err: wantErr, afterBytes: 2 * muLawFrame20ms},
		func([]byte) error {
			sends++
			return nil
		},
		func(bool) {},
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("error: got %v, want %v — a non-EOF read error must propagate, not read as a clean hangup", err, wantErr)
	}
	if sends != 2 {
		t.Errorf("send call count: got %d, want 2 (the whole frames delivered before the failure)", sends)
	}
}

// TestStreamFileFramesFrom_PreCancelledContext_NoSendsNoWarm mirrors
// TestDrainFrames_PreCancelledContext_ReturnsContextCancelled. onMicWarm drives
// dial.go's earcon, so a warm signal emitted during teardown plays a tone into
// a call that is already closing.
func TestStreamFileFramesFrom_PreCancelledContext_NoSendsNoWarm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := make([]byte, 3*muLawFrame20ms)
	sends, warmCalls := 0, 0
	err := streamFileFramesFrom(
		ctx,
		bytes.NewReader(input),
		func([]byte) error { sends++; return nil },
		func(bool) { warmCalls++ },
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error: got %v, want context.Canceled", err)
	}
	if sends != 0 {
		t.Errorf("send call count: got %d, want 0", sends)
	}
	if warmCalls != 0 {
		t.Errorf("onMicWarm call count: got %d, want 0 — no warm signal on an already-cancelled call", warmCalls)
	}
}

// TestStreamFileFramesFrom_ContextCancelledMidStream_StopsPromptly is the
// failure mode pacing itself introduces: a naive time.Sleep per frame ignores
// ctx, so Ctrl-C part-way through a long clip would keep streaming for the rest
// of it. Nothing else in the suite pins cancellation latency.
func TestStreamFileFramesFrom_ContextCancelledMidStream_StopsPromptly(t *testing.T) {
	const frames = 500 // 10 s of audio at 20 ms/frame
	input := make([]byte, frames*muLawFrame20ms)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	sends := 0

	done := make(chan error, 1)
	go func() {
		done <- streamFileFramesFrom(
			ctx,
			bytes.NewReader(input),
			func([]byte) error {
				mu.Lock()
				sends++
				n := sends
				mu.Unlock()
				if n == 2 {
					cancel()
				}
				return nil
			},
			func(bool) {},
		)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("streamFileFramesFrom did not return within 2s of cancellation — the pacing wait must honor ctx, not sleep blindly")
	}

	mu.Lock()
	got := sends
	mu.Unlock()
	if got > 10 {
		t.Errorf("send count after cancel: got %d, want <= 10 — streaming must stop promptly, not drain the remaining clip", got)
	}
}

// TestStreamFileFramesFrom_ExactlyOneFrame_OneSendOneWarm is the smallest input
// that produces output, and the only case that distinguishes sleeping before
// each frame from sleeping after it. A trailing sleep after the final frame
// adds dead latency before dial's sendStop.
func TestStreamFileFramesFrom_ExactlyOneFrame_OneSendOneWarm(t *testing.T) {
	input := make([]byte, muLawFrame20ms)
	for i := range input {
		input[i] = 0x2A
	}

	var got [][]byte
	warmCalls := 0
	start := time.Now()
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func(f []byte) error {
			got = append(got, append([]byte(nil), f...))
			return nil
		},
		func(bool) { warmCalls++ },
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frame count: got %d, want 1", len(got))
	}
	if !bytes.Equal(got[0], input) {
		t.Errorf("frame content does not match the input")
	}
	if warmCalls != 1 {
		t.Errorf("onMicWarm call count: got %d, want 1", warmCalls)
	}
	if elapsed >= 20*time.Millisecond {
		t.Errorf("elapsed: got %v, want < 20ms — a single frame has no pacing gap; there must be no trailing sleep after the final frame", elapsed)
	}
}

// TestStreamFileFramesFrom_EmptyAndSubFrameInput pins the contract for inputs
// that yield no complete frame. "onMicWarm fires before the first frame" has no
// first frame here, so it must not fire at all — pinned as a decision rather
// than left to accident, since dial.go's earcon hangs off it.
func TestStreamFileFramesFrom_EmptyAndSubFrameInput(t *testing.T) {
	for _, size := range []int{0, 1, muLawFrame20ms - 1} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			sends, warmCalls := 0, 0
			err := streamFileFramesFrom(
				context.Background(),
				bytes.NewReader(make([]byte, size)),
				func([]byte) error { sends++; return nil },
				func(bool) { warmCalls++ },
			)
			if err != nil {
				t.Errorf("error: got %v, want nil — too little data is a clean EOF, not a failure", err)
			}
			if sends != 0 {
				t.Errorf("send call count: got %d, want 0 — a partial frame is dropped", sends)
			}
			if warmCalls != 0 {
				t.Errorf("onMicWarm call count: got %d, want 0 — no first frame means no warm signal", warmCalls)
			}
		})
	}
}

// TestStreamFileFramesFrom_AllSilenceBeyondCap_NothingDiscarded guards the
// ticket's explicit fence at the boundary where the two frame sources diverge
// most: the mic path discards leading silence up to a 75-frame cap, the file
// path must not discard any of it. A clip that opens with a long pause is the
// realistic replay input.
func TestStreamFileFramesFrom_AllSilenceBeyondCap_NothingDiscarded(t *testing.T) {
	const frames = 80 // beyond the mic path's 75-frame discard cap
	input := make([]byte, frames*muLawFrame20ms)
	for i := range input {
		input[i] = telephony.MuLawSilence
	}

	got, err := collectFrames(t, input)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(got) != frames {
		t.Fatalf("frame count: got %d, want %d — the file source must not discard leading silence at any depth", len(got), frames)
	}
	silent := bytes.Repeat([]byte{telephony.MuLawSilence}, muLawFrame20ms)
	for i, f := range got {
		if !bytes.Equal(f, silent) {
			t.Fatalf("frame %d is not a verbatim silence frame", i)
		}
	}
}

// TestStreamFileFramesFrom_WarmFiresOncePerCallNotPerProcess guards the trap
// that "fires exactly once" invites: a sync.Once or package-level flag, correct
// for one call and silently wrong for every replay after it.
func TestStreamFileFramesFrom_WarmFiresOncePerCallNotPerProcess(t *testing.T) {
	input := make([]byte, 2*muLawFrame20ms)

	for attempt := 1; attempt <= 2; attempt++ {
		sends, warmCalls := 0, 0
		err := streamFileFramesFrom(
			context.Background(),
			bytes.NewReader(input),
			func([]byte) error { sends++; return nil },
			func(bool) { warmCalls++ },
		)
		if err != nil {
			t.Fatalf("call %d: %v", attempt, err)
		}
		if warmCalls != 1 {
			t.Errorf("call %d: onMicWarm count: got %d, want 1 — the warm signal is per call, not per process", attempt, warmCalls)
		}
		if sends != 2 {
			t.Errorf("call %d: send count: got %d, want 2", attempt, sends)
		}
	}
}

// TestStreamFileFramesFrom_NoSendsAfterReturn pins that the pacing loop is
// synchronous on the caller's goroutine. If frames were paced from a background
// goroutine they could still land after dial.go has already called sendStop,
// and the plain-local counters in these tests would be races that pass.
func TestStreamFileFramesFrom_NoSendsAfterReturn(t *testing.T) {
	const frames = 4
	input := make([]byte, frames*muLawFrame20ms)

	var mu sync.Mutex
	sends := 0
	err := streamFileFramesFrom(
		context.Background(),
		bytes.NewReader(input),
		func([]byte) error {
			mu.Lock()
			sends++
			mu.Unlock()
			return nil
		},
		func(bool) {},
	)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}

	mu.Lock()
	atReturn := sends
	mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	later := sends
	mu.Unlock()

	if atReturn != frames {
		t.Errorf("send count at return: got %d, want %d", atReturn, frames)
	}
	if later != atReturn {
		t.Errorf("send count grew from %d to %d after the call returned — no frame may be in flight once it returns", atReturn, later)
	}
}

// TestStreamFileFramesFrom_SmallChunkReader_AssemblesCompleteFrames mirrors
// TestDrainFrames_SmallChunkReader_AssemblesCompleteFrames. A file source that
// reimplements the read loop inline while adding pacing silently loses
// drainFrames' short-read reassembly.
func TestStreamFileFramesFrom_SmallChunkReader_AssemblesCompleteFrames(t *testing.T) {
	data := make([]byte, 2*muLawFrame20ms)
	for i := range data {
		data[i] = byte(i % 251)
	}

	var got [][]byte
	err := streamFileFramesFrom(
		context.Background(),
		&slowReader{data: data, chunkSize: 10}, // 10 bytes per Read
		func(f []byte) error {
			got = append(got, append([]byte(nil), f...))
			return nil
		},
		func(bool) {},
	)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("frame count: got %d, want 2 — short reads must be reassembled into complete frames", len(got))
	}
	if !bytes.Equal(bytes.Join(got, nil), data) {
		t.Errorf("reassembled frames do not match the input")
	}
}

// TestStreamFileFramesFrom_RealFixturePrefix is the only test that streams real
// μ-law bytes, end to end through resolveAudioPath and an actual file handle.
// It would catch an assumed header or a wrong frame size that synthetic
// all-zero buffers cannot.
func TestStreamFileFramesFrom_RealFixturePrefix(t *testing.T) {
	fixture := filepath.Join("..", "..", "telephony", "testdata", "how_are_you.ulaw")

	resolved, err := resolveAudioPath(fixture)
	if err != nil {
		t.Fatalf("resolveAudioPath(%q): %v", fixture, err)
	}
	f, err := os.Open(resolved)
	if err != nil {
		t.Fatalf("open resolved path: %v", err)
	}
	defer f.Close()

	const frames = 5
	var got [][]byte
	err = streamFileFramesFrom(
		context.Background(),
		io.LimitReader(f, frames*muLawFrame20ms),
		func(fr []byte) error {
			got = append(got, append([]byte(nil), fr...))
			return nil
		},
		func(bool) {},
	)
	if err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	if len(got) != frames {
		t.Fatalf("frame count: got %d, want %d", len(got), frames)
	}

	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture independently: %v", err)
	}
	if !bytes.Equal(bytes.Join(got, nil), want[:frames*muLawFrame20ms]) {
		t.Errorf("streamed bytes do not match the fixture's first %d bytes", frames*muLawFrame20ms)
	}
}

// TestResolveAudioPath_ReturnedPathOpensToTheSameFile pins the return value the
// four TestResolveAudioPath subtests all discard with `_, err :=`. An
// implementation returning "" or the wrong path passes every one of them, yet
// the returned path is what the caller actually opens.
func TestResolveAudioPath_ReturnedPathOpensToTheSameFile(t *testing.T) {
	content := []byte("aatk56-distinctive-bytes-for-identity-check")

	rel := "aatk56_identity.ulaw"
	if err := os.WriteFile(rel, content, 0o644); err != nil {
		t.Fatalf("write relative fixture: %v", err)
	}
	t.Cleanup(func() { os.Remove(rel) })

	got, err := resolveAudioPath(rel)
	if err != nil {
		t.Fatalf("resolveAudioPath(%q): %v", rel, err)
	}

	readBack, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("returned path %q is not openable: %v", got, err)
	}
	if !bytes.Equal(readBack, content) {
		t.Errorf("returned path %q does not open the file that was resolved", got)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	wantInfo, err := os.Stat(filepath.Join(cwd, rel))
	if err != nil {
		t.Fatalf("stat expected file: %v", err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat returned path: %v", err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Errorf("returned path %q is not the same file as %q — a relative path must resolve against the process working directory", got, filepath.Join(cwd, rel))
	}
}

// TestResolveAudioPath_UnreadableFileRejected exercises the "readable" half of
// the validator's contract, which the 0o644 happy-path subtest cannot.
func TestResolveAudioPath_UnreadableFileRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; mode bits do not restrict access")
	}
	p := filepath.Join(t.TempDir(), "locked.ulaw")
	if err := os.WriteFile(p, make([]byte, muLawFrame20ms), 0o000); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := resolveAudioPath(p); err == nil {
		t.Errorf("resolveAudioPath(%q): got nil error, want a rejection — the file exists but is not readable", p)
	}
}

// streamFileFrames must be assignable to the streamMic seam (dial.go). A
// signature mismatch is otherwise a compile error found at integration time
// rather than here.
var _ = func() {
	streamMic = streamFileFrames("unused")
}
