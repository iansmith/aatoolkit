package main

import (
	"context"
	"errors"
	"io"

	"github.com/coder/websocket"
)

// This file is the file-backed frame source: an alternative to capture_darwin.go's
// mic capture, selected by the -audio flag. It carries no build tag, so unlike mic
// capture it is available on every platform.
//
// Phase 0 (AATK-56): declarations only, so the red tests compile and fail on
// behavior rather than on a missing symbol. No implementation yet.

// errNotImplemented marks the Phase 0 stubs below. It disappears with the
// implementation.
var errNotImplemented = errors.New("not implemented")

// resolveAudioPath validates that path names a readable regular file and returns
// the path to open. It is a pure validator in validateE164's shape (main.go) so
// it is unit-testable without driving main(), which ends in log.Fatalf/os.Exit.
//
// A relative path resolves against the process working directory.
// The Phase 0 stub returns ("", nil) rather than an error deliberately: a stub
// that rejected everything would make every rejection-shaped test pass
// vacuously. Returning success makes them genuinely red, so the baseline
// actually constrains the implementation.
func resolveAudioPath(path string) (string, error) {
	return "", nil
}

// streamFileFramesFrom reads muLawFrame20ms-sized frames from r and hands each to
// send, paced at real time (one frame per 20 ms) so the server's VAD silence
// accounting behaves as it does on a live call. onMicWarm fires exactly once,
// before the first frame, with capHit=false.
//
// Unlike the mic path it does NOT discard leading silence: a fixture streams
// verbatim so replays stay deterministic.
//
// This io.Reader + send seam is the level capture_test.go's existing
// TestDrainFrames_* suite tests at; streamFileFrames wraps it for the live path.
func streamFileFramesFrom(ctx context.Context, r io.Reader, send func([]byte) error, onMicWarm func(bool)) error {
	return errNotImplemented
}

// streamFileFrames returns the frame source dial() calls through when -audio is
// set: it opens path, wraps each frame with the call's mediaFrameEncoder, and
// writes it to conn. The returned func matches the streamMic seam's signature
// (dial.go), mirroring capture_darwin.go's streamMicFrames shape.
func streamFileFrames(path string) func(context.Context, *websocket.Conn, string, *int, func(bool)) error {
	return func(context.Context, *websocket.Conn, string, *int, func(bool)) error {
		return errNotImplemented
	}
}
