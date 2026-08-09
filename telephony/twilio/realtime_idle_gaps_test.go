package twilio

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Adversary gap tests for the idle timeout (AATK-76, round 1).
//
// realtime_idle_test.go pins the two headline cases: a silent call ends, and a
// call fed backend audio deltas does not. Round 1 found that pair sufficient
// for neither half of the contract:
//
//   - Observable behavior 3 names FOUR activity sources — backend audio deltas,
//     speech-started, transcript events, and carrier media frames — and the
//     bridge routes them through genuinely different side channels
//     (MediaSink.Media, MediaSink.Clear, and bridge.Transcripts()). A timer
//     reset from only the audio-delta path passes both existing tests and still
//     kills a live call.
//   - Observable behavior 2 requires the carrier connection to be closed, which
//     nothing asserted for the idle path.
//   - Observable behavior 1 says 0 preserves today's behavior exactly, which
//     nothing pinned: an implementation reading 0 as "use a default" passed
//     every test that existed.
//
// These tests use a wider tick-to-timeout margin (10x) than the frozen pair,
// so the added coverage does not inherit their 4x scheduler-jitter headroom.

// idleGapTimeout is deliberately longer than the 100ms the frozen tests use:
// each test below emits at idleGapTimeout/10, so a delayed tick has ten times
// the room before it could be mistaken for silence.
const idleGapTimeout = 200 * time.Millisecond

// emitEvery writes ev to the backend's accepted connection every interval until
// the test ends. Additive harness surface: the frozen emitInterval knob only
// ever emits audio deltas, and three of the four activity sources the contract
// names are not audio deltas.
//
// Safe to call only once the handshake has completed — the backend handler
// writes session.created itself, and two concurrent writers on one connection
// are undefined. waitBackendReady below is how a test establishes that.
func (b *fakeRealtimeBackend) emitEvery(t *testing.T, interval time.Duration, ev map[string]string) {
	t.Helper()

	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		t.Fatal("backend has no connection to emit on — call waitBackendReady first")
	}

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// A write failure means the call has ended; stop rather than
				// spinning on a dead connection.
				if err := writeRealtimeJSON(context.Background(), c, ev); err != nil {
					return
				}
			}
		}
	}()
}

// waitBackendReady drives one carrier frame through to the backend and waits
// for it to arrive. Arrival proves the handshake finished and the backend is in
// its read loop, which is the precondition emitEvery needs. It is also the only
// carrier activity these tests produce, at t=0 — every assertion below covers a
// window many multiples of idleGapTimeout after it.
func waitBackendReady(t *testing.T, be *fakeRealtimeBackend, h *realtimeHarness) {
	t.Helper()
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 1, 5*time.Second)
}

// idleHarness wires HandleStreamRealtime directly with an explicit idleTimeout,
// which is the entry a consumer supplying its own bound would use.
func idleHarness(t *testing.T, url string, idleTimeout time.Duration) *realtimeHarness {
	t.Helper()
	return newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, url, idleTimeout)
	})
}

// assertStillRunning fails if the call has already ended, quoting the error so a
// premature idle-timer firing is distinguishable from any other ending.
func assertStillRunning(t *testing.T, h *realtimeHarness, why string) {
	t.Helper()
	select {
	case err := <-h.done:
		t.Fatalf("%s, got: %v", why, err)
	default:
	}
}

// TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnSpeechStartedOnly covers the
// EventSpeechStarted arm of observable behavior 3. Speech-started reaches the
// handler through MediaSink.Clear, a different call site from the audio deltas
// the frozen test drives, so an implementation instrumenting only Media passes
// there and fails here.
func TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnSpeechStartedOnly(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), idleGapTimeout)
	waitBackendReady(t, be, h)

	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type": "input_audio_buffer.speech_started",
	})

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "speech-started events faster than idleTimeout apart must keep the call alive")
}

// TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnTranscriptEventsOnly covers
// the transcript arm of observable behavior 3. Transcript events do not touch
// the media sink at all — they are published on bridge.Transcripts() — so this
// is the arm an implementation that hooks only the sink cannot see.
func TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnTranscriptEventsOnly(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), idleGapTimeout)
	waitBackendReady(t, be, h)

	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type":       "conversation.item.input_audio_transcription.delta",
		"transcript": "still talking",
	})

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "transcript events faster than idleTimeout apart must keep the call alive")
}

// TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnCarrierMediaOnly covers the
// carrier arm of observable behavior 3 — the file map's own "every frame
// pumpCarrierToBridge reads". The backend stays silent throughout, so this is
// the case where a caller is still talking to a backend that has gone quiet:
// the call must survive on the carrier's activity alone.
func TestHandleStreamRealtime_IdleTimeoutDoesNotFireOnCarrierMediaOnly(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), idleGapTimeout)

	emitCarrierEvery(t, h, idleGapTimeout/10, 0)

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "carrier media frames faster than idleTimeout apart must keep the call alive")
}

// emitCarrierEvery sends a carrier media frame every interval, starting after
// initialDelay, until the test ends.
//
// Written directly rather than through h.sendRaw: sendRaw fails the test on a
// write error, and the carrier socket closing at the end of the run is expected
// rather than a failure. It touches no *testing.T method, so it cannot panic by
// logging after the test returns; t.Cleanup runs LIFO, so this stop fires
// before the harness's own conn close, which was registered first.
func emitCarrierEvery(t *testing.T, h *realtimeHarness, interval, initialDelay time.Duration) {
	t.Helper()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	send := func() bool {
		err := h.conn.Write(context.Background(), websocket.MessageText,
			mediaFrameRaw(h.streamSID, carrierPayloadB64()))
		return err == nil
	}

	go func() {
		if initialDelay > 0 {
			select {
			case <-stop:
				return
			case <-time.After(initialDelay):
			}
			if !send() {
				return
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !send() {
					return
				}
			}
		}
	}()
}

// TestHandleStreamRealtime_IdleTimeoutClosesCarrierConnection pins the half of
// observable behavior 2 the frozen test does not check: the idle path must
// close the carrier socket, not merely return an error. Mirrors the assertion
// TestHandleStreamRealtime_DialFailureEndsCallAndClosesCarrier makes for the
// dial-failure path.
func TestHandleStreamRealtime_IdleTimeoutClosesCarrierConnection(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), idleGapTimeout)

	err := h.waitDone(idleGapTimeout + 500*time.Millisecond)
	if err == nil {
		t.Fatal("a silent backend and silent carrier must end the call with a non-nil error")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("error must name the idle timeout, got: %v", err)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := h.conn.Read(readCtx); err == nil {
		t.Fatal("handler must close the carrier connection on the idle-timeout path")
	}
}

// TestHandleStreamRealtime_ZeroIdleTimeoutNeverEndsSilentCall pins observable
// behavior 1 under the one condition where a timer would show: total silence.
// Passing 0 must arm nothing. An implementation treating 0 as "use a default"
// is invisible to every other test, because every other test either supplies a
// positive timeout or ends the call another way well before a default elapses.
func TestHandleStreamRealtime_ZeroIdleTimeoutNeverEndsSilentCall(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), 0)

	// Many multiples of the timeout every other test in this package considers
	// long enough to end a silent call.
	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "idleTimeout 0 must arm no timer, leaving a silent call running exactly as it does today")
}

// TestHandleStreamRealtime_IdleTimeoutDoesNotFireWithConcurrentBackendAndCarrierActivity
// covers the shape every other activity test misses: both sides talking at once.
//
// Round 2 raised it, and the reason is in the production code's structure. Backend
// activity is observed inside bridge.Run's goroutine and carrier activity inside
// pumpCarrierToBridge's — two goroutines that must both reset one shared timer.
// Every other test drives one side while the other is silent, so the reset path is
// never contended and an implementation racing on Reset/Stop passes all of them.
// A caller talking while the backend answers is the ordinary case, not an edge one.
//
// The intervals are chosen so neither source alone would keep the call alive —
// each fires slower than the timeout — while their interleaving always does. That
// makes the test fail if either arm's reset is dropped under contention, not only
// if both are.
func TestHandleStreamRealtime_IdleTimeoutDoesNotFireWithConcurrentBackendAndCarrierActivity(t *testing.T) {
	// Wider than idleGapTimeout: this test's discriminating power comes from the
	// interleave, which caps the usable margin at 25%, so the absolute slack has
	// to come from a longer timeout.
	const timeout = 400 * time.Millisecond
	const perSource = timeout * 3 / 2 // 600ms — each source alone is too slow
	const offset = perSource / 2      // 300ms — interleaved, the union is fast enough

	be := newFakeRealtimeBackend(t)
	h := idleHarness(t, be.url(), timeout)
	waitBackendReady(t, be, h)

	be.emitEvery(t, perSource, map[string]string{
		"type":  "response.output_audio.delta",
		"delta": "AAAA",
	})
	emitCarrierEvery(t, h, perSource, offset)

	time.Sleep(timeout * 5)

	assertStillRunning(t, h, "interleaved backend and carrier activity must keep the call alive even though neither source alone would")
}

// TestHandleStreamRealtime_StopFrameEndsCallWhileIdleTimeoutArmed pins that an
// armed idle timer does not disturb the ordinary hangup path: the call still
// ends cleanly on the stop frame, with a nil error, and not via the timer.
func TestHandleStreamRealtime_StopFrameEndsCallWhileIdleTimeoutArmed(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	// Far longer than this test's own runtime, so any ending it observes is the
	// stop frame's and never the timer's.
	h := idleHarness(t, be.url(), 30*time.Second)
	waitBackendReady(t, be, h)

	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))

	if err := h.waitDone(5 * time.Second); err != nil {
		t.Fatalf("a stop frame must end the call cleanly even with an idle timeout armed, got error: %v", err)
	}
}
