package twilio

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// Filler-audio tests for the realtime path (AATK-108, "the relay plays a paced
// loop between the caller's last word and the first reply frame").
//
// The wait these tests describe is real and measured: on the demo call this
// ticket was cut from, the median gap between the caller's last word and the
// backend's first audio frame was 10.5 s. Today the relay forwards silence
// across it. WithFillerAudio hands the relay a loop to play instead, and the
// whole contract is about the two edges of that loop — when it may start, and
// how fast it must stop.
//
// Every assertion here is made against the CARRIER'S OWN WIRE (captureCarrierWire,
// realtime_carrieraudio_test.go), not against a consumer channel, because what
// this ticket promises is what the caller hears.

// fillerTestDelay is the arm-to-start delay these tests configure.
//
// The ticket's own worked example uses 1 s against a 3 s silent backend. This
// is the same property at a fifth of the wall clock: what matters is that the
// delay is many multiples of one 20 ms frame (so "started late" and "started at
// once" are far apart) and that the observation window after it holds tens of
// frames (so "paced" and "burst" are far apart). At 300 ms the loop must not
// have played for 15 frames' worth of time, and the window below holds 20.
const fillerTestDelay = 300 * time.Millisecond

// fillerLoopFills are the μ-law byte values of the three frames the test loop is
// built from — distinct from each other so the wrap order is observable, and
// distinct from carrierPayloadB64's 0xFF fill so a loop frame is never confused
// with a backend delta on the wire.
var fillerLoopFills = []byte{0x01, 0x02, 0x03}

// fillerTestLoop is the consumer-supplied loop: three 20 ms μ-law frames.
func fillerTestLoop() []byte {
	var out []byte
	for _, f := range fillerLoopFills {
		out = append(out, bytes.Repeat([]byte{f}, defaultFrameBytes)...)
	}
	return out
}

// fillerFrameB64 is the base64 the carrier must receive for the nth frame of
// fillerTestLoop, so a test compares wire bytes against the loop it supplied
// rather than against a re-derivation of it.
func fillerFrameB64(n int) string {
	return base64.StdEncoding.EncodeToString(
		bytes.Repeat([]byte{fillerLoopFills[n%len(fillerLoopFills)]}, defaultFrameBytes))
}

// isFillerFrame reports whether a wire record is one of the loop's frames.
func isFillerFrame(rec carrierWireRecord) bool {
	if rec.clear || rec.markName != "" {
		return false
	}
	for i := range fillerLoopFills {
		if rec.payload == fillerFrameB64(i) {
			return true
		}
	}
	return false
}

// fillerFrames returns only the loop frames from a wire capture, preserving
// order and arrival time.
func fillerFrames(recs []carrierWireRecord) []carrierWireRecord {
	var out []carrierWireRecord
	for _, r := range recs {
		if isFillerFrame(r) {
			out = append(out, r)
		}
	}
	return out
}

// fillerHarness wires HandleStreamRealtime directly with a filler config, which
// is the entry a consumer supplying its own options would use. Mirrors
// idleHarness (realtime_idle_gaps_test.go).
func fillerHarness(t *testing.T, url string, cfg FillerConfig) *realtimeHarness {
	t.Helper()
	return newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, url, WithFillerAudio(cfg))
	})
}

// armFiller emits the event the ticket names as the arm trigger: the backend
// reporting that the caller stopped speaking.
func armFiller(t *testing.T, be *fakeRealtimeBackend) {
	t.Helper()
	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
}

// waitFillerPlaying blocks until at least n loop frames have reached the
// carrier, failing the test if they never do.
func waitFillerPlaying(t *testing.T, wire func() []carrierWireRecord, n int) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(fillerFrames(wire())) >= n })
}

// --- behaviour 3: start after Delay, paced at one frame per 20 ms ----------

// TestFiller_StartsAfterDelayAndPaces is the ticket's central case: the loop
// must not start before Delay, and once started it must be written in real
// time rather than dumped. The burst is the failure that matters — Twilio
// buffers outbound media, so a loop written ahead of the clock delays the real
// reply by the buffered length even after a clear.
//
// slopstop:test contract
func TestFiller_StartsAfterDelayAndPaces(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armedAt := time.Now()
	armFiller(t, be)

	waitFillerPlaying(t, wire, 1)
	first := fillerFrames(wire())[0]
	if waited := first.at.Sub(armedAt); waited < fillerTestDelay {
		t.Fatalf("the loop must not start before Delay: first frame at %v after arming, Delay is %v",
			waited, fillerTestDelay)
	}

	// One frame per 20 ms, never ahead: over a 400 ms window the carrier may
	// see about 20 frames, and a burst-written loop would show thousands.
	window := 400 * time.Millisecond
	time.Sleep(window)
	got := fillerFrames(wire())

	// Measured between the FIRST and LAST frame's own stamps rather than
	// against this goroutine's clock. Both stamps come from the capture
	// goroutine, so whatever lag it is running under cancels out of the
	// difference; comparing a capture stamp against time.Now() here would
	// count that lag as time the loop had no frames for, and fail a correctly
	// paced loop on a loaded machine.
	span := got[len(got)-1].at.Sub(first.at)
	most := int(span/(telephony.MuLawFrameMS*time.Millisecond)) + 3
	if len(got)-1 > most {
		t.Fatalf("the loop must be paced at one frame per %d ms and never run ahead: %d frames across %v (at most %d)",
			telephony.MuLawFrameMS, len(got), span, most+1)
	}

	// The floor only has to separate "playing" from "not playing at all" —
	// zero is the failure this catches. A quarter of the nominal rate leaves
	// room for a ticker that drops ticks under load, which Go's does rather
	// than firing them late in a burst.
	least := int(window/(telephony.MuLawFrameMS*time.Millisecond)) / 4
	if len(got) < least {
		t.Fatalf("the loop must keep playing while the backend is silent: only %d frames across %v (at least %d)",
			len(got), span, least)
	}
}

// --- behaviour 5: a fast turn is silent ------------------------------------

// TestFiller_NoStartWhenBackendIsFast pins the disarm: audio arriving inside
// Delay must leave the caller hearing nothing at all. A relay that armed a
// timer it never cancelled would play the loop over the reply.
//
// slopstop:test contract
func TestFiller_NoStartWhenBackendIsFast(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	time.Sleep(fillerTestDelay / 3)
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	// Well past the delay the loop would have started at.
	time.Sleep(fillerTestDelay * 3)

	if got := fillerFrames(wire()); len(got) != 0 {
		t.Fatalf("a backend that answers inside Delay must produce no loop frame at all, got %d", len(got))
	}
}

// --- behaviour 4: clear, then the reply's own first frame ------------------

// TestFiller_ClearThenFirstDelta is the ordering the whole design turns on. The
// carrier must see the clear BEFORE the reply's first media frame — a clear
// sent after it would discard the reply — and no loop frame may follow it.
//
// slopstop:test contract
func TestFiller_ClearThenFirstDelta(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
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
		t.Fatalf("the reply frame must arrive after the loop, got records %+v", got)
	}
	if !got[replyAt-1].clear {
		t.Fatalf("the record immediately before the reply's first frame must be the clear, got %+v",
			got[replyAt-1])
	}
	for _, r := range got[replyAt:] {
		if isFillerFrame(r) {
			t.Fatalf("no loop frame may reach the carrier after the reply's first frame:\n%+v", got)
		}
	}
}

// --- behaviour 4: barge-in stops the loop, with one clear ------------------

// TestFiller_StopsOnSpeechStarted pins the barge-in edge. The relay already
// sends exactly one clear on speech_started; the loop must stop against that
// clear rather than earning a second one, because a doubled clear is a
// behaviour change to barge-in that this ticket puts out of scope.
//
// slopstop:test contract
func TestFiller_StopsOnSpeechStarted(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	waitFillerPlaying(t, wire, 3)

	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})
	waitFor(t, 5*time.Second, func() bool {
		for _, r := range wire() {
			if r.clear {
				return true
			}
		}
		return false
	})
	time.Sleep(150 * time.Millisecond)

	got := wire()
	clears, clearAt := 0, -1
	for i, r := range got {
		if r.clear {
			clears++
			if clearAt < 0 {
				clearAt = i
			}
		}
	}
	if clears != 1 {
		t.Fatalf("speech_started must produce exactly one clear, got %d:\n%+v", clears, got)
	}
	for _, r := range got[clearAt:] {
		if isFillerFrame(r) {
			t.Fatalf("the loop must stop on speech_started, but a frame followed the clear:\n%+v", got)
		}
	}
}

// --- behaviour 2: the tool round trip re-arms ------------------------------

// TestFiller_RearmsAfterFunctionCall covers the longest wait measured on the
// demo call: the response that ends in a function call is not the reply, it is
// the start of a second wait — the tool round trip plus the second LLM leg —
// and the caller hears that as one silence.
//
// slopstop:test contract
func TestFiller_RearmsAfterFunctionCall(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	be.emitAny(t, map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"output": []any{
				map[string]any{"type": "function_call", "name": "lookup", "call_id": "c1"},
			},
		},
	})

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
	time.Sleep(100 * time.Millisecond)

	got := wire()
	replyAt := -1
	for i, r := range got {
		if r.payload == reply {
			replyAt = i
			break
		}
	}
	for _, r := range got[replyAt:] {
		if isFillerFrame(r) {
			t.Fatalf("the second leg's first delta must stop the loop:\n%+v", got)
		}
	}
}

// --- behaviour 3: the loop wraps without a seam ----------------------------

// TestFiller_LoopBoundaryIsSeamless pins the wrap. A loop that dropped or
// repeated a frame at the boundary would click once per pass, which is exactly
// the artefact a caller notices.
//
// slopstop:test contract
func TestFiller_LoopBoundaryIsSeamless(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	waitFillerPlaying(t, wire, 10)

	got := fillerFrames(wire())[:10]
	for i, r := range got {
		if want := fillerFrameB64(i); r.payload != want {
			t.Fatalf("loop frame %d must be frame %d of the loop, unbroken across the wrap:\n got  %q\nwant %q\nsequence %+v",
				i, i%len(fillerLoopFills), r.payload, want, got)
		}
	}
}

// --- behaviour 1: unset is today's behaviour -------------------------------

// TestFiller_UnsetWritesNothingWhileTheBackendIsSilent pins the off case against the same script
// every test above drives: a call supplying no filler option writes nothing to
// the carrier while the backend is silent, exactly as it did before the option
// existed.
//
// slopstop:test contract
func TestFiller_UnsetWritesNothingWhileTheBackendIsSilent(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	time.Sleep(fillerTestDelay * 3)

	if got := wire(); len(got) != 0 {
		t.Fatalf("a call with no filler option must write nothing to the carrier while the backend is silent, got %+v", got)
	}
}

// --- behaviour 7: a call's record says when the loop played ----------------

// TestFiller_CarrierAudioRecordsAreMarkedFiller pins the observation half of
// the ticket: a consumer watching what shipped to the carrier must be able to
// tell the loop from the reply, and the run of filler-marked records is what
// says exactly when the loop played.
//
// The ticket asks for this on the Tap. There is no Tap on the realtime path —
// it is wired only by the classic session (session.go) — and this path's
// equivalent seam is WithCarrierAudioChan, which already carries every record
// the sink writes. So the flag goes there, where a consumer can actually read
// it, rather than onto a recorder this transport does not construct.
//
// slopstop:test contract
func TestFiller_CarrierAudioRecordsAreMarkedFiller(t *testing.T) {
	ch := make(chan CarrierAudio, 128)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(),
			WithCarrierAudioChan(ch),
			WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}))
	})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
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

	var sawFiller, sawReply bool
	deadline := time.After(5 * time.Second)
	for !sawReply {
		select {
		case rec := <-ch:
			switch {
			case rec.Clear:
				// The clear that ends the loop; carries no payload.
			case rec.Payload == reply:
				if rec.Filler {
					t.Fatalf("the backend's own audio must not be marked as filler: %+v", rec)
				}
				sawReply = true
			default:
				if !rec.Filler {
					t.Fatalf("a loop frame must be marked as filler: %+v", rec)
				}
				sawFiller = true
			}
		case <-deadline:
			t.Fatal("the consumer never received the reply's record")
		}
	}
	if !sawFiller {
		t.Fatal("the consumer must receive the loop's frames as filler-marked records")
	}
}

// --- behaviour 2, the ordering that actually happens on a call -------------

// TestFiller_KeepsPlayingWhenRearmedMidLoop is the re-arm case in the order a
// real call produces it, and the one TestFiller_RearmsAfterFunctionCall does
// NOT reach: the loop is already playing when the tool call is announced.
//
// On a live call the function-call response.done arrives seconds into the
// wait, long after the loop has started — the caller stopped speaking, the
// loop began, and only then did the model finish deciding to call a tool. A
// re-arm that stopped the loop instead of continuing it would go silent for
// the whole tool round trip and second LLM leg, which is the longest wait this
// ticket measured and the exact case the re-arm exists for.
//
// slopstop:test contract
func TestFiller_KeepsPlayingWhenRearmedMidLoop(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	waitFillerPlaying(t, wire, 5)
	before := len(fillerFrames(wire()))

	be.emitAny(t, map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"output": []any{
				map[string]any{"type": "function_call", "name": "lookup", "call_id": "c1"},
			},
		},
	})

	// Well under Delay: a machine that stopped and re-armed would be silent
	// across this whole window, while one that kept playing adds ~10 frames.
	time.Sleep(fillerTestDelay / 2)

	after := len(fillerFrames(wire()))
	if after <= before {
		t.Fatalf("a function call announced mid-loop must not silence the loop: %d frames before, %d after",
			before, after)
	}

	// And the machine must still be live afterwards: the second leg's first
	// delta stops it, with the clear the carrier is owed.
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
	got := wire()
	replyAt := -1
	for i, r := range got {
		if r.payload == reply {
			replyAt = i
			break
		}
	}
	if replyAt < 1 || !got[replyAt-1].clear {
		t.Fatalf("the second leg's first frame must be preceded by the clear:\n%+v", got)
	}
}

// --- behaviour 4: the turn ending stops the loop, without a clear ----------

// TestFiller_StopsOnTurnEndingResponseDone pins the other half of the
// response.done branch. A response that produced no audio still ends the turn,
// and the loop must not outlive it waiting for a delta that is never coming.
//
// No clear goes with this one, deliberately: nothing is being interrupted, and
// the frames already written are the caller's to hear out. The assertion that
// no clear was sent is what distinguishes this stop from every other one.
//
// slopstop:test contract
func TestFiller_StopsOnTurnEndingResponseDone(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	waitFillerPlaying(t, wire, 3)

	be.emitAny(t, map[string]any{
		"type":     "response.done",
		"response": map[string]any{"output": []any{}},
	})

	// Let the stop take effect, then measure a window it must not write in.
	time.Sleep(100 * time.Millisecond)
	settled := len(fillerFrames(wire()))
	time.Sleep(fillerTestDelay)

	if got := len(fillerFrames(wire())); got != settled {
		t.Fatalf("a turn-ending response.done must stop the loop: %d frames at rest, %d after a further %v",
			settled, got, fillerTestDelay)
	}
	for _, r := range wire() {
		if r.clear {
			t.Fatalf("a turn-ending response.done must stop the loop without a clear:\n%+v", wire())
		}
	}
}

// --- behaviour 1: the per-call resolver -----------------------------------

// TestWithFillerAudioFor_ResolvesFromTheStartFrame pins the "For" twin the
// ticket names beside the value form. It is the only entry that can vary the
// loop or the delay by caller, and what makes that possible is the start frame
// it is handed — so the assertion is that the frame arrives, not merely that
// something played.
//
// slopstop:test contract
func TestWithFillerAudioFor_ResolvesFromTheStartFrame(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(),
			WithFillerAudioFor(func(s Frame) FillerConfig {
				mu.Lock()
				seen = append(seen, s.StreamSID)
				mu.Unlock()
				return FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}
			}))
	})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	waitFillerPlaying(t, wire, 3)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != h.streamSID {
		t.Fatalf("the resolver must be called once with the call's own start frame, got %q (want [%q])",
			seen, h.streamSID)
	}
}

// --- the write-error exit ---------------------------------------------------

// scriptedWSWriter records every message written and can be told to fail one
// of them, so a test can drive carrierMediaSink's error path deterministically
// and then keep using the same sink afterwards.
//
// Distinct from failingWSWriter (realtime_carrieraudio_gaps_test.go), which
// fails EVERY write: what this test needs is a carrier that fails once and
// then works, because the property under test is what the machine does AFTER
// a failed write. Recording the messages is the other half — the assertion is
// about which frames reached the wire and in what order.
type scriptedWSWriter struct {
	mu     sync.Mutex
	failAt int // 1-based index of the write that fails; 0 = never
	n      int
	msgs   [][]byte
}

func (w *scriptedWSWriter) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	if w.failAt != 0 && w.n == w.failAt {
		return errors.New("carrier write failed")
	}
	w.msgs = append(w.msgs, append([]byte(nil), data...))
	return nil
}

// failNext arms the failure for the write after the ones already made, and
// returns that write's index so a caller can wait for it to be ATTEMPTED.
// Waiting on the recorded frames instead would not do: the failing write is by
// definition the one that never gets recorded.
func (w *scriptedWSWriter) failNext() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failAt = w.n + 1
	return w.failAt
}

// attempts is how many writes have been tried, recorded or not.
func (w *scriptedWSWriter) attempts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// records decodes what the carrier received, reusing the same DecodeFrame the
// wire capture trusts.
func (w *scriptedWSWriter) records() []carrierWireRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []carrierWireRecord
	for _, m := range w.msgs {
		f, err := DecodeFrame(m)
		if err != nil {
			continue
		}
		switch f.Event {
		case EventMedia:
			out = append(out, carrierWireRecord{payload: f.EncodedPayload})
		case EventClear:
			out = append(out, carrierWireRecord{clear: true})
		}
	}
	return out
}

// TestFiller_SurvivesACarrierWriteFailure pins what the machine does after one
// of its own frames fails to reach the carrier.
//
// The play goroutine returns on a write error, and the state it leaves behind
// is the whole point: if it exits with the machine still marked as playing,
// nothing is running, nothing is scheduled, and every later arm returns at the
// "already playing" guard — the loop is dead for the rest of the call. Worse,
// the next reply reads that stale flag and sends a clear the carrier is not
// owed, discarding audio it had every right to play.
//
// The comment that used to sit on that exit said the failure "ends the call by
// its own path — the bridge's next write reports it". That is only true if the
// bridge writes again, and a filler frame fails precisely when the backend is
// SILENT, which is when no reply frame is coming to report anything.
//
// Built against carrierMediaSink directly rather than through
// HandleStreamRealtime, for the reason
// TestCarrierMediaSink_WriteFailureNeverDeliversCarrierAudio states in full:
// the sink's conn is the unexported wsWriter interface precisely so a test can
// fail a write deterministically, while HandleStreamRealtime takes a concrete
// *websocket.Conn.
//
// slopstop:test contract
func TestFiller_SurvivesACarrierWriteFailure(t *testing.T) {
	w := &scriptedWSWriter{}
	fill := newFiller(context.Background(), FillerConfig{
		Loop:  fillerTestLoop(),
		Delay: 20 * time.Millisecond,
	})
	if fill == nil {
		t.Fatal("test setup: the config must produce a live filler")
	}
	t.Cleanup(fill.shutdown)
	sink := newCarrierMediaSink(w, "SSwritefail", nil, nil, fill)
	fill.attach(sink)

	countFiller := func() int { return len(fillerFrames(w.records())) }

	fill.arm()
	waitFor(t, 5*time.Second, func() bool { return countFiller() >= 2 })

	// Fail the loop's next frame, and wait for that write to have been tried
	// rather than for the frame count to settle: the failing write is the one
	// that never gets recorded, so the count is not what moves.
	failing := w.failNext()
	waitFor(t, 5*time.Second, func() bool { return w.attempts() >= failing })

	// And let the play goroutine act on the error it just got.
	settled := countFiller()
	waitFor(t, 5*time.Second, func() bool { return countFiller() == settled })

	// The reply arrives. Nothing is queued that the caller has not already
	// heard, so no clear is owed — a clear here would flush audio the carrier
	// was entitled to play.
	reply := carrierPayloadB64()
	if err := sink.Media(context.Background(), reply); err != nil {
		t.Fatalf("Media after a filler write failure: %v", err)
	}
	for _, r := range w.records() {
		if r.clear {
			t.Fatalf("a failed filler write must not leave a clear owed to the carrier:\n%+v", w.records())
		}
	}

	// And the machine must still be usable: the next wait gets a loop.
	before := countFiller()
	fill.arm()
	waitFor(t, 5*time.Second, func() bool { return countFiller() > before })
}

// --- the loop restarts at the top of the clip -------------------------------

// TestFiller_EachEpisodeStartsAtTheTopOfTheLoop pins the read offset resetting
// between waits. Two waits on one call is the ordinary shape of a
// conversation, and a loop that resumed mid-clip on the second one would start
// the caller somewhere arbitrary in the audio the consumer supplied — which is
// exactly what a consumer supplying a clip with a deliberate beginning does
// not expect.
//
// slopstop:test contract
func TestFiller_EachEpisodeStartsAtTheTopOfTheLoop(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	// First wait: let the loop get past the wrap, so a machine that failed to
	// reset would resume at a frame that is provably not the first.
	armFiller(t, be)
	waitFillerPlaying(t, wire, 4)

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

	// Second wait.
	armFiller(t, be)
	waitFor(t, 5*time.Second, func() bool { return len(secondEpisode(wire(), reply)) > 0 })

	if got := secondEpisode(wire(), reply)[0]; got.payload != fillerFrameB64(0) {
		t.Fatalf("the second wait must start at the top of the loop:\n got  %q\nwant %q",
			got.payload, fillerFrameB64(0))
	}
}

// secondEpisode returns the loop frames written after the record carrying
// reply — that is, the frames belonging to the wait that followed it.
func secondEpisode(recs []carrierWireRecord, reply string) []carrierWireRecord {
	for i, r := range recs {
		if r.payload == reply {
			return fillerFrames(recs[i:])
		}
	}
	return nil
}

// --- the call ending stops the loop -----------------------------------------

// TestFiller_NoPlayGoroutineOutlivesTheCall pins the last of the ticket's stop
// conditions: "also stop on backend error, or the call ending". The play
// goroutine is the only thing on this path that keeps running with no event to
// drive it, so a call that ended while the loop was playing is the one way
// this option could leak a goroutine per call.
//
// Measured differentially, mirroring
// TestCarrierAudioChan_NoEngineGoroutineOutlivesTheCall: each call leaves
// fixed overhead behind (the httptest server and carrier conn live until
// t.Cleanup), so an absolute count proves nothing.
//
// slopstop:test non-interference — paired: asserts the loop was actually
// PLAYING on each call, so the measurement covers a live goroutine rather than
// one that never started
func TestFiller_NoPlayGoroutineOutlivesTheCall(t *testing.T) {
	const calls = 5

	run := func(withFiller bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			if withFiller {
				opts = append(opts, WithFillerAudio(FillerConfig{
					Loop: fillerTestLoop(), Delay: 50 * time.Millisecond,
				}))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			wire := h.captureCarrierWire(t)
			if withFiller {
				armFiller(t, be)
				waitFillerPlaying(t, wire, 3)
			}
			// End the call from the backend side, with the loop still running.
			be.closeNow()
			h.waitDone(5 * time.Second)
		}

		runtime.GC()
		time.Sleep(200 * time.Millisecond)
		return runtime.NumGoroutine() - before
	}

	base := run(false)
	withFiller := run(true)

	if withFiller > base+1 {
		t.Fatalf("a call whose loop was playing when it ended must leave no goroutine behind: "+
			"%d goroutines grew with filler audio, %d without", withFiller, base)
	}
}

// TestFiller_ShutdownReleasesAPlayGoroutineParkedOnTheWriteSlot pins the one
// thing shutdown does that nothing else on this path does.
//
// The play goroutine's other exits all depend on getting an answer from the
// carrier: a frame declined, or a write that returns an error. Neither happens
// while it is parked WAITING for the write slot, and that wait is real — Media
// and Clear hold the slot on the call's own unbounded context, so a carrier
// that has stopped draining parks whoever is behind them indefinitely. The
// filler's private context is what bounds it, and shutdown is what cancels
// that context.
//
// Measured against the parked goroutine directly rather than through a call,
// because a call ending closes the carrier connection and the resulting write
// error would end the loop by a different door — which is exactly why
// TestFiller_NoPlayGoroutineOutlivesTheCall passes even with shutdown gutted,
// and why this test exists beside it.
//
// slopstop:test contract
func TestFiller_ShutdownReleasesAPlayGoroutineParkedOnTheWriteSlot(t *testing.T) {
	w := &blockingWSWriter{entered: make(chan struct{}, 4), release: make(chan struct{})}
	fill := newFiller(context.Background(), FillerConfig{
		Loop:  fillerTestLoop(),
		Delay: 20 * time.Millisecond,
	})
	sink := newCarrierMediaSink(w, "SSpark", nil, nil, fill)
	fill.attach(sink)

	// Released through Cleanup rather than at the end of the body, so a
	// t.Fatal on any assertion below still frees the parked media write. Left
	// blocked, it would sit there for the life of the test binary and inflate
	// every later goroutine count in this package — including the two other
	// differentials in this file.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.release) }) }
	t.Cleanup(release)

	// Occupy the slot on an unbounded context, as Media does on a real call.
	mediaDone := make(chan error, 1)
	go func() { mediaDone <- sink.Media(context.Background(), carrierPayloadB64()) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("test setup: the media write never reached the carrier connection")
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	parked := runtime.NumGoroutine()

	// The loop starts and immediately parks behind that write.
	fill.arm()
	time.Sleep(300 * time.Millisecond)
	if got := runtime.NumGoroutine(); got <= parked {
		t.Fatalf("test setup: the play goroutine must be parked on the write slot (%d goroutines, was %d)",
			got, parked)
	}

	fill.shutdown()

	deadline := time.After(5 * time.Second)
	for {
		if runtime.NumGoroutine() <= parked {
			break
		}
		select {
		case <-deadline:
			t.Fatal("shutdown must release a play goroutine parked on the carrier write slot")
		case <-time.After(20 * time.Millisecond):
		}
	}

	release()
	<-mediaDone
}

// --- the play goroutine's own exits -----------------------------------------

// TestFiller_PlayGoroutineEndsWithTheEpisode pins the exit that runs on every
// ordinary turn: the frame-declined return, taken when writeFrame reports that
// the machine has left the generation this goroutine was playing under.
//
// Without it the goroutine keeps ticking at 50 Hz for the REST OF THE CALL,
// taking the carrier write slot on every tick and contending with the reply's
// own writes, while writing nothing — so no assertion about frames on the wire
// can see it. TestFiller_ClearThenFirstDelta passes either way, because the
// guard inside the slot still declines each frame. The goroutine is the only
// observable, and it has to be measured WITHIN a live call: measuring across a
// call boundary lets the goroutine exit by teardown instead, which is what
// TestFiller_NoPlayGoroutineOutlivesTheCall would let it do.
//
// slopstop:test contract
func TestFiller_PlayGoroutineEndsWithTheEpisode(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	idle := runtime.NumGoroutine()

	armFiller(t, be)
	waitFillerPlaying(t, wire, 3)
	if got := runtime.NumGoroutine(); got <= idle {
		t.Fatalf("test setup: the play goroutine must be running (%d goroutines, was %d)", got, idle)
	}

	// The reply ends the episode. The call itself keeps running.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	deadline := time.After(5 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= idle {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the play goroutine must end with its episode, not outlive it inside a live call "+
				"(%d goroutines, idle was %d)", runtime.NumGoroutine(), idle)
		case <-time.After(20 * time.Millisecond):
		}
	}
	assertStillRunning(t, h, "the episode ending must not end the call")
}

// TestFiller_CallTeardownLogsNoCarrierError pins play's own context case.
//
// Its effect is an ABSENCE, which is why nothing caught it. The case is
// redundant for TERMINATION — the frame-declined exit would end the goroutine
// a tick later regardless — and what it buys is that the loop stops without
// waiting for that tick. Remove it and a tick lands on a connection being torn
// down, which fails the write and logs a carrier error on a call that ended
// perfectly normally.
//
// It does NOT pin HandleStreamRealtime's `defer fill.shutdown()`; deleting
// that call site leaves this green. TestFiller_ShutdownIsCalledWhenTheCallEnds
// below is what covers it, and the reason it takes a different shape is stated
// there.
//
// Five calls rather than one: the losing tick is a race against teardown, so a
// single call can win it by luck. Each call ends with the loop mid-flight,
// which is the only arrangement that can lose it at all.
//
// slopstop:test contract
func TestFiller_CallTeardownLogsNoCarrierError(t *testing.T) {
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	for i := 0; i < 5; i++ {
		be := newFakeRealtimeBackend(t)
		h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop(), Delay: 50 * time.Millisecond})
		waitBackendReady(t, be, h)
		wire := h.captureCarrierWire(t)

		armFiller(t, be)
		waitFillerPlaying(t, wire, 2)

		// End the call with the loop still playing.
		be.closeNow()
		h.waitDone(5 * time.Second)
	}

	// Let any tick that survived teardown land.
	time.Sleep(200 * time.Millisecond)

	if bytes.Contains(buf.Bytes(), []byte("filler audio")) {
		t.Fatalf("a call ending while the loop plays must stop it before the carrier connection goes away, "+
			"never write into a torn-down one; log output: %q", buf.String())
	}
}

// TestFiller_ShutdownIsCalledWhenTheCallEnds pins the call site rather than the
// method — HandleStreamRealtime's `defer fill.shutdown()`.
//
// It takes an unusual shape for a reason that is the finding itself. Normally
// the filler's context descends from the handler's, so the play goroutine dies
// with the request whether or not shutdown is ever called; deleting the defer
// leaves every other test in this file green, because the parent context
// covers for it within microseconds. That coverage is real but it is not the
// engine's own doing, and a consumer entering by HandleStreamRealtime with a
// long-lived context of its own — which the signature invites, and which
// TestFiller_ShutdownReleasesAPlayGoroutineParkedOnTheWriteSlot's rationale
// depends on — gets no such help.
//
// So these calls are given a context that OUTLIVES the handler. Nothing but the
// deferred shutdown then stops the loop before the carrier connection is torn
// down, and the observable is what a tick lands on afterwards: a write into a
// closed connection, which fails and logs. The goroutine differential is the
// second half — it still exits, by the error path, which is precisely why the
// count alone cannot see this and the log can.
//
// slopstop:test contract
func TestFiller_ShutdownIsCalledWhenTheCallEnds(t *testing.T) {
	const calls = 3

	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	run := func(withFiller bool) int {
		// Deliberately not the harness's context: see above. One per batch,
		// cancelled only after the measurement, so nothing but the deferred
		// shutdown can end a loop.
		callCtx, cancelCall := context.WithCancel(context.Background())
		defer cancelCall()

		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			if withFiller {
				opts = append(opts, WithFillerAudio(FillerConfig{
					Loop: fillerTestLoop(), Delay: 50 * time.Millisecond,
				}))
			}
			h := newRealtimeHarnessWith(t, func(_ context.Context, conn *websocket.Conn, start Frame) error {
				return HandleStreamRealtime(callCtx, conn, start, be.url(), opts...)
			})
			waitBackendReady(t, be, h)
			wire := h.captureCarrierWire(t)
			if withFiller {
				armFiller(t, be)
				waitFillerPlaying(t, wire, 3)
			}
			// End the call with the loop still playing. callCtx stays live.
			be.closeNow()
			h.waitDone(5 * time.Second)
		}

		runtime.GC()
		time.Sleep(300 * time.Millisecond)
		return runtime.NumGoroutine() - before
	}

	base := run(false)
	withFiller := run(true)

	if bytes.Contains(buf.Bytes(), []byte("filler audio")) {
		t.Fatalf("the call ending must stop the loop before the carrier connection goes away, even when "+
			"the caller's context outlives the call; log output: %q", buf.String())
	}
	if withFiller > base+1 {
		t.Fatalf("the call ending must stop the loop even when the caller's context outlives it: "+
			"%d goroutines grew with filler audio, %d without", withFiller, base)
	}
}
