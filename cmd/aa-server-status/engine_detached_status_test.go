package main

// Status-path coverage for detached servers (the `detached = true` shape
// added for containerized services such as `docker compose up -d`).
//
// The start path and the status path each grew their own detached branch.
// Start's is exercised by pollPortsReady; status's, on engine.go's
// classification switch, was unreachable for any detached server the
// supervisor had actually launched — which is every one of them in
// practice. These tests pin the status half.

import (
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// detachedServer is the config shape a containerized service declares:
// a launcher command that exits, teardown by re-running the command, and
// the ports held by something else entirely.
func detachedServer(port int) config.Server {
	return config.Server{
		Name:         "svc",
		Type:         config.TypeExec,
		Enabled:      true,
		Detached:     true,
		Host:         "127.0.0.1",
		Listens:      []int{port},
		Command:      "docker",
		Args:         []string{"compose", "up", "-d"},
		TeardownArgs: []string{"compose", "down"},
	}
}

// TestStatusForLocked_DetachedLauncherExited_ReportsUp is the postgres bug.
//
// `docker compose up -d` starts a container and exits — that exit is the
// whole definition of detached, not a failure. But the e.procs entry for the
// launcher stays behind (nothing removes it on death, by design: see
// engine_observation_gap_test.go), so isOurs stays true, and the status path
// observed the PID TREE OF A DEAD PROCESS. That tree holds nothing, so
// class.Actual came back empty and the switch's first arm rendered the
// server down — while the container it started was listening and serving.
//
// A detached server must be observed by HOST ports, never by process tree:
// once the launcher exits its PID says nothing about whether the service is
// up. The live foreign listener here stands in for the container.
func TestStatusForLocked_DetachedLauncherExited_ReportsUp(t *testing.T) {
	port := freeTestPort(t)
	container := spawnForeignListener(t, port) // stays alive: the "container"

	// A second process, killed and reaped, standing in for the launcher that
	// exited after `compose up -d` returned. Its port is irrelevant; only its
	// deadness matters.
	launcher := spawnForeignListener(t, freeTestPort(t))
	launcher.forceKill()

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{detachedServer(port)},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: launcher.cmd, LogPath: "detached-launcher.log"}

	// Prove the port really is held before asserting anything about it, so a
	// pass cannot come from the listener never having started.
	deadline := time.Now().Add(3 * time.Second)
	var held bool
	for time.Now().Before(deadline) {
		if holders, err := observe.SystemListenSet(); err == nil {
			if _, ok := holders[port]; ok {
				held = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !held {
		t.Fatalf("precondition: port %d never observed listening (pid %d)", port, container.pid)
	}

	statuses := eng.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected one status, got %d", len(statuses))
	}
	if statuses[0].State != StateUp {
		t.Errorf("detached server with its declared port listening: State = %v, want %v — "+
			"the launcher's exit is by design and must not make the service unreportable",
			statuses[0].State, StateUp)
	}
	if len(statuses[0].Ports) != 1 || !statuses[0].Ports[0].Up {
		t.Errorf("declared port %d rendered as not listening: %+v", port, statuses[0].Ports)
	}
}

// TestStatusForLocked_DetachedPortNotListening_ReportsDown is the other half,
// and it is what stops the fix above from becoming "detached servers always
// look up". A container that is genuinely gone must still report down.
func TestStatusForLocked_DetachedPortNotListening_ReportsDown(t *testing.T) {
	port := freeTestPort(t) // deliberately never bound

	launcher := spawnForeignListener(t, freeTestPort(t))
	launcher.forceKill()

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{detachedServer(port)},
	}
	eng := NewEngine(cfg, nil, nil)
	eng.procs["svc"] = &lifecycle.Process{Cmd: launcher.cmd, LogPath: "detached-down.log"}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateDown {
		t.Fatalf("a detached server whose port is not listening must report down, got %+v", statuses)
	}
}
