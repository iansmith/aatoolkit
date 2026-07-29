package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
