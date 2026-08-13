package observe_test

// AATK-87 stage-7 gap test, from adversary round 1 finding 5 [minor,
// test-adequacy]: "Neither degraded_test.go test exercises the Degraded
// distinction inside a multi-process tree (root + child); both use single,
// childless processes." internal/observe's own package doc frames tree
// walking — "a uvicorn parent plus its worker children, or an mlx launcher
// plus its subprocess" — as the primary use case, so a Degraded mechanism
// pinned only against single childless processes is pinned against the
// easy shape and not the one the package exists for.
//
// The distinction this forces is sharper than the single-process case: an
// unconfirmed read on ONE member of a tree must degrade THAT member without
// discarding the confirmed occupancy of its siblings. A fix that answers
// AATK-87 by degrading the whole observation whenever any member reads
// empty would pass the single-process contract test and lose the root's
// real, confirmed Holder here.

import (
	"testing"
	"time"

	gopsnet "github.com/shirou/gopsutil/v4/net"

	"github.com/iansmith/aatoolkit/internal/observe"
)

// slopstop:test contract
func TestTreeListenSet_ForcedEmptyReadOnChildOnly_DegradesChildKeepsRootHolder(t *testing.T) {
	rootPort := freePort(t)
	childPort := freePort(t)
	sl := spawnListener(t, rootPort, childPort)

	// Prove BOTH ports are really held, through the real unmocked call,
	// before lying about one of them — so the forced read below provably
	// contradicts reality rather than coincidentally matching it, and so
	// the child's pid is known from the observation rather than guessed.
	obs := waitForListenSet(t, observe.TreeListenSet, sl.pid, childPort, 5*time.Second)
	if _, ok := obs.Holders[rootPort]; !ok {
		t.Fatalf("precondition: root port %d not observed listening; Holders = %v", rootPort, obs.Holders)
	}
	childPID := obs.Holders[childPort].Identity.PID
	if childPID == 0 || childPID == sl.pid {
		t.Fatalf("precondition: expected the child port %d to be held by a distinct child pid, got %d (root is %d)", childPort, childPID, sl.pid)
	}

	orig := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		if pid == childPID {
			return nil, nil // succeeded, found nothing — the AATK-87 shape, for the child only
		}
		return orig(kind, pid)
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = orig })

	got, err := observe.TreeListenSet(sl.pid)
	if err != nil {
		t.Fatalf("TreeListenSet: %v", err)
	}

	degraded := false
	for _, pid := range got.Degraded {
		if pid == childPID {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("Degraded = %v, want it to contain the child pid %d — a successful-but-empty read on a tree member just confirmed listening must not be silently treated as a confirmed-empty listen-set", got.Degraded, childPID)
	}
	if _, ok := got.Holders[rootPort]; !ok {
		t.Fatalf("Holders = %v, want the root's confirmed port %d to survive — degrading one tree member must not discard another member's confirmed occupancy", got.Holders, rootPort)
	}
	if _, ok := got.Holders[childPort]; ok {
		t.Errorf("Holders still contains the child port %d after its forced empty read — an unconfirmed read must not also claim confident occupancy", childPort)
	}
}

// slopstop:test contract
func TestTreeListenSet_HostWideCorroborationFails_DegradesRatherThanConfirmsEmpty(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	// Prove the port really is held, through the real unmocked call, before
	// lying about it below — so the forced reads provably contradict
	// reality rather than coincidentally matching it.
	obs := waitForListenSet(t, observe.TreeListenSet, sl.pid, port, 5*time.Second)
	if _, ok := obs.Holders[port]; !ok {
		t.Fatalf("precondition: port %d not observed listening; Holders = %v", port, obs.Holders)
	}

	origPid := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		if pid == sl.pid {
			return nil, nil // succeeded, found nothing — the ambiguous AATK-87 per-pid shape
		}
		return origPid(kind, pid)
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = origPid })

	origConns := observe.ConnectionsHook
	observe.ConnectionsHook = func(kind string) ([]gopsnet.ConnectionStat, error) {
		return nil, nil // the corroborating host-wide scan swallows its own failure too
	}
	t.Cleanup(func() { observe.ConnectionsHook = origConns })

	got, err := observe.TreeListenSet(sl.pid)
	if err != nil {
		t.Fatalf("TreeListenSet: %v", err)
	}

	degraded := false
	for _, pid := range got.Degraded {
		if pid == sl.pid {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("Degraded = %v, want it to contain pid %d — when the corroborating host-wide scan itself returns the ambiguous zero-row shape, the per-pid empty read must not be promoted to confirmed-empty", got.Degraded, sl.pid)
	}
	if _, ok := got.Holders[port]; ok {
		t.Errorf("Holders still contains port %d after the corroborating scan itself failed — an unconfirmed read must not be promoted to confident occupancy", port)
	}
}
