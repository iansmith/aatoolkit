package observe_test

import (
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/internal/observe"
)

// --- Identity ---------------------------------------------------------

func TestNewOursIdentity_SpawnedChild_MarkedOurs(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	ident, err := observe.NewOursIdentity(sl.cmd)
	if err != nil {
		t.Fatalf("NewOursIdentity: %v", err)
	}
	if !ident.Ours {
		t.Errorf("Ours = false, want true for a process we hold the *exec.Cmd for")
	}
	if ident.PID != sl.pid {
		t.Errorf("PID = %d, want %d", ident.PID, sl.pid)
	}
	if len(ident.Cmdline) == 0 {
		t.Errorf("Cmdline is empty, want the fixture's argv")
	}
}

func TestNewOursIdentity_NilCmd_Errors(t *testing.T) {
	if _, err := observe.NewOursIdentity(nil); err == nil {
		t.Error("NewOursIdentity(nil) succeeded, want error")
	}
}

func TestNewOursIdentity_NotStarted_Errors(t *testing.T) {
	// An *exec.Cmd that has been constructed but never Start()'d has a nil
	// Process — must be rejected rather than panicking or silently
	// succeeding.
	cmd := notStartedCmd(t)
	if _, err := observe.NewOursIdentity(cmd); err == nil {
		t.Error("NewOursIdentity(not-started cmd) succeeded, want error")
	}
}

func TestNewForeignIdentity_MatchedViaCmdline(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	// The test did NOT construct this Identity via the *exec.Cmd handle —
	// simulating a process aa-server-status did not spawn but observes by PID
	// (e.g. discovered as a foreign listener on a needed port).
	ident, err := observe.NewForeignIdentity(sl.pid)
	if err != nil {
		t.Fatalf("NewForeignIdentity: %v", err)
	}
	if ident.Ours {
		t.Errorf("Ours = true, want false for a foreign-matched identity")
	}
	if ident.PID != sl.pid {
		t.Errorf("PID = %d, want %d", ident.PID, sl.pid)
	}
	if len(ident.Cmdline) == 0 {
		t.Errorf("Cmdline is empty, want the fixture's argv (identity match basis)")
	}
}

func TestNewForeignIdentity_NonexistentPID_Errors(t *testing.T) {
	// A PID that (almost certainly) does not exist. Using a very large PID
	// value avoids relying on any specific never-used PID guarantee.
	const bogusPID = int32(1<<31 - 2)
	if _, err := observe.NewForeignIdentity(bogusPID); err == nil {
		t.Error("NewForeignIdentity(bogus pid) succeeded, want error")
	}
}

// --- Listen-set gathering across a process tree ------------------------

func TestTreeListenSet_SingleProcess_FindsOwnPort(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	got := waitForListenSet(t, observe.TreeListenSet, sl.pid, port, 3*time.Second)

	holder, ok := got.Holders[port]
	if !ok {
		t.Fatalf("port %d not in listen-set %v", port, got.Holders)
	}
	if !holder.Identity.Ours {
		t.Errorf("holder.Identity.Ours = false, want true (TreeListenSet marks the whole rooted tree as ours)")
	}
	if holder.Identity.PID != sl.pid {
		t.Errorf("holder PID = %d, want %d", holder.Identity.PID, sl.pid)
	}
}

func TestTreeListenSet_WithChildProcess_UnionsChildPorts(t *testing.T) {
	parentPort := freePort(t)
	childPort := freePort(t)
	sl := spawnListener(t, parentPort, childPort)

	got := waitForListenSet(t, observe.TreeListenSet, sl.pid, childPort, 5*time.Second)

	if _, ok := got.Holders[parentPort]; !ok {
		t.Errorf("parent port %d missing from tree listen-set %v", parentPort, got.Holders)
	}
	childHolder, ok := got.Holders[childPort]
	if !ok {
		t.Fatalf("child port %d missing from tree listen-set %v — child-process ports must be unioned in (uvicorn workers / mlx subprocess case)", childPort, got.Holders)
	}
	if childHolder.Identity.PID == sl.pid {
		t.Errorf("child port %d attributed to parent pid %d, want the child's own pid", childPort, sl.pid)
	}
	if !childHolder.Identity.Ours {
		t.Errorf("child holder Ours = false, want true — descendants of an our-child root are part of that server's tree")
	}
}

func TestTreeListenSet_ThreeLevelTree_FindsGrandchildPort(t *testing.T) {
	// Adversary gap: prior tests only covered a two-level tree (root +
	// direct child). uvicorn workers and mlx subprocesses can nest deeper
	// than one level, so the walk must recurse past direct children.
	rootPort := freePort(t)
	childPort := freePort(t)
	grandchildPort := freePort(t)
	sl := spawnListenerTree(t, rootPort, childPort, grandchildPort, false)

	got := waitForListenSet(t, observe.TreeListenSet, sl.pid, grandchildPort, 5*time.Second)

	grandchildHolder, ok := got.Holders[grandchildPort]
	if !ok {
		t.Fatalf("grandchild port %d missing from tree listen-set %v — walk must recurse past direct children", grandchildPort, got.Holders)
	}
	if !grandchildHolder.Identity.Ours {
		t.Errorf("grandchild holder Ours = false, want true — the whole tree under an our-child root is ours, at any depth")
	}
	if grandchildHolder.Identity.PID == sl.pid {
		t.Errorf("grandchild port %d attributed to root pid %d, want the grandchild's own pid", grandchildPort, sl.pid)
	}
}

func TestClassify_StrayAndForeignHolderTogether(t *testing.T) {
	// Adversary gap: no prior test combined a stray port anomaly with a
	// foreign-holder-of-a-needed-port anomaly in the same observation. Both
	// must be reported simultaneously — they are independent findings, not
	// mutually exclusive.
	needed := freePort(t)
	sl := spawnListener(t, needed, 0)

	obs := waitForListenSet(t, observe.ListenSet, sl.pid, needed, 3*time.Second)

	// Inject a stray port (not declared, not related to the foreign holder)
	// into the observed set to simulate a second, independent anomaly.
	strayPort := freePort(t)
	obs.Holders[strayPort] = observe.Holder{
		Port:     strayPort,
		Identity: observe.Identity{PID: 999999, Ours: true, Cmdline: []string{"stray"}},
	}

	result := observe.Classify([]int{needed}, obs)

	if result.Classification != observe.StrayPort {
		t.Errorf("Classification = %v, want StrayPort", result.Classification)
	}
	holder, ok := result.ForeignHolders[needed]
	if !ok {
		t.Fatalf("ForeignHolders missing needed port %d even though a separate stray port also exists — the two anomalies must not suppress each other", needed)
	}
	if holder.PID != sl.pid {
		t.Errorf("ForeignHolders[%d].PID = %d, want %d", needed, holder.PID, sl.pid)
	}
}

func TestListenSet_PlainVariant_DoesNotMarkOurs(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	got := waitForListenSet(t, observe.ListenSet, sl.pid, port, 3*time.Second)

	holder := got.Holders[port]
	if holder.Identity.Ours {
		t.Errorf("ListenSet holder.Identity.Ours = true, want false — plain ListenSet does not assert ownership")
	}
}

func TestTreeListenSet_TransientChildExits_RootStillReported(t *testing.T) {
	// Adversary gap: no prior test exercised the tolerance path where a
	// process discovered during the tree walk has already exited by the
	// time its ports/cmdline are queried (a short-lived worker racing the
	// observer). The fixture spawns a genuinely short-lived, non-listening
	// child (exits ~50ms after starting) alongside the root; repeatedly
	// walking the tree across that window must never fail the whole
	// observation — it must keep reporting the ports it CAN read (the
	// still-running root) even while the transient child is exiting or
	// already gone.
	rootPort := freePort(t)
	sl := spawnListenerTree(t, rootPort, 0, 0, true)

	deadline := time.Now().Add(2 * time.Second)
	sawRootPort := false
	for time.Now().Before(deadline) {
		got, err := observe.TreeListenSet(sl.pid)
		if err != nil {
			t.Fatalf("TreeListenSet during transient-child race: %v — a per-PID error for an already-exited process must not fail the whole walk", err)
		}
		if _, ok := got.Holders[rootPort]; ok {
			sawRootPort = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawRootPort {
		t.Fatalf("root port %d never observed across the transient-child race window", rootPort)
	}
}

func TestTreeListenSet_UnknownPID_Errors(t *testing.T) {
	const bogusPID = int32(1<<31 - 2)
	if _, err := observe.TreeListenSet(bogusPID); err == nil {
		t.Error("TreeListenSet(bogus pid) succeeded, want error")
	}
}

// --- Classification ------------------------------------------------------

func TestClassify_ActualEqualsDeclared_CandidateUp(t *testing.T) {
	obs := observe.TreeObservation{Holders: map[int]observe.Holder{
		8080: {Port: 8080, Identity: observe.Identity{PID: 100, Ours: true}},
		8081: {Port: 8081, Identity: observe.Identity{PID: 100, Ours: true}},
	}}
	result := observe.Classify([]int{8080, 8081}, obs)

	if result.Classification != observe.CandidateUp {
		t.Errorf("Classification = %v, want CandidateUp", result.Classification)
	}
	if len(result.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", result.Missing)
	}
	if len(result.Stray) != 0 {
		t.Errorf("Stray = %v, want empty", result.Stray)
	}
}

func TestClassify_ActualSubsetOfDeclared_Partial(t *testing.T) {
	// actual ⊊ declared: 8081 declared but not yet listening.
	obs := observe.TreeObservation{Holders: map[int]observe.Holder{
		8080: {Port: 8080, Identity: observe.Identity{PID: 100, Ours: true}},
	}}
	result := observe.Classify([]int{8080, 8081}, obs)

	if result.Classification != observe.Partial {
		t.Errorf("Classification = %v, want Partial", result.Classification)
	}
	if len(result.Missing) != 1 || result.Missing[0] != 8081 {
		t.Errorf("Missing = %v, want [8081]", result.Missing)
	}
}

func TestClassify_ActualSupersetOfDeclared_StrayPort(t *testing.T) {
	// actual ⊋ declared: 9999 listening but not declared anywhere — the loud
	// anomaly case.
	obs := observe.TreeObservation{Holders: map[int]observe.Holder{
		8080: {Port: 8080, Identity: observe.Identity{PID: 100, Ours: true}},
		9999: {Port: 9999, Identity: observe.Identity{PID: 100, Ours: true}},
	}}
	result := observe.Classify([]int{8080}, obs)

	if result.Classification != observe.StrayPort {
		t.Errorf("Classification = %v, want StrayPort", result.Classification)
	}
	if len(result.Stray) != 1 || result.Stray[0] != 9999 {
		t.Errorf("Stray = %v, want [9999]", result.Stray)
	}
}

func TestClassify_StrayTakesPriorityOverPartial(t *testing.T) {
	// Both a missing declared port (8081) and a stray port (9999) present
	// simultaneously — stray is the louder anomaly and must win.
	obs := observe.TreeObservation{Holders: map[int]observe.Holder{
		8080: {Port: 8080, Identity: observe.Identity{PID: 100, Ours: true}},
		9999: {Port: 9999, Identity: observe.Identity{PID: 100, Ours: true}},
	}}
	result := observe.Classify([]int{8080, 8081}, obs)

	if result.Classification != observe.StrayPort {
		t.Errorf("Classification = %v, want StrayPort (priority over Partial)", result.Classification)
	}
}

func TestClassify_DuplicateDeclaredPorts_Deduplicated(t *testing.T) {
	obs := observe.TreeObservation{Holders: map[int]observe.Holder{
		8080: {Port: 8080, Identity: observe.Identity{PID: 100, Ours: true}},
	}}
	result := observe.Classify([]int{8080, 8080, 8080}, obs)

	if len(result.Declared) != 1 {
		t.Errorf("Declared = %v, want deduplicated to [8080]", result.Declared)
	}
	if result.Classification != observe.CandidateUp {
		t.Errorf("Classification = %v, want CandidateUp", result.Classification)
	}
}

func TestClassify_EmptyDeclared_EmptyActual_CandidateUp(t *testing.T) {
	result := observe.Classify(nil, observe.TreeObservation{Holders: map[int]observe.Holder{}})
	if result.Classification != observe.CandidateUp {
		t.Errorf("Classification = %v, want CandidateUp for empty/empty", result.Classification)
	}
}

// --- Foreign-holder surfacing --------------------------------------------

func TestClassify_ForeignHolderOfNeededPort_Surfaced(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	// Deliberately use the "unmarked ours" variant, simulating: aa-server-status
	// wants `port` for one of its servers, but a process it does NOT own is
	// already listening there.
	obs := waitForListenSet(t, observe.ListenSet, sl.pid, port, 3*time.Second)

	result := observe.Classify([]int{port}, obs)

	holder, ok := result.ForeignHolders[port]
	if !ok {
		t.Fatalf("ForeignHolders missing port %d — a needed port held by a non-ours process must be surfaced (PID + cmdline)", port)
	}
	if holder.PID != sl.pid {
		t.Errorf("ForeignHolders[%d].PID = %d, want %d", port, holder.PID, sl.pid)
	}
	if len(holder.Cmdline) == 0 {
		t.Errorf("ForeignHolders[%d].Cmdline is empty, want the holder's argv so the user can identify it", port)
	}
}

func TestClassify_OurOwnHolderOfNeededPort_NotSurfacedAsForeign(t *testing.T) {
	port := freePort(t)
	sl := spawnListener(t, port, 0)

	obs := waitForListenSet(t, observe.TreeListenSet, sl.pid, port, 3*time.Second)

	result := observe.Classify([]int{port}, obs)

	if _, ok := result.ForeignHolders[port]; ok {
		t.Errorf("ForeignHolders unexpectedly contains our own port %d — already-up-by-our-own-child must not be reported as a foreign holder", port)
	}
	if result.Classification != observe.CandidateUp {
		t.Errorf("Classification = %v, want CandidateUp", result.Classification)
	}
}
