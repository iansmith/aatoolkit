package twilio

import (
	"fmt"
	"testing"
)

// --- AATK-157: the pending-call map ServeHTTP fills for ServeStreams ---

// pendingSID and pendingFrom name the i-th call these tests record, so every
// entry carries values of its own and a lookup cannot pass by finding some
// other call's entry.
func pendingSID(i int) string  { return fmt.Sprintf("CA%05d", i) }
func pendingFrom(i int) string { return fmt.Sprintf("+1510555%04d", i) }

const pendingTo = "+14155550000"

// requirePendingSize fails unless the map and its eviction order both hold
// exactly want entries. Checking both catches a slot taken in one and not
// the other.
func requirePendingSize(t *testing.T, s *Server, want int) {
	t.Helper()
	if len(s.fromBySID) != want || len(s.fromOrder) != want {
		t.Fatalf("pending map holds %d entries, eviction order %d; want %d of each",
			len(s.fromBySID), len(s.fromOrder), want)
	}
}

// requirePending fails unless callSid looks up to exactly from and to.
func requirePending(t *testing.T, s *Server, callSid, from, to string) {
	t.Helper()
	if got := s.lookupCall(callSid); got.from != from || got.to != to {
		t.Fatalf("lookupCall(%q) = From %q To %q, want From %q To %q", callSid, got.from, got.to, from, to)
	}
}

// One CallSid past maxPendingFrom evicts the oldest, and only the oldest: its
// lookup comes back "" for both From and To, every later call is still
// there with its own values, and the map never holds more than the bound.
func TestRecordCall_EvictsOldestPastBound(t *testing.T) {
	s := &Server{}
	for i := 0; i <= maxPendingFrom; i++ {
		s.recordCall(pendingSID(i), pendingFrom(i), pendingTo)
		if n := len(s.fromBySID); n > maxPendingFrom {
			t.Fatalf("after recording %d calls the pending map holds %d entries, want at most %d",
				i+1, n, maxPendingFrom)
		}
	}

	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), "", "")
	for i := 1; i <= maxPendingFrom; i++ {
		requirePending(t, s, pendingSID(i), pendingFrom(i), pendingTo)
	}
}

// Recording a CallSid that is already pending replaces its From and To in
// place. With the map full, re-recording the second-oldest call evicts
// nothing: it takes no new slot. It also keeps its place in the eviction
// order, so the next two new calls evict the oldest and then it, not the
// call recorded after it.
func TestRecordCall_ReRecordUpdatesInPlace(t *testing.T) {
	s := &Server{}
	for i := 0; i < maxPendingFrom; i++ {
		s.recordCall(pendingSID(i), pendingFrom(i), pendingTo)
	}

	const newFrom, newTo = "+15105559999", "+15105558888"
	s.recordCall(pendingSID(1), newFrom, newTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), pendingFrom(0), pendingTo)
	requirePending(t, s, pendingSID(1), newFrom, newTo)

	s.recordCall(pendingSID(maxPendingFrom), pendingFrom(maxPendingFrom), pendingTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), "", "")
	requirePending(t, s, pendingSID(1), newFrom, newTo)

	s.recordCall(pendingSID(maxPendingFrom+1), pendingFrom(maxPendingFrom+1), pendingTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(1), "", "")
	requirePending(t, s, pendingSID(2), pendingFrom(2), pendingTo)
}
