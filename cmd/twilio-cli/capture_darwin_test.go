//go:build darwin

package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// AATK-2 behavior 1: on ctx-cancel the mic process must be stopped with a graceful
// SIGINT (so ffmpeg flushes its capture buffer and closes stdout at EOF), NOT
// exec.CommandContext's default SIGKILL, which drops buffered audio and truncates the
// tail of the recording (the confirmed cause of dropped trailing words like D4's
// "done"). Verified against a real child: cancelling the context must terminate it by
// SIGINT, not SIGKILL. WaitDelay must stay set so a wedged process is still bounded.
func TestGracefulCancel_SendsSIGINTNotSIGKILL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	gracefulCancel(cmd)

	if cmd.WaitDelay != 3*time.Second {
		t.Errorf("WaitDelay = %v, want 3s (bounds the graceful drain)", cmd.WaitDelay)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	cancel() // triggers cmd.Cancel

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait: want *exec.ExitError from a signal, got %v", err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("WaitStatus unavailable: %T", exitErr.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("child terminated by %v, want SIGINT (default SIGKILL drops buffered audio)", ws.Signal())
	}
}
