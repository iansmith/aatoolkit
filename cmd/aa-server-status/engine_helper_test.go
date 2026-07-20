package main

import (
	"bufio"
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
	bin := tdlistenerBinary(t)

	cmd := exec.Command(bin, "-port", strconv.Itoa(port))
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
