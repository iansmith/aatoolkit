//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// newFFmpegCmd builds the avfoundation→μ-law ffmpeg capture command for mic,
// which is the operator's raw AATOOLKIT_STT_MIC value (possibly empty).
//
// Normalization happens HERE rather than at the call site so no caller can
// route around it: the whole defect was a raw value reaching avfoundation's -i,
// and a helper the caller must remember to apply can be forgotten by the next
// one. There is exactly one way to build this command, and it normalizes.
func newFFmpegCmd(ctx context.Context, mic string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-i", normalizeMicSpec(mic),
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
// recording's tail. WaitDelay bounds the graceful window: if ffmpeg does not exit
// within it, os/exec escalates to SIGKILL, so a wedged process can never hang teardown.
//
// It also runs the child in its OWN process group (Setpgid). A terminal Ctrl-C signals
// twilio-cli's whole foreground group; without this the child ffmpeg would receive that
// SIGINT directly AND cmd.Cancel's — a double signal ffmpeg treats as "Immediate exit
// requested", skipping the very flush this function exists to enable. Isolated, ffmpeg
// sees only cmd.Cancel's single, controlled SIGINT.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGINT)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// normalizeMicSpec turns the AATOOLKIT_STT_MIC value into an avfoundation
// device spec.
//
// avfoundation's -i takes "[video]:[audio]", so a value with no colon names the
// *video* device: AATOOLKIT_STT_MIC=1, meant as "audio device 1", opens the
// camera and dies on an unsupported framerate — an error that names nothing the
// operator did. Nobody points a microphone variable at a camera, so a
// colon-less value can only have been meant as the audio half; supply the
// colon rather than rejecting it. A value that already carries a colon is a
// complete spec and is passed through untouched.
func normalizeMicSpec(v string) string {
	if v == "" {
		return ":default"
	}
	if strings.Contains(v, ":") {
		return v
	}
	return ":" + v
}

// streamMicFrames captures mic input via ffmpeg, slices it into 8 kHz μ-law
// 20 ms frames (160 bytes each), discards leading all-0xFF silence frames
// (bounded at 75 frames / 1500 ms), and sends each frame to conn as a Twilio
// media event. onMicWarm fires exactly once when the first real frame is emitted
// (capHit=false) or the discard cap is hit (capHit=true). rec is nil unless
// -record-sent named a file. Returns when ctx is cancelled or the connection
// closes.
func streamMicFrames(ctx context.Context, conn *websocket.Conn, streamSID string, seqNum *int, rec *streamRecorder, onMicWarm func(bool)) error {
	cmd := newFFmpegCmd(ctx, os.Getenv("AATOOLKIT_STT_MIC"))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("streamMicFrames: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("streamMicFrames: start ffmpeg (installed? `brew install ffmpeg`): %w", err)
	}

	send := mediaFrameSender(newMediaFrameEncoder(streamSID, seqNum), rec, connFrameWriter(conn))
	// Drain on context.Background(), NOT ctx: on shutdown ctx is cancelled, which
	// (via newFFmpegCmd's cmd.Cancel) sends ffmpeg a SIGINT so it flushes its buffer
	// and closes stdout. drainFrames must read that flushed tail through to EOF rather
	// than bail on the cancelled ctx — otherwise the last ~100-300ms of audio is lost.
	// Termination is EOF-driven (ffmpeg's close), bounded by cmd.WaitDelay. Each frame
	// is sent with a fresh short-lived write context (connFrameWriter) so the now-dead
	// ctx can't abort the trailing writes.
	_, drainErr := drainFramesWithDiscard(context.Background(), stdout, muLawFrame20ms, send, onMicWarm)
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
