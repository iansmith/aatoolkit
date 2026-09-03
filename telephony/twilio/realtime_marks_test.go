package twilio

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// Mark tests for the realtime path (AATK-105, "no way to learn when the
// carrier has finished playing what was sent").
//
// The classic path has answered "has everything I sent actually played?" since
// SOP-125: Session.sendMarkAndArmEcho writes a Twilio mark after a clip and
// handleMarkEchoControlEvent reads the echo as playout-complete. The realtime
// path dropped the same signal in pumpCarrierToBridge's default: case, so a
// consumer that wants to hang up after a spoken line had to estimate the
// playout position from the audio deltas it happened to observe — and those
// deltas drop when the consumer is behind (Bridge.publishEvent), so the
// estimate is short by however much it missed.
//
// These tests reuse realtime_wiring_test.go's harness (newRealtimeHarnessWith,
// realtimeHarness.captureCarrierWire via realtime_carrieraudio_test.go,
// waitFor, waitBackendReady, testChanBuffer) rather than growing a second copy
// of any of them.

// markHarness wires HandleStreamRealtime directly with the two mark options,
// which is the entry a consumer supplying its own channels would use. Both are
// resolved per call by the option pair's non-"For" sugar, mirroring
// idleHarness (realtime_idle_gaps_test.go).
func markHarness(t *testing.T, url string, requests <-chan string, echoes chan<- MarkEcho) *realtimeHarness {
	t.Helper()
	return newRealtimeHarnessWith(t, NewStreamHandler(url,
		WithMarkRequestChan(requests),
		WithMarkEchoChan(echoes),
	))
}

// silencePayloadB64 is one 20 ms μ-law frame of silence as it would arrive
// base64 from the backend. Distinct from carrierPayloadB64 only in intent:
// these tests drive audio purely to occupy the carrier's playout queue, and
// silence is what an end-to-end run can safely put through a real player.
func silencePayloadB64() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{telephony.MuLawSilence}, oneFrameBytes))
}

// echoMarkFromCarrier writes a Twilio mark echo back to the handler on the
// carrier connection, exactly as Twilio does once the named mark's audio has
// finished playing.
func echoMarkFromCarrier(t *testing.T, h *realtimeHarness, name string) {
	t.Helper()
	msg, err := EncodeMark(h.streamSID, name)
	if err != nil {
		t.Fatalf("EncodeMark: %v", err)
	}
	h.sendRaw(msg)
}

// captureCarrierRaw records the RAW bytes of every message the handler writes
// to the carrier, in arrival order.
//
// captureCarrierWire cannot serve the byte-identity assertion below: it
// decodes each frame through DecodeFrame and keeps only the fields it models,
// so a comparison against it pins the decoded projection rather than the
// wire. Behavior 3 is a claim about the bytes.
func (h *realtimeHarness) captureCarrierRaw(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var raw []string

	go func() {
		for {
			_, data, err := h.conn.Read(context.Background())
			if err != nil {
				return
			}
			mu.Lock()
			raw = append(raw, string(data))
			mu.Unlock()
		}
	}()

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), raw...)
	}
}

// markIndexes filters a carrier-wire capture down to the marks it saw, and
// reports each one's index in the full sequence — the ordering assertion needs
// the position, not just the presence.
func markIndexes(records []carrierWireRecord) []int {
	var out []int
	for i, r := range records {
		if r.markName != "" {
			out = append(out, i)
		}
	}
	return out
}

// --- RED #1: behavior 1 — the mark lands after the audio already written ----

// TestMarkRequestChan_MarkReachesCarrierAfterQueuedAudio is the ticket's
// behavior 1: a requested mark reaches the carrier AFTER every media frame the
// engine had already written, never interleaved ahead of queued audio.
//
// The assertion is positional, on the sequence the carrier actually received:
// three media frames are driven and observed on the wire FIRST, and only then
// is the mark requested, so an implementation that wrote the mark from
// HandleStreamRealtime's select loop without routing it through the sink's own
// writer would be free to land it anywhere — including ahead of a media frame
// Bridge.Run had not yet flushed, which is the defect the routing exists to
// prevent. Counting marks, or asserting one merely arrived, is green against
// that implementation.
//
// slopstop:test contract
func TestMarkRequestChan_MarkReachesCarrierAfterQueuedAudio(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	const mediaFrames = 3
	for i := 0; i < mediaFrames; i++ {
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": silencePayloadB64()})
	}
	waitFor(t, 5*time.Second, func() bool { return len(wire()) >= mediaFrames })

	requests <- "goodbye"
	waitFor(t, 5*time.Second, func() bool { return len(markIndexes(wire())) > 0 })

	got := wire()
	marks := markIndexes(got)
	if len(marks) != 1 {
		t.Fatalf("exactly one mark must reach the carrier, got %d in %+v", len(marks), got)
	}
	if marks[0] < mediaFrames {
		t.Fatalf("the mark landed at index %d, ahead of media already written; the whole sequence was %+v",
			marks[0], got)
	}
	for i := 0; i < mediaFrames; i++ {
		if got[i].markName != "" || got[i].payload == "" {
			t.Fatalf("frame %d must be one of the media frames written before the mark, got %+v", i, got[i])
		}
	}
}

// --- RED #2: behavior 2 — the echo reaches the consumer, by name ------------

// TestMarkEchoChan_EchoReachesTheConsumerWithItsName is the ticket's behavior
// 2: when the carrier echoes a requested mark, the consumer is told, with the
// name it asked for, and the record says the carrier answered rather than the
// engine's bound elapsing.
//
// The name is taken from the wire, not from the test's own variable: the
// contract is that the name the CARRIER echoed is the name delivered, so
// reading it back off the frame the handler wrote is what makes a hard-coded
// or reassigned name fail.
//
// slopstop:test contract
func TestMarkEchoChan_EchoReachesTheConsumerWithItsName(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	requests <- "goodbye"
	waitFor(t, 5*time.Second, func() bool { return len(markIndexes(wire())) > 0 })

	got := wire()
	onWire := got[markIndexes(got)[0]].markName
	if onWire != "goodbye" {
		t.Fatalf("the requested name must reach the carrier verbatim: got %q, want %q", onWire, "goodbye")
	}

	echoMarkFromCarrier(t, h, onWire)

	select {
	case rec := <-echoes:
		if rec.Name != onWire {
			t.Fatalf("the echo delivered must carry the name the carrier echoed: got %q, want %q", rec.Name, onWire)
		}
		if rec.TimedOut {
			t.Fatalf("a mark the carrier genuinely echoed must not be reported as timed out: got %+v", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the carrier echoed the mark and the consumer was never told")
	}
}

// TestMarkEchoChan_UnrequestedEchoIsNotDeliveredAsAMatch is behavior 2's other
// half: an echo whose name matches nothing outstanding is logged and NOT
// delivered as a match. handleMarkEchoControlEvent already draws exactly that
// distinction on the classic path, and it is what stops a consumer with two
// marks in flight from reading one as the other.
//
// slopstop:test contract
func TestMarkEchoChan_UnrequestedEchoIsNotDeliveredAsAMatch(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	// syncBuffer, not a bare bytes.Buffer: the line under test is written by
	// pumpCarrierToBridge's goroutine while this one reads it
	// (realtime_serverevents_gaps_test.go, where it was introduced for the
	// same reason). log.Writer is restored rather than nilled — a nil writer
	// panics the next log.Printf on any goroutine.
	var buf syncBuffer
	origOutput := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	requests <- "goodbye"
	waitFor(t, 5*time.Second, func() bool { return len(markIndexes(wire())) > 0 })

	echoMarkFromCarrier(t, h, "some-other-mark")

	waitFor(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "some-other-mark") })

	// Every record that arrives in the window is inspected, and the assertion
	// is on the NAME rather than on the channel merely being empty. "goodbye"
	// is genuinely outstanding here — it has to be, or "matches nothing
	// outstanding" would be vacuous — and nothing echoes it, so its own bound
	// fires markEchoGrace after it was written and puts a legitimate
	// {goodbye, TimedOut} record on this channel. That record is not the
	// defect under test, and a bare "the channel stayed empty" assertion
	// racing it would fail on a slow machine while the behavior was correct.
	deadline := time.After(2 * telephony.MarkEchoGraceMS * time.Millisecond)
	for done := false; !done; {
		select {
		case rec := <-echoes:
			if rec.Name == "some-other-mark" {
				t.Fatalf("an echo matching nothing outstanding must not be delivered as a match, got %+v", rec)
			}
		case <-deadline:
			done = true
		}
	}

	assertStillRunning(t, h, "an unmatched mark echo must not end the call")
}

// --- RED #3: behavior 3 — no options is byte-identical to today -------------

// TestRealtime_NoMarkOptionsIsByteIdenticalToToday is behavior 3: a call
// supplying neither option sends no mark and still ignores an inbound mark
// frame.
//
// The expected value is the frame EncodeMediaB64 produces as the code stands
// before this ticket, spelled out here rather than derived from the same
// helper the implementation calls — a comparison against EncodeMediaB64's own
// output would follow the implementation wherever it went, which is what
// realtime_instructions_test.go's handshakeBaseline comment says about a
// baseline that is not pinned.
//
// slopstop:test regression — guards: "A call that supplies neither option
// behaves byte-for-byte as it does today."
func TestRealtime_NoMarkOptionsIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())
	waitBackendReady(t, be, h)
	raw := h.captureCarrierRaw(t)

	payload := silencePayloadB64()
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": payload})
	waitFor(t, 5*time.Second, func() bool { return len(raw()) > 0 })

	// An inbound mark, which today's pumpCarrierToBridge drops on the floor.
	echoMarkFromCarrier(t, h, "nobody-asked-for-this")

	// Give the handler room to write a mark it must not write.
	time.Sleep(500 * time.Millisecond)

	want := fmt.Sprintf(`{"event":"media","streamSid":%q,"media":{"payload":%q}}`, h.streamSID, payload)
	got := raw()
	if len(got) != 1 {
		t.Fatalf("a call with no mark options must write exactly the media frames it wrote before, got %d frames: %q", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("the outbound frame must be byte-identical to today:\n got %s\nwant %s", got[0], want)
	}

	assertStillRunning(t, h, "an inbound mark on a call with no mark options must still be ignored")
}

// --- RED #4: the bound — a carrier that never echoes must not hang ----------

// TestMarkRequestChan_NonEchoingCarrierDoesNotHang is the ticket's fourth DoD
// item. The carrier here never echoes anything, so the engine's own bound is
// the only thing that can complete the consumer's wait.
//
// No audio is driven before the mark, so the derived bound is its floor —
// telephony.MarkEchoGraceMS, the same grace the classic path's MarkEchoTimeout
// adds atop the clip's playout. The assertion allows generous slack above that
// but is still far below the test's own failure deadline, so an implementation
// with no bound at all fails here rather than passing slowly.
//
// slopstop:test contract
func TestMarkRequestChan_NonEchoingCarrierDoesNotHang(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)

	start := time.Now()
	requests <- "goodbye"

	select {
	case rec := <-echoes:
		if rec.Name != "goodbye" {
			t.Fatalf("the timed-out record must name the mark that was requested: got %+v", rec)
		}
		if !rec.TimedOut {
			t.Fatalf("a mark the carrier never echoed must be reported as timed out: got %+v", rec)
		}
		if elapsed := time.Since(start); elapsed < telephony.MarkEchoGraceMS*time.Millisecond/2 {
			t.Fatalf("the bound fired after %s, well before the grace the carrier is owed; a mark must not be abandoned early", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a carrier that never echoes hung the consumer: no record ever arrived on the echo channel")
	}
}

// TestMarkRequestChan_BoundCoversQueuedPlayout pins the derivation the bound
// above has a floor of. A mark written behind queued audio must be given that
// audio's playout duration on top of the grace, or a carrier that is honouring
// the protocol perfectly gets reported as having timed out simply because it
// was still playing.
//
// The expected duration comes from telephony.MuLawDuration, which is the one
// definition of the μ-law byte-to-duration conversion (telephony/service.go).
// This test is what stops the bound being derived from a second, restated
// spelling of that arithmetic.
//
// slopstop:test contract
func TestMarkRequestChan_BoundCoversQueuedPlayout(t *testing.T) {
	// One second of μ-law audio, so the playout term dominates the grace and a
	// bound that ignored it would be unmistakable.
	const queued = telephony.SampleRateHz

	sink := newCarrierMediaSink(&discardWSWriter{}, "SSbound", nil, nil)
	base := time.Now()
	sink.playout.fed(queued, base)

	got := sink.markEchoBound(base)
	want := telephony.MuLawDuration(queued) + telephony.MarkEchoGraceMS*time.Millisecond
	if got != want {
		t.Fatalf("markEchoBound after %d bytes of queued audio = %s, want %s (MuLawDuration(%d) + the grace)",
			queued, got, want, queued)
	}

	// A clear discards the queue, so the next mark is owed the grace and
	// nothing more: waiting out audio nobody will hear is exactly what the
	// classic path's flush avoids.
	sink.playout.flush(base)
	if got, want := sink.markEchoBound(base), telephony.MarkEchoGraceMS*time.Millisecond; got != want {
		t.Fatalf("markEchoBound after a clear = %s, want %s", got, want)
	}
}

// discardWSWriter accepts every write. carrierMediaSink.conn is typed as the
// unexported wsWriter interface precisely so tests can substitute one (see
// output.go's comment on wsWriter, and failingWSWriter in
// realtime_carrieraudio_gaps_test.go, which does the same for the failure
// direction).
type discardWSWriter struct{}

func (discardWSWriter) Write(context.Context, websocket.MessageType, []byte) error { return nil }

// --- regression guards around the two options -------------------------------

// TestMarkRequestChan_EmptyNameIsNotSent guards the empty-name case, which
// mirrors WithVoiceUpdateChan's empty-voice rule for the same reason: an
// unnamed mark cannot be matched against an echo, so sending one would put a
// frame on the wire that no echo can ever resolve.
//
// slopstop:test regression — guards: "An echo whose name matches nothing
// outstanding is logged and not delivered as a match."
func TestMarkRequestChan_EmptyNameIsNotSent(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	requests <- ""
	time.Sleep(500 * time.Millisecond)

	if marks := markIndexes(wire()); len(marks) != 0 {
		t.Fatalf("an empty mark name must not reach the carrier, got %+v", wire())
	}
	assertStillRunning(t, h, "an empty mark name must leave the call running")
}

// TestMarkRequestChan_ClosedChannelLeavesTheCallRunning mirrors the guard the
// client-event and voice-update channels carry: closing the channel is not an
// error and does not end the call, and the loop must not busy-spin on a closed
// channel that is always ready.
//
// slopstop:test regression — guards: "A call that supplies neither option
// behaves byte-for-byte as it does today."
func TestMarkRequestChan_ClosedChannelLeavesTheCallRunning(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := markHarness(t, be.url(), requests, echoes)
	waitBackendReady(t, be, h)

	close(requests)
	time.Sleep(500 * time.Millisecond)

	assertStillRunning(t, h, "closing the mark-request channel must not end the call")
}

// TestMarkRequestChan_DoesNotResetTheIdleGuard keeps the mark seam consistent
// with every other consumer-driven channel on this path: a mark request is a
// consumer event, not backend activity, so a consumer marking steadily against
// a backend that has gone silent must not mask that silence.
//
// slopstop:test regression — guards: "A mark request does not reset the idle
// timeout."
func TestMarkRequestChan_DoesNotResetTheIdleGuard(t *testing.T) {
	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithIdleTimeout(idleGapTimeout),
		WithMarkRequestChan(requests),
		WithMarkEchoChan(echoes),
	))
	waitBackendReady(t, be, h)

	// Mark steadily, faster than the idle bound, against a backend that says
	// nothing after the handshake.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case requests <- fmt.Sprintf("mark-%d", i):
			}
			time.Sleep(idleGapTimeout / 4)
		}
	}()

	err := h.waitDone(10 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("a steady stream of mark requests must not hold a silent backend's call open; got err %v", err)
	}
}
