package observe_test

import (
	"bufio"
	"fmt"
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

// buildListenerOnce compiles the testdata/listener fixture exactly once per
// test binary run and caches the resulting path.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func listenerBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		// t.TempDir() is per-test and gets removed at test end, which would
		// delete the shared binary out from under other tests; build into a
		// package-level temp dir instead.
		root, err := os.MkdirTemp("", "observe-listener-bin")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(root, "listener")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/listener")
		cmd.Dir = "."
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build listener fixture: %w\n%s", err, output)
			return
		}
		binPath = out
	})
	if buildErr != nil {
		t.Fatalf("listenerBinary: %v", buildErr)
	}
	return binPath
}

// freePort asks the OS for an unused TCP port by binding to :0 and closing
// immediately.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// spawnedListener is a running testdata/listener fixture process, started as
// a real child via exec.Cmd (satisfying "unit-testable against processes the
// test spawns itself").
type spawnedListener struct {
	cmd  *exec.Cmd
	pid  int32
	port int
}

// spawnListener starts the fixture listening on one port. If childPort is
// non-zero, the fixture also spawns its own child listening on childPort,
// producing a two-level process tree for tree-walk tests.
func spawnListener(t *testing.T, port int, childPort int) *spawnedListener {
	t.Helper()
	return spawnListenerTree(t, port, childPort, 0, false)
}

// spawnListenerTree is spawnListener plus two optional extras:
//   - grandchildPort (requires childPort non-zero): the spawned child itself
//     spawns a grandchild listening on grandchildPort, producing a
//     three-level process tree (root -> child -> grandchild) — the shape a
//     real uvicorn-with-workers or mlx-launcher-with-subprocess tree can
//     take.
//   - transientChild: the root also spawns a short-lived, non-listening
//     child that exits ~50ms after starting, to race the tree walk against
//     a process that may have already exited by query time.
func spawnListenerTree(t *testing.T, port, childPort, grandchildPort int, transientChild bool) *spawnedListener {
	t.Helper()
	bin := listenerBinary(t)

	args := []string{"-ports", strconv.Itoa(port)}
	if childPort != 0 {
		args = append(args, "-child-ports", strconv.Itoa(childPort))
	}
	if grandchildPort != 0 {
		args = append(args, "-grandchild-ports", strconv.Itoa(grandchildPort))
	}
	if transientChild {
		args = append(args, "-transient-child")
	}
	cmd := exec.Command(bin, args...)
	// Own process group so the test can group-kill without touching the
	// test binary's own group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start listener fixture: %v", err)
	}

	sl := &spawnedListener{cmd: cmd, pid: int32(cmd.Process.Pid), port: port}

	// Wait for the fixture's readiness line so the test doesn't race the
	// listener's bind (and, if a child was requested, the child's own
	// bind — the parent only prints "ready" after starting the child, but
	// the child's own bind happens asynchronously, so callers that need the
	// child port ready should additionally poll waitForPort).
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
		t.Fatalf("listener fixture pid %d never signaled ready", sl.pid)
	}

	t.Cleanup(func() { sl.kill(t) })
	return sl
}

func (sl *spawnedListener) kill(t *testing.T) {
	t.Helper()
	if sl.cmd.Process == nil {
		return
	}
	// Negative PID signals the whole process group (parent + any spawned
	// child), so fixtures with a child listener are fully cleaned up.
	_ = syscall.Kill(-int(sl.pid), syscall.SIGKILL)
	_, _ = sl.cmd.Process.Wait()
}

// notStartedCmd returns an *exec.Cmd that has been constructed but never
// Start()'d, so cmd.Process is nil — the shape NewOursIdentity must reject.
func notStartedCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	return exec.Command("true")
}

// waitForListenSet polls query(rootPID) until wantPort shows up in the
// returned TreeObservation, or fails the test after timeout. Needed because a
// freshly spawned process (especially a child spawned by the fixture itself)
// may not have completed its bind() by the time the parent's readiness line
// is observed. query is observe.ListenSet or observe.TreeListenSet.
func waitForListenSet(t *testing.T, query func(int32) (observe.TreeObservation, error), rootPID int32, wantPort int, timeout time.Duration) observe.TreeObservation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		obs, err := query(rootPID)
		if err != nil {
			lastErr = err
		} else if _, ok := obs.Holders[wantPort]; ok {
			return obs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d not observed within %s (last error: %v)", wantPort, timeout, lastErr)
	return observe.TreeObservation{}
}
