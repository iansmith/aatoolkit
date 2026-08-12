package realtime

import (
	"bytes"
	"encoding/json"
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
// slopstop:test contract
func TestBridgeEvents_ReceivesModelledEventsToo(t *testing.T) {
	be := newFakeBackend(t)
	be.toSend = []any{ServerEvent{Type: EventTranscriptDelta, Transcript: "hello"}}
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

	// And the new channel, because behavior 2 says EVERY event Run reads.
	// Dial consumes session.created before returning, so the transcript delta
	// is the first event Run observes and the first Events() can carry.
	select {
	case ev, ok := <-b.Events():
		if !ok {
			t.Fatal("Events() closed before delivering the modelled event")
		}
		if ev.Type != EventTranscriptDelta {
			t.Fatalf("Events() delivered type %q, want %q", ev.Type, EventTranscriptDelta)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a MODELLED event never reached Events(); behavior 2 says Events() is fed " +
			"for EVERY event Run reads, not only the ones its switch drops into default:")
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
