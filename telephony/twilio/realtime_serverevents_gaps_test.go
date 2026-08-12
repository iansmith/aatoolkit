package twilio

import (
	"testing"
	"time"
)

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

	emitTranscript(t, be, "hello", false)

	// The existing destination still sees it.
	select {
	case tr := <-tch:
		if tr.Text != "hello" {
			t.Fatalf("transcript = %q, want %q", tr.Text, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the modelled event never reached the transcript channel")
	}

	// And so does the server-event channel.
	const want = "conversation.item.input_audio_transcription.delta"
	select {
	case ev := <-sech:
		if ev.Type != want {
			t.Fatalf("server-event channel delivered type %q, want %q", ev.Type, want)
		}
		if len(ev.Raw) == 0 {
			t.Fatal("a modelled event must carry Raw too, like any other event")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a MODELLED event never reached the server-event channel; behavior 2 "+
			"feeds it for EVERY event Run reads, not only unmodelled ones (want %q)", want)
	}
}
