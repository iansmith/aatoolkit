package realtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Adversary gap tests for AATK-81, round 1. Both close gaps the Phase 0 suite
// left open — each was demonstrated by mutating production code into a wrong
// implementation that the frozen suite accepted.

// TestBridgeEvents_ReceivesModelledEventsToo closes the round-1 blocker.
//
// Observable behavior 2 says Events() is "fed for every event Run reads,
// including ones its switch does not model". The frozen suite only ever proved
// the "including" half: every test that reaches Events() sends an UNMODELLED
// type, and the one test that sends modelled types
// (TestBridgeRun_ModelledEventsStillReachExistingDestinations) drains Events()
// without asserting anything about what arrived.
//
// The gap is not hypothetical. An implementation that publishes to b.events
// from Run's default: branch only — dropping every modelled event from the new
// channel — passed all twelve frozen tests. This test is the discriminating
// case that implementation fails: a modelled event must reach BOTH its existing
// destination and Events(), because "every" means every.
//
// Round 3 widened this from one modelled type to all four. Checking only
// EventTranscriptDelta left three types unpinned, and it was not a theoretical
// hole: excluding {EventAudioDelta, EventSpeechStarted} from the publish, or
// excluding EventTranscriptDone alone, passed the entire 17-test suite. "Every"
// means each of the four cases Run's switch handles, not one representative.
//
// slopstop:test contract
func TestBridgeEvents_ReceivesModelledEventsToo(t *testing.T) {
	delta := frameB64()
	be := newFakeBackend(t)
	be.toSend = []any{
		ServerEvent{Type: EventAudioDelta, Delta: delta},
		ServerEvent{Type: EventSpeechStarted},
		ServerEvent{Type: EventTranscriptDelta, Transcript: "hello"},
		ServerEvent{Type: EventTranscriptDone, Transcript: "hello there"},
	}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	go func() { _ = b.Run(ctx) }()

	// Existing destination, unchanged.
	select {
	case tr, ok := <-b.Transcripts():
		if !ok {
			t.Fatal("transcript channel closed before delivering the modelled event")
		}
		if tr.Text != "hello" {
			t.Fatalf("transcript = %q, want %q", tr.Text, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the modelled event never reached Transcripts()")
	}

	// And the new channel, for EVERY modelled type — behavior 2 says Events()
	// is fed for every event Run reads. Dial consumes session.created before
	// returning, so these four are the first four events Run observes.
	want := []string{
		EventAudioDelta,
		EventSpeechStarted,
		EventTranscriptDelta,
		EventTranscriptDone,
	}
	var got []string
	for len(got) < len(want) {
		select {
		case ev, ok := <-b.Events():
			if !ok {
				t.Fatalf("Events() closed after %d of %d modelled events (%v)", len(got), len(want), got)
			}
			got = append(got, ev.Type)
		case <-time.After(3 * time.Second):
			t.Fatalf("a MODELLED event never reached Events(): got %v, want %v. Behavior 2 "+
				"feeds Events() for EVERY event Run reads, not only the ones its switch "+
				"drops into default:, and not only one representative modelled type", got, want)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Events() delivered %v, want %v", got, want)
		}
	}
}

// TestClientRead_RawIsByteIdenticalToWireFrame closes the round-1 major.
//
// events.go commits Raw to "the whole frame exactly as it arrived on the wire"
// and to being assigned "from the same bytes it unmarshalled". The frozen
// suite only ever decodes Raw back into a map and inspects one field, which a
// semantically-equivalent reconstruction satisfies just as well as the real
// bytes: replacing the assignment with an unmarshal-then-remarshal round trip
// kept TestClientRead_RawCarriesWholeFrameVerbatim green.
//
// Key order is what separates the two. Go's encoder sorts map keys, so a frame
// whose keys are NOT in sorted order survives a verbatim assignment byte for
// byte and comes back reordered from any round trip through a map.
//
// slopstop:test contract
func TestClientRead_RawIsByteIdenticalToWireFrame(t *testing.T) {
	// Deliberately unsorted: "zz_last" precedes "aa_first" on the wire.
	frame := []byte(`{"type":"input_audio_buffer.speech_stopped","zz_last":"z","aa_first":"a"}`)

	be := newFakeBackend(t)
	// json.RawMessage marshals to itself, so these exact bytes reach the wire.
	be.toSend = []any{json.RawMessage(frame)}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var ev ServerEvent
	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err = c.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if ev.Type == EventSpeechStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never observed the speech_stopped event")
		}
	}

	if !bytes.Equal(ev.Raw, frame) {
		t.Fatalf("Raw must be the wire bytes verbatim, not a re-encoding.\n got: %s\nwant: %s",
			ev.Raw, frame)
	}
}

// TestBridgeEvents_PreservesArrivalOrder closes the round-2 major one layer
// below TestServerEventChan_PreservesArrivalOrder.
//
// Transcripts() is documented to yield "transcription results in arrival
// order". Events() carries the same expectation and had none of the same
// enforcement: a publish path that dispatched every other event from a
// short-lived goroutine reordered consecutive events and was accepted by every
// other test in the suite.
//
// slopstop:test contract
func TestBridgeEvents_PreservesArrivalOrder(t *testing.T) {
	ids := []string{"first", "second", "third"}

	be := newFakeBackend(t)
	for _, id := range ids {
		be.toSend = append(be.toSend, json.RawMessage(
			`{"type":"`+EventSpeechStopped+`","item_id":"`+id+`"}`))
	}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	go func() { _ = b.Run(ctx) }()

	var got []string
	for len(got) < len(ids) {
		select {
		case ev, ok := <-b.Events():
			if !ok {
				t.Fatalf("Events() closed after %d of %d", len(got), len(ids))
			}
			var raw map[string]string
			if err := json.Unmarshal(ev.Raw, &raw); err != nil {
				t.Fatalf("Raw did not decode as the original frame: %v", err)
			}
			got = append(got, raw["item_id"])
		case <-time.After(3 * time.Second):
			t.Fatalf("received %d of %d events (%v)", len(got), len(ids), got)
		}
	}

	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("Events() must yield events in arrival order: got %v, want %v", got, ids)
		}
	}
}

// --- pinning tests: stage-9 mutation-check --implemented ------------------------
//
// The pinning pass mutates the REAL implementation and asks what the suite
// notices. Three production behaviours survived every mutation. The two below
// close the two that can be pinned through the public API.

// TestBridgeEvents_ClosedWhenRunReturns pins `defer close(b.events)`.
//
// Removing that defer leaks the twilio-side deliverServerEvents goroutine on
// EVERY call — its `for ev := range src` never returns. The differential
// goroutine test cannot see it: the leak fires identically with and without a
// consumer, so both arms grow equally and the difference stays zero. That is
// precisely the shape a differential measurement is blind to, and it left the
// ticket's own "no engine goroutine outlives a call" item unguarded against
// the one bug most likely to break it.
//
// Transcripts() has had this guarantee since before the ticket ("The channel
// is closed when Run returns"); Events() now states it and this enforces it.
//
// slopstop:test contract
func TestBridgeEvents_ClosedWhenRunReturns(t *testing.T) {
	be := newFakeBackend(t)
	be.toSend = []any{ServerEvent{Type: EventSpeechStopped}}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	b := NewBridge(c, &recordingSink{})
	runDone := make(chan struct{})
	go func() { _ = b.Run(ctx); close(runDone) }()

	// End the call the way a backend going away ends it.
	time.Sleep(100 * time.Millisecond)
	_ = c.Close()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run never returned after the connection closed")
	}

	// Drain whatever is buffered; the channel must then be closed.
	for {
		select {
		case _, ok := <-b.Events():
			if !ok {
				return // closed, as required
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Events() was never closed after Run returned — a consumer ranging " +
				"over it is stranded, which leaks a goroutine on every call")
		}
	}
}

// TestBridgeEvents_BufferFullBlocksRatherThanDrops pins publishEvent's
// blocking-with-context-abandon discipline.
//
// Swapping it for non-blocking-drop was unobserved, because b.events is
// buffered 16 and no other test ever fills it. The two disciplines are
// deliberately different and the difference is load-bearing: the engine-
// internal channel must not silently lose events, while the consumer-facing
// one in the twilio package drops by design. Conflating them would lose
// events with nothing reporting it.
//
// Sending well past the buffer with no reader, then draining, distinguishes
// them: blocking delivers all of them, dropping does not.
//
// slopstop:test contract
func TestBridgeEvents_BufferFullBlocksRatherThanDrops(t *testing.T) {
	const n = 40 // comfortably past b.events' 16-slot buffer

	be := newFakeBackend(t)
	for i := 0; i < n; i++ {
		be.toSend = append(be.toSend, json.RawMessage(
			fmt.Sprintf(`{"type":%q,"item_id":"e%d"}`, EventSpeechStopped, i)))
	}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	go func() { _ = b.Run(ctx) }()

	// Do not read yet: let the buffer fill so the publish path meets a full
	// channel. This is the state the two disciplines disagree about.
	time.Sleep(300 * time.Millisecond)

	var got []string
	for len(got) < n {
		select {
		case ev, ok := <-b.Events():
			if !ok {
				t.Fatalf("Events() closed after %d of %d", len(got), n)
			}
			var raw map[string]string
			if err := json.Unmarshal(ev.Raw, &raw); err != nil {
				t.Fatalf("Raw did not decode as the original frame: %v", err)
			}
			got = append(got, raw["item_id"])
		case <-time.After(3 * time.Second):
			t.Fatalf("received %d of %d events — a full buffer must block the publish "+
				"until the reader catches up, never drop silently. got=%v", len(got), n, got)
		}
	}

	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("e%d", i); got[i] != want {
			t.Fatalf("event %d = %q, want %q (full-buffer handling must not reorder either)", i, got[i], want)
		}
	}
}
