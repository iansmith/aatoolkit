package telephony

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestTimerFacilityArm verifies that Arm starts a timer and fires a completion
// within the expected duration.
func TestTimerFacilityArm(t *testing.T) {
	ctx := context.Background()
	tf := NewTimerFacility(ctx)

	timerId := tf.Arm(ctx, "test", 10*time.Millisecond)

	select {
	case completion := <-tf.Completions():
		if completion.Name != "test" {
			t.Errorf("expected completion.Name == 'test', got %q", completion.Name)
		}
		if completion.TimerID != timerId {
			t.Errorf("expected completion.TimerID == %d, got %d", timerId, completion.TimerID)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected completion within 50ms")
	}
}

// TestTimerFacilityCancel verifies that cancelling a timer prevents its
// completion from being sent.
func TestTimerFacilityCancel(t *testing.T) {
	ctx := context.Background()
	tf := NewTimerFacility(ctx)

	tf.Arm(ctx, "test", 50*time.Millisecond)
	tf.Cancel("test")

	select {
	case <-tf.Completions():
		t.Fatal("expected no completion after Cancel")
	case <-time.After(200 * time.Millisecond):
		// Success — no completion was sent
	}
}

// TestTimerFacilityRearm verifies that re-arming a name cancels the previous
// timer and arms a new one with an incremented generation counter.
func TestTimerFacilityRearm(t *testing.T) {
	ctx := context.Background()
	tf := NewTimerFacility(ctx)

	timerId1 := tf.Arm(ctx, "test", 200*time.Millisecond)
	timerId2 := tf.Arm(ctx, "test", 10*time.Millisecond)

	if timerId2 <= timerId1 {
		t.Errorf("expected timerId2 > timerId1, got %d > %d", timerId2, timerId1)
	}

	select {
	case completion := <-tf.Completions():
		if completion.TimerID != timerId2 {
			t.Errorf("expected completion.TimerID == %d (second Arm), got %d", timerId2, completion.TimerID)
		}
		if !tf.IsCurrent(completion) {
			t.Errorf("expected IsCurrent to return true for current completion")
		}

		// Should not receive a second completion from the first Arm
		select {
		case <-tf.Completions():
			t.Fatal("expected only one completion (the second one)")
		case <-time.After(100 * time.Millisecond):
			// Success — only one completion received
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected completion within 100ms")
	}
}

// TestTimerFacilityTeardown verifies that cancelling the facility's context
// exits all goroutines cleanly without leaks.
func TestTimerFacilityTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tf := NewTimerFacility(ctx)

	// Record initial goroutine count
	initialGoroutines := runtime.NumGoroutine()

	// Arm multiple timers to ensure we clean them all up
	for i := 0; i < 5; i++ {
		tf.Arm(ctx, "timer"+string(rune('0'+i)), 10*time.Second)
	}

	// Cancel the context — should stop all goroutines
	cancel()

	// Give goroutines time to exit
	time.Sleep(100 * time.Millisecond)

	// Verify no goroutine leaks
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines {
		t.Fatalf("expected %d goroutines after teardown, got %d (leaked %d)",
			initialGoroutines, finalGoroutines, finalGoroutines-initialGoroutines)
	}
}

// TestTimerFacilityMultipleNames verifies that multiple names can be armed
// simultaneously and fire independently.
func TestTimerFacilityMultipleNames(t *testing.T) {
	ctx := context.Background()
	tf := NewTimerFacility(ctx)

	timerId1 := tf.Arm(ctx, "name1", 10*time.Millisecond)
	timerId2 := tf.Arm(ctx, "name2", 20*time.Millisecond)

	completions := make(map[string]int)
	timeout := time.After(100 * time.Millisecond)

	for len(completions) < 2 {
		select {
		case completion := <-tf.Completions():
			completions[completion.Name] = completion.TimerID
		case <-timeout:
			t.Fatalf("expected 2 completions, got %d", len(completions))
		}
	}

	if completions["name1"] != timerId1 {
		t.Errorf("expected name1 completion with timerId %d, got %d", timerId1, completions["name1"])
	}
	if completions["name2"] != timerId2 {
		t.Errorf("expected name2 completion with timerId %d, got %d", timerId2, completions["name2"])
	}
}

// TestTimerFacilityActiveTimersIsBounded verifies that firing many
// never-repeated timer names does not grow activeTimers without bound.
func TestTimerFacilityActiveTimersIsBounded(t *testing.T) {
	ctx := context.Background()
	tf := NewTimerFacility(ctx)

	const n = maxSettledTimers * 2
	for i := 0; i < n; i++ {
		name := "unique" + string(rune(i))
		tf.Arm(ctx, name, time.Millisecond)
		select {
		case <-tf.Completions():
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("expected completion %d within 50ms", i)
		}
	}

	if len(tf.activeTimers) > maxSettledTimers+1 {
		t.Fatalf("expected activeTimers bounded near %d, got %d", maxSettledTimers, len(tf.activeTimers))
	}
}
