package twilio

import (
	"runtime"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
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

// TestMarkRequestChan_ClosedChannelDoesNotBusyLoop is the measured half of the
// closed-channel guard, the sibling of
// TestClientEventChan_ClosedChannelDoesNotBusyLoop and
// TestVoiceUpdateChan_ClosedChannelDoesNotBusyLoop. The mark-request channel
// arrived with only the liveness half
// (TestMarkRequestChan_ClosedChannelLeavesTheCallRunning), and liveness cannot
// see this defect: an implementation returning the channel unchanged on a
// closed receive spins the select loop on the zero value forever while the
// call stays up and every other assertion in the suite still passes.
//
// Mutation-verified during review: replacing handleMarkRequest's
// `return nil, nil` with `return ch, nil` left
// TestMarkRequestChan_ClosedChannelLeavesTheCallRunning green.
//
// slopstop:test regression — guards: "a closed mark-request channel retires its select case rather than busy-looping on the zero value"
func TestMarkRequestChan_ClosedChannelDoesNotBusyLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("getrusage is not available on windows")
	}

	const window = 300 * time.Millisecond

	closed := make(chan string)
	close(closed)

	baseline := measureCallCPU(t, window)
	withClosed := measureCallCPU(t, window, WithMarkRequestChan(closed))

	if withClosed > baseline+50*time.Millisecond {
		t.Fatalf("a closed mark-request channel must not busy-loop: baseline CPU %v, with-closed-channel CPU %v (over a %v wall-clock window)",
			baseline, withClosed, window)
	}
}

// TestMarkEchoBound_KeepsTheSubMillisecondRemainder is what actually holds the
// bound to telephony.MuLawDuration, the module's one definition of the μ-law
// byte-to-duration conversion.
//
// TestMarkRequestChan_BoundCoversQueuedPlayout says it does that, but cannot:
// its byte count is telephony.SampleRateHz, a multiple of 8, and the truncating
// whole-millisecond spelling (`n * 1000 / SampleRateHz`) agrees with
// MuLawDuration exactly on multiples of 8. Mutation-verified during review —
// substituting that spelling in playoutClock.fed left it green.
//
// A byte count that is NOT a multiple of 8 is what separates them: 8001 bytes
// is 1.000125s exactly, and 1s under the truncating spelling. The difference is
// sub-millisecond by construction, so the assertion is on equality with
// MuLawDuration rather than on a tolerance.
//
// slopstop:test regression — guards: "playout arithmetic goes through
// telephony.MuLawDuration, never a restated longhand"
func TestMarkEchoBound_KeepsTheSubMillisecondRemainder(t *testing.T) {
	// Deliberately not a multiple of 8: the whole point is the remainder the
	// truncating spelling drops.
	const queued = telephony.SampleRateHz + 1

	sink := newCarrierMediaSink(&discardWSWriter{}, "SSremainder", nil, nil)
	base := time.Now()
	sink.playout.fed(queued, base)

	got := sink.markEchoBound(base)
	want := telephony.MuLawDuration(queued) + telephony.MarkEchoGraceMS*time.Millisecond
	if got != want {
		t.Fatalf("markEchoBound after %d bytes = %s, want %s; a truncating whole-millisecond spelling would give %s",
			queued, got, want,
			time.Duration(queued*1000/telephony.SampleRateHz)*time.Millisecond+telephony.MarkEchoGraceMS*time.Millisecond)
	}
}
