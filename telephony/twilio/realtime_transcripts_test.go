package twilio

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Transcript-sink tests for the realtime path (AATK-75).
//
// Bridge publishes transcripts and HandleStreamRealtime used to throw them
// away, so a consumer had no way to observe what was said. The drain it threw
// them away with was not incidental: Bridge.publish parks Run's read loop when
// nobody reads, and that loop also drives audio to the carrier, so a consumer
// handed a raw channel could break audio silently. Every test here exists to
// keep that protection while opening the seam.

// collector is a sink that records what it receives, safely.
type collector struct {
	mu   sync.Mutex
	got  []Transcript
	hold time.Duration // block this long inside the sink, simulating a slow consumer
}

func (c *collector) sink(tr Transcript) {
	if c.hold > 0 {
		time.Sleep(c.hold)
	}
	c.mu.Lock()
	c.got = append(c.got, tr)
	c.mu.Unlock()
}

func (c *collector) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.got))
	for i, tr := range c.got {
		out[i] = tr.Text
	}
	return out
}

// emitTranscript pushes one transcription event from the backend.
func emitTranscript(t *testing.T, be *fakeRealtimeBackend, text string, final bool) {
	t.Helper()
	typ := "conversation.item.input_audio_transcription.delta"
	if final {
		typ = "conversation.item.input_audio_transcription.completed"
	}
	be.emitOnce(t, map[string]string{"type": typ, "transcript": text})
}

// TestTranscriptSink_ConsumerReceivesPartialAndFinal is the headline: a consumer
// can observe what was said. Red before the seam existed, because there was no
// way to receive anything.
func TestTranscriptSink_ConsumerReceivesPartialAndFinal(t *testing.T) {
	var c collector

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptSink(c.sink)))
	waitBackendReady(t, be, h)

	emitTranscript(t, be, "hello", false)
	emitTranscript(t, be, "hello there", true)

	waitFor(t, 5*time.Second, func() bool { return len(c.texts()) >= 2 })

	got := strings.Join(c.texts(), "|")
	if got != "hello|hello there" {
		t.Fatalf("consumer must receive partial and final in order, got %q", got)
	}
}

// TestTranscriptSink_SlowConsumerDoesNotStallAudio is the important one, and the
// one most likely to be skipped. A sink that is far slower than the transcript
// stream must not delay the carrier's audio, because both are driven by the same
// read loop.
func TestTranscriptSink_SlowConsumerDoesNotStallAudio(t *testing.T) {
	c := collector{hold: 200 * time.Millisecond}

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptSink(c.sink)))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	// Far more transcripts than the sink can absorb in the window below.
	for i := 0; i < transcriptSinkBuffer*3; i++ {
		emitTranscript(t, be, "chatter", false)
	}
	// Audio queued behind all of it must still reach the carrier promptly.
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
}

// TestTranscriptSink_SlowConsumerDoesNotTripTheIdleTimer covers the interaction
// AATK-76's idle bound introduced. A sink that stalls the read loop would starve
// the activity signal and end the call reporting "idle timeout" — blaming the
// backend for a consumer's fault. The bounded, lossy seam is what prevents it,
// and this is the configuration a real consumer runs.
func TestTranscriptSink_SlowConsumerDoesNotTripTheIdleTimer(t *testing.T) {
	c := collector{hold: 100 * time.Millisecond}

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithTranscriptSink(c.sink),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)

	// Backend stays active throughout, so nothing should end this call — but a
	// sink that stalled the read loop would starve the activity signal.
	be.emitEvery(t, idleGapTimeout/10, map[string]string{
		"type":       "conversation.item.input_audio_transcription.delta",
		"transcript": "still talking",
	})

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "a slow transcript sink must not let the idle timer fire on a live backend")
}

// TestTranscriptSink_PanickingConsumerDoesNotEndTheCall pins the stated decision
// at the seam: the consumer's bug is contained.
func TestTranscriptSink_PanickingConsumerDoesNotEndTheCall(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTranscriptSink(func(Transcript) {
		panic("consumer exploded")
	})))
	waitBackendReady(t, be, h)

	emitTranscript(t, be, "boom", true)
	time.Sleep(200 * time.Millisecond)

	assertStillRunning(t, h, "a panicking transcript sink must not take the call down")
}

// TestTranscriptSink_AbsentSinkIsTodaysDrain pins the no-regression half: a
// consumer that asks for nothing gets exactly the previous behaviour, including
// that transcripts never wedge the read loop.
func TestTranscriptSink_AbsentSinkIsTodaysDrain(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < transcriptSinkBuffer*3; i++ {
		emitTranscript(t, be, "ignored", false)
	}
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})

	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "an absent sink must leave the call exactly as it was")
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
