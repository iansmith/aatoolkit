package observe_test

// AATK-87: internal/observe's own comment on TreeObservation.Degraded says
// "the exhaustive-set contract Classify relies on only holds for what was
// actually observed" — but that vocabulary is never reached when
// gopsnet.ConnectionsPid succeeds while returning zero LISTEN rows.
// treeListenSet's `len(ports) == 0 { continue }` arm then drops the process
// silently: no Holder, no Degraded entry. "I could not enumerate this
// process's sockets right now" and "this process is genuinely listening on
// nothing" collapse into the same value.
//
// observe.ConnectionsPidHook (added for these tests — see its doc comment
// in observe.go) is the seam that makes the ambiguous case producible on
// demand: a live subprocess alone cannot be made to return "the call
// succeeded but is wrong," because a real listener genuinely reports its
// ports and a real non-listener genuinely reports none. The forced-empty
// test below spawns a fixture that really is listening (proven by a real,
// unmocked observation first) and then lies about it through the hook — so
// the resulting Degraded assertion is pinned against a provably wrong
// empty read, not a coincidentally-correct one. The paired test below uses
// no hook at all: a real, alive process that genuinely binds nothing, read
// through the real gopsutil call, must NOT end up in Degraded — otherwise
// the two cases the ticket requires stay indistinguishable, just via a
// different mechanism (everything conservatively marked Degraded).

import (
	"os/exec"
	"testing"
	"time"

	gopsnet "github.com/shirou/gopsutil/v4/net"

	"github.com/iansmith/aatoolkit/internal/observe"
)

// slopstop:test contract
func TestTreeListenSet_ForcedEmptyReadOnConfirmedListener_RecordsDegraded(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	// Prove the process really is listening, via the real (unmocked)
	// observation, before forcing the ambiguous read below — otherwise the
	// forced read might just coincidentally match reality instead of
	// provably contradicting it.
	waitForListenSet(t, observe.TreeListenSet, sl.pid, port, 3*time.Second)

	orig := observe.ConnectionsPidHook
	observe.ConnectionsPidHook = func(kind string, pid int32) ([]gopsnet.ConnectionStat, error) {
		return nil, nil // succeeded, found nothing — the AATK-87 shape
	}
	t.Cleanup(func() { observe.ConnectionsPidHook = orig })

	obs, err := observe.TreeListenSet(sl.pid)
	if err != nil {
		t.Fatalf("TreeListenSet: %v", err)
	}

	degraded := false
	for _, pid := range obs.Degraded {
		if pid == sl.pid {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("Degraded = %v, want it to contain pid %d — a successful-but-empty read on a process just confirmed listening must not be silently treated as a confirmed-empty listen-set", obs.Degraded, sl.pid)
	}
	if _, ok := obs.Holders[port]; ok {
		t.Errorf("Holders still contains port %d after the forced empty read — an unconfirmed read must not also claim confident occupancy", port)
	}
}

// slopstop:test regression — guards: "distinguishably from a process confirmed to hold no ports" (ticket "Observable behaviors" §1)
func TestTreeListenSet_RealNonListeningProcess_NotDegraded(t *testing.T) {
	// A real, running process that genuinely binds no ports at all, read
	// through the real (unmocked) gopsutil call — no seam substitution here.
	// This is the "confirmed to hold no ports" bucket the ticket requires to
	// stay distinguishable from the forced-ambiguous case above; a fix that
	// "solves" AATK-87 by marking every zero-row read Degraded would pass
	// the contract test above but fail this one.
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting harmless non-listening subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pid := int32(cmd.Process.Pid)

	obs, err := observe.TreeListenSet(pid)
	if err != nil {
		t.Fatalf("TreeListenSet: %v", err)
	}
	for _, p := range obs.Degraded {
		if p == pid {
			t.Fatalf("Degraded = %v, want it NOT to contain pid %d — a process genuinely confirmed to hold no ports must not be conflated with an unconfirmed read", obs.Degraded, pid)
		}
	}
	if len(obs.Holders) != 0 {
		t.Errorf("Holders = %v, want empty for a process that binds nothing", obs.Holders)
	}
}
