package twilio

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"runtime"
	"sync"
	"testing"
	"time"
)

// CarrierAudio-channel tests for the realtime path (AATK-84, "Let a consumer
// observe the audio the engine sends to the carrier").
//
// carrierMediaSink is the carrier-facing side of Bridge — it writes the
// backend's audio and barge-in signals to the Twilio Media Streams
// WebSocket — and today there is no way to observe what it sent. This ticket
// adds WithCarrierAudioChan / WithCarrierAudioChanFor, mirroring the shape
// WithServerEventChan already uses: the consumer owns its channel, the engine
// never closes it, and delivery is non-blocking so a slow or absent consumer
// never stalls carrier audio (the same hazard AATK-75/AATK-79 fought for
// transcripts, and the reason this ticket was rewritten away from a
// synchronous MediaSink wrapper before any code existed).

// carrierWireRecord is one message this suite observed on the carrier
// connection — a media payload, a clear, or a mark — so a test can compare
// what the consumer's channel delivered against what the carrier actually
// received, rather than against what the test merely intended to send.
//
// markName was added by AATK-105, which needed the mark's POSITION in the
// sequence rather than only its presence: the ordering property that ticket
// buys is "the mark lands after the media already written", and a capture that
// dropped marks could not see it. Recording it here rather than in a second
// capture helper keeps one definition of what this suite saw on the wire.
//
// at was added by AATK-108 for the same reason one step further: filler audio
// is paced, so its contract is about WHEN each frame arrived, not only in what
// order. Stamped as the record is appended, on the reader goroutine, which is
// the closest this suite can stand to the carrier's own clock.
type carrierWireRecord struct {
	payload  string
	clear    bool
	markName string
	at       time.Time
}

// captureCarrierWire mirrors realtimeHarness.countMediaFrames
// (realtime_wiring_test.go:330), but records the payload string of each
// media frame and observes clear frames too, via the same DecodeFrame this
// package already trusts (realtime_wiring_test.go:43,
// TestDecodeFrame_CarriesEncodedPayloadVerbatim) — so the test asserts
// against the carrier's own wire bytes, not a re-derived approximation.
//
// Opt-in rather than always-on, for the same reason countMediaFrames is:
// several tests read h.conn themselves, and a background reader would race
// them for the same bytes.
func (h *realtimeHarness) captureCarrierWire(t *testing.T) func() []carrierWireRecord {
	t.Helper()
	var mu sync.Mutex
	var records []carrierWireRecord

	go func() {
		for {
			_, data, err := h.conn.Read(context.Background())
			if err != nil {
				return
			}
			f, err := DecodeFrame(data)
			if err != nil {
				continue
			}
			now := time.Now()
			switch f.Event {
			case EventMedia:
				mu.Lock()
				records = append(records, carrierWireRecord{payload: f.EncodedPayload, at: now})
				mu.Unlock()
			case EventClear:
				mu.Lock()
				records = append(records, carrierWireRecord{clear: true, at: now})
				mu.Unlock()
			case EventMark:
				mu.Lock()
				records = append(records, carrierWireRecord{markName: f.MarkName, at: now})
				mu.Unlock()
			}
		}
	}()

	return func() []carrierWireRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]carrierWireRecord(nil), records...)
	}
}

// sameMessage reports whether two records describe the same carrier message,
// ignoring when it was observed. at is metadata about the OBSERVATION, not
// part of the message, so a whole-struct comparison against a want literal
// would compare an arrival instant no test can predict.
func (r carrierWireRecord) sameMessage(other carrierWireRecord) bool {
	return r.payload == other.payload && r.clear == other.clear && r.markName == other.markName
}

// distinctCarrierPayload builds a carrier-shaped base64 payload filled with a
// chosen byte, distinguishable from carrierPayloadB64's fixed 0xFF fill, so a
// test driving two calls can tell which call's audio it observed.
func distinctCarrierPayload(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, oneFrameBytes))
}

// --- behavior 1 & the RED-first case: a media record reaches the consumer --

// TestCarrierAudioChan_ConsumerReceivesMediaPayload is the ticket's own
// RED-first case: "a test asserting an observer receives a media record,
// failing because there is no way to install one." The observed payload is
// asserted against captureCarrierWire's record of what the carrier actually
// received, per the ticket's test expectations, not against the delta value
// the test itself chose to send.
//
// slopstop:test contract
func TestCarrierAudioChan_ConsumerReceivesMediaPayload(t *testing.T) {
	ch := make(chan CarrierAudio, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	payload := carrierPayloadB64()
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": payload})

	waitFor(t, 5*time.Second, func() bool { return len(wire()) > 0 })
	got := wire()
	if got[0].clear || got[0].payload != payload {
		t.Fatalf("test setup: carrier must have received the media payload first, got %+v", got[0])
	}

	select {
	case rec := <-ch:
		if rec.Clear {
			t.Fatalf("consumer must receive a media record, not a clear: %+v", rec)
		}
		if rec.Payload != got[0].payload {
			t.Fatalf("consumer must receive the exact payload the carrier received:\n got  %q\nwant %q",
				rec.Payload, got[0].payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received a media record")
	}
}

// --- behavior 2: a Clear is distinguishable from a Media -------------------

// TestCarrierAudioChan_ConsumerReceivesClearRecord pins the DoD item easy to
// miss: "A Clear is asserted as well as a Media." A speech-started event
// drives carrierMediaSink.Clear (precedent:
// TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnSpeechStartedOnly,
// realtime_idle_gaps_test.go:118), and the record the consumer receives for
// it must be distinguishable from a media record — barge-in observable, not
// just audio.
//
// slopstop:test contract
func TestCarrierAudioChan_ConsumerReceivesClearRecord(t *testing.T) {
	ch := make(chan CarrierAudio, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)

	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})

	select {
	case rec := <-ch:
		if !rec.Clear {
			t.Fatalf("a speech-started event must produce a Clear record, got %+v", rec)
		}
		if rec.Payload != "" {
			t.Fatalf("a Clear record must carry no payload, got %q", rec.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received a clear record")
	}
}

// --- behavior 3: the engine never closes the consumer's channel ------------

// TestCarrierAudioChan_EngineNeverClosesTheConsumersChannel mirrors
// TestTranscriptChan_EngineNeverClosesTheConsumersChannel and
// TestClientEventChan_EngineNeverClosesTheConsumersChannel: a channel reused
// across two calls must still be open, and receiving from it, after both.
// The positive delivery check on each call is what actually exercises this
// ticket's wiring — a negative-only "still open" check would pass against an
// inert stub that never touches the channel at all.
//
// slopstop:test contract
func TestCarrierAudioChan_EngineNeverClosesTheConsumersChannel(t *testing.T) {
	ch := make(chan CarrierAudio, testChanBuffer)

	for i, payload := range []string{distinctCarrierPayload(0xAA), distinctCarrierPayload(0xBB)} {
		be := newFakeRealtimeBackend(t)
		h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
		waitBackendReady(t, be, h)

		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": payload})

		select {
		case rec := <-ch:
			if rec.Payload != payload {
				t.Fatalf("call %d: got %q, want %q", i+1, rec.Payload, payload)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call %d: no carrier-audio record arrived", i+1)
		}

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)
	}

	// A closed channel would yield a zero value immediately rather than block.
	select {
	case rec, ok := <-ch:
		t.Fatalf("engine must not close the consumer's channel; received %+v (open=%v)", rec, ok)
	case <-time.After(200 * time.Millisecond):
	}
}

// --- behavior 4 (part 1): a consumer that never reads does not stall audio -

// TestCarrierAudioChan_ConsumerThatNeverReadsDoesNotStallAudio pins the DoD
// item "A consumer that never reads does not delay carrier audio." Mirrors
// TestTranscriptChan_ConsumerThatNeverReadsDoesNotStallAudio: a tiny,
// undrained consumer channel must not slow the carrier-facing writes that
// share the same backend read loop.
//
// slopstop:test non-interference — paired: first proves the channel is
// genuinely wired (one record must actually reach it) before checking that
// 50 further media frames still reach the carrier (h.countMediaFrames) while
// the channel is never drained again — an inert stub would pass the
// never-reads half vacuously without the leading proof read, since a
// consumer wired to nothing cannot possibly slow anything down
func TestCarrierAudioChan_ConsumerThatNeverReadsDoesNotStallAudio(t *testing.T) {
	ch := make(chan CarrierAudio, 1) // deliberately tiny

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	// Positive: the channel is genuinely wired to receive at least this
	// first record.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received a carrier-audio record")
	}

	// Negative: never drain ch again while audio keeps flowing.
	for i := 0; i < 50; i++ {
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	}

	waitFor(t, 5*time.Second, func() bool { return seen() >= 51 })
}

// --- behavior 4 (part 2): a consumer that never reads does not trip idle ---

// TestCarrierAudioChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer pins the
// DoD item "and does not trip the idle timer when one is armed." A consumer
// that stalled the backend read loop would starve the activity signal
// bridge.Activity() feeds and end the call reporting "idle timeout" —
// blaming the backend for a consumer's fault, the same interaction
// TestTranscriptChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer guards for
// transcripts.
//
// slopstop:test non-interference — paired: first proves the channel is
// genuinely wired (one record must actually reach it) before checking that
// carrier audio keeps arriving (h.countMediaFrames) and the idle timer never
// fires while the channel goes undrained for the rest of the test — without
// the leading proof read an inert stub would pass this vacuously, since a
// consumer wired to nothing cannot possibly trip anything
func TestCarrierAudioChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer(t *testing.T) {
	ch := make(chan CarrierAudio, 1)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithCarrierAudioChan(ch),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	// Positive: the channel is genuinely wired.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received a carrier-audio record")
	}

	// Negative: keep driving backend activity while ch goes undrained for
	// the rest of the test; the idle timer must not fire.
	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type":  "response.output_audio.delta",
		"delta": carrierPayloadB64(),
	})

	time.Sleep(idleGapTimeout * 5)

	if seen() < 2 {
		t.Fatal("carrier audio must keep flowing while the idle timer is armed and the consumer channel is full")
	}
	assertStillRunning(t, h, "a consumer that never reads must not let the idle timer fire on a live backend")
}

// --- behavior 5: no engine goroutine outlives a call whose consumer never
// --- reads ------------------------------------------------------------------

// TestCarrierAudioChan_NoEngineGoroutineOutlivesTheCall pins the DoD item
// "No engine goroutine outlives a call whose consumer never reads.
// Differential measurement, as AATK-79, and mutation-checked." Mirrors
// TestTranscriptChan_NoEngineGoroutineOutlivesTheCall and
// TestClientEventChan_NoEngineGoroutineOutlivesTheCall: measured
// differentially, since each call leaves fixed overhead behind (the
// httptest server and carrier conn live until t.Cleanup), so an absolute
// count proves nothing.
//
// slopstop:test non-interference — paired: asserts the consumer channel
// actually receives one record per call (proving genuine wiring, not an
// inert stub) before measuring goroutine growth against a run with no option
func TestCarrierAudioChan_NoEngineGoroutineOutlivesTheCall(t *testing.T) {
	const calls = 5

	run := func(withConsumer bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			var ch chan CarrierAudio
			if withConsumer {
				// Buffer 1 and drained only once below: the consumer is
				// present and permanently behind after its proof read,
				// which is the worst case.
				ch = make(chan CarrierAudio, 1)
				opts = append(opts, WithCarrierAudioChan(ch))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			if withConsumer {
				be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("consumer never received a carrier-audio record")
				}
			}
			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			_ = h.waitDone(5 * time.Second)
		}

		// Poll rather than trust one instantaneous reading: a goroutine that
		// is exiting correctly may not have been descheduled yet.
		deadline := time.After(3 * time.Second)
		growth := runtime.NumGoroutine() - before
		for {
			select {
			case <-deadline:
				return growth
			default:
			}
			runtime.GC()
			time.Sleep(25 * time.Millisecond)
			if g := runtime.NumGoroutine() - before; g < growth {
				growth = g
			}
		}
	}

	withConsumer := run(true)
	withNone := run(false)

	if withConsumer > withNone {
		t.Fatalf("a consumer that never reads must cost no goroutines: %d over %d calls, against %d with no consumer",
			withConsumer, calls, withNone)
	}
}

// --- behavior 6: drops are logged -------------------------------------------

// TestCarrierAudioChan_DropIsLogged pins the DoD item "Drops are logged
// rather than silent." Mirrors TestServerEventChan_DropIsLogged's
// log-capture pattern. This test does not pin whether Media and Clear drops
// share one counter or are counted separately — the ticket leaves that
// choice open — only that a drop is logged at all.
//
// slopstop:test contract
func TestCarrierAudioChan_DropIsLogged(t *testing.T) {
	// syncBuffer, not bytes.Buffer: a compliant implementation may log drops
	// from a goroutine other than this test's, so an unguarded buffer read
	// here would be a data race under -race. See realtime_serverevents_gaps_test.go.
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	ch := make(chan CarrierAudio, 1) // tiny, never drained

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)

	for i := 0; i < 20; i++ {
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	}
	time.Sleep(500 * time.Millisecond)

	if !bytes.Contains(buf.Bytes(), []byte("dropped")) {
		t.Fatalf("a dropped carrier-audio record must be logged; log output: %q", buf.String())
	}
}

// --- behavior 7: unset is byte-identical to today --------------------------

// TestCarrierAudioChan_AbsentOptionIsTodaysBehaviour guards the DoD item
// "Unset is byte-identical to today." Supplying no WithCarrierAudioChan
// option must leave the call running exactly as it did before this option
// existed.
//
// slopstop:test regression — guards: "With no option supplied, behaviour is
// byte-identical to today."
func TestCarrierAudioChan_AbsentOptionIsTodaysBehaviour(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "no WithCarrierAudioChan option must leave the call exactly as it was")
}

// --- gap tests from adversary round 1 --------------------------------------

// TestCarrierAudioChan_ClearRecordMatchesTheCarrierWire closes round 1's
// [major] finding against TestCarrierAudioChan_ConsumerReceivesClearRecord:
// that test checks only that *something* with Clear==true arrived, never
// cross-checking it against the carrier, unlike its Media sibling three tests
// above. captureCarrierWire decodes EventClear precisely so this comparison
// can be made, and the Clear test never calls it.
//
// The two are not interchangeable. As written, the existing test passes just
// as well against an implementation that derives the Clear record from the raw
// speech_started server event — piggybacking on bridge.Events() — instead of
// from carrierMediaSink.Clear. Events() delivery drops once its 16-slot buffer
// fills (telephony/realtime/bridge.go, "once the 16-event buffer is full the
// publish drops"), while Bridge.Run calls sink.Clear regardless: "Dropping is
// one-sided... a dropped audio delta still reaches the MediaSink". An
// Events()-sourced implementation would therefore under-report Clears under
// load — recording what was intended rather than what shipped, which is the
// exact failure the ticket's test expectations exist to prevent.
//
// The existing Clear test is frozen by the Phase 0 commit and is deliberately
// NOT edited; this adds the missing cross-check beside it as a pure addition.
//
// slopstop:test contract
func TestCarrierAudioChan_ClearRecordMatchesTheCarrierWire(t *testing.T) {
	ch := make(chan CarrierAudio, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})

	waitFor(t, 5*time.Second, func() bool { return len(wire()) > 0 })
	got := wire()
	if !got[0].clear {
		t.Fatalf("test setup: the carrier must have received a clear first, got %+v", got[0])
	}

	select {
	case rec := <-ch:
		if rec.Clear != got[0].clear {
			t.Fatalf("the consumer's record must be the clear the carrier actually received: got %+v, carrier saw %+v",
				rec, got[0])
		}
		if rec.Payload != got[0].payload {
			t.Fatalf("a clear on the wire carries no payload; consumer got %q, carrier saw %q",
				rec.Payload, got[0].payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received a clear record")
	}
}

// TestCarrierAudioChan_RecordsArriveInOrder closes round 1's [blocker]
// finding: no test in this file drains more than one record from a single
// running call, so the DoD's own word in bullet 1 — "Every Media payload the
// engine sends to the carrier reaches the consumer's channel, IN ORDER,
// byte-identical to what shipped" — is pinned by nothing. No test produces
// both a Media and a Clear within one call either, so their relative order is
// unpinned too.
//
// An implementation delivering each record from its own goroutine — a
// plausible reading of "non-blocking" that avoids literal blocking without the
// synchronous select/default that deliver already uses — satisfies every other
// test in this file yet can scramble record order under scheduling
// non-determinism. Asserting the consumer's whole sequence against
// captureCarrierWire's recording of the same call is what detects that, and it
// pins the "byte-identical to what shipped" half at the same time: the
// comparison is against the carrier's own bytes, not against what this test
// intended to send.
//
// The assertion detects out-of-order delivery once it occurs; it does not
// force it to occur. Probed in adversary round 2: a per-record-goroutine
// implementation passed this 10/10 under the test's natural event cadence,
// which does not interleave the goroutines, and failed 5/5 once a 20ms delay
// was injected into the Media path. So this is a real guard against scrambling
// under production timing skew, not a proof that no such implementation can
// pass under a light cadence.
//
// slopstop:test contract
func TestCarrierAudioChan_RecordsArriveInOrder(t *testing.T) {
	ch := make(chan CarrierAudio, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithCarrierAudioChan(ch)))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	first, second := distinctCarrierPayload(0xA1), distinctCarrierPayload(0xB2)

	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": first})
	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": second})

	want := []carrierWireRecord{{payload: first}, {clear: true}, {payload: second}}

	waitFor(t, 5*time.Second, func() bool { return len(wire()) >= len(want) })
	got := wire()
	for i, w := range want {
		if !got[i].sameMessage(w) {
			t.Fatalf("test setup: the carrier must receive media/clear/media in that order; wire[%d] = %+v, want %+v",
				i, got[i], w)
		}
	}

	for i, w := range want {
		select {
		case rec := <-ch:
			if rec.Clear != w.clear || rec.Payload != w.payload {
				t.Fatalf("record %d: consumer received %+v, want {Payload:%q Clear:%v} — the carrier's own message %d",
					i, rec, w.payload, w.clear, i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("consumer received only %d of %d records; delivery must be ordered and complete",
				i, len(want))
		}
	}
}

// TestCarrierAudioChan_ClearsDoNotComeFromTheServerEventStream closes round 1's
// finding 1, which round 2 proved TestCarrierAudioChan_ClearRecordMatchesTheCarrierWire
// does NOT close. That test compares the consumer's clear record against the
// carrier's own wire record — and both a sink-sourced and an Events()-sourced
// implementation produce byte-identical values (Clear:true, Payload:"") for one
// unhurried speech_started, because the divergence it was reaching for (Events()'s
// 16-slot buffer dropping the event) never triggers when nothing is driving volume.
// Round 2 wired the Events()-sourced implementation and watched that test pass 5/5
// under -race. A value comparison cannot pin where a value came from.
//
// This can, and it does not depend on buffer pressure or on timing. bridge.Events()
// is ONE channel, and a value sent on a channel is received by exactly ONE receiver.
// HandleStreamRealtime already runs `deliver(bridge.Events(), cfg.serverEventChan(start), …)`
// unconditionally, so an implementation that reads Events() to source its clears must
// either steal from that drain — splitting the stream, so each consumer sees roughly
// half — or replace it, so the server-event consumer sees nothing at all. Both are
// visible here, and neither is a race: with N events the chance a splitting
// implementation lets both channels see all N is (1/2)^N.
//
// A correct implementation passes trivially. carrierMediaSink.Clear is called once
// per speech-started event by Run's dispatch, independently of publishEvent, so the
// two streams are fed from two places and neither can starve the other. That
// independence is the actual contract behind "the observed payload is asserted
// against the bytes the carrier actually received" — the record must come from the
// sink, not from a parallel view of the same events that drops under load.
//
// What this was measured to catch, and what it was measured NOT to catch — stated
// because the round-2 finding against the previous attempt was precisely an
// overclaiming comment. Three implementations were wired and run against it:
//
//   - sink-sourced (correct)                          -> PASSES
//   - a SECOND Events() consumer beside the existing
//     drain (the natural way to "read Events() for
//     clears", since deliver already exists)          -> FAILS 5/5, and the counts
//     show the split: 3/3, 3/3, 2/4, 5/1 of 6
//   - the observer REPLACING the drain                -> fails by construction; the
//     server-event channel receives nothing
//
// It does NOT catch a fourth shape: one merged consumer that REPLACES the
// server-event drain, forwards every event onward stealing nothing, and derives
// its records from bridge.Events(). Measured — it passes.
//
// That gap applies to MEDIA EXACTLY AS IT DOES TO CLEAR, and this comment named
// only clears until adversary round 3 measured the difference: generalized to
// derive Media from ev.Delta as well, that one implementation passes every test in
// this file, reproducibly, under -race. So the residual gap is the whole file's,
// not this test's alone. Recorded here because this is where a reader looks for it.
//
// It survives because it diverges from correct behaviour only when Events()'s
// 16-slot buffer overflows — publishEvent drops there while dispatch calls
// sink.Media and sink.Clear unconditionally — and nothing in this suite can reach
// that, since whatever drains Events() does so with a non-blocking send and never
// falls behind. Closing it needs a test that can force publishEvent to drop, which
// no test here can. What that shape does contradict is the ticket's own file map,
// which names "the observation points in carrierMediaSink": an implementation that
// rebuilds a drain instead of instrumenting the sink is a conformance failure
// against the ticket, which is a different gate's question and not this file's.
//
// slopstop:test non-interference — paired: the positive half is that all N clears
// reach the consumer's carrier-audio channel (an inert or unwired option fails here
// first, so the negative half cannot pass vacuously); the negative half is that
// installing WithCarrierAudioChan costs WithServerEventChan none of its events
func TestCarrierAudioChan_ClearsDoNotComeFromTheServerEventStream(t *testing.T) {
	const clears = 6

	audio := make(chan CarrierAudio, 64)
	events := make(chan ServerEvent, 64)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithCarrierAudioChan(audio),
		WithServerEventChan(events),
	))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	for i := 0; i < clears; i++ {
		be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})
	}

	// The carrier itself must have received every clear: the sink is driven once
	// per speech-started event whatever any observer does. If this fails, the
	// disagreement below would be about the engine, not about the observer.
	waitFor(t, 5*time.Second, func() bool {
		n := 0
		for _, r := range wire() {
			if r.clear {
				n++
			}
		}
		return n >= clears
	})

	gotAudio, gotEvents := 0, 0
	deadline := time.After(5 * time.Second)
	for gotAudio < clears || gotEvents < clears {
		select {
		case rec := <-audio:
			if rec.Clear {
				gotAudio++
			}
		case ev := <-events:
			if ev.Type == "input_audio_buffer.speech_started" {
				gotEvents++
			}
		case <-deadline:
			t.Fatalf("the carrier received %d clears; the consumer's carrier-audio channel got %d of %d "+
				"and the server-event channel got %d of %d. Both must see all %d: a clear record sourced "+
				"from bridge.Events() rather than from carrierMediaSink.Clear would take these events away "+
				"from the server-event drain, since each value on that channel reaches exactly one receiver",
				clears, gotAudio, clears, gotEvents, clears, clears)
		}
	}
}

// TestCarrierAudioChan_MediaDoesNotComeFromTheServerEventStream is the Media
// analog of TestCarrierAudioChan_ClearsDoNotComeFromTheServerEventStream, and it
// exists because adversary round 3 found the provenance question had been closed
// for Clear and left open for Media — the asymmetry, not a new mechanism.
//
// Media is the ticket's PRIMARY behaviour: "Every Media payload the engine sends to
// the carrier reaches the consumer's channel, in order, byte-identical to what
// shipped." An observer sourced from bridge.Events() rather than from
// carrierMediaSink.Media satisfies that in the quiet case and silently under-reports
// under load, because publishEvent drops when the 16-slot buffer fills while
// dispatch calls sink.Media regardless. That is the same defect three rounds went
// into closing for Clear, and until this test it was unguarded on the side the DoD
// names first.
//
// Same discriminator, same reason it is deterministic: bridge.Events() is one
// channel, a value on it reaches exactly one receiver, and HandleStreamRealtime
// already drains it unconditionally to feed the server-event option. An
// implementation that reads Events() for media must steal from that drain or
// replace it, and requiring BOTH channels to see all N deltas catches either.
//
// The residual shape this cannot catch is the one named at length on the Clear
// test above — a merged consumer that replaces the drain and steals nothing. It is
// the file's gap, not this test's, and it is recorded in one place rather than
// twice (universal §5).
//
// slopstop:test non-interference — paired: the positive half is that all N media
// payloads reach the consumer's carrier-audio channel, byte-identical to what the
// carrier received (an inert or unwired option fails here first, so the negative
// half cannot pass vacuously); the negative half is that installing
// WithCarrierAudioChan costs WithServerEventChan none of its events
func TestCarrierAudioChan_MediaDoesNotComeFromTheServerEventStream(t *testing.T) {
	const deltas = 6

	audio := make(chan CarrierAudio, 64)
	events := make(chan ServerEvent, 64)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithCarrierAudioChan(audio),
		WithServerEventChan(events),
	))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	// Distinct payloads, so a record cannot be matched to the wire by accident.
	sent := make([]string, deltas)
	for i := range sent {
		sent[i] = distinctCarrierPayload(byte(0xC0 + i))
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": sent[i]})
	}

	// The carrier itself must have received every payload: the sink is driven once
	// per delta whatever any observer does. If this fails, the disagreement below
	// would be about the engine, not about the observer.
	waitFor(t, 5*time.Second, func() bool {
		n := 0
		for _, r := range wire() {
			if !r.clear {
				n++
			}
		}
		return n >= deltas
	})

	onWire := map[string]bool{}
	for _, r := range wire() {
		if !r.clear {
			onWire[r.payload] = true
		}
	}

	gotAudio, gotEvents := 0, 0
	deadline := time.After(5 * time.Second)
	for gotAudio < deltas || gotEvents < deltas {
		select {
		case rec := <-audio:
			if rec.Clear {
				t.Fatalf("no clear was sent on this call; got %+v", rec)
			}
			// Byte-identical to what the CARRIER received, not to what this
			// test intended to send.
			if !onWire[rec.Payload] {
				t.Fatalf("consumer received a payload the carrier never got: %q", rec.Payload)
			}
			gotAudio++
		case ev := <-events:
			if ev.Type == "response.output_audio.delta" {
				gotEvents++
			}
		case <-deadline:
			t.Fatalf("the carrier received %d media payloads; the consumer's carrier-audio channel got %d of %d "+
				"and the server-event channel got %d of %d. Both must see all %d: a media record sourced from "+
				"bridge.Events() rather than from carrierMediaSink.Media would take these events away from the "+
				"server-event drain, since each value on that channel reaches exactly one receiver",
				deltas, gotAudio, deltas, gotEvents, deltas, deltas)
		}
	}
}
