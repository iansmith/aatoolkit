package realtime

import (
	"encoding/json"
	"testing"
	"time"
)

// Behavior tests for AATK-69, transcribed from the ticket's Test expectations.
// Fixtures and the fake backend live in fixtures_test.go.

// --- handshake --------------------------------------------------------------

func TestDialSendsSessionUpdateDeclaringMuLawBothDirections(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	evs := be.events()
	if len(evs) == 0 {
		t.Fatal("backend received no client event")
	}

	var got struct {
		Type    string `json:"type"`
		Session struct {
			Audio struct {
				Input  struct{ Format struct{ Type string } } `json:"input"`
				Output struct{ Format struct{ Type string } } `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(evs[0], &got); err != nil {
		t.Fatalf("decoding first client event: %v", err)
	}

	if got.Type != EventSessionUpdate {
		t.Errorf("first client event = %q, want %q", got.Type, EventSessionUpdate)
	}
	// Both directions asserted deliberately: declaring input and leaving output
	// at the backend default is the silent half-configuration being guarded.
	if got.Session.Audio.Input.Format.Type != FormatG711ULaw {
		t.Errorf("input format = %q, want %q", got.Session.Audio.Input.Format.Type, FormatG711ULaw)
	}
	if got.Session.Audio.Output.Format.Type != FormatG711ULaw {
		t.Errorf("output format = %q, want %q", got.Session.Audio.Output.Format.Type, FormatG711ULaw)
	}
}

// --- pass-through, inbound --------------------------------------------------

func TestForwardSendsPayloadVerbatim(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	payload := frameB64()
	if err := b.Forward(ctx, payload); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	var appended string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && appended == "" {
		for _, raw := range be.events() {
			var ev struct {
				Type  string `json:"type"`
				Audio string `json:"audio"`
			}
			if json.Unmarshal(raw, &ev) == nil && ev.Type == EventAudioAppend {
				appended = ev.Audio
			}
		}
		if appended == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// String equality on the ENCODED form. Decoding both sides first would pass
	// even if the bridge decoded and re-encoded, which is the defect this guards.
	if appended != payload {
		t.Errorf("appended audio must be the carrier payload verbatim:\n got %q\nwant %q", appended, payload)
	}
}

// --- pass-through, outbound -------------------------------------------------

func TestOutboundDeltaReachesSinkVerbatim(t *testing.T) {
	delta := frameB64()
	be := newFakeBackend(t)
	be.toSend = []any{ServerEvent{Type: EventAudioDelta, Delta: delta}}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	sink := &recordingSink{}
	b := NewBridge(c, sink)
	go func() { _ = b.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if media, _ := sink.snapshot(); len(media) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	media, _ := sink.snapshot()
	if len(media) != 1 {
		t.Fatalf("sink received %d media payloads, want 1", len(media))
	}
	if media[0] != delta {
		t.Errorf("outbound payload must equal the delta verbatim:\n got %q\nwant %q", media[0], delta)
	}
}

// --- barge-in ---------------------------------------------------------------

func TestTwoConsecutiveSpeechStartedProduceTwoClears(t *testing.T) {
	be := newFakeBackend(t)
	// No speech_stopped between them: deduplicating here would be a behavior
	// change, not an optimisation, so two events must produce two clears.
	be.toSend = []any{
		ServerEvent{Type: EventSpeechStarted},
		ServerEvent{Type: EventSpeechStarted},
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, clears := sink.snapshot(); clears >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, clears := sink.snapshot(); clears != 2 {
		t.Errorf("two speech_started events produced %d clears, want exactly 2", clears)
	}
}

// --- transcripts ------------------------------------------------------------

func TestTranscriptsSurfaceInArrivalOrder(t *testing.T) {
	be := newFakeBackend(t)
	be.toSend = []any{
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

	var got []Transcript
	timeout := time.After(2 * time.Second)
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
}

// --- failure ----------------------------------------------------------------

func TestBackendCloseSurfacesErrorAndUnblocksRun(t *testing.T) {
	be := newFakeBackend(t)
	be.closeHard = true
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})
	errCh := make(chan error, 1)
	go func() { errCh <- b.Run(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run returned nil after the backend closed; the caller must see the failure")
		}
	case <-time.After(3 * time.Second):
		// Run still blocked on a read is precisely the leak being guarded.
		t.Fatal("Run did not return after the backend closed")
	}
}

// --- unknown events ---------------------------------------------------------

func TestUnknownServerEventIsIgnoredAndReadLoopSurvives(t *testing.T) {
	delta := frameB64()
	be := newFakeBackend(t)
	be.toSend = []any{
		ServerEvent{Type: "response.some_future_thing.delta"},
		ServerEvent{Type: EventAudioDelta, Delta: delta},
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if media, _ := sink.snapshot(); len(media) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The event after the unknown one still arrived => the loop survived it.
	media, _ := sink.snapshot()
	if len(media) != 1 || media[0] != delta {
		t.Errorf("event following an unknown type was not delivered; sink media = %v", media)
	}
}
