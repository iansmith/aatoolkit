package smsfake

import (
	"fmt"
	"testing"
	"time"
)

// SubscribeWait bounds the tests' waits on a subscription, a wake-up, a POST
// to the capture server, an unsubscribe or a Close, through Collect,
// postClient, returnsWithin and ReceiveWithin, or directly, and their waits on
// what a webhook server or its handler sends, through ReceiveWithin or, where
// the wait also watches PostWebhook's return, directly. It is a
// failure detector only: a correct server delivers in milliseconds, and no
// test sleeps to let delivery happen.
//
// Not every wait has a bound. A webhook test's POST through PostWebhook is
// bounded by its ctx alone, mostly context.Background(); the internal tests
// end a subscription unbounded; and the tests moved here from twilio-cli,
// TestNewCaptureServer_BindsRequestedPort, _PortInUseNamesThePort and
// _RejectsUnusablePorts and the two 403 tests, keep the unbounded waits they
// had there.
const SubscribeWait = 5 * time.Second

// ReceiveWithin receives one value from ch and returns it, failing the test
// unless it arrives within SubscribeWait. what names the value in a failure.
// The webhook tests wait through it on what a webhook server or its handler
// sends, so a PostWebhook or ServeSMS that answers without doing its part
// fails the test, naming what never came, rather than hanging it; the capture
// tests wait through it on a wake-up, and the internal tests on enqueue's
// return. It is here, in package smsfake, so the internal tests reach it too.
func ReceiveWithin[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(SubscribeWait):
		t.Fatalf("no %s within %s", what, SubscribeWait)
		var zero T
		return zero
	}
}

// Collect receives exactly n messages from msgs, and returns an error if the
// channel closes first or they have not all arrived within SubscribeWait, one
// bound on the whole collection. It reports through its error, so any
// goroutine can call it. Like ReceiveWithin, it is here so the internal tests
// reach it too.
func Collect(msgs <-chan Message, n int) ([]Message, error) {
	return collectUntil(msgs, fmt.Sprintf("all %d messages", n), func(got []Message) bool {
		return len(got) == n
	})
}

// CollectThrough receives from msgs up to and including the first message
// whose Body is last, failing as Collect does.
func CollectThrough(msgs <-chan Message, last string) ([]Message, error) {
	return collectUntil(msgs, fmt.Sprintf("the message %q", last), func(got []Message) bool {
		return len(got) > 0 && got[len(got)-1].Body == last
	})
}

// collectUntil receives from msgs until done reports true of everything
// received so far, and returns an error if the channel closes first or done
// has not been met within SubscribeWait. want names what done waits for.
func collectUntil(msgs <-chan Message, want string, done func(got []Message) bool) ([]Message, error) {
	deadline := time.NewTimer(SubscribeWait)
	defer deadline.Stop()
	var got []Message
	for !done(got) {
		select {
		case msg, ok := <-msgs:
			if !ok {
				return got, fmt.Errorf("subscription closed after %d messages, before %s arrived", len(got), want)
			}
			got = append(got, msg)
		case <-deadline.C:
			return got, fmt.Errorf("received %d messages within %s, not %s", len(got), SubscribeWait, want)
		}
	}
	return got, nil
}

// SubscriberCount reports how many subscriptions c holds. It is for the tests,
// which otherwise cannot see that unsubscribe and Close release what they end:
// a subscription left in the set after its feed has stopped would go on
// collecting a copy of every later message, with nothing ever reading them.
func (c *CaptureServer) SubscriberCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}
