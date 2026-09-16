package main

import (
	"bufio"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- RunAuto: up mode ---

// TestRunAuto_UpCallsFleetUpAndBlocksUntilStop pins the core contract:
// --auto up brings the fleet up (Up("") for all enabled servers) and then
// blocks until the stop channel closes — it must NOT return on its own the
// way a piped REPL does on EOF.
func TestRunAuto_UpCallsFleetUpAndBlocksUntilStop(t *testing.T) {
	eng := &fakeEngine{}
	var out strings.Builder
	stop := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- RunAuto("up", &out, eng, stop)
	}()

	// Wait for RunAuto to call Up and reach the blocking <-stop.
	// fakeEngine.Up is synchronous, so once upCalls is populated the
	// goroutine is past Up and blocked on the channel.
	deadline := time.After(2 * time.Second)
	for len(eng.upCalls) == 0 {
		select {
		case <-deadline:
			t.Fatal("RunAuto did not call Up within timeout")
		default:
			runtime.Gosched()
		}
	}

	if eng.upCalls[0] != "" {
		t.Fatalf("expected Up(\"\") called once for fleet-wide up, got %v", eng.upCalls)
	}

	// Must NOT have torn down while still running.
	if eng.teardownCalls != 0 {
		t.Fatalf("expected no teardown while blocked, got %d", eng.teardownCalls)
	}

	// Must not have returned yet.
	select {
	case err := <-done:
		t.Fatalf("RunAuto returned prematurely (err=%v); it must block until stop", err)
	default:
	}

	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunAuto did not return after stop was closed")
	}

	if eng.teardownCalls != 1 {
		t.Fatalf("expected teardown after stop, got %d", eng.teardownCalls)
	}
}

// TestRunAuto_UpErrorReturnedImmediately pins: when Up("") fails, RunAuto
// returns that error without blocking — a half-up fleet must not sit there
// waiting for a signal.
func TestRunAuto_UpErrorReturnedImmediately(t *testing.T) {
	eng := &fakeEngine{failNotImplemented: true}
	var out strings.Builder
	stop := make(chan struct{})

	err := RunAuto("up", &out, eng, stop)
	if err == nil {
		t.Fatal("expected error from failing Up, got nil")
	}
	if eng.teardownCalls != 0 {
		t.Fatalf("a failed Up must not trigger teardown, got %d calls", eng.teardownCalls)
	}
}

// --- RunAuto: down mode ---

// TestRunAuto_DownCallsPerServerDownAndReturns pins: --auto down iterates
// enabled servers and calls Down(name) for each, so the per-server path's
// port-discovery teardown is used (not the fleet-wide path that only
// touches owned servers).
func TestRunAuto_DownCallsPerServerDownAndReturns(t *testing.T) {
	eng := &fakeEngine{
		statuses: []ServerStatus{
			{Name: "web", Enabled: true},
			{Name: "worker", Enabled: true},
			{Name: "debug", Enabled: false},
		},
	}
	var out strings.Builder

	err := RunAuto("down", &out, eng, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(eng.downCalls) != 2 {
		t.Fatalf("expected Down called for each enabled server, got %v", eng.downCalls)
	}
	if eng.downCalls[0] != "web" || eng.downCalls[1] != "worker" {
		t.Fatalf("expected Down(\"web\"), Down(\"worker\"); got %v", eng.downCalls)
	}
}

// TestRunAuto_DownErrorReturnedForExitCode pins: a failing Down returns
// its error so main can exit non-zero — operators and Ansible need a clear
// signal that the fleet did not come down cleanly.
func TestRunAuto_DownErrorReturnedForExitCode(t *testing.T) {
	eng := &fakeEngine{
		failNotImplemented: true,
		statuses:           []ServerStatus{{Name: "web", Enabled: true}},
	}
	var out strings.Builder

	err := RunAuto("down", &out, eng, nil)
	if err == nil {
		t.Fatal("expected error from failing Down, got nil")
	}
	if !strings.Contains(out.String(), ErrorMark) {
		t.Errorf("down failure output = %q, want it to contain %q so software_update can grep the log", out.String(), ErrorMark)
	}
}

// --- RunAuto: existing REPL unchanged ---

// TestRunAuto_DoesNotAffectREPLPath confirms that the REPL path (Run) is
// completely independent: EOF still tears down, as it always did.
func TestRunAuto_DoesNotAffectREPLPath(t *testing.T) {
	eng := &fakeEngine{teardownReturn: []string{"server"}}
	in := strings.NewReader("")
	var out strings.Builder

	if err := Run(bufio.NewReader(in), &out, eng); err != nil {
		t.Fatalf("Run on EOF: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("REPL EOF must still teardown, got %d", eng.teardownCalls)
	}
}
