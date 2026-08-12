package realtime

import (
	"bytes"
	"encoding/json"
	"log"
	"sync"
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
// Removing that defer leaks the twilio-side deliver goroutine on
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

// TestBridgeEvents_UndrainedEventsDoNotStallTheSink pins the Bridge-layer
// delivery discipline, and it REPLACES an earlier test of this orchestrator's
// that pinned the opposite.
//
// The stage-9 pinning pass found publishEvent's discipline unpinned and I
// pinned it as blocking-with-context-abandon, reasoning from publish and
// Transcripts. That was the wrong precedent. Both stage-10b checkers then
// proved the consequence independently, with the same measurement: 40 audio
// deltas with Events() undrained delivered 40 of 40 media frames at base and
// 16 of 40 on the branch. b.events is exported through Events(), Run's loop is
// also what drives the MediaSink, and every event is published — including one
// audio delta per 20 ms frame — so blocking there silences a live call within
// ~320 ms of a consumer looking away, with Run neither returning nor erroring.
//
// The right precedent is signalActivity, the engine's other every-event
// mechanism, which is non-blocking by explicit design: "Run's read loop can
// never stall waiting on a consumer that is not currently receiving." It is
// also what observable behavior 4 says in its own words — "delivery is
// non-blocking: when the consumer is behind, events are dropped and each drop
// is logged" — with no layer qualifier.
//
// This is the checkers' own probe, promoted to a test.
//
// slopstop:test contract
func TestBridgeEvents_UndrainedEventsDoNotStallTheSink(t *testing.T) {
	const n = 40 // comfortably past b.events' 16-slot buffer

	delta := frameB64()
	be := newFakeBackend(t)
	for i := 0; i < n; i++ {
		be.toSend = append(be.toSend, ServerEvent{Type: EventAudioDelta, Delta: delta})
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

	// Events() is deliberately NEVER read. Every media frame must still reach
	// the sink: an undrained events channel may lose events, but it must never
	// park the read loop that carries audio.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if media, _ := sink.snapshot(); len(media) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	media, _ := sink.snapshot()
	t.Fatalf("sink received %d of %d media frames with Events() undrained — an undrained "+
		"events channel must drop, never park Run's read loop, because that loop is also "+
		"what drives the MediaSink", len(media), n)
}

// syncLogBuffer is a log sink safe to read while another goroutine writes to
// it. Run's read loop does the dropping and the logging; the test reads. A
// bare bytes.Buffer here would be a data race under -race, which is the same
// defect the twilio-layer drop-log test had to be repaired for.
type syncLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// TestBridgeEvents_DropIsLogged pins the other half of publishEvent.
//
// The SALVAGE repair made delivery non-blocking-with-drop and logged each drop,
// and only the drop half was pinned: removing the log call left both packages
// green. The twilio-layer drop test cannot cover this — measured, the Bridge
// drop path never fires through telephony/twilio at all (199 drops at the
// twilio layer, 0 at the Bridge layer), because that layer drains promptly. So
// only a test driving realtime.Bridge directly can observe it.
//
// Observable behavior 4 asks for both halves in one sentence: "delivery is
// non-blocking: when the consumer is behind, events are dropped and each drop
// is logged."
//
// slopstop:test contract
func TestBridgeEvents_DropIsLogged(t *testing.T) {
	var buf syncLogBuffer
	origOutput, origFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	const n = 40 // comfortably past b.events' 16-slot buffer
	delta := frameB64()
	be := newFakeBackend(t)
	for i := 0; i < n; i++ {
		be.toSend = append(be.toSend, ServerEvent{Type: EventAudioDelta, Delta: delta})
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

	// Events() is never read, so everything past the buffer must be dropped.
	// Wait on the sink rather than the clock: once all n frames have shipped,
	// every event has been through publishEvent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if media, _ := sink.snapshot(); len(media) == n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if media, _ := sink.snapshot(); len(media) != n {
		t.Fatalf("sink received %d of %d media frames; the drop path was never exercised", len(media), n)
	}

	if !bytes.Contains(buf.Bytes(), []byte("dropped")) {
		t.Fatalf("a dropped server event must be logged, not silent; log output: %q", buf.Bytes())
	}
}

// TestBridgeEvents_DropLoggingIsRateBounded pins the bound on the drop log.
//
// Stage-10b round 3 reported it as a major behavioural finding: Events() is
// new, so every existing direct Bridge consumer is undrained by construction,
// and after the first 16 events every event is a drop. A line per drop is
// ~50/second at one audio delta per 20 ms frame — roughly 15,000 for a
// five-minute call — and log.Printf writes synchronously under a mutex on
// Run's goroutine, the one driving the MediaSink, so a slow stderr stalls
// audio. That is the failure mode the non-blocking publish exists to remove,
// arriving through the logging that replaced it.
//
// The bound must not make drops silent: every drop is still counted and every
// line carries the running total. This test pins both halves — that something
// is logged, and that it is not one line per drop.
//
// slopstop:test contract
func TestBridgeEvents_DropLoggingIsRateBounded(t *testing.T) {
	var buf syncLogBuffer
	origOutput, origFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	// 40 events against a 16-slot buffer that nothing drains: 24 drops, well
	// under dropLogEvery, so a bounded policy logs exactly the first one.
	const n = 40
	const wantDrops = n - 16

	delta := frameB64()
	be := newFakeBackend(t)
	for i := 0; i < n; i++ {
		be.toSend = append(be.toSend, ServerEvent{Type: EventAudioDelta, Delta: delta})
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if media, _ := sink.snapshot(); len(media) == n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if media, _ := sink.snapshot(); len(media) != n {
		t.Fatalf("sink received %d of %d media frames; the drop path was never exercised", len(media), n)
	}

	// "dropped on this call" appears once per line; a bare "dropped" appears
	// twice, so count the phrase that identifies a whole line.
	got := bytes.Count(buf.Bytes(), []byte("dropped on this call"))
	if got == 0 {
		t.Fatal("drops must not be silent: nothing was logged")
	}
	// Exactly one, not merely fewer than the drop count: every one of these
	// drops is below dropLogEvery, so the first is the only line the policy
	// permits. Measured — a `got >= wantDrops` assertion here passes with
	// dropLogEvery set to 2, which is 25 lines/second at audio rate and is the
	// regression this test exists to catch.
	if got != 1 {
		t.Fatalf("drop logging must be rate-bounded: %d lines for ~%d drops, want 1. A line per "+
			"drop is ~50/second at audio rate and writes synchronously on the goroutine "+
			"driving the MediaSink", got, wantDrops)
	}
}

// TestLogDrop pins the policy's arithmetic, and specifically its periodic arm.
//
// Review gap, measured: replacing LogDrop's body with `return n == 1` left both
// packages green, because no test drops as many as dropLogEvery times. That
// mutation is the one failure the bound must not have — a five-minute call
// dropping ~15,000 events would report "1 dropped on this call" once and never
// again, and the running total that makes the aggregate recoverable would be
// gone. The bound is a deliberate departure from "each drop is logged"; it is
// only defensible while the total keeps arriving.
//
// The wanted values are literals rather than expressions over dropLogEvery: a
// test deriving its expectation from the constant it is pinning would follow
// any change to it and assert nothing.
//
// slopstop:test contract
func TestLogDrop(t *testing.T) {
	cases := []struct {
		n    int
		want bool
	}{
		{1, true},    // the first drop always surfaces: a lossy call is never wholly invisible
		{2, false},   // ...and the second does not, or the bound would not bound
		{99, false},  // just below the period
		{100, true},  // the periodic arm: the running total arrives again
		{101, false}, // just past it
		{199, false},
		{200, true},   // and keeps arriving, every dropLogEvery-th, for the whole call
		{15000, true}, // ~a five-minute call's worth of drops at audio rate
	}
	for _, c := range cases {
		if got := LogDrop(c.n); got != c.want {
			t.Errorf("LogDrop(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
