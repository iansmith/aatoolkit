package lifecycle

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// buildTdlistenerOnce compiles the testdata/tdlistener fixture exactly once
// per test binary run and caches the resulting path — mirrors
// internal/observe/helper_test.go's buildListenerOnce.
var (
	tdBuildOnce sync.Once
	tdBinPath   string
	tdBuildErr  error
)

func tdlistenerBinary(t *testing.T) string {
	t.Helper()
	tdBuildOnce.Do(func() {
		root, err := os.MkdirTemp("", "lifecycle-tdlistener-bin")
		if err != nil {
			tdBuildErr = err
			return
		}
		out := filepath.Join(root, "tdlistener")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/tdlistener")
		cmd.Dir = "."
		if output, err := cmd.CombinedOutput(); err != nil {
			tdBuildErr = fmt.Errorf("build tdlistener fixture: %w\n%s", err, output)
			return
		}
		tdBinPath = out
	})
	if tdBuildErr != nil {
		t.Fatalf("tdlistenerBinary: %v", tdBuildErr)
	}
	return tdBinPath
}

// freeTestPort asks the OS for an unused TCP port by binding to :0 and
// closing immediately.
func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTestPort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// tdProc is a running testdata/tdlistener fixture, spawned as a real child
// (its own process group) for teardown tests to signal.
type tdProc struct {
	cmd  *exec.Cmd
	pid  int32
	port int
}

// spawnTdlistener starts the fixture bound to port. If ignoreTerm is true,
// the fixture ignores SIGTERM/SIGINT (forcing the SIGKILL branch); if
// serveHealth is true, it also answers 200 OK at /healthz on that port. A
// non-zero childPort additionally spawns a nested child listener, producing
// a two-level process tree so the group-kill can be verified against more
// than a single PID.
func spawnTdlistener(t *testing.T, port, childPort int, ignoreTerm, serveHealth bool) *tdProc {
	t.Helper()
	bin := tdlistenerBinary(t)

	args := []string{"-port", strconv.Itoa(port)}
	if childPort != 0 {
		args = append(args, "-child-port", strconv.Itoa(childPort))
	}
	if ignoreTerm {
		args = append(args, "-ignore-term")
	}
	if serveHealth {
		args = append(args, "-serve-health")
	}

	cmd := exec.Command(bin, args...)
	// Own process group — teardown targets this via negative PID, and
	// tests must not accidentally signal their own test-binary group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start tdlistener fixture: %v", err)
	}

	tp := &tdProc{cmd: cmd, pid: int32(cmd.Process.Pid), port: port}

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
		t.Fatalf("tdlistener fixture pid %d never signaled ready", tp.pid)
	}

	t.Cleanup(func() { tp.forceKill() })
	return tp
}

// forceKill unconditionally group-kills the fixture — a safety net for test
// cleanup, independent of (and in addition to) whatever the test's own
// Teardown call already did.
func (tp *tdProc) forceKill() {
	if tp.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-int(tp.pid), syscall.SIGKILL)
	_, _ = tp.cmd.Process.Wait()
}

// waitForPortFree polls until port has no LISTEN holder (per
// portListening) or fails the test after timeout.
func waitForPortFree(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portListening(port) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d still listening after %s", port, timeout)
}

// waitForListenerReady polls until port shows up as LISTEN, or fails the
// test after a timeout. Needed for a nested child listener, whose bind()
// can lag behind the parent's own "ready" line.
func waitForListenerReady(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if portListening(port) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d never came up listening within timeout", port)
}

// waitForHealthReady polls the fixture's /healthz until it answers 200, or
// fails the test after a timeout — the fixture's HTTP server starts in a
// goroutine after the readiness line, so it can lag slightly.
func waitForHealthReady(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered 200 within timeout", url)
}
