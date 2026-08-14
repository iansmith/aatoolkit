package twilio

import (
	"bytes"
	"log"
	"testing"
	"time"
)

// TestCarrierAudioChan_AbsentOptionNeverLogsADrop closes a gap found by the
// stage-9 mutation check against telephony/twilio/realtime.go (AATK-84):
// deliverCarrierAudio's nil-channel guard —
//
//	if s.carrierAudioCh == nil {
//		return
//	}
//
// — has nothing pinning it. Removing it leaves the suite green, because a
// send on a nil channel inside a select with a default case is never ready,
// so every Media/Clear call on a call with NO WithCarrierAudioChan option
// would silently fall into the default branch: incrementing
// carrierAudioDropped and, at realtime.LogDrop's bounded rate, logging
// "carrier audio dropped, consumer is behind" — even though no consumer ever
// asked to observe anything.
//
// TestCarrierAudioChan_AbsentOptionIsTodaysBehaviour (realtime_carrieraudio_test.go,
// frozen) already asserts the call keeps running and audio keeps flowing
// with no option supplied, but it never inspects the log, so that spurious
// bookkeeping and log noise passes it unnoticed. This test adds the missing
// check beside it as a pure addition — the frozen file is untouched.
func TestCarrierAudioChan_AbsentOptionNeverLogsADrop(t *testing.T) {
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url())) // no WithCarrierAudioChan
	waitBackendReady(t, be, h)
	seen := h.countMediaFrames(t)

	for i := 0; i < 20; i++ {
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	}
	waitFor(t, 5*time.Second, func() bool { return seen() >= 20 })

	// Give any spurious drop-logging goroutine a chance to have run.
	time.Sleep(200 * time.Millisecond)

	if bytes.Contains(buf.Bytes(), []byte("carrier audio dropped")) {
		t.Fatalf("no WithCarrierAudioChan option must never log a carrier-audio drop; log output: %q", buf.String())
	}
}
