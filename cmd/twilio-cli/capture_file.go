package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/coder/websocket"
)

// This file is the file-backed frame source: an alternative to capture_darwin.go's
// mic capture, selected by the -audio flag. It carries no build tag, so unlike mic
// capture it is available on every platform — which is what lets the frame path
// (VAD -> STT -> Turn) run headless, cross-platform, and deterministically.

// frameInterval is the wall-clock spacing between consecutive frames: exactly one
// frame's playout duration, so streaming any faster would hand the server a whole
// utterance at once and break the VAD's silence accounting. Derived from the frame
// size rather than written as a bare 20ms, so a frame-size retune cannot silently
// desynchronize the two — and taken from dial.go's mulawPlayoutDuration rather than
// restating its arithmetic, so μ-law bytes→duration has one definition here.
var frameInterval = mulawPlayoutDuration(muLawFrame20ms)

// resolveAudioPath validates that path names a readable regular file and returns
// the path to open. It is a pure validator in validateE164's shape (main.go) so
// it is unit-testable without driving main(), which ends in log.Fatalf/os.Exit.
//
// A relative path is returned unchanged, so it resolves against the process
// working directory when opened — never against the repository root.
//
// Every error names the path as given: -audio is operator input, and a rejection
// that doesn't quote it back is unactionable.
func resolveAudioPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		// Wrap rather than reformat: the underlying os error carries the
		// distinguishing detail (not found vs permission vs bad path).
		return "", fmt.Errorf("-audio %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("-audio %s: is a directory, not a μ-law audio file", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("-audio %s: not a regular file (mode %s)", path, info.Mode())
	}
	// Stat says it exists and is regular; only an open proves it is readable.
	// The two are not redundant, and the order is load-bearing: opening a FIFO
	// blocks until a writer appears, so the IsRegular rejection above must run
	// before any open — collapsing this into a single os.Open would hang the CLI
	// on a named pipe instead of rejecting it.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("-audio %s: %w", path, err)
	}
	f.Close()
	return path, nil
}

// streamFileFramesFrom reads muLawFrame20ms-sized frames from r and hands each to
// send, paced at real time so the server's VAD silence accounting behaves as it
// does on a live call. onMicWarm fires exactly once, immediately before the first
// frame, with capHit=false — the file source has no discard cap to hit. An input
// carrying no complete frame sends nothing and fires no warm signal.
//
// Unlike the mic path it does NOT discard leading silence: a fixture streams
// verbatim so replays stay deterministic. That is why it builds on drainFrames
// rather than drainFramesWithDiscard.
//
// It runs entirely on the caller's goroutine: when it returns, no frame is still
// in flight. A clean EOF returns nil, which is what dial's naturalEnd branch
// reads as a caller hangup; any other read or send error propagates.
//
// This io.Reader + send seam is the level capture_test.go's existing
// TestDrainFrames_* suite tests at; streamFileFrames wraps it for the live path.
func streamFileFramesFrom(ctx context.Context, r io.Reader, send func([]byte) error, onMicWarm func(bool)) error {
	start := time.Now()
	frame := 0

	return drainFrames(ctx, r, muLawFrame20ms, func(f []byte) error {
		// One frame counter drives both halves, so warm cannot fire twice and
		// frame 0 cannot wait.
		if frame == 0 {
			// Warm fires lazily at the first real frame, so an input with no
			// complete frame never signals one (dial's earcon hangs off this).
			if onMicWarm != nil {
				onMicWarm(false)
			}
		} else {
			// Absolute deadline schedule rather than a per-frame sleep: the
			// target is start+n*frameInterval, so time spent in send() does not
			// accumulate into drift over a long clip. Frame 0 goes out
			// immediately and there is no wait after the final frame — a
			// trailing sleep would just delay dial's stop frame.
			if err := waitUntil(ctx, start.Add(time.Duration(frame)*frameInterval)); err != nil {
				return err
			}
		}
		frame++

		return send(f)
	})
}

// waitUntil blocks until deadline or ctx is done, whichever comes first. It
// selects on ctx rather than sleeping blindly so a cancelled call stops within
// one frame interval instead of draining the rest of the clip.
func waitUntil(ctx context.Context, deadline time.Time) error {
	d := time.Until(deadline)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// installAudioFrameSource points the streamMic seam at a file-backed source when
// -audio names one, and reports an unusable path as an error for main to fatal on
// (webhookTarget's idiom — validation returns, the caller decides how to die).
//
// An empty audioPath is a no-op, so mic capture stays the default: this is the
// one place that decides which frame source a run uses, and it sets streamMic and
// frameSourceLabel together so the two can never disagree.
func installAudioFrameSource(audioPath string) error {
	if audioPath == "" {
		return nil
	}
	resolved, err := resolveAudioPath(audioPath)
	if err != nil {
		return err
	}
	streamMic = streamFileFrames(resolved)
	frameSourceLabel = fmt.Sprintf("audio file %s", resolved)
	return nil
}

// streamFileFrames returns the frame source dial() calls through when -audio is
// set: it opens path, wraps each frame with the call's mediaFrameEncoder, and
// writes it to conn. The returned func matches the streamMic seam's signature
// (dial.go), mirroring capture_darwin.go's streamMicFrames shape.
func streamFileFrames(path string) func(context.Context, *websocket.Conn, string, *int, func(bool)) error {
	return func(ctx context.Context, conn *websocket.Conn, streamSID string, seqNum *int, onMicWarm func(bool)) error {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("streamFileFrames: %w", err)
		}
		defer f.Close()

		enc := newMediaFrameEncoder(streamSID, seqNum)
		return streamFileFramesFrom(ctx, f, func(frame []byte) error {
			msg, err := enc.encode(frame)
			if err != nil {
				return err
			}
			// Fresh short-lived write context per frame, matching
			// streamMicFrames: a write must not inherit an unbounded deadline.
			writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return conn.Write(writeCtx, websocket.MessageText, msg)
		}, onMicWarm)
	}
}
