package twilio

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// farewellTestClip is the consumer-supplied farewell: two 20 ms μ-law frames.
func farewellTestClip() []byte {
	var out []byte
	for _, f := range farewellFills {
		out = append(out, bytes.Repeat([]byte{f}, defaultFrameBytes)...)
	}
	return out
}

// farewellFrameB64 is the base64 the carrier must receive for the nth frame of
// farewellTestClip, so a test compares wire bytes against the clip it supplied
// rather than against a re-derivation of it.
func farewellFrameB64(n int) string {
	return base64.StdEncoding.EncodeToString(
		bytes.Repeat([]byte{farewellFills[n]}, defaultFrameBytes))
}

// isFarewellFrame reports whether a wire record is one of the clip's frames.
func isFarewellFrame(rec carrierWireRecord) bool {
	if rec.clear || rec.closed || rec.markName != "" {
		return false
	}
	for i := range farewellFills {
		if rec.payload == farewellFrameB64(i) {
			return true
		}
	}
	return false
}

// farewellFrames returns only the clip's frames from a wire capture, preserving
// order and arrival time.
func farewellFrames(recs []carrierWireRecord) []carrierWireRecord {
	var out []carrierWireRecord
	for _, r := range recs {
		if isFarewellFrame(r) {
			out = append(out, r)
		}
	}
	return out
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
	waitFor(t, 5*time.Second, func() bool {
		for _, r := range wire() {
			if r.payload == reply {
				return true
			}
		}
		return false
	})
	// Let anything the relay wrongly wrote after the reply arrive too.
	time.Sleep(100 * time.Millisecond)

	got := wire()
	replyAt := -1
	for i, r := range got {
		if r.payload == reply {
			replyAt = i
			break
		}
	}
	if replyAt < 1 {
		t.Fatalf("the reply frame must arrive after the cover, got records %+v", got)
	}
	if !got[replyAt-1].clear {
		t.Fatalf("the record immediately before the backend's first frame must be the clear, got %+v",
			got[replyAt-1])
	}
	for _, r := range got[replyAt:] {
		if isFillerFrame(r) {
			t.Fatalf("a late backend must never be talked over by the cover:\n%+v", got)
		}
	}
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

	time.Sleep(fillerTestDelay * 3)

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

	err := h.waitDone(idleTimeout + 5*time.Second)
	if err == nil {
		t.Fatal("a silent backend must still end the call with a non-nil error when a farewell is configured")
	}

	// The close is observed by the capture goroutine, not by this one, so wait
	// for it rather than assuming the handler's return already put it on the
	// wire.
	waitFor(t, 5*time.Second, func() bool {
		for _, r := range wire() {
			if r.closed {
				return true
			}
		}
		return false
	})

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

	closedAt := -1
	for i, r := range got {
		if r.closed {
			closedAt = i
			break
		}
	}
	for _, r := range got[closedAt:] {
		if isFarewellFrame(r) {
			t.Fatalf("no farewell frame may reach the carrier after the close:\n%+v", got)
		}
	}
	last := farewell[len(farewell)-1]
	if !last.at.Before(got[closedAt].at) {
		t.Fatalf("the farewell must be written before the carrier is closed: last frame at %v, close at %v",
			last.at, got[closedAt].at)
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

	endedAt := time.Now()
	if err := h.waitDone(idleTimeout + 2*time.Second); err == nil {
		t.Fatal("a silent backend must end the call with a non-nil error")
	}
	if took := time.Since(endedAt); took > idleTimeout*4 {
		t.Fatalf("with no farewell configured the idle branch must end the call at its bound, took %v (bound %v)",
			took, idleTimeout)
	}

	for _, r := range wire() {
		if r.closed {
			continue
		}
		t.Fatalf("a call with no farewell option must write nothing to the carrier on the idle path, got %+v", wire())
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

// --- behaviour 5: a healthy call is untouched -------------------------------

// TestHealthyCall_WritesNeitherCoverNorFarewell pins the promise that keeps
// this ticket safe to deploy: neither behaviour may put audio on a call that is
// working. A backend answering steadily is never covered, and never says
// goodbye — the idle guard does not fire at all.
//
// slopstop:test contract
func TestHealthyCall_WritesNeitherCoverNorFarewell(t *testing.T) {
	const idleTimeout = 400 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	be.emitInterval = idleTimeout / 8 // well inside both the idle bound and Delay

	h := silenceHarness(t, be.url(),
		WithIdleTimeout(idleTimeout),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithFarewellAudio(farewellTestClip()))
	wire := h.captureCarrierWire(t)

	// Several multiples of both bounds, with the backend talking the whole time.
	time.Sleep(idleTimeout * 4)

	assertStillRunning(t, h, "a backend answering steadily must not trip the idle guard")

	got := wire()
	if n := len(fillerFrames(got)); n != 0 {
		t.Fatalf("a healthy call must never hear the cover, got %d cover frames:\n%+v", n, got)
	}
	if n := len(farewellFrames(got)); n != 0 {
		t.Fatalf("a healthy call must never hear the farewell, got %d farewell frames:\n%+v", n, got)
	}
}
