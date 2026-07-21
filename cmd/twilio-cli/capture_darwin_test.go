//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
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

// AATK-6: the mic process must run in its OWN process group, so a terminal Ctrl-C —
// which signals twilio-cli's whole foreground group — does NOT reach ffmpeg directly.
// Otherwise ffmpeg gets two SIGINTs (the terminal's, then gracefulCancel's cmd.Cancel),
// escalates to "Immediate exit requested", and abandons the buffer flush AATK-2 relies
// on — reintroducing the tail truncation. Verified against a real child: its process
// group must be distinct from this test's, and be its own (it is the group leader).
func TestGracefulCancel_IsolatesFromCallerProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	gracefulCancel(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	childPgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid(child %d): %v", cmd.Process.Pid, err)
	}
	ownPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("getpgid(self): %v", err)
	}
	if childPgid == ownPgid {
		t.Errorf("child shares the caller's process group (pgid %d): a terminal Ctrl-C would SIGINT ffmpeg directly, and gracefulCancel's cmd.Cancel would then be a second SIGINT — ffmpeg escalates to \"Immediate exit requested\" and abandons the buffer flush", childPgid)
	}
	if childPgid != cmd.Process.Pid {
		t.Errorf("child pgid %d is not its own pid %d — expected a new process-group leader", childPgid, cmd.Process.Pid)
	}
}
