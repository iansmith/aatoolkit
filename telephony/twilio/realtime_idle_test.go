package twilio

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Idle-timeout tests for HandleStreamRealtime (AATK-76). Once the backend
// dial succeeds, nothing previously bounded how long the call could run with
// both the backend and the carrier silent — idleTimeout closes that gap.
// Fixtures (fakeRealtimeBackend, newRealtimeHarnessWith) come from
// realtime_wiring_test.go.

// TestHandleStreamRealtime_IdleTimeoutEndsSilentCall pins the core guarantee:
// a backend that completes the handshake and then sends nothing, paired with
// a carrier that sends nothing after the start frame, must not hold the call
// open indefinitely once a positive idleTimeout is supplied. The call must
// end within idleTimeout plus a bounded margin, with an error naming the
// reason.
func TestHandleStreamRealtime_IdleTimeoutEndsSilentCall(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	const idleTimeout = 100 * time.Millisecond
	h := newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(), idleTimeout)
	})

	// The harness's own start frame is the only thing either side ever sends;
	// after that both the backend (post-handshake) and the carrier stay
	// silent, which is exactly the case idleTimeout exists to bound.
	err := h.waitDone(idleTimeout + 500*time.Millisecond)
	if err == nil {
		t.Fatal("a silent backend and silent carrier must end the call with a non-nil error once idleTimeout is set")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("error must name the idle timeout, got: %v", err)
	}
}

// TestHandleStreamRealtime_IdleTimeoutDoesNotFireDuringActiveTraffic pins the
// other half of the contract: activity resets the timer, so a call that keeps
// receiving backend events faster than idleTimeout apart must run exactly as
// it does today — the idle timer must never end it. The call is only ended
// when the harness closes the backend, and it must end for that reason, not
// "idle timeout".
func TestHandleStreamRealtime_IdleTimeoutDoesNotFireDuringActiveTraffic(t *testing.T) {
	const idleTimeout = 100 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	be.emitInterval = idleTimeout / 4 // well inside idleTimeout, every time

	h := newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(), idleTimeout)
	})

	// Run for several multiples of idleTimeout while the backend keeps
	// talking — long enough that a timer armed without resetting on activity
	// would already have fired.
	time.Sleep(idleTimeout * 6)

	select {
	case err := <-h.done:
		t.Fatalf("active traffic faster than idleTimeout apart must not end the call, got: %v", err)
	default:
		// Still running, as it must be.
	}

	be.closeNow()

	err := h.waitDone(5 * time.Second)
	if err == nil {
		t.Fatal("the backend going away must end the call with a non-nil error")
	}
	if strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("call must not end via the idle timer while active traffic was flowing, got: %v", err)
	}
}
