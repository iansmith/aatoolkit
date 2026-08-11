package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
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
