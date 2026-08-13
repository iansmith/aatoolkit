package twilio

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Client-event-channel tests for the realtime path (AATK-82, "Let a consumer
// send client events to the realtime backend mid-call").
//
// Before this ticket the only thing a consumer could ever put on the wire to
// the backend was carrier audio. WithClientEventChan / WithClientEventChanFor
// open a second path — response.create, conversation.item.create, a mid-call
// session.update, function-call output, or anything else the backend's
// protocol accepts — without this package modelling any of it.
//
// The arrows reverse relative to WithTranscriptChan and WithServerEventChan:
// there the engine writes and the consumer reads; here the consumer writes
// and the engine reads. AATK-79's rule still applies in mirror image: the
// engine must never run consumer code on a goroutine it owns, and a new
// reader goroutine blocked on a channel the consumer never closes would be
// the same leak reversed. Hence the read lives in HandleStreamRealtime's
// existing select loop rather than a goroutine of its own.

// clientEventRaw builds a consumer-supplied client event, deliberately
// containing characters json.Marshal rewrites when it re-encodes a
// json.RawMessage (<, >, &) plus insignificant whitespace, so a test can tell
// verbatim wire delivery from a re-marshalled approximation. id disambiguates
// events across calls and across concurrent sends.
func clientEventRaw(id int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"conversation.item.create","id":%d,  "tag":"<a>&b</a>"}`, id))
}

// receivedEvents returns every client event the backend received, in arrival
// order, as the raw bytes that arrived. appendedAudio only surfaces
// input_audio_buffer.append events; these tests need the wider view to see
// consumer-originated events too.
func (b *fakeRealtimeBackend) receivedEvents() []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]json.RawMessage(nil), b.received...)
}

// waitForClientEvent waits for the backend to receive the client event with
// the given id, verbatim, failing the test if it never arrives.
func waitForClientEvent(t *testing.T, be *fakeRealtimeBackend, id int, d time.Duration) {
	t.Helper()
	want := string(clientEventRaw(id))
	deadline := time.After(d)
	for {
		for _, raw := range be.receivedEvents() {
			if string(raw) == want {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("backend never received client event id=%d verbatim", id)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- headline: byte-for-byte delivery ---------------------------------

// TestClientEventChan_ConsumerEventReachesBackendByteForByte is the ticket's
// own RED-first case at the twilio wiring layer: a consumer-supplied event
// put on the channel must reach the backend exactly as supplied.
//
// slopstop:test contract
func TestClientEventChan_ConsumerEventReachesBackendByteForByte(t *testing.T) {
	ch := make(chan json.RawMessage, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
	waitBackendReady(t, be, h)

	ch <- clientEventRaw(0)
	waitForClientEvent(t, be, 0, 5*time.Second)
}

// --- WithClientEventChanFor varies per call -----------------------------

// TestClientEventChanFor_VariesPerCallThroughOneHandler mirrors
// TestServerEventChanFor_VariesPerCallThroughOneHandler: two calls through
// the SAME handler must route to two different channels, which is the
// assertion WithClientEventChan's constant channel cannot satisfy.
//
// slopstop:test contract
func TestClientEventChanFor_VariesPerCallThroughOneHandler(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	calls := 0
	handler := NewStreamHandler(be.url(), WithClientEventChanFor(func(start Frame) <-chan json.RawMessage {
		calls++
		if calls == 1 {
			ch1 := make(chan json.RawMessage, testChanBuffer)
			ch1 <- clientEventRaw(1)
			return ch1
		}
		ch2 := make(chan json.RawMessage, testChanBuffer)
		ch2 <- clientEventRaw(2)
		return ch2
	}))

	h1 := newRealtimeHarnessWith(t, handler)
	h1.sendRaw(mediaFrameRaw(h1.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 1, 5*time.Second)
	waitForClientEvent(t, be, 1, 5*time.Second)
	h1.sendRaw([]byte(`{"event":"stop","streamSid":"` + h1.streamSID + `"}`))
	_ = h1.waitDone(5 * time.Second)

	h2 := newRealtimeHarnessWith(t, handler)
	h2.sendRaw(mediaFrameRaw(h2.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 2, 5*time.Second)
	waitForClientEvent(t, be, 2, 5*time.Second)
}

// --- the engine never closes the consumer's channel ----------------------

// TestClientEventChan_EngineNeverClosesTheConsumersChannel mirrors
// TestServerEventChan_EngineNeverClosesTheConsumersChannel and
// TestTranscriptChan_EngineNeverClosesTheConsumersChannel: a channel reused
// across two calls must still be open, and receiving from it, after both.
// The positive delivery check on each call is what actually exercises this
// ticket's wiring — a negative-only "still open" check would pass against an
// inert stub that never touches the channel at all.
//
// slopstop:test contract
func TestClientEventChan_EngineNeverClosesTheConsumersChannel(t *testing.T) {
	ch := make(chan json.RawMessage, testChanBuffer)

	for _, id := range []int{10, 11} {
		be := newFakeRealtimeBackend(t)
		h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
		waitBackendReady(t, be, h)

		ch <- clientEventRaw(id)
		waitForClientEvent(t, be, id, 5*time.Second)

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)
	}

	// A closed channel would let this send panic; it must not.
	select {
	case ch <- clientEventRaw(999):
	case <-time.After(200 * time.Millisecond):
		t.Fatal("engine must not close the consumer's channel; could not send to it after two calls")
	}
}

// --- idle timer ------------------------------------------------------------

// TestClientEventChan_ConsumerEventDoesNotResetIdleTimer pins the DoD item
// the ticket calls a discriminating case, not a formality: "A consumer event
// does not reset the idle timer: a call whose backend is silent but whose
// consumer is sending steadily still ends with idle timeout." Without this,
// a chatty consumer would mask a hung backend — the same misattribution
// AATK-76 and AATK-79 both fought.
//
// The positive proof (one event actually reaches the backend) comes first:
// without it, "the idle timer still fires" would hold trivially against an
// inert stub that never reads the channel at all, which proves nothing about
// this ticket's wiring.
//
// slopstop:test non-interference — paired: asserts one consumer event actually reaches the backend before checking that steady consumer traffic against a silent backend still ends the call with "idle timeout"
func TestClientEventChan_ConsumerEventDoesNotResetIdleTimer(t *testing.T) {
	ch := make(chan json.RawMessage, 1)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithClientEventChan(ch),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)

	// Positive: the channel is genuinely wired.
	ch <- clientEventRaw(20)
	waitForClientEvent(t, be, 20, 5*time.Second)

	// Negative: keep the consumer sending steadily while the backend stays
	// silent. The call must still end on the idle timer.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(idleGapTimeout / 10)
		defer ticker.Stop()
		id := 21
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case ch <- clientEventRaw(id):
				default:
				}
				id++
			}
		}
	}()

	err := h.waitDone(idleGapTimeout + 2*time.Second)
	if err == nil {
		t.Fatal("a silent backend must still end the call even while the consumer sends steadily")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("error must name the idle timeout, got: %v", err)
	}
}

// --- closed consumer channel -----------------------------------------------

// TestClientEventChan_ClosedChannelDoesNotCrashOrBusyLoop covers the
// investigation's flagged risk: the DoD says the engine never REQUIRES the
// channel to be closed, but does not forbid a consumer closing it. A closed
// channel yields a zero value with ok==false immediately and repeatedly, so
// an unguarded read in the select loop would busy-loop rather than block.
//
// The positive delivery check proves the channel was genuinely wired before
// it closes; the call remaining healthy and still delivering audio
// afterward is the property under test.
//
// slopstop:test non-interference — paired: asserts the consumer's one event reaches the backend before closing the channel and checking the call keeps working normally afterward
func TestClientEventChan_ClosedChannelDoesNotCrashOrBusyLoop(t *testing.T) {
	ch := make(chan json.RawMessage, 1)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	ch <- clientEventRaw(30)
	waitForClientEvent(t, be, 30, 5*time.Second)

	close(ch)

	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "a closed consumer channel must not crash or stall the call")
}

// --- no goroutine leak -------------------------------------------------

// TestClientEventChan_NoEngineGoroutineOutlivesTheCall mirrors
// TestTranscriptChan_NoEngineGoroutineOutlivesTheCall and
// TestServerEventChan_NoGoroutineOutlivesCallAndEventsActuallyFlow: measured
// differentially, since each call leaves fixed overhead behind. The ticket's
// own DoD item: "No engine goroutine outlives a call whose consumer never
// sends and never closes its channel."
//
// One event is drained per call in the "with consumer" run before the
// channel goes quiet for the rest of that call, proving the channel is
// genuinely read — the differential alone cannot distinguish a correct
// implementation from an inert stub, since a channel with nothing on it
// leaks nothing either way regardless of whether anything reads it.
//
// slopstop:test non-interference — paired: asserts the consumer channel actually gets read (its one event reaches the backend) on each call before measuring goroutine growth against a run with no option
func TestClientEventChan_NoEngineGoroutineOutlivesTheCall(t *testing.T) {
	const calls = 5

	run := func(withConsumer bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			var ch chan json.RawMessage
			if withConsumer {
				ch = make(chan json.RawMessage, 1)
				opts = append(opts, WithClientEventChan(ch))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			if withConsumer {
				id := 1000 + i
				select {
				case ch <- clientEventRaw(id):
				default:
				}
				waitForClientEvent(t, be, id, 5*time.Second)
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
		t.Fatalf("a consumer that never sends after its one proof event, and never closes its channel, must cost no goroutines: %d over %d calls, against %d with no consumer",
			withConsumer, calls, withNone)
	}
}

// --- send failure is logged, not fatal --------------------------------

// TestClientEventChan_SendFailureIsLoggedNotFatal pins the DoD item "A send
// failing does not end the call silently — it is logged, and the call ends
// only for the reasons it already ends for." The backend is dropped mid-call
// (the same fixture TestRealtime_BackendDropMidCallEndsCallWithError uses)
// while the consumer keeps offering events, so at least one Send is attempted
// against the now-dead connection; the call must still end with a non-nil
// error (the pre-existing backend-gone path), and the failed send must be
// logged rather than silently swallowed.
//
// slopstop:test contract
func TestClientEventChan_SendFailureIsLoggedNotFatal(t *testing.T) {
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	ch := make(chan json.RawMessage, 1)
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
	waitBackendReady(t, be, h)

	be.closeNow() // the backend goes away mid-call

	// Keep offering consumer events for a while so a Send is attempted
	// against the now-dead connection before the read-side failure ends the
	// call.
	deadline := time.Now().Add(2 * time.Second)
	id := 40
	for time.Now().Before(deadline) {
		select {
		case ch <- clientEventRaw(id):
			id++
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := h.waitDone(5 * time.Second); err == nil {
		t.Fatal("a dropped backend must still end the call with a non-nil error")
	}

	if !strings.Contains(strings.ToLower(buf.String()), "send") {
		t.Fatalf("a failed consumer send must be logged rather than swallowed; log output: %q", buf.String())
	}
}

// --- no regression -----------------------------------------------------

// TestClientEventChan_NeverWrittenChannelDoesNotBlockTheCall guards the DoD
// item verbatim: "The engine never blocks waiting for the consumer to send,
// survives the channel never being written to, and never requires it to be
// closed." An unbuffered channel that the consumer never writes to, and
// never closes, for the whole call, must still let the call proceed and end
// cleanly on a stop frame — exactly as if no option had been supplied at
// all. This must hold both before and after the wiring exists: an inert
// select case blocks on nothing it does not already block on, precisely
// because select never blocks on one case when another is ready.
//
// slopstop:test regression — guards: "The engine never blocks waiting for the consumer to send, survives the channel never being written to, and never requires it to be closed."
func TestClientEventChan_NeverWrittenChannelDoesNotBlockTheCall(t *testing.T) {
	ch := make(chan json.RawMessage) // unbuffered, never written, never closed

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })

	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
	if err := h.waitDone(5 * time.Second); err != nil {
		t.Fatalf("a call whose consumer never writes or closes its event channel must still end cleanly on stop, got: %v", err)
	}
}

// TestClientEventChan_AbsentOptionIsTodaysBehaviour guards observable
// behavior 5 verbatim: "With no option supplied, behaviour is byte-identical
// to today." Supplying no WithClientEventChan option must leave the call
// running exactly as it did before this option existed.
//
// slopstop:test regression — guards: "With no option supplied, behaviour is byte-identical to today."
func TestClientEventChan_AbsentOptionIsTodaysBehaviour(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "no WithClientEventChan option must leave the call exactly as it was")
}
