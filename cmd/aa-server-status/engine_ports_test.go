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
