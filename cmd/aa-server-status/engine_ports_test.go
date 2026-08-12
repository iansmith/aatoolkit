package main

import (
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
)

// The exact-listen-set contract (design/aa-server-status.md §6.2) applied at
// boot: `up` does not report success until the server's observed listen-set
// across its whole process tree equals its declared {port} ∪ listens.
//
// These spawn the real tdlistener fixture — no mock stands in for
// internal/observe — and use a short ready-timeout so the negative cases
// fail on their own deadline rather than the fixture's.

func portCheckSupervisor(t *testing.T) config.Supervisor {
	t.Helper()
	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: 1500 * time.Millisecond}
	return sup
}

// A server that declares two ports but only ever binds one is ⊊ declared —
// `partial`, not up. Up must fail naming the port that never appeared, and
// must leave the child running and tracked, exactly as a failed health check
// at boot does.
func TestRealEngine_Up_MissingDeclaredPort_FailsAndLeavesChildTracked(t *testing.T) {
	bound := freeTestPort(t)
	missing := freeTestPort(t)

	cfg := config.Config{
		Supervisor: portCheckSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Listens: []int{bound, missing},
			Command: tdlistenerBinary(t),
			Args:    []string{"-port", strconv.Itoa(bound), "-serve-health"},
			Health:  config.Health{Path: "/healthz", Port: bound},
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Up("")
	if err == nil {
		t.Fatalf("expected Up to fail: port %d is declared and never bound", missing)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(missing)) {
		t.Fatalf("expected the error to name the missing port %d, got: %v", missing, err)
	}

	// Same disposition as a failed health check: reported failed, still
	// running, still ours, so `down` can clean it up.
	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].PID == 0 {
		t.Fatalf("expected the child to stay running and tracked after a failed port check, got %+v", statuses)
	}
}

// A server whose tree binds a port outside its declared set is ⊋ declared —
// the loud anomaly. Up must fail naming both the stray port and the PID
// holding it, which is what makes the report actionable.
func TestRealEngine_Up_StrayPortInTree_FailsNamingPortAndPID(t *testing.T) {
	declared := freeTestPort(t)
	stray := freeTestPort(t)

	cfg := config.Config{
		Supervisor: portCheckSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Listens: []int{declared},
			Command: tdlistenerBinary(t),
			Args: []string{
				"-port", strconv.Itoa(declared),
				"-child-port", strconv.Itoa(stray),
				"-serve-health",
			},
			Health: config.Health{Path: "/healthz", Port: declared},
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Up("")
	if err == nil {
		t.Fatalf("expected Up to fail: port %d is bound by the tree and not declared", stray)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(stray)) {
		t.Fatalf("expected the error to name the stray port %d, got: %v", stray, err)
	}
	if !strings.Contains(err.Error(), "pid ") {
		t.Fatalf("expected the error to name the PID holding the stray port, got: %v", err)
	}
}

// The port check is not gated on a health check existing. A server declaring
// no health endpoint gets no readiness verification at all today — `up`
// succeeds on exec not failing — so this is the case the check is worth most
// for, and the one an early return on Health.Declared() would silently skip.
func TestRealEngine_Up_NoHealthDeclared_StillVerifiesPorts(t *testing.T) {
	bound := freeTestPort(t)
	missing := freeTestPort(t)

	cfg := config.Config{
		Supervisor: portCheckSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Listens: []int{bound, missing},
			Command: tdlistenerBinary(t),
			Args:    []string{"-port", strconv.Itoa(bound)},
			// No Health: nothing declared, so pollReady's health stage is skipped.
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Up("")
	if err == nil {
		t.Fatalf("expected Up to fail on the missing port %d even with no health check declared", missing)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(missing)) {
		t.Fatalf("expected the error to name the missing port %d, got: %v", missing, err)
	}
}

// The happy path must stay green: a server binding exactly its declared set
// passes the new check without slowing down, since the ports are already
// there by the time health has answered.
func TestRealEngine_Up_ExactListenSet_StillSucceeds(t *testing.T) {
	a := freeTestPort(t)
	b := freeTestPort(t)

	cfg := config.Config{
		Supervisor: portCheckSupervisor(t),
		Servers: []config.Server{{
			Name:    "svc",
			Type:    config.TypeExec,
			Enabled: true,
			Host:    "127.0.0.1",
			Listens: []int{a, b},
			Command: tdlistenerBinary(t),
			Args: []string{
				"-port", strconv.Itoa(a),
				"-child-port", strconv.Itoa(b),
				"-serve-health",
			},
			Health: config.Health{Path: "/healthz", Port: a},
		}},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("expected Up to succeed when the tree binds exactly its declared set: %v", err)
	}
	if s := eng.Status(); len(s) != 1 || s[0].State != StateUp {
		t.Fatalf("expected StateUp after a clean Up(), got %+v", s)
	}
}

// TestPollReady_PortVerdictStillRendersAfterHealthTimesOut pins the actual
// defect behind AATK-86: pollReady's health stage (engine.go) returns
// immediately on its own error, so the port-check stage that would name a
// missing declared port never runs. Under full-suite load the fixture's own
// ready_timeout can fire before the fixture answers healthy, and the error
// `up` returns names the readiness deadline instead of the missing port —
// this is the failure the four tests above reproduce only under contention
// (see the ticket's recorded reproduction).
//
// This test pins the same ordering deterministically instead of racing
// contention: health is made to fail on *every* probe (not merely slow to
// succeed), and the process pollPortsReady inspects is a real, harmless
// subprocess that never binds any port — so "the declared port never
// appeared" is guaranteed, not timing-dependent. The only thing timing
// governs is how long the health stage's own configured ready_timeout takes
// to elapse. Following warm_order_test.go's fabricated-process style: a real
// *lifecycle.Process wrapping an actual PID, but not launched through
// upOne/tdlistener — pollReady is exercised directly, with no mock standing
// in for internal/observe or internal/health.
//
// slopstop:test contract
func TestPollReady_PortVerdictStillRendersAfterHealthTimesOut(t *testing.T) {
	var mu sync.Mutex
	var order []string
	host, healthPort := recordingServer(t, &order, &mu, map[string]int{"GET /healthz": http.StatusInternalServerError})

	missing := freeTestPort(t)

	// A real, running process that binds no ports at all. pollPortsReady
	// walks its process tree via the real internal/observe package and
	// finds nothing listening — exactly the observation a server that
	// declared a port it never bound would produce.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting harmless subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	proc := &lifecycle.Process{Cmd: cmd, LogPath: "port-verdict.log"}

	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: 150 * time.Millisecond}
	sup.PollInterval = config.Duration{Duration: 20 * time.Millisecond}
	eng := NewEngine(config.Config{Supervisor: sup}, nil, nil)

	s := config.Server{
		Name:    "svc",
		Type:    config.TypeExec,
		Enabled: true,
		Host:    host,
		Listens: []int{missing},
		Health:  config.Health{Path: "/healthz", Port: healthPort},
	}

	start := time.Now()
	err := eng.pollReady(s, proc)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected pollReady to fail: the declared port never bound and health never became healthy")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(missing)) {
		t.Fatalf("expected the error to name the missing port %d even though health timed out first, got: %v", missing, err)
	}

	// Bounded per the ticket's DoD: the port verdict must not be bought by
	// waiting out the 5s default ready_timeout on every negative case.
	// ready_timeout here is 150ms and pollPortsReady's own contract is one
	// unconditional probe regardless of remaining budget, so this should
	// finish in a small multiple of 150ms — well under half of 5s.
	if elapsed > 2*time.Second {
		t.Fatalf("pollReady took %s to report a never-binding port — expected materially less than the 5s default ready timeout", elapsed)
	}
}

// TestPollReady_PortVerdictStillRendersAfterWarmUpTimesOut pins the sibling
// defect to the one pinned above, at pollReady's other early-return branch:
// the warm-up stage (engine.go:738-740) returns immediately on its own
// error, so the port-check stage that would name a missing declared port
// never runs. A stage-7 adversary round proved by mutation that this branch
// carries the structurally identical bug and was untested; the ticket owner
// has ruled it in scope.
//
// This test pins the ordering deterministically instead of racing
// contention, exactly as the sibling test does: warm-up is made to fail on
// *every* probe (not merely slow to succeed), and the process
// pollPortsReady inspects is a real, harmless subprocess that never binds
// any port — so "the declared port never appeared" is guaranteed, not
// timing-dependent. The only thing timing governs is how long the warm-up
// stage's own configured ready_timeout takes to elapse.
//
// Health is deliberately left undeclared, which is what makes the failure
// unambiguously the warm-up stage's: TestPollReady_HealthNeverProbedIfWarmFails
// (warm_order_test.go) pins that a failing warm-up means health is never
// probed at all, so declaring a health check here would add a second
// possible source for the error without changing what actually happens.
// With no health declared, the only way pollReady can return an error here
// is warm-up failing.
//
// slopstop:test contract
func TestPollReady_PortVerdictStillRendersAfterWarmUpTimesOut(t *testing.T) {
	var mu sync.Mutex
	var order []string
	host, warmPort := recordingServer(t, &order, &mu, map[string]int{"POST /warm": http.StatusInternalServerError})

	missing := freeTestPort(t)

	// A real, running process that binds no ports at all. pollPortsReady
	// walks its process tree via the real internal/observe package and
	// finds nothing listening — exactly the observation a server that
	// declared a port it never bound would produce.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting harmless subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	proc := &lifecycle.Process{Cmd: cmd, LogPath: "port-verdict-warmup.log"}

	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: 150 * time.Millisecond}
	sup.PollInterval = config.Duration{Duration: 20 * time.Millisecond}
	eng := NewEngine(config.Config{Supervisor: sup}, nil, nil)

	s := config.Server{
		Name:    "svc",
		Type:    config.TypeExec,
		Enabled: true,
		Host:    host,
		Listens: []int{missing},
		// Warm.Port is set explicitly rather than left to default from
		// s.Port, so s.Port stays 0 and does not itself become a declared
		// port (lifecycle.DeclaredPorts treats a nonzero s.Port as
		// declared) — mirroring how the sibling test keeps Health.Port
		// separate from s.Listens.
		Warm: config.Warm{Method: "POST", Path: "/warm", Port: warmPort},
		// No Health: see the doc comment above for why this is what makes
		// the failure unambiguously warm-up's.
	}

	start := time.Now()
	err := eng.pollReady(s, proc)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected pollReady to fail: the declared port never bound and warm-up never succeeded")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(missing)) {
		t.Fatalf("expected the error to name the missing port %d even though warm-up timed out first, got: %v", missing, err)
	}

	// Bounded per the ticket's DoD, scaled to the configured ready_timeout
	// rather than a hardcoded absolute. sup.ReadyTimeout is 150ms here, so
	// the bound is 8*150ms = 1.2s. The sibling health-stage test's observed
	// cost under CPU contention is ~0.3s (one warm-up budget plus one
	// lsof-backed port probe), so 1.2s is roughly 4x that observed headroom
	// while staying materially less than the 5s ready timeout
	// testSupervisor declares (engine_test.go:43) — not "the 5s default",
	// which is config.DefaultReadyTimeout (15s, config/types.go:32) and is
	// out of scope for this ticket.
	if elapsed > 8*sup.ReadyTimeout.Duration {
		t.Fatalf("pollReady took %s to report a never-binding port — expected materially less than the 5s ready timeout testSupervisor declares", elapsed)
	}
}

// TestPollReady_PortVerdictStillRendersAfterWarmUpFastRejects pins a second,
// structurally distinct way into the same early-return at engine.go:738-740
// as the sibling TestPollReady_PortVerdictStillRendersAfterWarmUpTimesOut.
// That sibling pins the *slow* warm-up failure — POST /warm answering 500,
// which internal/health's warm loop retries until the ready-timeout budget
// is exhausted. A stage-7 adversary round proved by mutation that this only
// pins the slow path: a plausible scoped fix — fall through to the port
// check only once the remaining budget is exhausted, otherwise return the
// warm-up error as before — makes that test pass while leaving a second
// path through the same early return broken.
//
// The second path is fast rejection. isWarmRequestRejected
// (internal/health/health.go:360-366) treats a plain 4xx — anything except
// 408 and 429 — as "the server understood and refused, not pending", so
// health.Warm returns immediately, in ~1-2ms, with no retry and no budget
// exhaustion. A budget-gated fix keys off the ready-timeout deadline, which
// a fast rejection never approaches, so it would still return this error
// straight through the early return and never reach pollPortsReady.
// Pinning both paths is the only way to close the early-return gap for
// real, rather than for whichever failure mode happens to get exercised.
//
// Mirrors the sibling exactly but for the fixture's /warm handler: it
// answers 404 (a plain 4xx, not 408/429) instead of 500, so
// isWarmRequestRejected reports true and health.Warm returns on its first
// probe instead of retrying. Health is left undeclared for the same reason
// the sibling states: TestPollReady_HealthNeverProbedIfWarmFails
// (warm_order_test.go) already pins that a failing warm-up means health is
// never probed, so declaring one here would add a second possible source
// for the error without changing what actually happens.
//
// slopstop:test contract
func TestPollReady_PortVerdictStillRendersAfterWarmUpFastRejects(t *testing.T) {
	var mu sync.Mutex
	var order []string
	host, warmPort := recordingServer(t, &order, &mu, map[string]int{"POST /warm": http.StatusNotFound})

	missing := freeTestPort(t)

	// A real, running process that binds no ports at all. pollPortsReady
	// walks its process tree via the real internal/observe package and
	// finds nothing listening — exactly the observation a server that
	// declared a port it never bound would produce.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting harmless subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	proc := &lifecycle.Process{Cmd: cmd, LogPath: "port-verdict-warmup-fastreject.log"}

	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: 150 * time.Millisecond}
	sup.PollInterval = config.Duration{Duration: 20 * time.Millisecond}
	eng := NewEngine(config.Config{Supervisor: sup}, nil, nil)

	s := config.Server{
		Name:    "svc",
		Type:    config.TypeExec,
		Enabled: true,
		Host:    host,
		Listens: []int{missing},
		// Warm.Port is set explicitly rather than left to default from
		// s.Port, so s.Port stays 0 and does not itself become a declared
		// port (lifecycle.DeclaredPorts treats a nonzero s.Port as
		// declared) — mirroring how the sibling test keeps Health.Port
		// separate from s.Listens.
		Warm: config.Warm{Method: "POST", Path: "/warm", Port: warmPort},
		// No Health: see the doc comment above for why this is what makes
		// the failure unambiguously warm-up's.
	}

	start := time.Now()
	err := eng.pollReady(s, proc)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected pollReady to fail: the declared port never bound and warm-up was rejected")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(missing)) {
		t.Fatalf("expected the error to name the missing port %d even though warm-up rejected immediately, got: %v", missing, err)
	}

	// Matches the sibling's 8x, not the tighter bound this test used to
	// carry. That original bound assumed this path "exhausts no budget at
	// all" because isWarmRequestRejected fires on the first probe and
	// health.Warm returns in ~1-2ms — true for the warm-up leg, but the
	// premise stopped there. The pattern-consistent fix for the
	// early-return gap (extending pollReady's existing
	// e.pollPortsReady(s, pid, time.Until(deadline)) call to both early
	// returns) still runs the *same* pollPortsReady this path always ran,
	// which guarantees one unconditional probe "whatever the budget
	// arithmetic says" (see pollPortsReady's own comment) and, on darwin,
	// pays the same ~100ms lsof-backed process-tree probe every port check
	// pays. A stage-7 adversary round proved the old 2x (300ms) bound too
	// tight by mutation under exactly that fix: 0.26s measured across 8
	// idle runs — already 87% of the bound with no contention — then a
	// FAIL at 313.75ms under induced full-suite load. 8x (1.2s at this
	// test's 150ms ReadyTimeout) absorbs that overshoot with real margin
	// and remains well under the 5s ready timeout testSupervisor declares,
	// while still failing a fixer who "solves" fast-reject by waiting out
	// the full deadline before attempting the port check.
	if elapsed > 8*sup.ReadyTimeout.Duration {
		t.Fatalf("pollReady took %s to report a never-binding port after an immediate warm-up rejection — expected well under 8x the 150ms ready timeout testSupervisor declares here (the warm-up leg is fast, but the port-check leg pays the same lsof probe cost every port check pays), not a wait for the budget to elapse", elapsed)
	}
}
