package smsfake

import (
	"fmt"
	"testing"
)

// TestSubscription_EndReleasesItsQueue pins that ending a subscription lets go
// of the messages it had not delivered. The unsubscribe func Subscribe returns
// holds its subscription for as long as the caller keeps the func, so a queue
// left in place once the feed has stopped stays reachable through it — every
// undelivered message, each with a Form of its own — though nothing can
// deliver them any more. Unsubscribing is how a subscriber that fell behind
// sheds its unbounded backlog, so it must not keep that backlog alive.
// unsubscribe and Close both end a subscription through end, which is what
// this drives.
//
// Nobody reads, so every message the feed has not taken in hand is still
// queued when the subscription ends. The queue must then be nil, not merely
// empty: an emptied slice still holds its array, and with it every message
// that array held.
func TestSubscription_EndReleasesItsQueue(t *testing.T) {
	const queued = 5
	s := newSubscription()
	for i := range queued {
		s.enqueue(Message{Body: fmt.Sprintf("never read %d", i)})
	}
	s.end()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue != nil {
		t.Errorf("an ended subscription still holds its queue (length %d, capacity %d), want it nil: released, not merely emptied", len(s.queue), cap(s.queue))
	}
}
