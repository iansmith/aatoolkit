package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests describe the expected SOP-18 signal-handling behavior:
//   - SIGINT is swallowed with a hint on the 1st/2nd delivery within a
//     rolling 2s window, and triggers the same teardown as "bye" on the 3rd.
//   - SIGTSTP is always swallowed and never advances or resets the SIGINT
//     burst counter.
//
// They fail against current code because sigintCounter, handleSignal, and
// runSignalLoop do not exist yet.

var t0 = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// --- sigintCounter: edge / boundary cases ---

func TestSigintCounter_FirstCallNeverTriggers(t *testing.T) {
	c := &sigintCounter{}
	if c.register(t0) {
		t.Fatal("first SIGINT ever must not trigger teardown")
	}
}

func TestSigintCounter_SecondWithinWindowDoesNotTrigger(t *testing.T) {
	c := &sigintCounter{}
	c.register(t0)
	if c.register(t0.Add(1 * time.Second)) {
		t.Fatal("second SIGINT within the window must not trigger teardown")
	}
}

func TestSigintCounter_ThirdWithinWindowTriggers(t *testing.T) {
	c := &sigintCounter{}
	c.register(t0)
	c.register(t0.Add(500 * time.Millisecond))
	if !c.register(t0.Add(1900 * time.Millisecond)) {
		t.Fatal("third SIGINT within the 2s window must trigger teardown")
	}
}

func TestSigintCounter_BurstGoesStaleResetsCount(t *testing.T) {
	// Two SIGINTs, then a long gap (> window) before a third: the third
	// must NOT complete a "3 total" count — the stale burst resets and the
	// third becomes a fresh "1st" instead.
	c := &sigintCounter{}
	c.register(t0)
	c.register(t0.Add(1 * time.Second))
	if c.register(t0.Add(10 * time.Second)) {
		t.Fatal("a third SIGINT arriving long after the window expired must not trigger teardown — the burst should have gone stale")
	}
	// Confirm it was treated as a fresh 1st, not a fresh 2nd/3rd: two more
	// rapid deliveries right after should now be needed to trigger.
	if c.register(t0.Add(10500 * time.Millisecond)) {
		t.Fatal("expected the stale-reset burst's 2nd delivery to still not trigger")
	}
	if !c.register(t0.Add(11 * time.Second)) {
		t.Fatal("expected the stale-reset burst's 3rd delivery to trigger")
	}
}

// Adversary gap (Step 0f, state-interaction): a completed 3x burst must
// leave the counter in a clean state, not "stuck triggered" or somehow
// still primed — a later, independent burst must still need its own
// fresh 3 deliveries.
func TestSigintCounter_ResetsAfterTriggerAllowingANewBurstLater(t *testing.T) {
	c := &sigintCounter{}
	c.register(t0)
	c.register(t0.Add(500 * time.Millisecond))
	if !c.register(t0.Add(1 * time.Second)) {
		t.Fatal("expected the first burst's 3rd delivery to trigger")
	}

	// A wholly new burst, well after the first: must require its own 3
	// deliveries, not immediately trigger on the very next SIGINT.
	if c.register(t0.Add(30 * time.Second)) {
		t.Fatal("expected the counter to have reset after triggering — a single new SIGINT must not immediately re-trigger")
	}
	if c.register(t0.Add(30500 * time.Millisecond)) {
		t.Fatal("expected the new burst's 2nd delivery to still not trigger")
	}
	if !c.register(t0.Add(31 * time.Second)) {
		t.Fatal("expected the new burst's 3rd delivery to trigger")
	}
}

func TestSigintCounter_ExactlyAtWindowBoundaryStillCounts(t *testing.T) {
	// now.Sub(first) == sigintWindow exactly must still be "within the
	// window" (only strictly greater than the window goes stale).
	c := &sigintCounter{}
	c.register(t0)
	c.register(t0.Add(sigintWindow))
	if !c.register(t0.Add(sigintWindow)) {
		t.Fatal("a delivery exactly at the window boundary must still count toward the burst")
	}
}

// --- handleSignal: SIGTSTP is always swallowed silently ---

func TestHandleSignal_SigtstpNeverPrintsOrTearsDown(t *testing.T) {
	eng := &fakeEngine{}
	c := &sigintCounter{}
	var out strings.Builder

	for i := 0; i < 3; i++ {
		if handleSignal(syscall.SIGTSTP, c, t0, &out, eng) {
			t.Fatal("SIGTSTP must never report a shutdown request")
		}
	}
	if out.Len() != 0 {
		t.Fatalf("SIGTSTP must produce no output at all, got %q", out.String())
	}
	if eng.teardownCalls != 0 {
		t.Fatalf("SIGTSTP must never call TeardownAll, got %d calls", eng.teardownCalls)
	}
}

func TestHandleSignal_SigtstpDoesNotAdvanceOrResetSigintCounter(t *testing.T) {
	eng := &fakeEngine{}
	c := &sigintCounter{}
	var out strings.Builder

	// SIGINT, SIGTSTP, SIGINT, SIGTSTP, SIGINT — the SIGTSTPs must be
	// transparent noise: the 3rd real SIGINT (5th call overall) must
	// still be the one that triggers teardown, not delayed or skipped.
	if handleSignal(os.Interrupt, c, t0, &out, eng) {
		t.Fatal("1st SIGINT must not trigger")
	}
	if handleSignal(syscall.SIGTSTP, c, t0.Add(200*time.Millisecond), &out, eng) {
		t.Fatal("interspersed SIGTSTP must not trigger")
	}
	if handleSignal(os.Interrupt, c, t0.Add(400*time.Millisecond), &out, eng) {
		t.Fatal("2nd SIGINT must not trigger")
	}
	if handleSignal(syscall.SIGTSTP, c, t0.Add(600*time.Millisecond), &out, eng) {
		t.Fatal("interspersed SIGTSTP must not trigger")
	}
	if !handleSignal(os.Interrupt, c, t0.Add(800*time.Millisecond), &out, eng) {
		t.Fatal("3rd real SIGINT must trigger despite interspersed SIGTSTPs")
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected exactly one teardown, got %d", eng.teardownCalls)
	}
}

// --- handleSignal: SIGINT hint + teardown-parity cross-feature checks ---

func TestHandleSignal_FirstAndSecondSigintPrintExactHintAndDoNotTeardown(t *testing.T) {
	eng := &fakeEngine{}
	c := &sigintCounter{}
	var out strings.Builder

	if handleSignal(os.Interrupt, c, t0, &out, eng) {
		t.Fatal("1st SIGINT must not report shutdown")
	}
	if handleSignal(os.Interrupt, c, t0.Add(time.Second), &out, eng) {
		t.Fatal("2nd SIGINT must not report shutdown")
	}
	if eng.teardownCalls != 0 {
		t.Fatalf("expected no teardown after only 2 SIGINTs, got %d calls", eng.teardownCalls)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != interruptHint || lines[1] != interruptHint {
		t.Fatalf("expected the exact hint printed twice, got %q", out.String())
	}
}

func TestHandleSignal_ThirdSigintUsesSameTeardownPathAsBye(t *testing.T) {
	eng := &fakeEngine{teardownReturn: []string{"server"}}
	c := &sigintCounter{}
	var out strings.Builder

	handleSignal(os.Interrupt, c, t0, &out, eng)
	handleSignal(os.Interrupt, c, t0.Add(500*time.Millisecond), &out, eng)
	triggered := handleSignal(os.Interrupt, c, t0.Add(1*time.Second), &out, eng)

	if !triggered {
		t.Fatal("3rd SIGINT within the window must report a shutdown request")
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once, got %d", eng.teardownCalls)
	}
	// teardown() in repl.go formats "tearing down %d owned server(s): %v\n"
	// — the SIGINT path must render the identical message, not a bespoke one.
	want := "tearing down 1 owned server(s): [server]"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("expected output to contain the shared teardown message %q, got %q", want, out.String())
	}
}

// --- runSignalLoop: integration of the counter + handler over a channel ---

func TestRunSignalLoop_ThreeRapidSigintsExitsZeroAndTearsDownOnce(t *testing.T) {
	eng := &fakeEngine{}
	var out strings.Builder
	sigCh := make(chan os.Signal, 8)
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt

	var exitCode int
	exitCalls := 0
	exit := func(code int) { exitCode = code; exitCalls++ }

	runSignalLoop(sigCh, &out, eng, exit)

	if exitCalls != 1 {
		t.Fatalf("expected exit called exactly once, got %d", exitCalls)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit(0) to match the 'bye' exit code, got exit(%d)", exitCode)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once, got %d", eng.teardownCalls)
	}
}

func TestRunSignalLoop_SigtstpInterspersedDoesNotDelayShutdown(t *testing.T) {
	eng := &fakeEngine{}
	var out strings.Builder
	sigCh := make(chan os.Signal, 8)
	sigCh <- syscall.SIGTSTP
	sigCh <- os.Interrupt
	sigCh <- syscall.SIGTSTP
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt

	exitCalls := 0
	exit := func(code int) { exitCalls++ }

	runSignalLoop(sigCh, &out, eng, exit)

	if exitCalls != 1 {
		t.Fatalf("expected exit called exactly once even with interspersed SIGTSTP, got %d", exitCalls)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected exactly one teardown, got %d", eng.teardownCalls)
	}
}

func TestRunSignalLoop_StopsConsumingAfterShutdownTriggered(t *testing.T) {
	// After the 3rd SIGINT triggers exit, the loop must return rather than
	// keep draining the channel (a stray 4th signal must not be processed).
	eng := &fakeEngine{}
	var out strings.Builder
	sigCh := make(chan os.Signal, 8)
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt
	sigCh <- os.Interrupt // must never be consumed by runSignalLoop

	exitCalls := 0
	exit := func(code int) { exitCalls++ }

	runSignalLoop(sigCh, &out, eng, exit)

	if exitCalls != 1 {
		t.Fatalf("expected exit called exactly once, got %d", exitCalls)
	}
	if len(sigCh) != 1 {
		t.Fatalf("expected the loop to stop right after triggering shutdown, leaving the 4th signal undrained; got %d left in channel", len(sigCh))
	}
}
