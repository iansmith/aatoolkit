//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/coder/websocket"
)

// newFFmpegCmd builds the avfoundation→μ-law ffmpeg capture command for mic.
func newFFmpegCmd(ctx context.Context, mic string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-i", mic,
		"-ar", "8000", "-ac", "1",
		"-acodec", "pcm_mulaw", "-f", "mulaw", "-",
	)
	cmd.Stderr = os.Stderr
	gracefulCancel(cmd)
	return cmd
}

// gracefulCancel wires cmd so a ctx-cancel stops the process with SIGINT — letting
// ffmpeg flush its capture buffer and close stdout at EOF — instead of
// exec.CommandContext's default SIGKILL, which drops buffered audio and truncates the
// recording's tail. WaitDelay bounds the graceful window before the hard kill.
//
// STUB (AATK-2 Phase 0): only WaitDelay is set here; cmd.Cancel is left at
// exec.CommandContext's default SIGKILL, so the graceful-cancel test is red.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
}

// streamMicFrames captures mic input via ffmpeg, slices it into 8 kHz μ-law
// 20 ms frames (160 bytes each), and sends each frame to conn as a Twilio
// media event. Returns when ctx is cancelled or the connection closes.
func streamMicFrames(ctx context.Context, conn *websocket.Conn, streamSID string) error {
	mic := os.Getenv("AATOOLKIT_STT_MIC")
	if mic == "" {
		mic = ":default"
	}
	cmd := newFFmpegCmd(ctx, mic)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("streamMicFrames: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("streamMicFrames: start ffmpeg (installed? `brew install ffmpeg`): %w", err)
	}

	enc := newMediaFrameEncoder(streamSID)
	drainErr := drainFrames(ctx, stdout, muLawFrame20ms, func(frame []byte) error {
		msg, encErr := enc.encode(frame)
		if encErr != nil {
			return encErr
		}
		return conn.Write(ctx, websocket.MessageText, msg)
	})
	// If drainFrames exited due to a send error (not context cancellation),
	// ffmpeg may still be running and will fill the pipe buffer, blocking
	// cmd.Wait indefinitely. Kill it now so Wait returns promptly.
	if drainErr != nil && ctx.Err() == nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	// Surface ffmpeg device failures only when drain was clean and teardown
	// was not requested. When drainErr is also set, it is the primary cause;
	// ffmpeg's exit is secondary (its stderr already carries the detail).
	if drainErr == nil && waitErr != nil && ctx.Err() == nil {
		return fmt.Errorf("streamMicFrames: ffmpeg: %w", waitErr)
	}
	return drainErr
}
