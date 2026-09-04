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
	waitForPortRelease(t, port)

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateDown {
		t.Fatalf("expected a tracked child whose process has genuinely exited to still report down, got %+v", statuses)
	}
}

// AATK-87 F5: this used to be appended to
// TestStatusForLocked_TrackedButGenuinelyDeadChild_StillReportsDown, whose
// own t.Fatalf on the dead-child assertion would mask this second, unrelated
// scenario if it ever failed, and whose name no longer described half the
// body. Split out as its own test with its own name.
//
// AATK-87 vacuity repair (stage 9): the dead-child assertion holds at base
// too — a process that was launched, killed, and reaped reports down
// whether or not this ticket's corroboration logic exists — so on its own
// that scenario pinned nothing against base. This is the missing positive
// half: a separate, LIVE tracked child whose per-pid read comes back empty
// (the unconfirmed-empty shape AATK-87 exists to fix) must NOT be rendered
// down, and its PID must still be reported. At base, this exact shape
// renders State: down, PID: 0 — precisely the defect this ticket fixes.
func TestStatusForLocked_ForcedUnconfirmedEmptyRead_KeepsTrackedPIDAndNotDown(t *testing.T) {
	port2 := freeTestPort(t)
	f2 := spawnForeignListener(t, port2)

	cfg2 := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    port2,
		}},
	}
	eng2 := NewEngine(cfg2, nil, nil)
	eng2.procs["svc"] = &lifecycle.Process{Cmd: f2.cmd, LogPath: "live-child-forced-empty.log"}

	// Prove the real observation genuinely sees the port before lying about
	// it, so the forced read is provably wrong rather than coincidentally
	// right.
	deadline2 := time.Now().Add(3 * time.Second)
	var sawPort2 bool
	for time.Now().Before(deadline2) {
		obs, err := observe.TreeListenSet(f2.pid)
		if err == nil {
			if _, ok := obs.Holders[port2]; ok {
				sawPort2 = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawPort2 {
		t.Fatalf("precondition: pid %d never observed listening on port %d", f2.pid, port2)
	}

	orig2 := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		if pid == f2.pid {
			return nil, nil // succeeded, found nothing — the AATK-87 shape
		}
		return orig2(kind, pid)
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = orig2 })

	statuses2 := eng2.Status()
	if len(statuses2) != 1 {
		t.Fatalf("expected exactly one status, got %+v", statuses2)
	}
	if statuses2[0].State == StateDown {
		t.Fatalf("expected a tracked LIVE child NOT to be rendered down after its per-pid read came back empty, got %+v", statuses2)
	}
	if statuses2[0].PID == 0 {
		t.Fatalf("expected the tracked live child's PID to still be reported after an unconfirmed empty read, got %+v", statuses2)
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
func TestStatusForLocked_UnrelatedTreeMemberDegraded_KeepsConfirmedStateFlagsUncertainty(t *testing.T) {
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
	// AATK-87 F2 correction: StateUp is NOT "the real classification" here —
	// the child genuinely holds an undeclared port, so the truthful
	// classification (had its read not been forced empty) would be
	// StrayPort/StateExtraListener. The forced-empty read on the child
	// ERASES that stray from class.Stray exactly as it would erase a real
	// declared listener, so it is genuinely unseen this cycle, not
	// confirmed absent. StateUp is still the right STATE to render — the
	// declared port really is confirmed listening, and asserting
	// StateExtraListener here would report an anomaly nobody actually
	// observed — but it must not render as an unqualified, anomaly-free
	// green: AnomalyDetail must be set (checked below) whenever an unrelated
	// tree member is Degraded, precisely so a stray that got hidden by an
	// ambiguous read still surfaces as "can't be ruled out" instead of
	// silently vanishing. StateUp specifically — not merely "not down": an
	// unconfirmed-observation branch would also render not-down with a PID,
	// so a weaker assertion would not catch the defect this test exists for.
	if statuses[0].State != StateUp {
		t.Fatalf("expected a server whose declared port is confirmed listening to stay %q despite an unrelated tree member being Degraded, got %+v", StateUp, statuses)
	}
	if statuses[0].PID != int(f.pid) {
		t.Fatalf("expected the tracked root pid %d to be reported, got %+v", f.pid, statuses)
	}
	if statuses[0].AnomalyDetail == "" {
		t.Fatalf("expected AnomalyDetail to be set — a stray on the Degraded child cannot be ruled out even though the declared port stays confirmed, got %+v", statuses)
	}

	// AATK-87 vacuity repair (stage 9): the assertions above hold at base
	// too — at base, treeListenSet's `len(ports) == 0 { continue }` arm
	// records nothing for an ambiguous empty read, so the corroboration
	// mechanism this test claims to be non-interfering WITH never gets a
	// chance to fire, and nothing above proves it did. Close that gap
	// directly: under the SAME forced-empty hook, call TreeListenSet again
	// and assert the child's pid actually shows up in
	// TreeObservation.Degraded. At base this is red (Degraded stays empty);
	// at HEAD the corroboration path records it.
	treeObs, err := observe.TreeListenSet(f.pid)
	if err != nil {
		t.Fatalf("observe.TreeListenSet: %v", err)
	}
	var degradedFound bool
	for _, pid := range treeObs.Degraded {
		if pid == childPID {
			degradedFound = true
			break
		}
	}
	if !degradedFound {
		t.Fatalf("expected child pid %d to appear in TreeObservation.Degraded under a forced-empty per-pid read, got Degraded=%v", childPID, treeObs.Degraded)
	}
}
