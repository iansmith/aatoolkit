package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/internal/observe"
)

// buildTdlistenerOnce compiles the internal/lifecycle testdata/tdlistener
// fixture exactly once per test binary run and caches its path. RealEngine's
// tests spawn it as an "exec"-type server (command=<tdlistener path>, its own
// -port/-serve-health/-ignore-term flags stand in for a real health-checked
// server) rather than depending on mlx-serve/python/`go build ./cmd/server`
// being available in the test environment.
var (
	tdBuildOnce sync.Once
	tdBinPath   string
	tdBuildErr  error
)

func tdlistenerBinary(t *testing.T) string {
	t.Helper()
	tdBuildOnce.Do(func() {
		root, err := os.MkdirTemp("", "aa-server-status-tdlistener-bin")
		if err != nil {
			tdBuildErr = err
			return
		}
		out := filepath.Join(root, "tdlistener")
		cmd := exec.Command("go", "build", "-o", out, "../../internal/lifecycle/testdata/tdlistener")
		if output, err := cmd.CombinedOutput(); err != nil {
			tdBuildErr = err
			t.Logf("build tdlistener fixture output:\n%s", output)
			return
		}
		tdBinPath = out
	})
	if tdBuildErr != nil {
		t.Fatalf("tdlistenerBinary: %v", tdBuildErr)
	}
	return tdBinPath
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Not safe to use from t.Parallel() tests — engine
// tests in this package run sequentially (real subprocess/network fixtures),
// so a global os.Stdout swap is safe here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureStdout: pipe: %v", err)
	}
	os.Stdout = w

	fn()

	os.Stdout = orig
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("captureStdout: read: %v", err)
	}
	return string(out)
}

// freeTestPort asks the OS for an unused TCP port by binding to :0 and
// closing immediately. There is an inherent (and accepted, per the sibling
// helper in internal/lifecycle) TOCTOU race between freeing the port here
// and the real launch binding it later; in practice it's not observed to
// flake in this codebase's test suite.
func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTestPort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// foreignProc is a tdlistener instance spawned directly by the test (NOT
// through RealEngine), simulating a process our supervisor never launched —
// the "foreign holder" the §6.3 precondition gate must refuse against.
type foreignProc struct {
	cmd *exec.Cmd
	pid int32
}

func (f *foreignProc) forceKill() {
	if f.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-int(f.pid), syscall.SIGKILL)
	_, _ = f.cmd.Process.Wait()
}

// spawnForeignListener starts the tdlistener fixture bound to port, outside
// of any RealEngine instance, and waits for it to report readiness.
func spawnForeignListener(t *testing.T, port int) *foreignProc {
	t.Helper()
	return spawnForeignListenerOpts(t, port, false, 0)
}

// spawnForeignListenerIgnoringTerm is spawnForeignListener with the fixture's
// -ignore-term flag set, so the process survives SIGTERM and the teardown path
// is driven all the way to SIGKILL — the same lever internal/lifecycle's own
// failure-path tests pull (see TestTeardown_SurvivorAfterKill_IsLoudError).
// Reaching post-SIGKILL verification is the only way to exercise the
// "verified clean" failure rather than an incidental signal error against an
// already-dead group.
func spawnForeignListenerIgnoringTerm(t *testing.T, port int) *foreignProc {
	t.Helper()
	return spawnForeignListenerOpts(t, port, true, 0)
}

// spawnForeignListenerOpts is the one spawn implementation the wrappers above
// share. childPort, when non-zero, passes the fixture's -child-port flag,
// producing a real two-level process tree (parent plus a child listening on
// its own port) — the shape internal/observe's package doc names as the
// primary case (a uvicorn parent plus its workers). Added for AATK-87, whose
// engine-level tests need a tree in which one member holds a port the server
// never declared; that is the only way to construct a "declared set fully
// confirmed, yet some tree member is Degraded" observation without a seam
// over CmdlineSlice.
//
// The parent's "ready" line does not guarantee the child has bound yet, so
// callers needing the child's port must poll for it — see waitForTreePort.
func spawnForeignListenerOpts(t *testing.T, port int, ignoreTerm bool, childPort int) *foreignProc {
	t.Helper()
	bin := tdlistenerBinary(t)

	args := []string{"-port", strconv.Itoa(port)}
	if ignoreTerm {
		args = append(args, "-ignore-term")
	}
	if childPort != 0 {
		args = append(args, "-child-port", strconv.Itoa(childPort))
	}
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start foreign listener: %v", err)
	}

	f := &foreignProc{cmd: cmd, pid: int32(cmd.Process.Pid)}

	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("foreign listener pid %d never signaled ready", f.pid)
	}

	t.Cleanup(f.forceKill)
	return f
}

// spawnForeignListenerWithChild is spawnForeignListener plus the fixture's
// -child-port flag. A thin wrapper over spawnForeignListenerOpts, mirroring
// how internal/observe/helper_test.go's spawnListener wraps spawnListenerTree
// — the tree case is a parameter on the one spawn implementation, never a
// second copy of it.
func spawnForeignListenerWithChild(t *testing.T, port, childPort int) *foreignProc {
	t.Helper()
	return spawnForeignListenerOpts(t, port, false, childPort)
}

// waitForCondition polls want until it holds, or fails the test naming what
// it was waiting for. The one polling loop this package's tests share: every
// "kill something, then assert" test needs to wait for a transition it cannot
// synchronise on, and a hand-rolled deadline/sleep per test is how those
// waits drift apart in length and in whether they fail loudly or fall through
// silently on timeout.
func waitForCondition(t *testing.T, timeout time.Duration, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// waitForPortRelease blocks until port is no longer listening anywhere on the
// host, so a test that asserts "down" — or relaunches onto the same port —
// cannot pass or fail on a dying child's release lag.
func waitForPortRelease(t *testing.T, port int) {
	t.Helper()
	waitForCondition(t, 5*time.Second, fmt.Sprintf("port %d to be released", port), func() bool {
		holders, err := observe.SystemListenSet()
		if err != nil {
			return false
		}
		_, held := holders[port]
		return !held
	})
}

// waitForTreePort polls the real, unmocked TreeListenSet until wantPort shows
// up under rootPID, and returns the pid holding it. It exists so a test can
// prove a tree member genuinely holds a port before substituting a seam to
// lie about it — otherwise a forced empty read might merely coincide with
// reality instead of provably contradicting it.
func waitForTreePort(t *testing.T, rootPID int32, wantPort int, timeout time.Duration) int32 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obs, err := observe.TreeListenSet(rootPID)
		if err == nil {
			if h, ok := obs.Holders[wantPort]; ok {
				return h.Identity.PID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d never observed listening under root pid %d within %s", wantPort, rootPID, timeout)
	return 0
}
