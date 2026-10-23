package observe_test

import (
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readyWaitLabel is the label the observe package's spawn helper passes to
// awaitReady; the failure text is "<label> pid N never signaled ready...".
const readyWaitLabel = "listener fixture"

// readyWaitGuard is a fixed anti-hang limit for the tests below. It is far
// above every injected bound, so it only fires when awaitReady fails to
// return at all; it is not a timing assertion on a healthy run.
const readyWaitGuard = 30 * time.Second

// startTestChild starts a plain command in its own process group with its
// stdout piped, binding no port, and group-kills it on cleanup.
func startTestChild(t *testing.T, name string, args ...string) (*exec.Cmd, io.Reader) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd, stdout
}

// awaitReadyGuarded runs awaitReady and fails the test by name if it does not
// return within readyWaitGuard.
func awaitReadyGuarded(t *testing.T, cmd *exec.Cmd, stdout io.Reader, bound time.Duration) (error, time.Duration) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
	}
	res := make(chan result, 1)
	go func() {
		start := time.Now()
		err := awaitReady(readyWaitLabel, cmd, stdout, bound)
		res <- result{err, time.Since(start)}
	}()
	select {
	case r := <-res:
		return r.err, r.elapsed
	case <-time.After(readyWaitGuard):
		t.Fatalf("awaitReady with bound %s did not return within %s", bound, readyWaitGuard)
		return nil, 0
	}
}

func TestAwaitReady_SilentLiveChild_FailsAtBoundNamingItself(t *testing.T) {
	const bound = 300 * time.Millisecond
	cmd, stdout := startTestChild(t, "sleep", "30")

	err, elapsed := awaitReadyGuarded(t, cmd, stdout, bound)
	if err == nil {
		t.Fatal("awaitReady returned nil for a child that never prints ready")
	}
	if elapsed < bound {
		t.Errorf("returned after %s, before the %s bound", elapsed, bound)
	}
	msg := err.Error()
	prefix := fmt.Sprintf("%s pid %d never signaled ready", readyWaitLabel, cmd.Process.Pid)
	if !strings.Contains(msg, prefix) {
		t.Errorf("message %q does not contain %q", msg, prefix)
	}
	if !regexp.MustCompile(`child still running after [0-9.]+(ms|s)`).MatchString(msg) {
		t.Errorf("message %q does not report a still-running child and the elapsed wait", msg)
	}
	if strings.Contains(msg, "exited") {
		t.Errorf("message %q claims an exit for a live child", msg)
	}
}

func TestAwaitReady_ChildExitsWithoutReady_ReportsExit(t *testing.T) {
	cmd, stdout := startTestChild(t, "sh", "-c", "exit 3")

	err, _ := awaitReadyGuarded(t, cmd, stdout, 20*time.Second)
	if err == nil {
		t.Fatal("awaitReady returned nil for a child that exited without printing ready")
	}
	msg := err.Error()
	prefix := fmt.Sprintf("%s pid %d never signaled ready", readyWaitLabel, cmd.Process.Pid)
	if !strings.Contains(msg, prefix) {
		t.Errorf("message %q does not contain %q", msg, prefix)
	}
	if !regexp.MustCompile(`child exited \(exit status 3\) after [0-9.]+(ms|s)`).MatchString(msg) {
		t.Errorf("message %q does not report the exit status and the elapsed wait", msg)
	}
	if strings.Contains(msg, "still running") {
		t.Errorf("message %q calls an exited child still running", msg)
	}
}

// TestAwaitReady_ReadyLine_ReturnsNil is the premise check for the two tests
// above: awaitReady does return nil when the exact line "ready" arrives, after
// a different line, so those tests cannot pass merely because it always errs.
func TestAwaitReady_ReadyLine_ReturnsNil(t *testing.T) {
	cmd, stdout := startTestChild(t, "sh", "-c", "echo noise; echo ready; sleep 30")

	if err, _ := awaitReadyGuarded(t, cmd, stdout, 20*time.Second); err != nil {
		t.Fatalf("awaitReady: %v", err)
	}
}
