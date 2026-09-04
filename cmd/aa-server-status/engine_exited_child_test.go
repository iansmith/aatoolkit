package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
)

// AATK-106. Nothing reaped a child that exited on its own, so the ownership
// helper kept answering "ours" for a PID that was gone and `up` on a
// crashed server took the already-ours branch — printing ok and launching
// nothing, while Status (which observes ports, not the registry) correctly
// says down. The tests below pin the two verbs to the same truth.

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
// It no longer discriminates the s.Detached guard in ownedPIDLocked, and the
// comment here used to claim it did. That stopped being true when the fleet
// loop's gate moved from liveness to the registration (review round 2, F2):
// the server now routes to downOne either way, so deleting the guard leaves
// this test green. Verified by mutation, not assumed —
// Up_DetachedLauncherExited_StaysIdempotent and
// OwnedPID_ExitedLauncher_DependsOnDetached are what catch that mutant now.
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
			// Either way the entry survives the read: TeardownAll walks
			// e.procs for the PIDs it group-kills, and a dead leader's PID
			// is the only handle on an orphan left in its process group.
			if !stillRegistered {
				t.Fatalf("ownedPIDLocked pruned the registry entry; TeardownAll needs it to group-kill orphans")
			}
		})
	}
}

// orphanLeaderServer launches a leader that backgrounds a long-lived child
// and exits immediately, leaving a live orphan in the leader's process group
// — the shape a real crash leaves behind, and the one a PID-registry prune
// can silently strand. The orphan holds no declared port, so nothing in the
// host scan can find it: the registered leader PID is the only handle on it.
func orphanLeaderServer(t *testing.T, name string) config.Server {
	t.Helper()
	return config.Server{
		Name:    name,
		Type:    config.TypeExec,
		Enabled: true,
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 300 & exit 0"},
	}
}

// launchOrphanLeader launches s through the engine's real launch path,
// registers it, and returns once the leader has exited with its orphan still
// running. The returned pid is the leader's, which is also the process-GROUP
// id the orphan belongs to.
func launchOrphanLeader(t *testing.T, eng *RealEngine, s config.Server) int32 {
	t.Helper()
	proc, err := eng.launch(s)
	if err != nil {
		t.Fatalf("launching orphan-leader fixture: %v", err)
	}
	eng.mu.Lock()
	eng.procs[s.Name] = proc
	eng.mu.Unlock()

	pid := int32(proc.Cmd.Process.Pid)

	// Sweep the group however the test ends, registered before anything
	// below can call t.Fatalf. Without this a test that fails — including
	// the waitForExited timeout immediately after — leaves the orphan
	// running for its full five minutes, and the strays accumulate across
	// runs of the very suite that exists to catch stray processes.
	t.Cleanup(func() { _ = syscall.Kill(-int(pid), syscall.SIGKILL) })

	waitForExited(t, proc, s.Name)

	if !processGroupAlive(pid) {
		t.Fatalf("fixture did not leave an orphan alive in pid group %d", pid)
	}
	return pid
}

// processGroupAlive reports whether any process remains in the group led by
// pid. Signal 0 against a negative pid asks about the whole group, which is
// what makes it able to see an orphan whose leader is already reaped.
func processGroupAlive(pid int32) bool {
	return syscall.Kill(-int(pid), 0) != syscall.ESRCH
}

// AATK-106 review round 2, F1. `down` on a server whose leader crashed must
// still sweep the process group before forgetting the registration.
//
// The registered PID is the group id, and the group outlives its leader, so
// it is the only handle on an orphan the crash left running — one holding no
// declared port cannot be found by the host scan `down` falls back to. Before
// this test the branch deleted the entry and returned ok, stranding the
// orphan for the rest of the session: TeardownAll walks e.procs, so quit
// could not reach it either. That was a regression against master, which
// swept the group via the isOurs branch's teardownOne.
func TestRealEngine_Down_AfterLeaderCrash_SweepsOrphanedProcessGroup(t *testing.T) {
	s := orphanLeaderServer(t, "svc")
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	pid := launchOrphanLeader(t, eng, s)

	if err := eng.Down("svc"); err != nil {
		t.Fatalf("Down after a leader crash: %v", err)
	}

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("the orphan in pid group %d to be swept by down", pid),
		func() bool { return !processGroupAlive(pid) })

	eng.mu.Lock()
	_, still := eng.procs["svc"]
	eng.mu.Unlock()
	if still {
		t.Fatalf("down swept the group but left the registry entry behind")
	}
}

// AATK-106 review round 2, F2. Bare `down` (no server named) must not skip an
// enabled server whose child exited.
//
// downOrDead's fleet loop gates on the registration rather than on liveness
// precisely because of this case: an exited child is not "ours", so a
// liveness gate matches neither that arm nor the !s.Enabled stray arm, and
// the crashed server the operator is trying to clean up is passed over in
// silence — orphan still running, entry still registered.
func TestRealEngine_DownAll_AfterLeaderCrash_DoesNotSkipEnabledServer(t *testing.T) {
	s := orphanLeaderServer(t, "svc")
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	pid := launchOrphanLeader(t, eng, s)

	if err := eng.Down(""); err != nil {
		t.Fatalf("bare Down after a leader crash: %v", err)
	}

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("the orphan in pid group %d to be swept by bare down", pid),
		func() bool { return !processGroupAlive(pid) })

	eng.mu.Lock()
	_, still := eng.procs["svc"]
	eng.mu.Unlock()
	if still {
		t.Fatalf("bare down passed over an enabled server whose child had exited")
	}
}

// AATK-106 review round 2, F8. The other half of the detached exception's
// justification: `up` on a detached server whose launcher has exited must
// stay the idempotent skip, not fall through to the port-conflict gate.
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

// AATK-106 review round 3, finding 1. `dead` must sweep the orphaned process
// group of a DISABLED server whose registered child exited.
//
// This is the same defect as round 2's F1, one branch over. downOrDead's
// disabled-server arm reaches killForeignOrOwned through observedRunningPID,
// which asks the registry for the PID to signal — so making that lookup
// liveness-aware silently dropped the sweep for every disabled server, while
// the enabled arm kept it. The lookup is structural for exactly this reason:
// the registered PID is the group id, and an orphan holding no declared port
// cannot be found any other way.
func TestRealEngine_Dead_DisabledServerAfterLeaderCrash_SweepsOrphanedGroup(t *testing.T) {
	s := orphanLeaderServer(t, "svc")
	s.Enabled = false
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	pid := launchOrphanLeader(t, eng, s)

	if err := eng.Dead(""); err != nil {
		t.Fatalf("Dead on a disabled server after a leader crash: %v", err)
	}

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("the orphan in pid group %d to be swept by dead", pid),
		func() bool { return !processGroupAlive(pid) })

	eng.mu.Lock()
	_, still := eng.procs["svc"]
	eng.mu.Unlock()
	if still {
		t.Fatalf("dead swept the group but left the registry entry behind")
	}
}

// AATK-106 review round 4, finding 1. `up` on a crashed server must sweep the
// dead child's process group before launching its replacement.
//
// The launch overwrites e.procs[name] with the new child's PID, and the old
// PID was the group id. After the overwrite nothing can reach an orphan the
// crash left — TeardownAll walks the registry, so not even quit gets it — and
// an orphan holding no declared port is invisible to every port-based path.
//
// registerProc now retires the entry it replaces, so the overwrite cannot
// lose the handle. This test covers the OTHER reason upOne retires early: the
// sweep has to precede checkPortConflict, or an orphan still holding a
// declared port makes `up` refuse to relaunch the server whose own remains
// are holding it.
//
// Master did not have this hole, and got there by being wrong in the other
// direction: `up` treated the dead child as ours and skipped, so the entry
// survived and quit swept it. Making `up` correct is what put the leak in.
func TestRealEngine_Up_AfterLeaderCrash_SweepsOrphanBeforeRelaunching(t *testing.T) {
	s := orphanLeaderServer(t, "svc")
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	oldPID := launchOrphanLeader(t, eng, s)

	// Registered before Up, not after: Up can fail with a child already
	// launched and registered, and the t.Cleanup below is only reached if
	// Up returns nil.
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up after a leader crash: %v", err)
	}

	newPID := engineChildPID(t, eng, "svc")
	if newPID == 0 || newPID == oldPID {
		t.Fatalf("Up should have relaunched; registry holds pid %d (crashed leader was %d)", newPID, oldPID)
	}
	t.Cleanup(func() { _ = syscall.Kill(-int(newPID), syscall.SIGKILL) })

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("the orphan in the crashed leader's pid group %d to be swept by up", oldPID),
		func() bool { return !processGroupAlive(oldPID) })
}

// AATK-106, option A. The invariant that closes the whole defect class:
// every write to e.procs goes through registerProc, which retires what it
// replaces.
//
// This is a source-level guard, and it is deliberately that rather than a
// behavioral one. The class was found four times by four review rounds, each
// time as one site that overwrote or dropped a registration without sweeping
// the process group its PID led. No behavioral test can cover the site nobody
// thought of — but every one of them had to write to the map, so that is
// where the check belongs. A new direct assignment fails this immediately,
// with a pointer to the function to use instead.
func TestEngine_EveryRegistryWriteGoesThroughRegisterProc(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("reading engine.go: %v", err)
	}

	assign := regexp.MustCompile(`e\.procs\[[^\]]*\]\s*=`)
	var offenders []string
	for i, line := range strings.Split(string(src), "\n") {
		if assign.MatchString(line) {
			offenders = append(offenders, fmt.Sprintf("engine.go:%d: %s", i+1, strings.TrimSpace(line)))
		}
	}

	// Exactly one: registerProc's own. Everything else must call it.
	if len(offenders) != 1 {
		t.Fatalf("e.procs must be assigned in registerProc and nowhere else — found %d assignments:\n  %s\n\n"+
			"A direct assignment silently discards the registration it replaces. The registered PID is the\n"+
			"process-GROUP id of the child we launched, and it is the last handle on any orphan that child\n"+
			"left behind; overwriting it strands that orphan for the rest of the session. Call registerProc.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	if !strings.Contains(offenders[0], "e.procs[s.Name] = proc") {
		t.Fatalf("the one permitted assignment is not the one expected (registerProc's): %s", offenders[0])
	}
}

// AATK-106, option A. registerProc retires the entry it replaces — the
// mechanism the guard test above protects, asserted behaviorally.
//
// Round 5 found the case this covers: upOne decides the child is ours, then
// rebuildIfStaleOwned's Stop asks the liveness question a second time, gets
// "not ours" because the child died in between, tears nothing down, and lets
// Start overwrite the handle. Retiring at the point of the write closes that
// without every caller having to remember the race exists.
func TestRealEngine_RegisterProc_RetiresTheEntryItReplaces(t *testing.T) {
	s := orphanLeaderServer(t, "svc")
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}, nil, nil)
	oldPID := launchOrphanLeader(t, eng, s)

	// A second child for the same server, registered the way every launch
	// path does it.
	replacement, err := eng.launch(s)
	if err != nil {
		t.Fatalf("launching the replacement: %v", err)
	}
	replacementPID := int32(replacement.Cmd.Process.Pid)
	t.Cleanup(func() { _ = syscall.Kill(-int(replacementPID), syscall.SIGKILL) })

	if err := eng.registerProc(s, replacement); err != nil {
		t.Fatalf("registerProc: %v", err)
	}

	waitForCondition(t, 5*time.Second,
		fmt.Sprintf("the replaced registration's pid group %d to be swept", oldPID),
		func() bool { return !processGroupAlive(oldPID) })

	if got := engineChildPID(t, eng, "svc"); got != replacementPID {
		t.Fatalf("registry holds pid %d, want the replacement %d", got, replacementPID)
	}
}
