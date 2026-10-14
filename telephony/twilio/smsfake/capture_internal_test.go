package smsfake

import (
	"fmt"
	"testing"
	"time"
)

// TestSubscription_EnqueueLeavesAWakeUpPendingOnceQueued pins the two halves
// of enqueue that keep a message from stranding in a subscription's queue.
// pump finds the queue empty, lets the lock go, and only then parks on wake,
// so an enqueue can land between the two, and the message it queues is
// delivered only if its wake-up is still waiting when pump parks. So wake must
// keep a wake-up nobody is receiving yet — it is buffered by one — and no
// wake-up may reach pump before the message it announces can: one sent before
// enqueue takes the lock can be taken by a pump that then finds the queue
// still empty and parks again, with nothing left to wake it for the message
// queued behind.
//
// No test through the server sees either reliably. A POST over the network
// gives pump time to park before the next one arrives, and in a flood of
// arrivals the next one's wake-up recovers a lost one, so only a flood's last
// message can strand, and only when the scheduler lands it in that gap.
//
// Here the subscription has ended first, so pump has returned and nothing
// takes the wake-up: what enqueue leaves on wake is what a pump not yet parked
// would find. While the subscription's lock is held, enqueue cannot queue its
// message, so no wake-up may be pending; once the lock is released, exactly
// one must be. A wake-up sent under the lock but before the append passes, as
// it should: pump cannot look at the queue until the message is in it. The
// pass side cannot flake: enqueue blocks on the lock before it can send. The
// fail side relies on enqueue reaching its send within the hold window, which
// it does with plenty to spare.
func TestSubscription_EnqueueLeavesAWakeUpPendingOnceQueued(t *testing.T) {
	s := newSubscription()
	s.end()

	release := holdLock(t, &s.mu)
	enqueued := make(chan struct{})
	go func() {
		defer close(enqueued)
		s.enqueue(Message{Body: "announced"})
	}()

	time.Sleep(HoldWindow)
	if n := len(s.wake); n != 0 {
		t.Fatalf("%d wake-up(s) pending while the message could not yet be queued, want none: queue first, then wake", n)
	}

	release()
	select {
	case <-enqueued:
	case <-time.After(SubscribeWait):
		t.Fatalf("enqueue did not return within %s of the lock being released, want it never to wait", SubscribeWait)
	}
	if n := len(s.wake); n != 1 {
		t.Errorf("%d wake-up(s) pending once the message was queued with no feed parked, want 1 kept for the feed to find", n)
	}
}

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
