package twilio

import (
	"testing"
	"time"
)

// Gaps found reviewing AATK-105's mark seam, in the shape the sibling
// *_gaps_test.go files use: driven against markTracker directly, because the
// interleaving under test is between one arming and the previous arming's
// bound and cannot be provoked through the handler.

// TestMarkTracker_SpentBoundDoesNotResolveTheReArmThatReplacedIt pins the
// identity check in expire.
//
// time.Timer.Stop cannot unwind a callback that has already begun, so a mark
// re-armed under its own name — which arm explicitly allows, logging and
// re-arming rather than refusing — can have the PREVIOUS bound's callback
// still in flight, blocked on markTracker.mu. Matching on the name alone, that
// callback resolves whichever arming replaced it: the consumer gets a TimedOut
// record for a mark still well inside its bound, and the real echo that
// follows matches nothing outstanding and is discarded.
//
// Calling expire with the spent arming's number is that callback, arriving
// late — the same call time.AfterFunc would have made, at the only moment the
// race makes it wrong.
func TestMarkTracker_SpentBoundDoesNotResolveTheReArmThatReplacedIt(t *testing.T) {
	echoes := make(chan MarkEcho, 4)
	tr := newMarkTracker(echoes)

	tr.arm("goodbye", time.Hour)
	tr.mu.Lock()
	spent := tr.outstanding["goodbye"].gen
	tr.mu.Unlock()

	// The consumer reuses the name; the first arming's bound is now spent.
	tr.arm("goodbye", time.Hour)

	tr.expire("goodbye", spent)

	select {
	case rec := <-echoes:
		t.Fatalf("a spent bound must not resolve the arming that replaced it, got %+v", rec)
	default:
	}

	// And the live arming is still outstanding, so its real echo still lands.
	tr.echo("goodbye")
	select {
	case rec := <-echoes:
		if rec.Name != "goodbye" || rec.TimedOut {
			t.Fatalf("the re-armed mark's echo must be delivered as a match, got %+v", rec)
		}
	default:
		t.Fatal("the re-armed mark was no longer outstanding, so its echo was discarded")
	}
}

// TestMarkTracker_BoundStillFiresForTheLiveArming is the control on the test
// above: the identity check must reject a SPENT arming's callback, not every
// callback. A check that always returned early would take the test above green
// while removing the bound the ticket's fourth behavior turns on.
func TestMarkTracker_BoundStillFiresForTheLiveArming(t *testing.T) {
	echoes := make(chan MarkEcho, 4)
	tr := newMarkTracker(echoes)

	tr.arm("goodbye", time.Millisecond)

	select {
	case rec := <-echoes:
		if rec.Name != "goodbye" || !rec.TimedOut {
			t.Fatalf("a bound that elapsed must report the mark as timed out, got %+v", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound never fired")
	}
}
