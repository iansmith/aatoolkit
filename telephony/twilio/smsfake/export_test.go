package smsfake

import "time"

// SubscribeWait bounds every wait in these tests — on a subscription, a
// wake-up, a POST, an unsubscribe or a Close — through collect, postClient and
// returnsWithin, or directly. It is a failure detector only: a correct server
// delivers in milliseconds, and no test sleeps to let delivery happen.
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
