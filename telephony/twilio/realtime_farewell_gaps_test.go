package twilio

import (
	"bytes"
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gaps found reviewing AATK-128's farewell seam. The first case is driven
// through the handler, because the interleaving it pins is between two of the
// handler's own goroutines; the two after it are driven against markTracker
// directly, because neither can be provoked through the handler inside a test's
// patience — one needs a carrier that accepts a mark and then never echoes it,
// and the other needs an inbound mark frame on a call that named no mark option.

// TestFarewell_TheHoldLoopCannotReArmOverTheGoodbye pins the gap the
// farewelling flag alone does not close.
//
// The flag is read by Media and Clear, and the hold loop goes through NEITHER:
// filler.play writes via fillerMedia, straight to the carrier's write slot. So
// stopping the loop once, ahead of the clip, is not enough — the machine stays
// armable, and observe runs on the server-event drain goroutine, which is still
// draining while the select loop waits out the farewell's mark. One
// speech_stopped (the caller talking over the goodbye, which is exactly what a
// caller does) re-arms it, and Delay later the loop plays ON TOP of the clip.
//
// The wait this test exploits is real rather than contrived: it is the mark's
// own bound, which is as long as the clip takes to play.
//
// That bound is also what sizes the clip here. The two-frame farewellTestClip
// the other cases use buys a 40 ms playout plus telephony.MarkEchoGraceMS —
// 540 ms, against a re-arm that cannot produce a frame for fillerTestDelay. The
// margin is tens of milliseconds, and a window that closes early does not FAIL
// this test, it empties it: the socket is gone, so no loop frame could have
// reached the carrier whatever the machine did. A longer clip is the honest fix,
// since the bound it buys is derived from the clip rather than guessed.
//
// slopstop:test regression — guards: "the hold loop is latched off, not merely
// stopped, once the farewell owns the carrier"
func TestFarewell_TheHoldLoopCannotReArmOverTheGoodbye(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithFarewellAudio(clipOfFills(bytes.Repeat(farewellFills, 50))))
	wire := h.captureCarrierWire(t)

	// The mark is on the wire, so the clip is fully written and the select loop
	// is parked waiting for the echo — the window this test needs.
	waitForWireRecord(t, wire, isFarewellMark(h))
	firstFarewell := slices.IndexFunc(wire(), isFarewellFrame)
	if firstFarewell < 0 {
		t.Fatalf("the farewell must be on the wire before this test's window opens:\n%+v", wire())
	}

	// The caller talks over the goodbye. On the drain goroutine this is an arm.
	armFiller(t, be)
	time.Sleep(fillerTestDelay + 200*time.Millisecond)

	// The window has to still be open, or the assertion below proves nothing: a
	// closed socket stops the loop as surely as the latch does. See the clip
	// sizing on the doc comment.
	assertStillRunning(t, h, "the goodbye's mark bound must still be open, or this test cannot see its subject")

	if got := wire(); len(framesOfFills(got[firstFarewell:], fillerLoopFills)) != 0 {
		t.Fatalf("no hold-loop frame may reach the carrier once the farewell owns it:\n%+v", got)
	}

	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)
	if err := h.waitDone(idleTimeout + 5*time.Second); err == nil {
		t.Fatal("a silent backend must end the call with a non-nil error")
	}
}

// TestFarewell_AClearAlreadyQueuedForTheSlotIsStillDropped pins the half of
// the farewelling gate the early returns in Media and Clear cannot cover.
//
// Those returns test the flag BEFORE queueing for the write slot, on Bridge.Run's
// read loop and on the call's own unbounded context. playFarewell sets the flag
// on a different goroutine, so a Clear that already passed the test can be
// parked on the slot when it does — and writeSem has no fairness, so that clear
// lands in the MIDDLE of the clip, where Twilio empties its buffer and the
// queued goodbye is gone. That is the farewell cut off mid-word the whole exit
// path exists to prevent.
//
// Driven at the sink with a writer that parks inside Write, because holding one
// write in the slot while another queues behind it is the entire subject and no
// harness-level script can pin that instant.
//
// slopstop:test regression — guards: "a barge-in clear that had already been
// admitted before the farewell took the carrier is still dropped, inside the
// write slot"
func TestFarewell_AClearAlreadyQueuedForTheSlotIsStillDropped(t *testing.T) {
	w := &blockingWSWriter{entered: make(chan struct{}, 4), release: make(chan struct{})}
	sink := newCarrierMediaSink(w, "SSgate", nil, nil, nil)

	// One write in flight, holding the slot.
	inFlight := make(chan error, 1)
	go func() { inFlight <- sink.Media(context.Background(), silencePayloadB64()) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first write never reached the connection")
	}
	if n := w.writes(); n != 1 {
		t.Fatalf("exactly one write should be in flight, got %d", n)
	}

	// A barge-in arrives now: it passes Clear's early return (the flag is still
	// clear) and parks queueing for the slot.
	//
	// The sleep is a lower bound on "that goroutine has got past the early
	// return", and there is nothing better to wait on: writeSem is already full
	// (the in-flight write holds it), so its length says nothing about the
	// queuer, and Clear offers no other observable step. Sleeping too little
	// costs this test its subject rather than making it flaky — the assertion
	// below still holds, because Clear would then take its early return. What
	// proves the assertion binds is the mutation: with gatedMessage's flag test
	// removed, this fails.
	clearDone := make(chan error, 1)
	go func() { clearDone <- sink.Clear(context.Background()) }()
	time.Sleep(250 * time.Millisecond)

	// ...and only THEN does the idle guard fire and the goodbye take over.
	sink.farewelling.Store(true)

	close(w.release)
	if err := <-inFlight; err != nil {
		t.Fatalf("the in-flight media write: %v", err)
	}
	if err := <-clearDone; err != nil {
		t.Fatalf("a dropped clear must not be reported as a carrier failure: %v", err)
	}

	if n := w.writes(); n != 1 {
		t.Fatalf("the queued clear must be dropped inside the slot, not written over the farewell: %d writes reached the carrier, want 1", n)
	}
}

// TestMarkTracker_EngineOwnedMarkTimingOutIsNotDeliveredEither pins expire's
// half of "the engine's own mark is not the consumer's to hear about".
//
// echo's half is covered end to end by
// TestFarewell_MarkIsNotReportedOnTheConsumersEchoChannel — but that case
// ECHOES the mark, so it resolves in echo and never reaches expire. Removing
// expire's own suppression leaves that test green while a consumer with both
// options gets a TimedOut record for a mark it never wrote, which is exactly
// the unmatchable record markTracker's doc says a consumer must never get.
//
// slopstop:test regression — guards: "the engine's own farewell mark, and
// equally its TimedOut record, is never delivered on the consumer's channel"
func TestMarkTracker_EngineOwnedMarkTimingOutIsNotDeliveredEither(t *testing.T) {
	const name = realtimeFarewellMarkPrefix + "SSexpire"

	echoes := make(chan MarkEcho, 4)
	tr := newMarkTracker(echoes, true)
	t.Cleanup(tr.stop)

	played := tr.await(name)
	tr.arm(name, time.Millisecond)

	// The bound firing has to release the engine's wait: a wait that never
	// resolved would park playFarewell with the socket still open.
	select {
	case <-played:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine's wait must be released when its own mark's bound fires, or the call cannot close")
	}

	// And the consumer, who never wrote this mark, must not be told about it.
	select {
	case rec := <-echoes:
		t.Fatalf("the engine's own farewell mark timing out must not reach the consumer's echo channel: %+v", rec)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestMarkTracker_UnmatchedEchoLogLineFollowsTheConsumersMarks pins the
// consumerMarks flag AATK-128 threads into newMarkTracker.
//
// markTracker's own doc promises a call that named no mark option ignores an
// inbound mark frame "without even a log line". A farewell now makes a tracker
// exist underneath such a call, and that promise has to survive it — while a
// call that DID name a mark option keeps the line it has had since AATK-105.
//
// slopstop:test regression — guards: "a tracker built only for the engine's
// farewell does not start logging a consumer's unmatched echoes"
func TestMarkTracker_UnmatchedEchoLogLineFollowsTheConsumersMarks(t *testing.T) {
	const line = "matches no outstanding mark"

	for _, tc := range []struct {
		name          string
		consumerMarks bool
		wantLogged    bool
	}{
		{"farewell only", false, false},
		{"consumer asked for marks", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// syncBuffer, not a bare bytes.Buffer: log's writer is process
			// global and other tests' goroutines may still be writing to it.
			var buf syncBuffer
			origOutput, origFlags := log.Writer(), log.Flags()
			log.SetOutput(&buf)
			log.SetFlags(0)
			defer func() {
				log.SetOutput(origOutput)
				log.SetFlags(origFlags)
			}()

			tr := newMarkTracker(nil, tc.consumerMarks)
			t.Cleanup(tr.stop)
			tr.echo("a-mark-this-call-never-wrote")

			if logged := strings.Contains(buf.String(), line); logged != tc.wantLogged {
				t.Fatalf("unmatched echo logged = %v, want %v (consumerMarks=%v)\nlog: %q",
					logged, tc.wantLogged, tc.consumerMarks, buf.String())
			}
		})
	}
}

// TestFarewell_ClearsTheCarrierEvenWhenAnotherWriterTookTheStopEdge pins the
// clear the goodbye owes the caller against the one thing that can take it
// away: another writer having stopped the loop first.
//
// stopFillerAndClear sends its clear only on the stop EDGE — filler.stop
// reporting that the loop WAS playing — and that edge belongs to whichever
// caller consumes it. Media and Clear call stop too, from Bridge.Run's read
// loop, so a backend that finally speaks (or a caller who talks) as the idle
// guard fires can take the edge microseconds ahead of playFarewell. That
// caller's own clear is then dropped by the farewelling gate, playFarewell's
// finds nothing to report, and NEITHER is written: the goodbye ships behind
// whatever the carrier still holds of the hold loop, which is the queueing the
// clear exists to prevent.
//
// Driven at the sink, with the racing stop made explicit rather than raced for.
// The interleaving is nanoseconds wide, so a test that tried to hit it by timing
// would pass for the wrong reason on most runs; consuming the edge by hand is
// the same starting state, deterministically.
//
// slopstop:test regression — guards: "the farewell clears the carrier because a
// loop is configured, not because it happened to win the loop's stop edge"
func TestFarewell_ClearsTheCarrierEvenWhenAnotherWriterTookTheStopEdge(t *testing.T) {
	w := &scriptedWSWriter{}
	fill := newFiller(context.Background(), FillerConfig{Loop: fillerTestLoop(), Delay: 20 * time.Millisecond})
	sink := newCarrierMediaSink(w, "SSedge", nil, nil, fill)
	fill.attach(sink)
	t.Cleanup(fill.shutdown)

	// The loop is playing, so the carrier is holding frames of it.
	fill.arm(armedByTurn)
	waitFor(t, 5*time.Second, func() bool { return len(fillerFrames(w.records())) >= 2 })

	// A Media or Clear on Bridge.Run's goroutine gets there first and consumes
	// the edge — on a real call, the backend finally speaking or the caller
	// talking at the instant the idle guard fires.
	if !fill.stop() {
		t.Fatal("test setup: the loop must have been playing for there to be an edge to consume")
	}

	sink.playFarewell(context.Background(), farewellTestClip())

	got := w.records()
	firstFarewell := slices.IndexFunc(got, isFarewellFrame)
	if firstFarewell < 0 {
		t.Fatalf("the farewell must have reached the carrier:\n%+v", got)
	}
	if slices.IndexFunc(got[:firstFarewell], isClear) < 0 {
		t.Fatalf("the goodbye must be preceded by a clear even when another writer took the loop's stop edge, "+
			"or it ships behind whatever the carrier still holds of the loop:\n%+v", got)
	}
}

// TestFarewell_AWedgedCarrierCannotParkTheIdleExit pins the deadline on the
// goodbye's FIRST write, and specifically on the half of it no other case can
// reach: the wait for the write SLOT.
//
// That slot is held by carrier audio written on the call's own unbounded
// context, so a carrier that stopped reading parks whoever is queueing behind
// it — and playFarewell queues there from HandleStreamRealtime's select loop,
// AHEAD of the CloseNow that would otherwise unblock the holder. Unbounded,
// this branch would park forever and void the idle bound entirely: the call
// would hang exactly where it was supposed to end.
//
// Only the clear is exercised here, and unavoidably so: it never gets the slot,
// so no later write of the goodbye is ever attempted. The bound on the clip's
// frames and on the mark is
// TestFarewell_AWedgedCarrierCannotParkTheIdleExitAtAnyWrite's, which wedges the
// connection itself rather than the slot and can therefore choose where.
//
// slopstop:test regression — guards: "a carrier that stops reading cannot park
// the idle guard's exit path on the write slot"
func TestFarewell_AWedgedCarrierCannotParkTheIdleExit(t *testing.T) {
	w := &blockingWSWriter{entered: make(chan struct{}, 4), release: make(chan struct{})}
	sink := newCarrierMediaSink(w, "SSwedged", nil, nil, nil)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.release) }) }
	t.Cleanup(release)

	// The carrier has stopped reading: one write is in flight and holds the
	// slot, on the unbounded context Media uses on a real call.
	inFlight := make(chan error, 1)
	go func() { inFlight <- sink.Media(context.Background(), carrierPayloadB64()) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("test setup: the first write never reached the connection")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.playFarewell(context.Background(), farewellTestClip())
	}()

	// Generously past the one bound that can release it, and far short of
	// forever. Unbounded, this parks until the test binary is killed.
	select {
	case <-done:
	case <-time.After(3 * realtimeClientEventSendTimeout):
		t.Fatalf("the goodbye must give up at its own bound (%s per write) rather than parking the idle exit "+
			"behind a carrier that stopped reading", realtimeClientEventSendTimeout)
	}

	release()
	<-inFlight
}

// TestFarewell_AWedgedCarrierCannotParkTheIdleExitAtAnyWrite pins the bound on
// EVERY write playFarewell makes, which is what its own doc claims and what the
// slot case above cannot see.
//
// A carrier that stops reading after accepting the clear is the ordinary shape
// of the failure — buffers fill partway through the clip, not before it — and
// the writes that follow the clear are on exactly the same select loop, ahead
// of exactly the same CloseNow. Bound only the clear and a wedge at the first
// frame, or at the mark, parks the idle exit forever with the socket still
// open. Measured: with the clip's and the mark's writes moved back to the
// call's own context and only the clear left bounded, the case above stays
// green and the whole package with it.
//
// The wedge is at the CONNECTION here rather than at the slot, which is what
// lets each row choose which write stops being read: nothing else holds the
// slot, so every earlier write completes and the chosen one is genuinely
// attempted.
//
// slopstop:test regression — guards: "each of the goodbye's writes — the clear,
// the clip's frames and the mark — carries its own deadline"
func TestFarewell_AWedgedCarrierCannotParkTheIdleExitAtAnyWrite(t *testing.T) {
	clip := farewellTestClip()
	frames := len(clip) / defaultFrameBytes

	for _, tc := range []struct {
		name string
		// accepted is how many of the goodbye's writes the carrier reads
		// before it wedges; the wedge is therefore on write accepted+1.
		accepted int
	}{
		{"the clear", 0},
		{"the clip's first frame", 1},
		{"the mark", 1 + frames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each row waits out the full bound, so they overlap rather than
			// costing the package three of them.
			t.Parallel()

			w := &blockingWSWriter{
				entered:     make(chan struct{}, 8),
				release:     make(chan struct{}),
				passthrough: tc.accepted,
			}
			t.Cleanup(func() { close(w.release) })
			sink := newCarrierMediaSink(w, "SSwedgeat", nil, nil, nil)

			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				sink.playFarewell(context.Background(), clip)
			}()

			// Generously past the one bound that can release it, and far short
			// of forever. Unbounded, this parks until the test binary is killed.
			select {
			case <-done:
			case <-time.After(3 * realtimeClientEventSendTimeout):
				t.Fatalf("the goodbye must give up at its own bound (%s per write) when the carrier stops reading "+
					"at write %d, rather than parking the idle exit", realtimeClientEventSendTimeout, tc.accepted+1)
			}

			// Returning FAST would mean the wedge was never reached and the row
			// proved nothing — an empty test rather than a passing one.
			if took := time.Since(start); took < realtimeClientEventSendTimeout {
				t.Fatalf("playFarewell returned after %v, before the bound could have fired: "+
					"write %d was not the one that wedged", took, tc.accepted+1)
			}
			if n := w.writes(); n != tc.accepted+1 {
				t.Fatalf("the goodbye must stop at the wedged write: %d writes reached the carrier, want %d",
					n, tc.accepted+1)
			}
		})
	}
}

// TestFarewell_MarkBoundDoesNotCarryTheBackendsBacklog pins the bound
// playFarewell's own doc claims for its wait: "the mark's own bound, derived
// from the playout the clip queued".
//
// That claim only held when a filler was configured, because the clear that
// flushes the playout clock was taken under `if s.filler != nil`. Without one,
// whatever the backend burst-dumped before it went quiet was still queued, the
// mark's bound was derived from THAT, and the goodbye shipped behind it — so
// the clip was not what the caller heard next, and the select loop stayed
// parked for the backlog rather than for the clip. Every teardown the loop owns
// is behind that wait: marks.stop, the filler's shutdown, and the carrier's own
// CloseNow. A caller who hangs up as the guard fires holds all of it open for
// however much stale audio the carrier was still holding.
//
// Two seconds of backlog against a 40 ms clip, so the two answers are an order
// of magnitude apart and no timing margin has to be guessed.
//
// slopstop:test regression — guards: "the farewell clears the carrier whether
// or not a hold loop was configured, so its mark bound describes the clip
// rather than the backend's backlog"
func TestFarewell_MarkBoundDoesNotCarryTheBackendsBacklog(t *testing.T) {
	const backlog = 100 // frames, 20 ms each: 2 s of playout

	w := &scriptedWSWriter{}
	tr := newMarkTracker(nil, false)
	t.Cleanup(tr.stop)
	// nil filler: the case the conditional clear left uncovered.
	sink := newCarrierMediaSink(w, "SSbacklog", nil, tr, nil)

	// The backend dumped a long reply far faster than real time and then went
	// silent — which is what the idle guard fires on.
	for range backlog {
		if err := sink.Media(context.Background(), carrierPayloadB64()); err != nil {
			t.Fatalf("test setup: writing the backend's burst: %v", err)
		}
	}
	queued := sink.playout.outstanding(time.Now())
	if queued < time.Second {
		t.Fatalf("test setup: the carrier must still be holding the burst, got %v queued", queued)
	}

	// Nobody echoes the mark: the caller has hung up, so only the bound can
	// release the wait.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.playFarewell(context.Background(), farewellTestClip())
	}()

	select {
	case <-done:
	case <-time.After(queued + 5*time.Second):
		t.Fatal("playFarewell never returned")
	}

	if took := time.Since(start); took > queued/2 {
		t.Fatalf("the goodbye's wait must be the clip's own bound, not the backend's backlog: "+
			"returned after %v with %v of stale audio queued", took, queued)
	}

	got := w.records()
	firstFarewell := slices.IndexFunc(got, isFarewellFrame)
	if firstFarewell < 0 {
		t.Fatalf("the farewell must have reached the carrier:\n%+v", got)
	}
	if slices.IndexFunc(got[:firstFarewell], isClear) < 0 {
		t.Fatalf("the goodbye must be preceded by a clear even when no hold loop was configured, "+
			"or it ships behind whatever the carrier still holds:\n%+v", got)
	}
}
