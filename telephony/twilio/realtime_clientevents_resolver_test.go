package twilio

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Resolver-ordering regression test for AATK-82.
//
// WithClientEventChanFor's resolver is CONSUMER code, and it runs on
// HandleStreamRealtime's own goroutine — the same property the transcript and
// server-event resolvers have, and the same reason their resolution order is
// documented as load-bearing (a serverEventChanFor sleeping 3 s is the
// measured example in that comment). Nothing bounds how long a consumer's
// resolver takes.
//
// If the idle guard is armed BEFORE that resolver runs, the guard spends its
// whole first window inside consumer code and is already expired when the
// select loop is finally entered — so a call whose backend has been perfectly
// healthy the entire time ends immediately with "idle timeout", which is a
// misattribution of exactly the kind WithIdleTimeout exists to avoid making.
//
// slopstop:test regression — guards: "arming the idle guard happens after the client-event resolver runs, so a slow consumer resolver cannot spend the guard's first window and end a healthy call with a bogus idle timeout"
func TestClientEventChanFor_SlowResolverDoesNotBurnTheIdleWindow(t *testing.T) {
	const (
		idleTimeout  = 1500 * time.Millisecond
		resolverWork = 2 * time.Second
		// Past when a guard armed too early would have fired (≈resolverWork),
		// and well short of when a correctly armed one fires
		// (≈resolverWork+idleTimeout).
		checkAt = 2600 * time.Millisecond
	)

	be := newFakeRealtimeBackend(t)
	start := time.Now()
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithIdleTimeout(idleTimeout),
		WithClientEventChanFor(func(Frame) <-chan json.RawMessage {
			time.Sleep(resolverWork)
			// Never written to and never closed: this test is about the
			// resolver's cost, not about anything crossing the channel.
			return make(chan json.RawMessage)
		}),
	))
	waitBackendReady(t, be, h)

	// The backend deliberately emits nothing, so there is no bridge.Activity()
	// signal pending to compete with a prematurely fired timer — the ending is
	// decided by when the guard was armed and by nothing else.
	time.Sleep(time.Until(start.Add(checkAt)))
	assertStillRunning(t, h, "a slow client-event resolver must not spend the idle guard's first window")

	// The guard is deferred, not disarmed: the call must still end on it.
	err := h.waitDone(idleTimeout + 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("the idle guard must still fire once armed, got: %v", err)
	}
}
