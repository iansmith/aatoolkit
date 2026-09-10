package twilio

import (
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// Tests for the two halves of "is anyone there?" (AATK-128): the hold loop
// covering a backend that says nothing from the moment the call opens, and the
// farewell the caller hears before the idle guard drops the line.
//
// Both are about what the CALLER hears, so every assertion is made against the
// carrier's own wire (captureCarrierWire, realtime_carrieraudio_test.go) rather
// than against a function having returned. The defect this ticket was cut from
// looked like success from inside the engine: the call ended with an error
// naming the idle timeout, and the caller heard an open line and then nothing.

// farewellFills are the μ-law byte values of the two frames the test farewell
// clip is built from — distinct from each other so the clip's order is
// observable on the wire, and distinct from both fillerLoopFills and
// carrierPayloadB64's 0xFF fill so a farewell frame is never confused with a
// hold-loop frame or a backend delta.
var farewellFills = []byte{0x11, 0x12}

// The four below are the filler suite's clip helpers applied to these fills;
// see clipOfFills in realtime_filler_test.go for why they are parameterised
// rather than copied.

// farewellTestClip is the consumer-supplied farewell: two 20 ms μ-law frames.
func farewellTestClip() []byte { return clipOfFills(farewellFills) }

// farewellFrameB64 is the base64 of the nth frame of that clip. Played once
// rather than as a ring, so n does not wrap.
func farewellFrameB64(n int) string { return frameB64OfFill(farewellFills[n]) }

// isFarewellFrame reports whether a wire record is one of the clip's frames.
func isFarewellFrame(rec carrierWireRecord) bool { return isFrameOfFills(rec, farewellFills) }

// farewellFrames returns only the clip's frames from a wire capture.
func farewellFrames(recs []carrierWireRecord) []carrierWireRecord {
	return framesOfFills(recs, farewellFills)
}

// isClosed and isFarewellMark are the predicates this file waits on and scans
// for, named so the assertions below read as sentences.
func isClosed(r carrierWireRecord) bool { return r.closed }

func isFarewellMark(h *realtimeHarness) func(carrierWireRecord) bool {
	return func(r carrierWireRecord) bool {
		return r.markName == realtimeFarewellMarkPrefix+h.streamSID
	}
}

// silenceTestIdleTimeout is the idle bound these tests configure. It is many
// multiples of fillerTestDelay, which is the whole point of behaviour 1: the
// cover must reach the caller within a few seconds of the call opening, NOT at
// the idle bound, and a test whose two durations were close together could not
// tell those apart.
const silenceTestIdleTimeout = 3 * time.Second

// silenceHarness wires HandleStreamRealtime with whichever of the three options
// a case needs, which is the entry a consumer supplying its own set would use.
// Mirrors fillerHarness and idleHarness.
func silenceHarness(t *testing.T, url string, opts ...RealtimeOption) *realtimeHarness {
	t.Helper()
	return newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, url, opts...)
	})
}

// --- behaviour 1: the cover starts at call open, not at the idle bound ------

// TestCallOpen_CoversASilentBackendWellBeforeTheIdleBound is the ticket's
// central case. The backend completes the handshake and then produces nothing
// at all; today the caller hears an open line until the idle guard drops it.
//
// The assertion is on the carrier's frames AND on the clock: cover audio must
// arrive within a small multiple of the configured Delay, far short of the idle
// bound, because "the caller eventually hears something as the line is being
// dropped" is the defect rather than the fix.
//
// slopstop:test contract
func TestCallOpen_CoversASilentBackendWellBeforeTheIdleBound(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(silenceTestIdleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}))
	wire := h.captureCarrierWire(t)
	openedAt := time.Now()

	// The caller never speaks and the backend never answers: nothing but the
	// handshake happens on this call.
	waitFillerPlaying(t, wire, 3)

	first := fillerFrames(wire())[0]
	if waited := first.at.Sub(openedAt); waited > silenceTestIdleTimeout/2 {
		t.Fatalf("a silent backend must be covered within a few seconds of call open, not at the idle bound: "+
			"first frame %v after the call opened, idle bound is %v", waited, silenceTestIdleTimeout)
	}
	assertStillRunning(t, h, "covering the opening silence must not end the call")
}

// --- behaviour 2: a late backend is never talked over -----------------------

// TestCallOpen_CoverStopsWhenTheBackendFinallySpeaks pins the stop edge for the
// call-open cover, which is the same edge AATK-108 built for the mid-turn one:
// the clear must precede the reply's first frame, and no cover frame may follow
// it.
//
// slopstop:test contract
func TestCallOpen_CoverStopsWhenTheBackendFinallySpeaks(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(silenceTestIdleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	waitFillerPlaying(t, wire, 3)

	reply := carrierPayloadB64()
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": reply})

	// The same stop edge TestFiller_ClearThenFirstDelta asserts for the
	// speech_stopped episode; what this test contributes is the episode, which
	// no event armed.
	assertClearThenReply(t, wire, reply)
}

// --- behaviour 1 & 6: no filler option, no cover ----------------------------

// TestCallOpen_NoCoverWhenTheFillerOptionIsAbsent pins the off case for the
// call-open trigger specifically. TestFiller_UnsetWritesNothingWhileTheBackendIsSilent
// pins the same absence against the speech_stopped trigger; this one drives the
// script that needs no event at all, which is the trigger this ticket adds.
//
// slopstop:test contract
func TestCallOpen_NoCoverWhenTheFillerOptionIsAbsent(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithIdleTimeout(silenceTestIdleTimeout))
	wire := h.captureCarrierWire(t)

	// The call-open trigger needs no event, so the countdown would already have
	// started: past Delay is past the only instant anything could be written.
	time.Sleep(fillerTestDelay + 100*time.Millisecond)

	if got := wire(); len(got) != 0 {
		t.Fatalf("a call with no filler option must write nothing to the carrier while the backend is silent, got %+v", got)
	}
}

// --- behaviour 3 & 4: the farewell precedes the close -----------------------

// TestFarewell_FramesReachTheCarrierBeforeTheClose is the ordering the second
// half of the ticket turns on. HandleStreamRealtime hard-closes the carrier on
// every exit path, so a farewell written anywhere but INSIDE the idle branch —
// or written without waiting for it to drain — is a farewell the caller never
// hears, or hears cut off mid-word.
//
// The assertion is the ordering, not merely that frames were written: every
// farewell frame must carry a stamp earlier than the close's own.
//
// slopstop:test contract
func TestFarewell_FramesReachTheCarrierBeforeTheClose(t *testing.T) {
	const idleTimeout = 300 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	// Echoed as a live carrier would, so the call ends on the echo rather than
	// waiting out the mark's full bound. The drain itself is
	// TestFarewell_WaitsForTheCarrierToReportItPlayed's subject; here it is
	// only in the way of the ordering.
	waitForWireRecord(t, wire, isFarewellMark(h))
	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)

	if err := h.waitDone(idleTimeout + 5*time.Second); err == nil {
		t.Fatal("a silent backend must still end the call with a non-nil error when a farewell is configured")
	}

	// The close is observed by the capture goroutine, not by this one, so wait
	// for it rather than assuming the handler's return already put it on the
	// wire.
	waitForWireRecord(t, wire, isClosed)

	got := wire()
	farewell := farewellFrames(got)
	if len(farewell) != len(farewellFills) {
		t.Fatalf("every frame of the farewell must reach the carrier: got %d of %d\n%+v",
			len(farewell), len(farewellFills), got)
	}
	for i, r := range farewell {
		if want := farewellFrameB64(i); r.payload != want {
			t.Fatalf("farewell frame %d must be frame %d of the clip:\n got  %q\nwant %q",
				i, i, r.payload, want)
		}
	}

	// Records are appended by one reader goroutine under one mutex, so index
	// order IS arrival order: every farewell frame sitting before the close in
	// this slice is the ordering the ticket asks for.
	closedAt := slices.IndexFunc(got, isClosed)
	if n := len(farewellFrames(got[:closedAt])); n != len(farewell) {
		t.Fatalf("every farewell frame must reach the carrier before the close, %d of %d did:\n%+v",
			n, len(farewell), got)
	}
}

// TestFarewell_WaitsForTheCarrierToReportItPlayed pins the DRAIN, which is the
// half of behaviour 4 the ordering assertion above cannot see. Frames written
// and then immediately abandoned still show up on the wire ahead of the close —
// they were written first — while the caller hears the line drop mid-word,
// because Twilio buffers outbound media and CloseNow does not wait for it.
//
// The mark echo is the only honest report that the audio actually played, so
// the assertion is in two directions: the call must NOT have ended once the
// mark is on the wire, and it must end promptly once the carrier echoes it —
// well inside the bound the engine would otherwise wait out.
//
// slopstop:test contract
func TestFarewell_WaitsForTheCarrierToReportItPlayed(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	waitForWireRecord(t, wire, isFarewellMark(h))

	// The mark is written after the clip and before the close; every farewell
	// frame must already be behind it.
	got := wire()
	markAt := slices.IndexFunc(got, isFarewellMark(h))
	if n := len(farewellFrames(got[:markAt])); n != len(farewellFills) {
		t.Fatalf("the mark must be written after the whole clip: %d of %d frames precede it\n%+v",
			n, len(farewellFills), got)
	}

	assertStillRunning(t, h, "the call must not end until the carrier reports the farewell played")

	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)

	// Comfortably inside the mark's own bound, which is the clip's playout plus
	// telephony.MarkEchoGraceMS — so arriving this fast can only be the echo
	// having released the wait, not the bound expiring.
	if err := h.waitDone(telephony.MarkEchoGraceMS / 2 * time.Millisecond); err == nil {
		t.Fatal("the idle ending must still be reported as an error after the farewell has played")
	}
}

// TestFarewell_CarrierAudioRecordsAreMarkedFarewell pins the observation half,
// mirroring TestFiller_CarrierAudioRecordsAreMarkedFiller. A consumer watching
// what shipped to the carrier must be able to tell the goodbye from the
// backend's speech — and on this call especially, since the error the same call
// returns says the backend produced nothing at all.
//
// slopstop:test contract
func TestFarewell_CarrierAudioRecordsAreMarkedFarewell(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	ch := make(chan CarrierAudio, 128)
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithCarrierAudioChan(ch),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	waitForWireRecord(t, wire, isFarewellMark(h))
	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)
	if err := h.waitDone(idleTimeout + 5*time.Second); err == nil {
		t.Fatal("a silent backend must end the call with a non-nil error")
	}

	var got int
	for len(ch) > 0 {
		rec := <-ch
		if rec.Clear {
			continue
		}
		if !rec.Farewell {
			t.Fatalf("engine-originated farewell audio must not be reported as the backend's speech: %+v", rec)
		}
		if rec.Filler {
			t.Fatalf("a farewell frame must not also be marked as filler: %+v", rec)
		}
		got++
	}
	if got != len(farewellFills) {
		t.Fatalf("the consumer must receive every farewell frame as a farewell-marked record, got %d of %d",
			got, len(farewellFills))
	}
}

// --- behaviour 6: no farewell option, today's behaviour ---------------------

// TestFarewell_AbsentWhenTheOptionIsNotSupplied pins the off case: the idle
// guard's branch must behave exactly as it did before this option existed, so a
// consumer that does not opt in hears the same silence-then-hangup it always
// did — and, more to the point, the branch must not have grown a wait that
// delays the ending for a clip nobody supplied.
//
// slopstop:test contract
func TestFarewell_AbsentWhenTheOptionIsNotSupplied(t *testing.T) {
	const idleTimeout = 300 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithIdleTimeout(idleTimeout))
	wire := h.captureCarrierWire(t)

	startedAt := time.Now()
	if err := h.waitDone(idleTimeout + 2*time.Second); err == nil {
		t.Fatal("a silent backend must end the call with a non-nil error")
	}
	if took := time.Since(startedAt); took > idleTimeout*4 {
		t.Fatalf("with no farewell configured the idle branch must end the call at its bound, took %v (bound %v)",
			took, idleTimeout)
	}

	if got := wire(); slices.IndexFunc(got, func(r carrierWireRecord) bool { return !r.closed }) >= 0 {
		t.Fatalf("a call with no farewell option must write nothing to the carrier on the idle path, got %+v", got)
	}
}

// --- the exported sentinel --------------------------------------------------

// TestIdleTimeout_ErrorMatchesErrIdleTimeout pins the identification a consumer
// needs. Matching the message text is what a consumer had to do before this,
// and a message is not an API: it changes when the sentence changes.
//
// slopstop:test contract
func TestIdleTimeout_ErrorMatchesErrIdleTimeout(t *testing.T) {
	const idleTimeout = 100 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithIdleTimeout(idleTimeout))

	err := h.waitDone(idleTimeout + 5*time.Second)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("the idle-timeout ending must be identifiable with errors.Is(err, ErrIdleTimeout), got: %v", err)
	}
}

// --- behaviour 7: both conditions are named in the log ----------------------

// TestSilentBackend_BothConditionsAreLoggedOnce pins observable behaviour 7:
// each condition says its own name, once, so a call that sounded dead can be
// found afterwards without replaying it.
//
// It is a real assertion rather than a comment because nothing else can see
// these lines — the log is the whole observable. Both are checked in one call,
// which is also the call a live deployment would see them on: the backend goes
// silent at the open, the loop covers it, and the idle guard eventually ends
// the call with the farewell.
//
// slopstop:test contract
func TestSilentBackend_BothConditionsAreLoggedOnce(t *testing.T) {
	// Long enough that the cover starts first, and no longer.
	const idleTimeout = fillerTestDelay + 100*time.Millisecond
	const (
		coverLine    = "twilio: realtime: silent backend at call open: playing the filler loop"
		farewellLine = "twilio: realtime: idle timeout: playing the farewell before ending the call"
	)

	// syncBuffer, not a bare bytes.Buffer: these lines are written by the play
	// goroutine and the select loop while this one reads them.
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	waitForWireRecord(t, wire, isFarewellMark(h))
	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)

	if err := h.waitDone(idleTimeout + 5*time.Second); err == nil {
		t.Fatal("a silent backend must end the call with a non-nil error")
	}

	// This call is the one where both behaviours meet, so it is where the
	// handover between them is asserted: the cover was playing when the guard
	// fired, and the goodbye must not queue behind what the carrier still holds
	// of it. Exactly one clear, immediately before the clip's first frame.
	got := wire()
	firstFarewell := slices.IndexFunc(got, isFarewellFrame)
	if firstFarewell < 1 {
		t.Fatalf("the farewell must follow the cover on this call:\n%+v", got)
	}
	if !got[firstFarewell-1].clear {
		t.Fatalf("the record before the farewell's first frame must be the clear that discards the cover, got %+v",
			got[firstFarewell-1])
	}
	if n := len(framesOfFills(got[firstFarewell:], fillerLoopFills)); n != 0 {
		t.Fatalf("no cover frame may reach the carrier after the farewell begins, got %d:\n%+v", n, got)
	}

	for _, want := range []string{coverLine, farewellLine} {
		if n := strings.Count(buf.String(), want); n != 1 {
			t.Fatalf("each condition must be named in the log exactly once: %q appeared %d times\nlog: %q",
				want, n, buf.String())
		}
	}
}

// --- behaviour 5: a healthy call is untouched -------------------------------

// TestHealthyCall_WritesNeitherCoverNorFarewell pins the promise that keeps
// this ticket safe to deploy: neither behaviour may put audio on a call that is
// working. A backend answering steadily is never covered, and never says
// goodbye — the idle guard does not fire at all.
//
// slopstop:test contract
func TestHealthyCall_WritesNeitherCoverNorFarewell(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	be.emitInterval = idleTimeout / 8 // well inside both the idle bound and Delay

	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	// Several multiples of both bounds, with the backend talking the whole time.
	time.Sleep(idleTimeout * 2)

	assertStillRunning(t, h, "a backend answering steadily must not trip the idle guard")

	got := wire()
	if n := len(fillerFrames(got)); n != 0 {
		t.Fatalf("a healthy call must never hear the cover, got %d cover frames:\n%+v", n, got)
	}
	if n := len(farewellFrames(got)); n != 0 {
		t.Fatalf("a healthy call must never hear the farewell, got %d farewell frames:\n%+v", n, got)
	}
}
