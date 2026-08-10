package twilio

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// Transcript-channel tests for the realtime path (AATK-79, succeeding AATK-75).
//
// Bridge publishes transcripts and HandleStreamRealtime used to throw them away,
// so a consumer had no way to observe what was said. Two constraints shape the
// seam, and every test here defends one of them:
//
//   - Bridge.publish parks Run's read loop when nobody reads, and that loop also
//     drives audio to the carrier. A consumer handed a raw channel could break
//     audio silently. Hence non-blocking delivery with drops.
//   - The engine must never run consumer code on a goroutine it owns, because Go
//     cannot make a goroutine return from a call it is executing. Hence a channel
//     the consumer reads, not a callback the engine invokes.

// testChanBuffer is the buffer the tests give their own channels. The engine has
// no buffer of its own any more — the consumer owns the channel and its depth.
const testChanBuffer = 8

// emitTranscript pushes one transcription event from the backend.
func emitTranscript(t *testing.T, be *fakeRealtimeBackend, text string, final bool) {
	t.Helper()
	typ := "conversation.item.input_audio_transcription.delta"
	if final {
		typ = "conversation.item.input_audio_transcription.completed"
	}
	be.emitOnce(t, map[string]string{"type": typ, "transcript": text})
}

// TestTranscriptChan_ConsumerReceivesPartialAndFinal is the headline: a consumer
// can observe what was said, partial and final, in order.
func TestTranscriptChan_ConsumerReceivesPartialAndFinal(t *testing.T) {
	ch := make(chan Transcript, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptChan(ch)))
	waitBackendReady(t, be, h)

	emitTranscript(t, be, "hello", false)
	emitTranscript(t, be, "hello there", true)

	var got []string
	for len(got) < 2 {
		select {
		case tr := <-ch:
			got = append(got, tr.Text)
		case <-time.After(5 * time.Second):
			t.Fatalf("consumer received %d transcript(s), want 2", len(got))
		}
	}

	if strings.Join(got, "|") != "hello|hello there" {
		t.Fatalf("consumer must receive partial and final in order, got %q", got)
	}
}

// TestTranscriptChan_ConsumerThatNeverReadsDoesNotStallAudio is the constraint
// AATK-75 was written around. Both transcripts and audio are driven by the same
// backend read loop, so a consumer that stops reading must not delay audio.
func TestTranscriptChan_ConsumerThatNeverReadsDoesNotStallAudio(t *testing.T) {
	ch := make(chan Transcript, 1) // deliberately tiny, and never drained

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptChan(ch)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < 50; i++ {
		emitTranscript(t, be, "chatter", false)
	}
	// Audio queued behind all of it must still reach the carrier promptly.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
}

// TestTranscriptChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer covers the
// interaction AATK-76's idle bound introduced. A consumer that stalled the read
// loop would starve the activity signal and end the call reporting "idle
// timeout" — blaming the backend for a consumer's fault. This is the
// configuration a real consumer runs.
func TestTranscriptChan_ConsumerThatNeverReadsDoesNotTripTheIdleTimer(t *testing.T) {
	ch := make(chan Transcript, 1) // never drained

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithTranscriptChan(ch),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)

	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type":       "conversation.item.input_audio_transcription.delta",
		"transcript": "still talking",
	})

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "a consumer that never reads must not let the idle timer fire on a live backend")
}

// TestTranscriptChan_EngineNeverClosesTheConsumersChannel pins ownership. The
// engine did not create the channel, so closing it would hand the consumer a
// zero value indistinguishable from a real transcript — and would panic the
// second call to reuse it.
func TestTranscriptChan_EngineNeverClosesTheConsumersChannel(t *testing.T) {
	ch := make(chan Transcript, testChanBuffer)

	for i, text := range []string{"first call", "second call"} {
		be := newFakeRealtimeBackend(t)
		h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptChan(ch)))
		waitBackendReady(t, be, h)

		emitTranscript(t, be, text, true)

		select {
		case tr := <-ch:
			if tr.Text != text {
				t.Fatalf("call %d: got %q, want %q", i+1, tr.Text, text)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call %d: no transcript arrived", i+1)
		}

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)
	}

	// A closed channel would yield a zero value immediately rather than block.
	select {
	case tr, ok := <-ch:
		t.Fatalf("engine must not close the consumer's channel; received %+v (open=%v)", tr, ok)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestTranscriptChan_NoEngineGoroutineOutlivesTheCall is the item AATK-75 could
// not meet and the reason AATK-79 exists.
//
// Measured differentially rather than absolutely: each call leaves fixed
// overhead behind (the httptest server and carrier conn live until t.Cleanup),
// so an absolute count proves nothing. A leak scales with the number of calls
// and fixed overhead does not, so the same run with a consumer that never reads
// must cost no more than the same run with no consumer at all.
func TestTranscriptChan_NoEngineGoroutineOutlivesTheCall(t *testing.T) {
	const calls = 5

	run := func(withConsumer bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			opts := []RealtimeOption{}
			if withConsumer {
				// Buffer 1 and never drained: the consumer is present and
				// permanently behind, which is the worst case.
				opts = append(opts, WithTranscriptChan(make(chan Transcript, 1)))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			for j := 0; j < 20; j++ {
				emitTranscript(t, be, "chatter", false)
			}
			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			_ = h.waitDone(5 * time.Second)
		}

		// Poll rather than trust one instantaneous reading: a goroutine that is
		// exiting correctly may not have been descheduled yet.
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

	// Zero growth is the contract; the tolerance covers only the fixed
	// per-harness overhead both runs share, which cancels in the comparison.
	if withConsumer > withNone {
		t.Fatalf("a consumer that never reads must cost no goroutines: %d over %d calls, against %d with no consumer",
			withConsumer, calls, withNone)
	}
}

// TestTranscriptChan_AbsentConsumerIsTodaysDrain pins the no-regression half.
func TestTranscriptChan_AbsentConsumerIsTodaysDrain(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < 50; i++ {
		emitTranscript(t, be, "ignored", false)
	}
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "an absent consumer must leave the call exactly as it was")
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition never held within the deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
