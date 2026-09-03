package twilio

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// blockingWSWriter parks in Write until release is closed, and records every
// message it was handed. It is what lets a test hold carrierMediaSink's write
// slot open while a second writer tries to take it — the interleaving the sink
// exists to serialise, and one that cannot be provoked through the handler,
// where both writers complete in microseconds.
type blockingWSWriter struct {
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	sent [][]byte
}

// Write records the message BEFORE parking, so a caller that reached the
// connection without going through the slot is visible in writes() even though
// its write never completed — that caller having got this far is the defect.
// The park honours ctx so a bypassing write fails on its own deadline rather
// than hanging the test out to its timeout.
func (b *blockingWSWriter) Write(ctx context.Context, _ websocket.MessageType, msg []byte) error {
	b.mu.Lock()
	b.sent = append(b.sent, append([]byte(nil), msg...))
	b.mu.Unlock()
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingWSWriter) writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sent)
}

// TestCarrierMediaSink_MarkQueuesBehindTheWriteInFlight is what actually holds
// a mark to carrierMediaSink.writeSem, and holds that slot to a CANCELLABLE
// acquisition.
//
// TestMarkRequestChan_MarkReachesCarrierAfterQueuedAudio cannot: it observes
// the three media frames on the wire before requesting the mark, so the mark is
// asked for only once nothing is in flight, and a Mark that wrote the carrier
// connection directly — the second-writer defect the sink exists to prevent —
// still lands after them. Mutation-verified during review: replacing Mark's
// s.write call with a bare s.conn.Write left every mark test green under a
// plain `go test`; only `-race` failed it, and then on the playout clock rather
// than on the ordering.
//
// The deadline half is the same story one level down. writeSem is a one-slot
// channel rather than a sync.Mutex precisely so the wait honours ctx, because a
// mark is written under a bounded context from the select loop that observes
// every way the call can end, while the media it queues behind is written on
// the call's own unbounded one. Mutation-verified: replacing write's select
// with a bare `s.writeSem <- struct{}{}` left the whole package green under
// -race.
//
// slopstop:test regression — guards: "a mark reaches the carrier after every
// media frame the engine had already written, never from a goroutine racing
// the sink's writer"
func TestCarrierMediaSink_MarkQueuesBehindTheWriteInFlight(t *testing.T) {
	w := &blockingWSWriter{entered: make(chan struct{}, 4), release: make(chan struct{})}
	tr := newMarkTracker(make(chan MarkEcho, 4))
	sink := newCarrierMediaSink(w, "SSslot", nil, tr)

	mediaDone := make(chan error, 1)
	go func() { mediaDone <- sink.Media(context.Background(), silencePayloadB64()) }()

	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the media write never reached the carrier connection")
	}

	// The slot is held by that write. A mark taken under a short deadline must
	// come back on the WAIT rather than overtaking it onto the connection.
	markCtx, cancelMark := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelMark()
	// In a goroutine, so an acquisition that does NOT honour ctx — the
	// sync.Mutex spelling writeSem exists instead of — reports itself here
	// rather than hanging the package out to its test timeout.
	marked := make(chan error, 1)
	go func() { marked <- sink.Mark(markCtx, "queued") }()
	var err error
	select {
	case err = <-marked:
	case <-time.After(10 * time.Second):
		t.Fatal("a mark queued behind an in-flight write never came back; its 200ms deadline did not cover the wait for the slot")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a mark queued behind an in-flight write must fail on its own deadline, got err %v", err)
	}
	if n := w.writes(); n != 1 {
		t.Fatalf("only the media write may have reached the connection while it held the slot, got %d writes", n)
	}
	// A mark that never got the slot never went out, so nothing may be
	// outstanding waiting for an echo that can never come.
	tr.mu.Lock()
	outstanding := len(tr.outstanding)
	tr.mu.Unlock()
	if outstanding != 0 {
		t.Fatalf("a mark whose write never happened must not be armed, got %d outstanding", outstanding)
	}

	// Control: the refusal above is the slot being held, not Mark being broken.
	// Once the media write completes, the same mark goes out and is armed.
	close(w.release)
	select {
	case err := <-mediaDone:
		if err != nil {
			t.Fatalf("Media: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the media write never completed after the connection was released")
	}
	if err := sink.Mark(context.Background(), "queued"); err != nil {
		t.Fatalf("Mark once the slot is free: %v", err)
	}
	if n := w.writes(); n != 2 {
		t.Fatalf("the mark must reach the connection once the slot is free, got %d writes", n)
	}
	tr.mu.Lock()
	_, armed := tr.outstanding["queued"]
	tr.mu.Unlock()
	if !armed {
		t.Fatal("a mark that reached the carrier must be armed for its echo")
	}
}

// TestCarrierMediaSink_BoundTracksItsOwnWrites pins the playout clock to the
// sink's own Media and Clear, rather than to a test driving playoutClock by
// hand.
//
// TestMarkRequestChan_BoundCoversQueuedPlayout and
// TestMarkEchoBound_KeepsTheSubMillisecondRemainder both reach past those two
// methods — they call sink.playout.fed and sink.playout.flush directly — so
// they pin the ARITHMETIC and say nothing about the two lines that feed it.
// Mutation-verified during review: dropping playoutClock.flush from Clear's
// afterWrite left the whole suite green, the twilio-cli end-to-end run
// included; dropping playoutClock.fed from Media's left this package green and
// was caught only by that end-to-end run, which -short skips.
//
// Either is a bound describing the wrong queue, and the ticket's fourth
// behavior turns on the bound: a mark written behind a second of audio judged
// late after the bare grace, or a mark written after a barge-in waiting out the
// abandoned reply nobody will hear.
//
// slopstop:test regression — guards: "a mark's echo bound is derived from the
// playout the sink itself has written, and a clear discards it"
func TestCarrierMediaSink_BoundTracksItsOwnWrites(t *testing.T) {
	// One second of μ-law audio, in the 20 ms frames the backend sends.
	const frames = 50
	const grace = telephony.MarkEchoGraceMS * time.Millisecond

	sink := newCarrierMediaSink(&discardWSWriter{}, "SSwiring", nil, nil)

	if got := sink.markEchoBound(time.Now()); got != grace {
		t.Fatalf("with nothing written, markEchoBound = %s, want the bare grace %s", got, grace)
	}

	for i := 0; i < frames; i++ {
		if err := sink.Media(context.Background(), silencePayloadB64()); err != nil {
			t.Fatalf("Media frame %d: %v", i, err)
		}
	}

	// The clock runs from the first write, so the bound is the queued playout
	// plus the grace LESS however long the writes themselves took — bounded on
	// both sides rather than compared for equality. The lower bound is far
	// above anything write time plausibly costs here and far above the bare
	// grace, which is what a Media that fed nothing would leave.
	queued := telephony.MuLawDuration(frames * oneFrameBytes)
	got := sink.markEchoBound(time.Now())
	if got > queued+grace {
		t.Fatalf("markEchoBound after %s of written audio = %s, want at most %s", queued, got, queued+grace)
	}
	if got < queued+grace-200*time.Millisecond {
		t.Fatalf("markEchoBound after %s of written audio = %s; the audio Media wrote is not in the bound (the bare grace alone is %s)",
			queued, got, grace)
	}

	// A clear is barge-in: the carrier drops what it had buffered, so the next
	// mark is owed the grace and nothing more.
	if err := sink.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := sink.markEchoBound(time.Now()); got != grace {
		t.Fatalf("markEchoBound after a clear = %s, want the bare grace %s; the abandoned reply is still being waited out", got, grace)
	}
}

// TestCarrierMediaSink_MarkArmsTheDerivedBound closes the last link in the same
// chain: Mark arms the tracker with markEchoBound, not with the bare grace.
//
// Mutation-verified during review: arming
// `telephony.MarkEchoGraceMS*time.Millisecond` instead left this whole package
// green. TestMarkRequestChan_NonEchoingCarrierDoesNotHang cannot see it — it
// drives no audio, so the derived bound IS the bare grace there — and
// TestCarrierMediaSink_BoundTracksItsOwnWrites reads markEchoBound directly
// rather than through Mark. Only the twilio-cli end-to-end run caught it, and
// -short skips that.
//
// The assertion is that the bound has NOT elapsed: a second of audio is queued
// ahead of this mark, so a carrier still playing it is not late yet, and
// reporting TimedOut here is exactly the false alarm the derivation exists to
// prevent.
//
// slopstop:test regression — guards: "a mark's echo bound is derived from the
// playout the sink itself has written, and a clear discards it"
func TestCarrierMediaSink_MarkArmsTheDerivedBound(t *testing.T) {
	const frames = 50
	const grace = telephony.MarkEchoGraceMS * time.Millisecond

	echoes := make(chan MarkEcho, 4)
	tr := newMarkTracker(echoes)
	t.Cleanup(tr.stop)
	sink := newCarrierMediaSink(&discardWSWriter{}, "SSderived", nil, tr)

	for i := 0; i < frames; i++ {
		if err := sink.Media(context.Background(), silencePayloadB64()); err != nil {
			t.Fatalf("Media frame %d: %v", i, err)
		}
	}
	if err := sink.Mark(context.Background(), "derived"); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// Comfortably past the bare grace, comfortably inside the derived bound
	// (the queued second plus that grace).
	select {
	case rec := <-echoes:
		t.Fatalf("a mark written behind %s of queued audio was abandoned within %s: got %+v; its bound is the bare grace rather than the derived one",
			telephony.MuLawDuration(frames*oneFrameBytes), grace+150*time.Millisecond, rec)
	case <-time.After(grace + 150*time.Millisecond):
	}
}
