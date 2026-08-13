package main

// AATK-87: statusForLocked (engine.go) discards observe.TreeListenSet's
// error, never consults class.Degraded, and tests len(class.Actual) == 0
// before the isOurs branch that assigns status.PID — so a tracked, live
// child whose ports momentarily fail to enumerate renders as
// `State: down, PID: 0`, indistinguishable from a server that was never
// launched. These pin the fix's required outcome directly against
// statusForLocked, deterministically (via observe.ConnectionsPidHook,
// added in internal/observe for this ticket) rather than by racing real
// host contention the way the ticket's own recorded reproduction did.

import (
	"testing"
	"time"

	gopsnet "github.com/shirou/gopsutil/v4/net"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// slopstop:test contract
func TestStatusForLocked_ForcedUnconfirmedEmptyRead_KeepsTrackedPID(t *testing.T) {
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
	// Register f as a tracked ("ours") live child directly, the same shape
	// upOne leaves in e.procs after a real launch — this test isolates
	// statusForLocked's own handling of an unconfirmed observation rather
	// than also exercising Up()/pollPortsReady.
	eng.procs["svc"] = &lifecycle.Process{Cmd: f.cmd, LogPath: "forced-empty.log"}

	// Prove the real observation genuinely sees the port before lying about
	// it below, so the forced read is provably wrong, not coincidentally
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
	if len(statuses) != 1 || statuses[0].PID == 0 {
		t.Fatalf("expected the tracked live child's PID to still be reported after an unconfirmed (forced-empty) observation, got %+v", statuses)
	}
}

// slopstop:test regression — guards: "A server genuinely holding no declared ports still reports down — the fix must not make a real down state unreachable." (ticket Definition of done)
func TestStatusForLocked_NeverLaunchedServer_StillReportsDown(t *testing.T) {
	port := freeTestPort(t)
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
	// No eng.procs entry: never launched. Nothing in the real world is
	// bound to `port` either, so the host-wide scan genuinely finds
	// nothing — this is the "confirmed to hold no ports" case the fix must
	// not make unreachable.

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateDown {
		t.Fatalf("expected a never-launched server with a genuinely-unbound declared port to still report down, got %+v", statuses)
	}
}
