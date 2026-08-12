package twilio

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink that is safe to read while a goroutine is writing
// to it.
//
// Round-2 adversary finding (blocker): TestServerEventChan_DropIsLogged
// captured log output into a bare bytes.Buffer and read it after a sleep. But
// drops are logged from the delivery goroutine, by construction — the ticket
// requires delivery to be non-blocking and requires the engine to run no
// consumer code on a goroutine it owns — so the test's read races that write.
// It was reproduced 5/5 under -race against a reference implementation, and no
// compliant implementation can avoid it, while the DoD requires
// "GOWORK=off go test ./telephony/... -race" clean.
//
// A sleep is not a happens-before edge. This is.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Adversary gap test for AATK-81, round 1 — the twilio-level sibling of
// TestBridgeEvents_ReceivesModelledEventsToo.

// TestServerEventChan_ReceivesModelledEventsToo closes the round-1 blocker at
// the consumer-facing layer.
//
// Every frozen test that puts something on the server-event channel emits an
// UNMODELLED type, so the suite never established that a modelled event —
// one Run's switch already routes somewhere — also reaches the consumer.
// Observable behavior 2 requires both: the channel is fed "for every event Run
// reads", and the DoD separately requires that modelled events "still reach
// their existing destinations unchanged".
//
// An implementation publishing only from Run's default: branch satisfies the
// frozen suite and fails this test, which is the point. Asserting both
// channels on one call is what makes it discriminating: a transcript event
// must arrive on the transcript channel AND on the server-event channel, so
// neither "routed to the old destination only" nor "diverted to the new
// channel only" can pass.
//
// slopstop:test contract
func TestServerEventChan_ReceivesModelledEventsToo(t *testing.T) {
	sech := make(chan ServerEvent, testChanBuffer)
	tch := make(chan Transcript, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithServerEventChan(sech),
		WithTranscriptChan(tch),
	))
	waitBackendReady(t, be, h)

	// All four modelled types, not one representative. Round 3 proved that
	// checking only the transcript delta left three types unpinned: excluding
	// audio-delta and speech-started from the publish passed the whole suite.
	want := []string{
		"response.output_audio.delta",
		"input_audio_buffer.speech_started",
		"conversation.item.input_audio_transcription.delta",
		"conversation.item.input_audio_transcription.completed",
	}
	be.emitOnce(t, map[string]string{"type": want[0], "delta": carrierPayloadB64()})
	be.emitOnce(t, map[string]string{"type": want[1]})
	emitTranscript(t, be, "hello", false)
	emitTranscript(t, be, "hello there", true)

	// The existing destination still sees it.
	select {
	case tr := <-tch:
		if tr.Text != "hello" {
			t.Fatalf("transcript = %q, want %q", tr.Text, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the modelled event never reached the transcript channel")
	}

	// And so does the server-event channel, for every modelled type.
	var got []string
	for len(got) < len(want) {
		select {
		case ev := <-sech:
			if len(ev.Raw) == 0 {
				t.Fatalf("a modelled event must carry Raw too, like any other event (type %q)", ev.Type)
			}
			got = append(got, ev.Type)
		case <-time.After(5 * time.Second):
			t.Fatalf("a MODELLED event never reached the server-event channel: got %v, want %v. "+
				"Behavior 2 feeds it for EVERY event Run reads, not only unmodelled ones and "+
				"not only one representative modelled type", got, want)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("server-event channel delivered %v, want %v", got, want)
		}
	}
}

// TestServerEventChan_PreservesArrivalOrder closes the round-2 major.
//
// Every other test on this channel either sends one checked event per call, or
// floods it with indistinguishable copies and checks only that something
// arrived. So nothing pinned ordering — a delivery path that reordered
// consecutive events passed all fifteen tests, proven by a mutation that
// dispatched every other event from a short-lived goroutine.
//
// Transcripts() is documented to yield "transcription results in arrival
// order" (bridge.go). Events() carries the same expectation and, until this
// test, none of the same enforcement.
//
// slopstop:test contract
func TestServerEventChan_PreservesArrivalOrder(t *testing.T) {
	ch := make(chan ServerEvent, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithServerEventChan(ch)))
	waitBackendReady(t, be, h)

	want := []string{"first", "second", "third"}
	for _, id := range want {
		serverEventItemID(t, be, id)
	}

	var got []string
	for len(got) < len(want) {
		select {
		case ev := <-ch:
			var raw map[string]string
			if err := json.Unmarshal(ev.Raw, &raw); err != nil {
				t.Fatalf("Raw did not decode as the original frame: %v", err)
			}
			got = append(got, raw["item_id"])
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d of %d events (%v)", len(got), len(want), got)
		}
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events must arrive in the order the backend sent them: got %v, want %v", got, want)
		}
	}
}
