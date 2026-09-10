package twilio

import (
	"context"
	"log"
	"slices"
	"strings"
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
// slopstop:test regression — guards: "the hold loop is latched off, not merely
// stopped, once the farewell owns the carrier"
func TestFarewell_TheHoldLoopCannotReArmOverTheGoodbye(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithFarewellAudio(farewellTestClip()))
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
	// proves the assertion binds is the mutation: with admitUnlessFarewelling
	// hard-coded true, this fails.
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
