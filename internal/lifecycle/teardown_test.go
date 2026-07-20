package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/health"
)

// --- Edge / boundary ---------------------------------------------------

// TestTeardown_AlreadyGone_NoOpVerifiesClean covers the boundary where
// there is nothing left to signal at all (e.g. a process that already
// exited on its own before teardown ran) — Teardown must not error just
// because the group is already gone, and must report KillSignalNone.
func TestTeardown_AlreadyGone_NoOpVerifiesClean(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, false, false)

	// Kill it ourselves first, outside of Teardown, so by the time Teardown
	// runs the group is already gone.
	tp.forceKill()
	waitForPortFree(t, port, 3*time.Second)

	result, err := Teardown(context.Background(), Target{Name: "gone", PID: tp.pid, Ports: []int{port}}, 5*time.Second)
	if err != nil {
		t.Fatalf("expected no error tearing down an already-gone group, got: %v", err)
	}
	if result.Signal != KillSignalNone {
		t.Fatalf("expected KillSignalNone for an already-gone group, got %q", result.Signal)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true for an already-gone group with a free port")
	}
}

// TestTeardown_GroupKillsWholeTree_NotJustLeader is the process-group
// boundary case: SIGTERM must reach every process in the group, not just
// the leader, so a nested child listener also goes down.
func TestTeardown_GroupKillsWholeTree_NotJustLeader(t *testing.T) {
	parentPort := freeTestPort(t)
	childPort := freeTestPort(t)
	tp := spawnTdlistener(t, parentPort, childPort, false, false)
	waitForListenerReady(t, childPort)

	target := Target{Name: "tree", PID: tp.pid, Ports: []int{parentPort, childPort}}
	result, err := Teardown(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected both parent and child ports free after group teardown")
	}
	if portListening(childPort) {
		t.Fatalf("expected nested child listener port %d free after group SIGTERM", childPort)
	}
}

// TestTeardown_NoDeclaredPorts_StillKillsAndVerifies is the empty-set
// boundary: a target with zero declared ports (defensive — real config
// validation requires at least one port per server, but the function
// itself must not special-case or crash on an empty slice) must still
// signal and reap the process group, reporting VerifiedClean since there
// is vacuously nothing left listening to check.
func TestTeardown_NoDeclaredPorts_StillKillsAndVerifies(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, false, false)

	result, err := Teardown(context.Background(), Target{Name: "no-ports", PID: tp.pid}, 5*time.Second)
	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true for an empty declared-port set")
	}
	if groupAlive(tp.pid) {
		t.Fatalf("expected the process group to actually be signaled and reaped even with no ports to verify")
	}
}

// TestTeardownAll_SkipsServerWithNoLivePID confirms a server absent from
// the pids map (never launched this session) is skipped rather than
// causing a panic or a spurious failure — TeardownAll must not assume
// every configured server has a live process to tear down.
func TestTeardownAll_SkipsServerWithNoLivePID(t *testing.T) {
	livePort := freeTestPort(t)
	liveProc := spawnTdlistener(t, livePort, 0, false, false)

	servers := []config.Server{
		{Name: "never-launched", Port: freeTestPort(t)},
		{Name: "live", Port: livePort},
	}
	pids := map[string]int32{"live": liveProc.pid} // "never-launched" deliberately absent
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 2 * time.Second}}

	results, err := TeardownAll(context.Background(), servers, supervisor, pids)
	if err != nil {
		t.Fatalf("TeardownAll error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result (the live server only), got %d: %v", len(results), results)
	}
	if results[0].Name != "live" {
		t.Fatalf("expected the only result to be 'live', got %q", results[0].Name)
	}
}

// TestTeardownAll_MultipleFailures_AggregateCountsAll checks the error-path
// gap the single-failure aggregate test doesn't cover: with 2 of 2 servers
// failing to verify clean, the aggregate error must report both, not just
// one (e.g. an off-by-one that only records the first error).
func TestTeardownAll_MultipleFailures_AggregateCountsAll(t *testing.T) {
	port1 := freeTestPort(t)
	proc1 := spawnTdlistener(t, port1, 0, true, false) // ignores SIGTERM
	survivor1 := freeTestPort(t)
	sv1 := spawnTdlistener(t, survivor1, 0, false, false)
	defer sv1.forceKill()

	port2 := freeTestPort(t)
	proc2 := spawnTdlistener(t, port2, 0, true, false) // ignores SIGTERM
	survivor2 := freeTestPort(t)
	sv2 := spawnTdlistener(t, survivor2, 0, false, false)
	defer sv2.forceKill()

	servers := []config.Server{
		{Name: "fail-one", Port: port1, Listens: []int{survivor1}},
		{Name: "fail-two", Port: port2, Listens: []int{survivor2}},
	}
	pids := map[string]int32{"fail-one": proc1.pid, "fail-two": proc2.pid}
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 200 * time.Millisecond}}

	results, err := TeardownAll(context.Background(), servers, supervisor, pids)
	if err == nil {
		t.Fatalf("expected an aggregate error since both servers cannot verify clean")
	}
	if !strings.Contains(err.Error(), "2 server") {
		t.Fatalf("expected aggregate error to report exactly 2 failing servers, got: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both servers attempted, got %d results", len(results))
	}
	for _, r := range results {
		if r.VerifiedClean {
			t.Fatalf("expected both results unverified, got %+v", r)
		}
	}
}

// TestTeardown_CalledTwice_SecondCallIsIdempotent covers the state-
// interaction gap: tearing down an already-torn-down target a second time
// (e.g. a retry after a transient observation error) must not error just
// because the group is already gone — it degrades to the same
// already-gone/no-op behavior as the first boundary test, this time
// reached via a *second* call on the same Target rather than an
// independent kill.
func TestTeardown_CalledTwice_SecondCallIsIdempotent(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, false, false)

	target := Target{Name: "twice", PID: tp.pid, Ports: []int{port}}
	first, err := Teardown(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("first Teardown error: %v", err)
	}
	if !first.VerifiedClean {
		t.Fatalf("expected first Teardown to verify clean")
	}

	second, err := Teardown(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("second Teardown on an already-torn-down target should not error, got: %v", err)
	}
	if second.Signal != KillSignalNone {
		t.Fatalf("expected KillSignalNone on the second, no-op call, got %q", second.Signal)
	}
	if !second.VerifiedClean {
		t.Fatalf("expected second call to still verify clean")
	}
}

// TestTeardown_GracePeriodBoundary_TermSufficientJustBeforeDeadline checks
// the timing boundary: a process that exits well within the grace period
// must be reported as KillSignalTerm (no KILL needed), not KillSignalKill.
func TestTeardown_GracePeriodBoundary_TermSufficientJustBeforeDeadline(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, false, false) // honors SIGTERM

	result, err := Teardown(context.Background(), Target{Name: "graceful", PID: tp.pid, Ports: []int{port}}, 5*time.Second)
	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if result.Signal != KillSignalTerm {
		t.Fatalf("expected KillSignalTerm when the process honors SIGTERM within grace, got %q", result.Signal)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true")
	}
}

// --- Error / rejection ---------------------------------------------------

// TestTeardown_SurvivorAfterKill_IsLoudError is the core "never report a
// kill you didn't achieve" contract. If, hypothetically, a declared port is
// still listening even after the SIGKILL branch fires (simulated here by
// declaring a port that some *other*, untouched listener holds — standing
// in for a survivor Teardown's own kill could not reach), Teardown must
// return a non-nil error and VerifiedClean must be false, even though the
// target process group itself is gone.
func TestTeardown_SurvivorAfterKill_IsLoudError(t *testing.T) {
	targetPort := freeTestPort(t)
	tp := spawnTdlistener(t, targetPort, 0, true, false) // ignores SIGTERM -> forces SIGKILL

	// A second, independent listener stands in for a port that remains
	// occupied no matter what Teardown does to tp's group — e.g. a stray
	// process on the same declared port that isn't part of this group.
	survivorPort := freeTestPort(t)
	survivor := spawnTdlistener(t, survivorPort, 0, false, false)
	defer survivor.forceKill()

	target := Target{Name: "survivor-case", PID: tp.pid, Ports: []int{targetPort, survivorPort}}
	result, err := Teardown(context.Background(), target, 300*time.Millisecond)

	if err == nil {
		t.Fatalf("expected a loud error when a declared port is still listening after SIGKILL")
	}
	if !strings.Contains(err.Error(), "still listening") {
		t.Fatalf("expected error to mention the surviving listener, got: %v", err)
	}
	if result.VerifiedClean {
		t.Fatalf("expected VerifiedClean false when a declared port survives teardown")
	}
	if result.Signal != KillSignalKill {
		t.Fatalf("expected KillSignalKill was attempted, got %q", result.Signal)
	}
}

// TestTeardown_HealthStillAnsweringAfterKill_IsLoudError covers the health
// half of "never report a kill you didn't achieve": even with every
// declared port free, a health endpoint that somehow still answers 2xx
// (e.g. a different process now bound to that port and serving) must fail
// verification loudly rather than silently reporting success.
func TestTeardown_HealthStillAnsweringAfterKill_IsLoudError(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, true, true) // ignores SIGTERM, serves /healthz

	// A second server on its own port stands in for "the health endpoint
	// somehow still answers" — target.Health points at it directly so the
	// probe finds a live 2xx regardless of what happened to tp's own group.
	stillAliveHealthPort := freeTestPort(t)
	stillAlive := spawnTdlistener(t, stillAliveHealthPort, 0, false, true)
	defer stillAlive.forceKill()
	waitForHealthReady(t, stillAliveHealthPort)

	spec := health.Spec{Host: "127.0.0.1", Port: stillAliveHealthPort, Path: "/healthz"}
	target := Target{Name: "health-survivor", PID: tp.pid, Ports: []int{port}, Health: &spec}

	result, err := Teardown(context.Background(), target, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("expected a loud error when the health probe still answers 2xx after SIGKILL")
	}
	if !strings.Contains(err.Error(), "health probe") {
		t.Fatalf("expected error to mention the health probe, got: %v", err)
	}
	if result.VerifiedClean {
		t.Fatalf("expected VerifiedClean false when health still answers after teardown")
	}
}

// TestTeardownAll_AttemptsEveryServer_AggregatesErrors verifies §6.5's
// "attempt all, aggregate loudly" semantics: a failure tearing down one
// server must not skip the others, and the aggregate error must mention
// the failure count.
func TestTeardownAll_AttemptsEveryServer_AggregatesErrors(t *testing.T) {
	goodPort := freeTestPort(t)
	goodProc := spawnTdlistener(t, goodPort, 0, false, false) // honors SIGTERM

	badPort := freeTestPort(t)
	badProc := spawnTdlistener(t, badPort, 0, true, false) // ignores SIGTERM

	survivorPort := freeTestPort(t)
	survivor := spawnTdlistener(t, survivorPort, 0, false, false)
	defer survivor.forceKill()

	servers := []config.Server{
		{Name: "good", Port: goodPort},
		{Name: "bad", Port: badPort, Listens: []int{survivorPort}},
	}
	pids := map[string]int32{"good": goodProc.pid, "bad": badProc.pid}
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 300 * time.Millisecond}}

	results, err := TeardownAll(context.Background(), servers, supervisor, pids)

	if err == nil {
		t.Fatalf("expected an aggregate error since server 'bad' cannot verify clean")
	}
	if !strings.Contains(err.Error(), "1 server") {
		t.Fatalf("expected aggregate error to report exactly 1 failing server, got: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both servers attempted despite the failure, got %d results", len(results))
	}

	var goodResult, badResult *Result
	for i := range results {
		switch results[i].Name {
		case "good":
			goodResult = &results[i]
		case "bad":
			badResult = &results[i]
		}
	}
	if goodResult == nil || !goodResult.VerifiedClean {
		t.Fatalf("expected 'good' server torn down cleanly despite 'bad' failing, got %+v", goodResult)
	}
	if badResult == nil || badResult.VerifiedClean {
		t.Fatalf("expected 'bad' server to report unverified, got %+v", badResult)
	}
}

// --- Cross-feature interaction ---------------------------------------------------

// TestTeardownAll_ReverseConfigOrder confirms the multi-server path tears
// down in the reverse of the configured order — the ticket is explicit
// that this reversal belongs only to the multi-server function, not to
// Teardown itself. TeardownAll builds its Results slice in the order it
// actually attempts each server, so the returned order is itself the
// observable evidence.
func TestTeardownAll_ReverseConfigOrder(t *testing.T) {
	portA := freeTestPort(t)
	procA := spawnTdlistener(t, portA, 0, false, false)
	portB := freeTestPort(t)
	procB := spawnTdlistener(t, portB, 0, false, false)
	portC := freeTestPort(t)
	procC := spawnTdlistener(t, portC, 0, false, false)

	servers := []config.Server{
		{Name: "a", Port: portA},
		{Name: "b", Port: portB},
		{Name: "c", Port: portC},
	}
	pids := map[string]int32{"a": procA.pid, "b": procB.pid, "c": procC.pid}
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 2 * time.Second}}

	results, err := TeardownAll(context.Background(), servers, supervisor, pids)
	if err != nil {
		t.Fatalf("TeardownAll error: %v", err)
	}

	want := []string{"c", "b", "a"}
	if len(results) != len(want) {
		t.Fatalf("expected %d teardowns recorded, got %d: %v", len(want), len(results), results)
	}
	for i := range want {
		if results[i].Name != want[i] {
			t.Fatalf("expected reverse config order %v, got %v", want, resultNames(results))
		}
	}
}

func resultNames(results []Result) []string {
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.Name
	}
	return names
}

// TestTeardown_UsesPerServerGracePeriodOverride confirms grace_period comes
// from config: a per-server override shorter than the default must be
// honored (i.e. Teardown must not wait the supervisor default when an
// override is present), exercised end-to-end via ResolveGracePeriod feeding
// Teardown, against a process that ignores SIGTERM so the wait is actually
// observable.
func TestTeardown_UsesPerServerGracePeriodOverride(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, true, false) // ignores SIGTERM -> always needs KILL

	server := config.Server{Name: "fast-grace", Port: port, GracePeriod: config.Duration{Duration: 100 * time.Millisecond}}
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 30 * time.Second}} // would time out the test if used

	grace := ResolveGracePeriod(server, supervisor)
	if grace != 100*time.Millisecond {
		t.Fatalf("expected per-server override 100ms, got %s", grace)
	}

	start := time.Now()
	target := Target{Name: server.Name, PID: tp.pid, Ports: DeclaredPorts(server)}
	result, err := Teardown(context.Background(), target, grace)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Teardown took %s — expected it to use the 100ms per-server override, not a longer default", elapsed)
	}
}

// TestResolveGracePeriod_FallsBackToSupervisorDefault is the config-layer
// half of the same contract: when no per-server override is set, the
// supervisor's (already-defaulted) value must be used verbatim.
func TestResolveGracePeriod_FallsBackToSupervisorDefault(t *testing.T) {
	server := config.Server{Name: "no-override"}
	supervisor := config.Supervisor{GracePeriod: config.Duration{Duration: 5 * time.Second}}

	got := ResolveGracePeriod(server, supervisor)
	if got != 5*time.Second {
		t.Fatalf("expected supervisor default 5s, got %s", got)
	}
}

// TestTeardownForeign_GroupKillsByPID_NotProcessHandle exercises the
// foreign-stray path used only by `dead`: no *Process/*exec.Cmd handle is
// available (aa-server-status never launched this process), so the group must
// still be reachable and killable purely from an observed PID.
func TestTeardownForeign_GroupKillsByPID_NotProcessHandle(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, true, false) // ignores SIGTERM

	// Simulate "observed foreign PID" by using only the bare pid, never
	// touching tp.cmd — TeardownForeign's signature accepts nothing else.
	result, err := TeardownForeign(context.Background(), "foreign-stray", tp.pid, []int{port}, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("TeardownForeign error: %v", err)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true after foreign group-kill")
	}
	if result.Signal != KillSignalKill {
		t.Fatalf("expected KillSignalKill for a SIGTERM-ignoring foreign process, got %q", result.Signal)
	}
}

// --- Happy path ---------------------------------------------------

// TestTeardown_GracefulExit_NoKillNeeded is the plain happy path: a
// well-behaved server honors SIGTERM, and Teardown reports KillSignalTerm
// with a verified-clean result.
func TestTeardown_GracefulExit_NoKillNeeded(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, false, true)
	waitForHealthReady(t, port)

	spec := health.Spec{Host: "127.0.0.1", Port: port, Path: "/healthz"}
	target := Target{Name: "happy-term", PID: tp.pid, Ports: []int{port}, Health: &spec}

	result, err := Teardown(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if result.Signal != KillSignalTerm {
		t.Fatalf("expected KillSignalTerm, got %q", result.Signal)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true")
	}
}

// TestTeardown_SurvivesGrace_EscalatesToKill is the plain happy path for
// the escalation branch: a server that ignores SIGTERM is still
// successfully torn down via SIGKILL, and Teardown reports KillSignalKill.
func TestTeardown_SurvivesGrace_EscalatesToKill(t *testing.T) {
	port := freeTestPort(t)
	tp := spawnTdlistener(t, port, 0, true, false)

	target := Target{Name: "happy-kill", PID: tp.pid, Ports: []int{port}}
	result, err := Teardown(context.Background(), target, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Teardown error: %v", err)
	}
	if result.Signal != KillSignalKill {
		t.Fatalf("expected KillSignalKill, got %q", result.Signal)
	}
	if !result.VerifiedClean {
		t.Fatalf("expected VerifiedClean true")
	}
}

// TestDeclaredPorts_PortAndListensDeduplicated is a small happy-path unit
// test for the declared-set helper feeding Target.Ports.
func TestDeclaredPorts_PortAndListensDeduplicated(t *testing.T) {
	s := config.Server{Port: 9000, Listens: []int{9000, 9001}}
	got := DeclaredPorts(s)
	want := []int{9000, 9001}
	if len(got) != len(want) {
		t.Fatalf("expected deduplicated %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
