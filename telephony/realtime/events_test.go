package realtime

import (
	"encoding/json"
	"testing"
	"time"
)

// Phase 0 tests for AATK-81 ("Surface unmodelled backend events to the
// consumer") at the realtime package level: ServerEvent.Raw and
// Bridge.Events(). The twilio-level WithServerEventChan/WithServerEventChanFor
// pair lives in telephony/twilio/realtime_serverevents_test.go.

// --- ServerEvent.Raw ---------------------------------------------------------

// TestClientRead_RawCarriesWholeFrameVerbatim pins observable behavior 1:
// Client.Read must set Raw to the whole frame as it arrived, not just the
// fields ServerEvent models. A field ServerEvent does not know about
// ("unmodelled_field" below) proves this is the raw wire bytes and not a
// reconstruction from the decoded struct.
//
// slopstop:test contract
func TestClientRead_RawCarriesWholeFrameVerbatim(t *testing.T) {
	be := newFakeBackend(t)
	be.toSend = []any{map[string]any{
		"type":             EventSpeechStopped,
		"unmodelled_field": "not-on-ServerEvent",
	}}
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

	if len(ev.Raw) == 0 {
		t.Fatal("Raw must be set to the whole frame; got empty/nil")
	}

	var raw map[string]any
	if err := json.Unmarshal(ev.Raw, &raw); err != nil {
		t.Fatalf("Raw did not decode as the original frame: %v", err)
	}
	if raw["unmodelled_field"] != "not-on-ServerEvent" {
		t.Fatalf("Raw must carry the whole original frame verbatim, including "+
			"fields ServerEvent does not model; got %s", ev.Raw)
	}
}

// --- Bridge.Events() ---------------------------------------------------------

// TestBridgeEvents_ReceivesUnmodelledEventWithRaw pins observable behavior 2:
// Bridge.Events() is fed for every event Run reads, including ones its switch
// does not model. EventSpeechStopped is declared at events.go:24 but Run's
// switch never cases it — it is the ticket's own example of an unmodelled
// type — so this is the discriminating case an implementation that only wires
// the four modelled types into Events() would still fail.
//
// slopstop:test contract
func TestBridgeEvents_ReceivesUnmodelledEventWithRaw(t *testing.T) {
	be := newFakeBackend(t)
	be.toSend = []any{ServerEvent{Type: EventSpeechStopped}}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	go func() { _ = b.Run(ctx) }()

	select {
	case ev, ok := <-b.Events():
		if !ok {
			t.Fatal("Events() closed before delivering the unmodelled event")
		}
		if ev.Type != EventSpeechStopped {
			t.Fatalf("Events() delivered type %q, want %q", ev.Type, EventSpeechStopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events() never delivered the unmodelled speech_stopped event")
	}
}

// --- modelled events are unaffected ------------------------------------------

// TestBridgeRun_ModelledEventsStillReachExistingDestinations guards the DoD's
// no-regression half:
//
//	"Modelled events still reach their existing destinations unchanged;
//	transcripts and audio are unaffected."
//
// It drains Events() concurrently — a consumer of the new channel is present
// — while asserting the sink and Transcripts() still see exactly what they
// saw before Events() existed. Passing here is the point: this must hold both
// before and after the new channel is wired up.
//
// slopstop:test regression — guards: "Modelled events still reach their existing destinations unchanged; transcripts and audio are unaffected."
func TestBridgeRun_ModelledEventsStillReachExistingDestinations(t *testing.T) {
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

	sink := &recordingSink{}
	b := NewBridge(c, sink)
	go func() { _ = b.Run(ctx) }()

	// A consumer of the new channel, present but not the thing under test here
	// — draining it so it cannot be blamed if something downstream stalls.
	go func() {
		for range b.Events() {
		}
	}()

	var got []Transcript
	timeout := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case tr, ok := <-b.Transcripts():
			if !ok {
				t.Fatalf("transcript channel closed after %d of 2", len(got))
			}
			got = append(got, tr)
		case <-timeout:
			t.Fatalf("timed out with %d of 2 transcripts", len(got))
		}
	}
	if got[0].Text != "hello" || got[0].Final {
		t.Errorf("first transcript = %+v, want {hello false}", got[0])
	}
	if got[1].Text != "hello there" || !got[1].Final {
		t.Errorf("second transcript = %+v, want {hello there true}", got[1])
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		media, clears := sink.snapshot()
		if len(media) == 1 && clears == 1 {
			if media[0] != delta {
				t.Fatalf("media payload = %q, want %q", media[0], delta)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	media, clears := sink.snapshot()
	t.Fatalf("sink saw %d media (want 1) and %d clears (want 1)", len(media), clears)
}

// --- the concurrent-Run hazard ------------------------------------------------

// TestBridgeRun_ConcurrentSecondRunReturnsRefusalWithoutPanic guards the
// hazard the ticket calls out by name: adding a second channel means a second
// close, and "the loser would panic closing an already-closed transcript
// channel on the way out" (bridge.go:64) if a second close path were added
// under the CAS guard's losing branch. The ticket's own hazard section says:
//
//	"write a test asserting a concurrent second `Run` still returns the
//	refusal error (\"realtime: Run already in progress\") without panicking.
//	No such test exists today."
//
// This exercises current, unmodified CAS-refusal behavior — it must keep
// passing exactly as it does today once Events() is wired into Run.
//
// slopstop:test regression — guards: "write a test asserting a concurrent second `Run` still returns the refusal error (\"realtime: Run already in progress\") without panicking."
func TestBridgeRun_ConcurrentSecondRunReturnsRefusalWithoutPanic(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})

	firstDone := make(chan struct{})
	go func() {
		_ = b.Run(ctx)
		close(firstDone)
	}()

	// Give the first Run a chance to win the CAS before the second call.
	time.Sleep(100 * time.Millisecond)

	err = b.Run(ctx)
	if err == nil {
		t.Fatal("a concurrent second Run must be refused, not allowed to proceed")
	}
	if err.Error() != "realtime: Run already in progress" {
		t.Fatalf("refusal error = %q, want %q", err.Error(), "realtime: Run already in progress")
	}

	_ = c.Close()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the first Run never returned after the connection closed")
	}
}
