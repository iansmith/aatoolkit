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
// connection — either a media payload or a clear — so a test can compare
// what the consumer's channel delivered against what the carrier actually
// received, rather than against what the test merely intended to send.
type carrierWireRecord struct {
	payload string
	clear   bool
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
			switch f.Event {
			case EventMedia:
				mu.Lock()
				records = append(records, carrierWireRecord{payload: f.EncodedPayload})
				mu.Unlock()
			case EventClear:
				mu.Lock()
				records = append(records, carrierWireRecord{clear: true})
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
