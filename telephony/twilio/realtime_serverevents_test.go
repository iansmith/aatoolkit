package twilio

import (
	"bytes"
	"encoding/json"
	"log"
	"runtime"
	"testing"
	"time"
)

// Server-event-channel tests for the realtime path (AATK-81, "Surface
// unmodelled backend events to the consumer").
//
// Bridge.Run's switch only routes four event types anywhere a consumer can
// see (Media, Clear, Transcripts); everything else — EventSpeechStopped is
// the ticket's own example — fell into default: and vanished. WithServerEventChan
// / WithServerEventChanFor mirror WithTranscriptChan / WithTranscriptChanFor
// exactly, for the same two reasons those exist:
//
//   - a consumer handed a raw channel could stall the read loop that also
//     drives carrier audio, so delivery is non-blocking with drops logged;
//   - the engine must never run consumer code on a goroutine it owns.

// serverEventItemID pushes one unmodelled server event carrying a
// caller-supplied tag in a field ServerEvent does not model, so a test can
// tell which emitted event actually reached which consumer channel.
func serverEventItemID(t *testing.T, be *fakeRealtimeBackend, itemID string) {
	t.Helper()
	be.emitOnce(t, map[string]string{
		"type":    "input_audio_buffer.speech_stopped",
		"item_id": itemID,
	})
}

// assertServerEventItemID waits for ch to deliver an event carrying itemID.
func assertServerEventItemID(t *testing.T, ch <-chan ServerEvent, want string) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Type != "input_audio_buffer.speech_stopped" {
			t.Fatalf("got type %q, want %q", ev.Type, "input_audio_buffer.speech_stopped")
		}
		var raw map[string]string
		if err := json.Unmarshal(ev.Raw, &raw); err != nil {
			t.Fatalf("Raw did not decode as the original frame: %v", err)
		}
		if raw["item_id"] != want {
			t.Fatalf("got item_id %q, want %q", raw["item_id"], want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("channel never received the expected event (item_id=%q)", want)
	}
}

// TestServerEventChan_UnmodelledEventReachesConsumerWithRaw is the headline:
// an event type Run's switch does not model — EventSpeechStopped, the
// ticket's own example — reaches the consumer's channel with Raw carrying
// the whole original frame, including a field ServerEvent does not know
// about.
//
// slopstop:test contract
func TestServerEventChan_UnmodelledEventReachesConsumerWithRaw(t *testing.T) {
	ch := make(chan ServerEvent, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithServerEventChan(ch)))
	waitBackendReady(t, be, h)

	serverEventItemID(t, be, "only-call")
	assertServerEventItemID(t, ch, "only-call")
}

// TestServerEventChan_ConsumerThatNeverReadsDoesNotStallAudio mirrors
// TestTranscriptChan_ConsumerThatNeverReadsDoesNotStallAudio for the new
// channel. It first proves the channel is genuinely wired — an inert option
// would let the audio check below pass for the wrong reason, since audio
// delivery is untouched by this ticket regardless of whether the new channel
// works — then floods it past its buffer without draining further and checks
// audio queued behind the flood still arrives.
//
// slopstop:test non-interference — paired: asserts the consumer's channel actually receives the flooded unmodelled events (proving delivery is live) before checking that a full/never-drained channel does not stall carrier audio
func TestServerEventChan_ConsumerThatNeverReadsDoesNotStallAudio(t *testing.T) {
	ch := make(chan ServerEvent, 1) // deliberately tiny, and never drained further

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithServerEventChan(ch)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < 50; i++ {
		be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
	}

	// Positive: the channel must actually be wired to receive traffic.
	select {
	case ev := <-ch:
		if ev.Type != "input_audio_buffer.speech_stopped" {
			t.Fatalf("consumer received %+v, want type input_audio_buffer.speech_stopped", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received any server event; channel is not wired")
	}

	// Audio queued behind the flood must still reach the carrier promptly.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
}

// TestServerEventChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer mirrors
// TestTranscriptChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer. Bridge's
// idle-timer activity signal already covers unmodelled events independently
// of this channel (see TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnUnmodelledBackendEventOnly),
// so the "does not trip" half alone would hold even against an inert option —
// it is the positive receive that actually exercises this ticket's wiring.
//
// slopstop:test non-interference — paired: asserts the consumer's channel actually receives an unmodelled event before checking a never-drained channel does not trip the idle timer
func TestServerEventChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer(t *testing.T) {
	ch := make(chan ServerEvent, 1) // never drained

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithServerEventChan(ch),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)

	serverEventItemID(t, be, "wiring-check")
	assertServerEventItemID(t, ch, "wiring-check")

	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type": "input_audio_buffer.speech_stopped",
	})

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "a consumer that never reads a server event must not let the idle timer fire on a live backend")
}

// TestServerEventChan_EngineNeverClosesTheConsumersChannel pins the DoD item
// verbatim: "The engine never closes the consumer's channel — a test reuses
// one channel across two calls and reads after both." Mirrors
// TestTranscriptChan_EngineNeverClosesTheConsumersChannel.
//
// slopstop:test contract
func TestServerEventChan_EngineNeverClosesTheConsumersChannel(t *testing.T) {
	ch := make(chan ServerEvent, testChanBuffer)

	for _, itemID := range []string{"first-call", "second-call"} {
		be := newFakeRealtimeBackend(t)
		h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithServerEventChan(ch)))
		waitBackendReady(t, be, h)

		serverEventItemID(t, be, itemID)
		assertServerEventItemID(t, ch, itemID)

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)
	}

	// A closed channel would yield a zero value immediately rather than block.
	select {
	case ev, ok := <-ch:
		t.Fatalf("engine must not close the consumer's channel; received %+v (open=%v)", ev, ok)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestServerEventChan_NoGoroutineOutlivesCallAndEventsActuallyFlow copies the
// differential structure of TestTranscriptChan_NoEngineGoroutineOutlivesTheCall
// (realtime_transcripts_test.go): each call leaves fixed overhead behind, so a
// leak is measured as growth relative to a run with no consumer, not as an
// absolute count.
//
// It also drains one event per call in the "with consumer" run before letting
// the rest flood unread — otherwise an inert WithServerEventChan option would
// spawn nothing on either side of the comparison and pass this test having
// wired up nothing at all.
//
// slopstop:test non-interference — paired: asserts the consumer channel actually receives an event on each call before measuring goroutine growth against a run with no consumer
func TestServerEventChan_NoGoroutineOutlivesCallAndEventsActuallyFlow(t *testing.T) {
	const calls = 5

	run := func(withConsumer bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			var ch chan ServerEvent
			if withConsumer {
				// Buffer 1 and never drained beyond the one positive check
				// below: the consumer is present and permanently behind,
				// which is the worst case.
				ch = make(chan ServerEvent, 1)
				opts = append(opts, WithServerEventChan(ch))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			for j := 0; j < 20; j++ {
				be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
			}
			if withConsumer {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("consumer never received a server event; channel is not wired")
				}
			}
			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			_ = h.waitDone(5 * time.Second)
		}

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

// TestServerEventChan_DropIsLogged pins the DoD item "Drops are logged rather
// than silent." Mirrors the log-capture pattern used elsewhere in this
// package (e.g. telephony/twilio/demux_test.go).
//
// slopstop:test contract
func TestServerEventChan_DropIsLogged(t *testing.T) {
	// syncBuffer, not bytes.Buffer: drops are logged from the delivery
	// goroutine, so an unguarded buffer read here is a data race under -race
	// against any compliant implementation. See realtime_serverevents_gaps_test.go.
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	ch := make(chan ServerEvent, 1) // tiny, never drained

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithServerEventChan(ch)))
	waitBackendReady(t, be, h)

	for i := 0; i < 20; i++ {
		be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
	}
	time.Sleep(500 * time.Millisecond)

	if !bytes.Contains(buf.Bytes(), []byte("dropped")) {
		t.Fatalf("a dropped server event must be logged; log output: %q", buf.String())
	}
}

// TestServerEventChan_AbsentOptionIsTodaysBehaviour guards the DoD's other
// no-regression item: "With no option supplied, behaviour is byte-identical
// to today." Supplying no ServerEventChan option must leave the call running
// exactly as it did before this channel existed, unmodelled events included.
//
// slopstop:test regression — guards: "With no option supplied, behaviour is byte-identical to today."
func TestServerEventChan_AbsentOptionIsTodaysBehaviour(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < 20; i++ {
		be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
	}
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "no ServerEventChan option must leave the call exactly as it was")
}

// TestServerEventChanFor_VariesPerCallThroughOneHandler mirrors
// TestSessionUpdate_InstructionsVaryPerCallThroughOneHandler: two calls
// through the SAME handler must route to two different channels, which is
// the assertion WithServerEventChan's constant channel cannot satisfy.
//
// slopstop:test contract
func TestServerEventChanFor_VariesPerCallThroughOneHandler(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	ch1 := make(chan ServerEvent, testChanBuffer)
	ch2 := make(chan ServerEvent, testChanBuffer)

	calls := 0
	handler := NewStreamHandler(be.url(), WithServerEventChanFor(func(start Frame) chan<- ServerEvent {
		calls++
		if calls == 1 {
			return ch1
		}
		return ch2
	}))

	h1 := newRealtimeHarnessWith(t, handler)
	h1.sendRaw(mediaFrameRaw(h1.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 1, 5*time.Second)
	serverEventItemID(t, be, "call-one")
	assertServerEventItemID(t, ch1, "call-one")
	h1.sendRaw([]byte(`{"event":"stop","streamSid":"` + h1.streamSID + `"}`))
	_ = h1.waitDone(5 * time.Second)

	h2 := newRealtimeHarnessWith(t, handler)
	h2.sendRaw(mediaFrameRaw(h2.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 2, 5*time.Second)
	serverEventItemID(t, be, "call-two")
	assertServerEventItemID(t, ch2, "call-two")

	// The first call's channel must not also receive the second call's event.
	select {
	case ev := <-ch1:
		t.Fatalf("first call's channel received a second call's event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}
