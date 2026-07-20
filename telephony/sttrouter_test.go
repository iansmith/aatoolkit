package telephony

import (
	"context"
	"testing"
	"time"
)

// startRouter builds a router over a fresh results channel and runs it until
// the test ends.
func startRouter(t *testing.T) (*STTRouter, *BufferedChan[STTResult]) {
	t.Helper()
	results := NewBufferedChan[STTResult](8)
	router := NewSTTRouter(results)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go router.Run(ctx)
	return router, results
}

// recvResult receives one result from route or fails the test after a
// timeout.
func recvResult(t *testing.T, route STTOutput) STTResult {
	t.Helper()
	select {
	case res := <-route.Channel():
		return res
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for routed STT result")
		return STTResult{}
	}
}

// TestSTTRouterRoutesBySessionID: results interleaved across two registered
// sessions each arrive only on their own session's route.
func TestSTTRouterRoutesBySessionID(t *testing.T) {
	router, results := startRouter(t)
	routeA := router.Register("CA-a")
	routeB := router.Register("CA-b")

	ctx := context.Background()
	for _, res := range []STTResult{
		{SessionID: "CA-a", RequestID: 1, Text: "for a"},
		{SessionID: "CA-b", RequestID: 1, Text: "for b"},
		{SessionID: "CA-a", RequestID: 2, Text: "for a again"},
	} {
		if err := results.Send(ctx, res); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if got := recvResult(t, routeA); got.Text != "for a" {
		t.Errorf("routeA first result: got %q, want %q", got.Text, "for a")
	}
	if got := recvResult(t, routeB); got.Text != "for b" {
		t.Errorf("routeB result: got %q, want %q", got.Text, "for b")
	}
	if got := recvResult(t, routeA); got.Text != "for a again" {
		t.Errorf("routeA second result: got %q, want %q", got.Text, "for a again")
	}
	select {
	case res := <-routeB.Channel():
		t.Errorf("routeB received unexpected extra result: %+v", res)
	default:
	}
}

// TestSTTRouterDropsUnregisteredSession: a result for a session that was
// never registered (or already deregistered) is dropped without disturbing
// delivery to live sessions -- the exact stale-result scenario observed
// live, where a dead call's late transcript leaked into the next call.
func TestSTTRouterDropsUnregisteredSession(t *testing.T) {
	router, results := startRouter(t)
	route := router.Register("CA-live")

	ctx := context.Background()
	// Stale result for a call that ended before the router ever saw it.
	if err := results.Send(ctx, STTResult{SessionID: "CA-dead", RequestID: 7}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := results.Send(ctx, STTResult{SessionID: "CA-live", RequestID: 1, Text: "live"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := recvResult(t, route); got.Text != "live" {
		t.Errorf("live route: got %q, want %q", got.Text, "live")
	}
}

// TestSTTRouterDeregisterStopsDelivery: after Deregister, further results
// for that session are dropped instead of delivered.
//
// The absence is proven by ordering, not by waiting. Run dispatches results
// one at a time in the order they arrive, so a result sent for a still-live
// session *after* the deregistered one acts as a fence: once it has been
// delivered, the earlier result has already been through the dispatch path,
// and the dead route is either empty or it isn't. Sleeping instead would only
// establish that a wrong delivery is not fast.
func TestSTTRouterDeregisterStopsDelivery(t *testing.T) {
	router, results := startRouter(t)
	route := router.Register("CA-x")
	fence := router.Register("CA-fence")

	ctx := context.Background()
	if err := results.Send(ctx, STTResult{SessionID: "CA-x", RequestID: 1, Text: "before"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := recvResult(t, route); got.Text != "before" {
		t.Fatalf("pre-deregister result: got %q, want %q", got.Text, "before")
	}

	router.Deregister("CA-x", route)
	if err := results.Send(ctx, STTResult{SessionID: "CA-x", RequestID: 2, Text: "after"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := results.Send(ctx, STTResult{SessionID: "CA-fence", RequestID: 3, Text: "fence"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Once the fence has landed, the "after" result has already been routed.
	if got := recvResult(t, fence); got.Text != "fence" {
		t.Fatalf("fence result: got %q, want %q", got.Text, "fence")
	}
	select {
	case res := <-route.Channel():
		t.Errorf("received result after deregister: %+v", res)
	default:
	}
}

// TestSTTRouterFullRouteDoesNotStallOthers: a session that stops draining
// its route fills it, and the router drops that session's overflow instead
// of blocking -- other sessions keep receiving.
func TestSTTRouterFullRouteDoesNotStallOthers(t *testing.T) {
	router, results := startRouter(t)
	_ = router.Register("CA-stuck") // never drained
	live := router.Register("CA-live")

	ctx := context.Background()
	for i := 0; i < sttRouteDepth+4; i++ {
		if err := results.Send(ctx, STTResult{SessionID: "CA-stuck", RequestID: i}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if err := results.Send(ctx, STTResult{SessionID: "CA-live", RequestID: 1, Text: "still flowing"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := recvResult(t, live); got.Text != "still flowing" {
		t.Errorf("live route after stuck session overflow: got %q, want %q", got.Text, "still flowing")
	}
}

// TestSTTRouterDuplicateRegistrationDoesNotUnrouteLiveCall pins the identity
// check in Deregister.
//
// Two calls sharing a CallSID is something Twilio guarantees against, but
// twilio-cli is a stand-in that mints its own -- and Register logs a WARN
// about it rather than rejecting it, so the case is reachable by design. What
// must not happen is the older call's teardown taking the newer, live call's
// route with it: that would drop every result for a call that is still going,
// for the rest of its life, because an unrelated one hung up.
func TestSTTRouterDuplicateRegistrationDoesNotUnrouteLiveCall(t *testing.T) {
	router, results := startRouter(t)

	first := router.Register("CA-dup")  // call A
	second := router.Register("CA-dup") // call B replaces A's route
	if first == second {
		t.Fatal("re-registering a SessionID must mint a fresh route, not reuse the old one")
	}

	// Call A ends. Its route is already gone; it must not take B's.
	router.Deregister("CA-dup", first)

	ctx := context.Background()
	if err := results.Send(ctx, STTResult{SessionID: "CA-dup", RequestID: 1, Text: "still live"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := recvResult(t, second); got.Text != "still live" {
		t.Errorf("live call's route after the duplicate's teardown: got %q, want %q", got.Text, "still live")
	}

	// And B's own teardown does still remove it.
	router.Deregister("CA-dup", second)
	fence := router.Register("CA-fence")
	if err := results.Send(ctx, STTResult{SessionID: "CA-dup", RequestID: 2, Text: "after"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := results.Send(ctx, STTResult{SessionID: "CA-fence", RequestID: 3, Text: "fence"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := recvResult(t, fence); got.Text != "fence" {
		t.Fatalf("fence result: got %q, want %q", got.Text, "fence")
	}
	select {
	case res := <-second.Channel():
		t.Errorf("received result after the live call deregistered: %+v", res)
	default:
	}
}
