package main

// AATK-87 stage-7 gap tests, from adversary round 1.
//
// Finding 4 [minor, test-adequacy]: the frozen contract test
// TestStatusForLocked_ForcedUnconfirmedEmptyRead_KeepsTrackedPID asserts
// only that PID != 0 and never looks at State, while Observable behavior 2
// is about not rendering the child "as down" at all. The frozen test is
// frozen, so the missing half is added here rather than by editing it.
//
// Finding 3 [major, coverage]: nothing covered isOurs == true for a process
// that has genuinely exited. e.procs entries are removed only by explicit
// down/kill/stop verbs, never automatically on process death, so
// statusForLocked really can see isOurs == true for a dead pid — and a fix
// that keeps the PID whenever isOurs is true, which is the cheapest way to
// satisfy the contract test above, would make a crashed owned child
// unreportable as down.

import (
	"errors"
	"testing"
	"time"

	gopsnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// slopstop:test contract
func TestStatusForLocked_ForcedUnconfirmedEmptyRead_NotRenderedDown(t *testing.T) {
	port := freeTestPort(t)
	f := spawnForeignListener(t, port) // real, alive, genuinely bound to `port`

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    port,
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: f.cmd, LogPath: "forced-empty-state.log"}

	// Prove the real observation genuinely sees the port before lying about
	// it, so the forced read is provably wrong rather than coincidentally
	// right.
	deadline := time.Now().Add(3 * time.Second)
	var sawPort bool
	for time.Now().Before(deadline) {
		obs, err := observe.TreeListenSet(f.pid)
		if err == nil {
			if _, ok := obs.Holders[port]; ok {
				sawPort = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawPort {
		t.Fatalf("precondition: pid %d never observed listening on port %d", f.pid, port)
	}

	orig := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		return nil, nil // succeeded, found nothing — the AATK-87 shape
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = orig })

	statuses := eng.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected exactly one status, got %+v", statuses)
	}
	// The ticket's Observable behavior 2 in full: statusForLocked "does not
	// render a tracked, live child as `down` with `PID: 0` on the strength
	// of an observation it could not confirm." Asserting only on PID leaves
	// an incoherent "PID set, State still down" patch passing. Which
	// non-down state is correct is the implementation's call; that it is
	// not `down` is the ticket's.
	if statuses[0].State == StateDown {
		t.Fatalf("expected a tracked live child NOT to be rendered down after an unconfirmed (forced-empty) observation, got %+v", statuses)
	}
}

// slopstop:test non-interference — paired with TestStatusForLocked_ForcedUnconfirmedEmptyRead_KeepsTrackedPID
// (frozen, contract). That test's positive half demands the PID survive an
// unconfirmed observation; this negative half demands the fix not buy that
// by trusting isOurs unconditionally. Guards: "A server genuinely holding
// no declared ports is still reported `down` — the fix must not make a real
// down state unreachable." (ticket Observable behaviors §4), applied to the
// owned-child case the two existing regression tests do not reach.
func TestStatusForLocked_TrackedButGenuinelyDeadChild_StillReportsDown(t *testing.T) {
	port := freeTestPort(t)
	f := spawnForeignListener(t, port)

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    port,
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: f.cmd, LogPath: "dead-child.log"}

	// Kill and reap it, leaving the e.procs entry behind — exactly the state
	// a crashed child leaves, since nothing removes that entry on death.
	f.forceKill()

	// Confirm the port really is released before asserting, so a slow
	// teardown cannot make this pass for the wrong reason.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		holders, err := observe.SystemListenSet()
		if err == nil {
			if _, held := holders[port]; !held {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateDown {
		t.Fatalf("expected a tracked child whose process has genuinely exited to still report down, got %+v", statuses)
	}
}

// AATK-87 V2 DoD item 5, the main deliverable of this Phase 0 round: "A
// tracked child whose TreeListenSet call returns an error that does NOT
// wrap process.ErrorProcessNotRunning is not rendered down, and its PID is
// reported — pinned by a test that forces exactly that error through the
// sanctioned root-process seam." Forced via observe.NewProcessHook (added
// for this ticket, mirroring observe.ConnectionsPidHook's shape and
// t.Cleanup discipline exactly) against a genuinely live, tracked root — NOT
// a bogus (<=0 or never-alive) PID, which the ticket's own file map singles
// out as producing the same error class against a process that was never
// alive, proving nothing about a live tracked child (the load-bearing
// qualifier the V1 attempt's stage-7 adversary round caught one hop over
// from here).
//
// This test does not decide where errors.Is(err, process.ErrorProcessNotRunning)
// ends up living (inline in engine.go vs. a predicate exported from
// internal/observe) or whether a new ServerState is introduced — both are
// design forks the ticket leaves to implement. It only asserts the
// statusForLocked-level outcome the ticket states: not down, and the PID
// still reported — following TestStatusForLocked_ForcedUnconfirmedEmptyRead_NotRenderedDown's
// own precedent of asserting State != StateDown rather than a specific
// replacement state.
//
// slopstop:test contract
func TestStatusForLocked_ForcedNonNotRunningRootError_NotRenderedDownKeepsTrackedPID(t *testing.T) {
	port := freeTestPort(t)
	f := spawnForeignListener(t, port) // real, alive, genuinely bound to `port`

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    port,
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: f.cmd, LogPath: "forced-transient-root-error.log"}

	// Prove the real observation genuinely sees the port before forcing the
	// root-resolution error below, so the forced failure provably
	// contradicts a real, live, tracked child rather than coincidentally
	// matching a dead one.
	deadline := time.Now().Add(3 * time.Second)
	var sawPort bool
	for time.Now().Before(deadline) {
		obs, err := observe.TreeListenSet(f.pid)
		if err == nil {
			if _, ok := obs.Holders[port]; ok {
				sawPort = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawPort {
		t.Fatalf("precondition: pid %d never observed listening on port %d", f.pid, port)
	}

	forcedErr := errors.New("AATK-87 DoD item 5: forced transient existence-check failure, neither ESRCH nor EPERM")
	orig := observe.NewProcessHook
	observe.NewProcessHook = func(pid int32) (*process.Process, error) {
		if pid == f.pid {
			return nil, forcedErr
		}
		return orig(pid)
	}
	t.Cleanup(func() { observe.NewProcessHook = orig })

	// Confirm the forced seam actually produces, at TreeListenSet's own
	// (TreeObservation, error) boundary, an error that does NOT wrap
	// process.ErrorProcessNotRunning — so a failure below is attributable to
	// statusForLocked's handling of that error, not to a misconfigured seam
	// or a coincidentally-ErrorProcessNotRunning-shaped forced error.
	if _, err := observe.TreeListenSet(f.pid); err == nil || errors.Is(err, process.ErrorProcessNotRunning) {
		t.Fatalf("precondition: forced seam did not produce a non-ErrorProcessNotRunning error from TreeListenSet, got %v", err)
	}

	statuses := eng.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected exactly one status, got %+v", statuses)
	}
	if statuses[0].State == StateDown {
		t.Fatalf("expected a tracked live child NOT to be rendered down after a forced transient (non-ErrorProcessNotRunning) root-resolution error, got %+v", statuses)
	}
	if statuses[0].PID == 0 {
		t.Fatalf("expected the tracked live child's PID to still be reported after a forced transient root-resolution error, got %+v", statuses)
	}
}

// slopstop:test non-interference — paired with TestStatusForLocked_ForcedUnconfirmedEmptyRead_KeepsTrackedPID
// and TestStatusForLocked_ForcedNonNotRunningRootError_NotRenderedDownKeepsTrackedPID
// (both contract). Those demand that an UNCONFIRMED observation stop the
// child being rendered down; this demands the fix not pay for that by keying
// on `len(class.Degraded) > 0` alone. Guards: "A server genuinely holding no
// declared ports is still reported `down` — the fix must not make a real down
// state unreachable." (ticket Observable behaviors §4), applied in the
// opposite direction — a server whose declared ports are all genuinely
// confirmed must keep its real classification even when some unrelated tree
// member is Degraded.
//
// AATK-87 stage-7 round-1 finding 2 [major, test-adequacy]: nothing covered
// `class.Actual` non-empty AND `class.Degraded` non-empty at the same time,
// and that combination is reachable in today's code — internal/observe
// appends to Degraded when a member's CmdlineSlice() fails while KEEPING its
// port in Holders. An implementer who writes
// `case isOurs && len(class.Degraded) > 0:` without also gating on
// `len(class.Actual) == 0` passes every other test in this suite and silently
// downgrades a healthy uvicorn-style tree whose worker's identity read
// blipped.
//
// The shape is built without a CmdlineSlice seam: the child holds a port the
// server never declared, so forcing only the child's read empty leaves the
// declared set fully confirmed (no Missing, and no Stray either, since the
// child's undeclared port vanishes with it) while the child itself is the
// Degraded member.
func TestStatusForLocked_UnrelatedTreeMemberDegraded_KeepsRealClassification(t *testing.T) {
	port := freeTestPort(t)
	childPort := freeTestPort(t)
	f := spawnForeignListenerWithChild(t, port, childPort)

	// Prove the tree is real on both ports through the unmocked call, and
	// learn the child's pid from the observation rather than guessing it.
	childPID := waitForTreePort(t, f.pid, childPort, 5*time.Second)
	if childPID == 0 || childPID == f.pid {
		t.Fatalf("precondition: expected child port %d to be held by a distinct child pid, got %d (root is %d)", childPort, childPID, f.pid)
	}
	_ = waitForTreePort(t, f.pid, port, 5*time.Second)

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    port, // childPort is deliberately NOT declared
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: f.cmd, LogPath: "unrelated-degraded.log"}

	orig := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		if pid == childPID {
			return nil, nil // succeeded, found nothing — unconfirmed, for the CHILD only
		}
		return orig(kind, pid)
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = orig })

	statuses := eng.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected exactly one status, got %+v", statuses)
	}
	// The declared port is genuinely confirmed listening, so the server's real
	// classification must survive untouched. StateUp specifically — not merely
	// "not down": an unconfirmed-observation branch would also render
	// not-down with a PID, so a weaker assertion would not catch the defect
	// this test exists for.
	if statuses[0].State != StateUp {
		t.Fatalf("expected a server whose declared port is confirmed listening to stay %q despite an unrelated tree member being Degraded, got %+v", StateUp, statuses)
	}
	if statuses[0].PID != int(f.pid) {
		t.Fatalf("expected the tracked root pid %d to be reported, got %+v", f.pid, statuses)
	}
}
