package smsfake

import "time"

// SubscribeWait bounds the tests' waits on a subscription, a wake-up, a POST
// to the capture server, an unsubscribe or a Close, through collect,
// postClient, returnsWithin and receiveN, or directly, and their waits on
// what a webhook server or its handler sends, through receiveWithin. It is a
// failure detector only: a correct server delivers in milliseconds, and no
// test sleeps to let delivery happen.
//
// Not every wait has a bound. A webhook test's POST through PostWebhook is
// bounded by its ctx alone, mostly context.Background(); the internal tests
// end a subscription unbounded; and the tests moved here from twilio-cli,
// TestNewCaptureServer_* and the two 403 tests, keep the unbounded waits they
// had there.
const SubscribeWait = 5 * time.Second

// SubscriberCount reports how many subscriptions c holds. It is for the tests,
// which otherwise cannot see that unsubscribe and Close release what they end:
// a subscription left in the set after its feed has stopped would go on
// collecting a copy of every later message, with nothing ever reading them.
func (c *CaptureServer) SubscriberCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}
