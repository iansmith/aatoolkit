package twilio

import (
	"testing"
	"time"
)

// Inbound-audio-channel tests for the realtime path: WithInboundAudioChan /
// WithInboundAudioChanFor let a consumer observe caller→backend μ-law
// without wrapping the carrier WebSocket the pump owns.

func TestInboundAudioChan_ConsumerReceivesMediaPayload(t *testing.T) {
	ch := make(chan string, testChanBuffer)
	payload := carrierPayloadB64()

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInboundAudioChan(ch)))
	waitBackendReady(t, be, h)

	select {
	case got := <-ch:
		if got != payload {
			t.Fatalf("consumer must receive the inbound payload verbatim:\n got  %q\nwant %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received an inbound payload")
	}

	got := be.appendedAudio()
	if len(got) < 1 || got[0] != payload {
		t.Fatalf("observation must not replace forwarding; backend appended %v, want %q first", got, payload)
	}
}

func TestInboundAudioChan_EngineNeverClosesTheConsumersChannel(t *testing.T) {
	ch := make(chan string, testChanBuffer)
	payload := carrierPayloadB64()

	for i := 0; i < 2; i++ {
		be := newFakeRealtimeBackend(t)
		h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInboundAudioChan(ch)))
		waitBackendReady(t, be, h)

		select {
		case got := <-ch:
			if got != payload {
				t.Fatalf("call %d: got %q, want %q", i+1, got, payload)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call %d: consumer never received an inbound payload", i+1)
		}
		h.conn.CloseNow()
		_ = h.waitDone(5 * time.Second)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("engine must not close the consumer's channel")
		}
	default:
	}
}

func TestInboundAudioChan_FullChannelDropsAndStillForwards(t *testing.T) {
	ch := make(chan string, 1)
	payload := carrierPayloadB64()

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInboundAudioChan(ch)))
	waitBackendReady(t, be, h)

	const extra = 20
	for range extra {
		h.sendRaw(mediaFrameRaw(h.streamSID, payload))
	}

	waitForAppends(t, be, 1+extra, 5*time.Second)
	assertStillRunning(t, h, "a full inbound observer must not stall the carrier pump")
}

func TestInboundAudioChan_AbsentOptionIsTodaysBehaviour(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))
	waitBackendReady(t, be, h)
	assertStillRunning(t, h, "no WithInboundAudioChan option must leave the call exactly as it was")
}

func TestInboundAudioChanFor_ResolvesFromStartFrame(t *testing.T) {
	ch := make(chan string, testChanBuffer)
	var gotSID string

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInboundAudioChanFor(func(start Frame) chan<- string {
		gotSID = start.StreamSID
		return ch
	})))
	waitBackendReady(t, be, h)

	if gotSID != h.streamSID {
		t.Fatalf("resolver start.StreamSID = %q, want %q", gotSID, h.streamSID)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver-returned channel never received an inbound payload")
	}
}

func TestInboundAudioChanFor_NilMeansNoConsumer(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInboundAudioChanFor(func(Frame) chan<- string {
		return nil
	})))
	waitBackendReady(t, be, h)
	assertStillRunning(t, h, "a nil inbound channel must leave the call running")
	if got := be.appendedAudio(); len(got) < 1 {
		t.Fatal("a nil inbound channel must still forward media to the backend")
	}
}
