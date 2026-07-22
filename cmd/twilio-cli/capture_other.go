//go:build !darwin

package main

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// streamMicFrames is unsupported off macOS: mic capture uses ffmpeg's
// avfoundation input, which is macOS-only. It returns an error so the build
// stays honest on other platforms rather than silently doing nothing.
func streamMicFrames(_ context.Context, _ *websocket.Conn, _ string, _ *int) error {
	return errors.New("streamMicFrames: mic capture is only supported on macOS (avfoundation)")
}
