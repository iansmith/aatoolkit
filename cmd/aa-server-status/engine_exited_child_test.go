package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// AATK-106. Nothing reaps a child that exits on its own: e.procs entries are
// removed only by downOne, killForeignOrOwned, Build and TeardownAll. So
// livePIDLocked keeps answering "ours" for a PID that is gone, and `up` on a
// crashed server takes the already-ours branch — printing ok and launching
// nothing, while Status (which observes ports, not the registry) correctly
// says down. The tests below pin the two verbs to the same truth.

// killEngineChildOutOfBand kills the child RealEngine launched for name the
// way a crash does — the supervisor never asked for it, so no engine code
// path runs and the e.procs entry is left behind. It returns the PID that
// died.
//
// It does not return until that PID has been REAPED, not merely signalled:
// signal 0 succeeds against a zombie, so polling for ESRCH is what proves
// Launch's own Wait goroutine has run and the child is genuinely gone. A test
// that asserted before the reap would be racing the very transition it exists
// to observe.
func killEngineChildOutOfBand(t *testing.T, eng *RealEngine, name string) int32 {
	t.Helper()

	eng.mu.Lock()
	proc, ok := eng.procs[name]
	eng.mu.Unlock()
	if !ok || proc == nil || proc.Cmd == nil || proc.Cmd.Process == nil {
		t.Fatalf("killEngineChildOutOfBand: no launched process recorded for %q", name)
	}
	pid := int32(proc.Cmd.Process.Pid)

	// Kill the whole process group: tdlistener is launched with Setpgid, and
	// signalling only the leader would leave any tree member holding the port.
	if err := syscall.Kill(-int(pid), syscall.SIGKILL); err != nil {
		t.Fatalf("killEngineChildOutOfBand: SIGKILL pid group %d: %v", pid, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(int(pid), 0); err == syscall.ESRCH {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("killEngineChildOutOfBand: pid %d was still reapable %s after SIGKILL", pid, 5*time.Second)
	return 0
}

// waitForPortFree blocks until port is no longer listening, so a test that
// relaunches cannot fail on a port the dying child has not released yet.
func waitForPortFree(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		holders, err := observe.SystemListenSet()
		if err == nil {
			if _, held := holders[port]; !held {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d was still listening %s after the child was reaped", port, 5*time.Second)
}

// engineChildPID reports the PID currently registered for name, or 0 if the
// registry holds no usable process for it.
func engineChildPID(t *testing.T, eng *RealEngine, name string) int32 {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	proc, ok := eng.procs[name]
	if !ok || proc == nil || proc.Cmd == nil || proc.Cmd.Process == nil {
		return 0
	}
	return int32(proc.Cmd.Process.Pid)
}

// AATK-106 DoD 1 and 3: an exited child is not "ours", so `up` on it launches
// a replacement and records the new PID — and Status and up agree about the
// crashed server rather than contradicting each other.
//
// At base this fails on the relaunch assertion: upOne's livePIDLocked still
// reports the dead PID as ours, so it returns an ok outcome having launched
// nothing and the registry still holds the corpse's PID. The Status half
// passes at base (it observes ports), and is asserted here precisely because
// the disagreement between the two verbs is the defect.
func TestRealEngine_Up_AfterChildExitsOnItsOwn_LaunchesReplacement(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	oldPID := engineChildPID(t, eng, "svc")
	if oldPID == 0 {
		t.Fatalf("first Up recorded no process for svc")
	}

	deadPID := killEngineChildOutOfBand(t, eng, "svc")
	waitForPortFree(t, port)

	if statuses := eng.Status(); len(statuses) != 1 || statuses[0].State != StateDown {
		t.Fatalf("after the child exited, Status should report down, got %+v", statuses)
	}

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up after the child exited: %v", err)
	}

	newPID := engineChildPID(t, eng, "svc")
	if newPID == 0 {
		t.Fatalf("Up after the child exited recorded no process for svc")
	}
	if newPID == deadPID {
		t.Fatalf("Up after the child exited did not relaunch: registry still holds the dead pid %d", deadPID)
	}
	if err := syscall.Kill(int(newPID), 0); err != nil {
		t.Fatalf("relaunched pid %d is not alive: %v", newPID, err)
	}

	if statuses := eng.Status(); len(statuses) != 1 || statuses[0].State != StateUp {
		t.Fatalf("after relaunch, Status should report up, got %+v", statuses)
	}
	if got := eng.Status()[0].PID; int32(got) != newPID {
		t.Fatalf("Status should report the relaunched pid %d, got %d", newPID, got)
	}
}

// AATK-106 DoD 2: an exited SOURCE child must take upOne's cold-launch path,
// not the owned-stale path — rebuildIfStaleOwned's Stop callback would tear
// down a PID that is already dead.
//
// The binary is deliberately left FRESH, because that is the only shape that
// tells the two paths apart from outside. With a fresh binary
// rebuildIfStaleOwned returns handled=false and base falls straight into the
// idempotent already-up skip, launching nothing; the cold path launches. (A
// stale binary would not discriminate: base would enter rebuildIfStaleOwned,
// tear down the dead PID more or less harmlessly, and relaunch anyway.)
func TestRealEngine_Up_AfterSourceChildExits_TakesColdLaunchPath(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-source")

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerSourceServer(t, "svc", port, binPath)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	// The first Up builds the binary and launches it, leaving it fresh.
	if err := eng.Up("svc"); err != nil {
		t.Fatalf("first Up: %v", err)
	}

	deadPID := killEngineChildOutOfBand(t, eng, "svc")
	waitForPortFree(t, port)

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up after the source child exited: %v", err)
	}

	newPID := engineChildPID(t, eng, "svc")
	if newPID == 0 || newPID == deadPID {
		t.Fatalf("Up on an exited source child should cold-launch a replacement; registry holds pid %d (dead pid was %d)", newPID, deadPID)
	}
	if err := syscall.Kill(int(newPID), 0); err != nil {
		t.Fatalf("relaunched pid %d is not alive: %v", newPID, err)
	}
}

// slopstop:test non-interference — AATK-106 DoD 4. This one is GREEN at base
// and must stay green: `down` on a child that already exited is a no-op that
// clears the registry entry, which is today's manual workaround for the
// defect (`down <name>` then `<name> up` relaunches, because downOne deletes
// the entry). The fix changes what "ours" means underneath downOne, so the
// workaround is exactly the kind of behavior a fix can silently take away.
func TestRealEngine_Down_AfterChildExitsOnItsOwn_ClearsRegistryEntry(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	killEngineChildOutOfBand(t, eng, "svc")
	waitForPortFree(t, port)

	if err := eng.Down("svc"); err != nil {
		t.Fatalf("Down on an exited child should be a no-op, got: %v", err)
	}

	eng.mu.Lock()
	_, still := eng.procs["svc"]
	eng.mu.Unlock()
	if still {
		t.Fatalf("Down on an exited child left its registry entry behind")
	}
}
