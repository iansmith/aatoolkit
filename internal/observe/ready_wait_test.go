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

// oldFixtureReadyWait is the fixed ready-wait the spawn helpers used before
// fixtureReadyTimeout replaced it.
const oldFixtureReadyWait = 5 * time.Second

// TestStartFixture_ReadyAfterOldBound_Succeeds drives the real spawn path,
// startFixture, with a child that prints ready only after oldFixtureReadyWait
// has passed. It goes red if startFixture's wait is cut back to the old 5s,
// whether by passing 5s or by an inline wait in place of fixtureReadyTimeout.
// It pins "longer than the old bound", not the exact 30s value.
func TestStartFixture_ReadyAfterOldBound_Succeeds(t *testing.T) {
	delay := oldFixtureReadyWait + time.Second
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep %d; echo ready; exec sleep 30", int(delay.Seconds())))

	start := time.Now()
	if _, err := startFixture(t, cmd); err != nil {
		t.Fatalf("startFixture rejected a fixture ready after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	if elapsed := time.Since(start); elapsed < oldFixtureReadyWait {
		t.Fatalf("premise: ready arrived after %s, inside the old %s bound", elapsed, oldFixtureReadyWait)
	}
}

// TestStartFixture_ExitWithoutReady_FailureNamesFixture checks the failure
// text the real spawn path produces carries readyWaitLabel and the exit.
func TestStartFixture_ExitWithoutReady_FailureNamesFixture(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")

	f, err := startFixture(t, cmd)
	if err == nil {
		t.Fatal("startFixture returned nil for a fixture that exited without printing ready")
	}
	want := fmt.Sprintf("%s pid %d never signaled ready: child exited (exit status 3)", readyWaitLabel, f.pid)
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message %q does not start with %q", err.Error(), want)
	}
}
