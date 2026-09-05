package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
)

// AATK-106. Nothing reaped a child that exited on its own, so the ownership
// helper kept answering "ours" for a PID that was gone and `up` on a crashed
// server took the already-ours branch — printing ok and launching nothing,
// while Status, which observes ports rather than the registry, correctly said
// down. These tests pin the two verbs to the same truth.

// killEngineChildOutOfBand kills the child RealEngine launched for name the
// way a crash does — the supervisor never asked for it, so no engine code
// path runs and the e.procs entry is left behind. It returns the PID that
// died.
//
// It does not return until the engine can actually SEE the child as gone,
// which takes two waits, not one:
//
//   - ESRCH from signal 0. Signal 0 succeeds against a zombie, so a plain
//     "did the signal land" check proves nothing; ESRCH is what proves the
//     child has been reaped.
//   - proc.Exited(). Reaping happens inside cmd.Wait()'s wait4, but the flag
//     is stored after Wait RETURNS, so there is a real window where ESRCH is
//     already observable and Exited() is still false. Waiting on ESRCH alone
//     would leave these tests passing on the timing of Wait's bookkeeping.
//
// A test that asserted before both would be racing the very transition it
// exists to observe.
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

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("pid %d to be reaped and recorded as exited after SIGKILL", pid),
		func() bool {
			return syscall.Kill(int(pid), 0) == syscall.ESRCH && proc.Exited()
		})
	return pid
}

// waitForExited blocks until proc's own Wait goroutine has recorded its exit.
// For a launcher that is EXPECTED to exit immediately (a detached server's
// `compose up -d`), there is no signal to send first — the only thing to wait
// for is the engine noticing.
func waitForExited(t *testing.T, proc *lifecycle.Process, name string) {
	t.Helper()
	waitForCondition(t, 5*time.Second, "the launcher for "+name+" to be recorded as exited", func() bool {
		return proc.Exited()
	})
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
	waitForPortRelease(t, port)

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
	waitForPortRelease(t, port)

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
	waitForPortRelease(t, port)

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

// detachedMarkerServer is a detached server whose launcher exits immediately
// — the defining shape of `docker compose up -d`, reduced to the part that
// matters here — and whose teardown_args touch markerPath, so "the teardown
// command actually ran" is observable from outside the engine.
//
// It declares no ports on purpose. teardownDetached polls until the declared
// set is free, and an empty set is free at once, which keeps the test about
// routing rather than about container timing.
func detachedMarkerServer(name, markerPath string) config.Server {
	return config.Server{
		Name:         name,
		Type:         config.TypeExec,
		Enabled:      true,
		Detached:     true,
		Command:      "/bin/sh",
		Args:         []string{"-c", "exit 0"},
		TeardownArgs: []string{"-c", "touch " + markerPath},
	}
}

// AATK-106: a detached server's launcher exits BY DESIGN, so that exit must
// not be read as death. `down` must still route it to its teardown command.
//
// What this pins is the ROUTING: bare `down` reaches downOne for a detached
// server, and downOne sends it to its teardown command rather than to a PID
// signal, even though the launcher is long gone.
//
// The fixture still has to register through the engine's real launch path:
// Exited() is only ever set by the Wait goroutine Launch starts, so a
// hand-built lifecycle.Process reports "still running" forever and cannot
// reach the state this test is about.
func TestRealEngine_Down_DetachedLauncherExited_StillRunsTeardownCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "torn-down")
	s := detachedMarkerServer("svc", marker)
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)

	proc, err := eng.launch(s)
	if err != nil {
		t.Fatalf("launching the detached fixture: %v", err)
	}
	eng.mu.Lock()
	eng.procs["svc"] = proc
	eng.mu.Unlock()

	// The launcher is gone within milliseconds; wait until the engine can
	// see that, so the assertion is about routing and not about timing.
	waitForExited(t, proc, "svc")

	if err := eng.Down(""); err != nil {
		t.Fatalf("bare Down with a detached server: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("down did not run the detached server's teardown command: %v", err)
	}
}

// AATK-106 companion to the test above: the same exited-launcher state on a
// server that is NOT detached must go the other way. Without both halves,
// ownedPIDLocked could satisfy either one by ignoring liveness or ignoring
// Detached, and the pair is what forces it to distinguish them.
func TestRealEngine_OwnedPID_ExitedLauncher_DependsOnDetached(t *testing.T) {
	for _, tc := range []struct {
		name     string
		detached bool
		wantOurs bool
	}{
		{name: "detached launcher exiting is by design", detached: true, wantOurs: true},
		{name: "supervised child exiting is death", detached: false, wantOurs: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := detachedMarkerServer("svc", filepath.Join(t.TempDir(), "unused"))
			s.Detached = tc.detached
			if !tc.detached {
				s.TeardownArgs = nil // only valid with detached = true
			}
			eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)

			proc, err := eng.launch(s)
			if err != nil {
				t.Fatalf("launching fixture: %v", err)
			}
			eng.mu.Lock()
			eng.procs["svc"] = proc
			eng.mu.Unlock()
			waitForExited(t, proc, "svc")

			eng.mu.Lock()
			_, isOurs := eng.ownedPIDLocked(s)
			_, stillRegistered := eng.procs["svc"]
			eng.mu.Unlock()

			if isOurs != tc.wantOurs {
				t.Fatalf("ownedPIDLocked ours = %v, want %v", isOurs, tc.wantOurs)
			}
			// Either way the entry survives: ownedPIDLocked is a pure
			// read. `down` is where the operator asks us to forget a
			// crashed server, and that is the one place the prune lives.
			if !stillRegistered {
				t.Fatalf("ownedPIDLocked pruned the registry entry; it must be a pure read")
			}
		})
	}
}

// AATK-106. The caller the detached exception exists for: `up` on a detached
// server whose launcher has exited must stay the idempotent skip, not fall
// through to the port-conflict gate.
//
// The foreign listener stands in for the container. Without the exception
// isOurs goes false, checkPortConflict finds the declared port held by a PID
// it has no registration for, and `up` hard-refuses to start a server that is
// already running — naming the operator's own container as a foreign holder.
func TestRealEngine_Up_DetachedLauncherExited_StaysIdempotent(t *testing.T) {
	port := freeTestPort(t)
	spawnForeignListener(t, port) // the "container" the launcher started

	s := detachedMarkerServer("svc", filepath.Join(t.TempDir(), "unused"))
	s.Port = port
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)

	proc, err := eng.launch(s)
	if err != nil {
		t.Fatalf("launching the detached fixture: %v", err)
	}
	eng.mu.Lock()
	eng.procs["svc"] = proc
	eng.mu.Unlock()
	waitForExited(t, proc, "svc")

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("up on an already-running detached server should be an idempotent skip, got: %v", err)
	}
}

// AATK-106 non-interference. `up` on a RUNNING source server whose binary has
// gone stale must still rebuild it in place — the edit-then-`up` workflow.
//
// It is here because the ownership change runs straight through this path:
// upOne asks ownedPIDLocked, and a live child must still come back "ours" so
// the rebuild happens rather than a cold launch over a running server. Green
// before and after; the declared port and health spec are what make the
// rebuild's own teardown verification real rather than vacuous.
func TestRealEngine_Up_RunningStaleSourceServer_RebuildsInPlace(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-source")
	s := tdlistenerSourceServer(t, "svc", port, binPath)
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	firstPID := engineChildPID(t, eng, "svc")

	// Stale the binary the way an edit-and-rebuild would.
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staling the binary: %v", err)
	}

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("up on a running, stale source server should rebuild in place: %v", err)
	}

	if got := engineChildPID(t, eng, "svc"); got == 0 || got == firstPID {
		t.Fatalf("rebuild should have replaced the child; registry holds %d, first was %d", got, firstPID)
	}
}
