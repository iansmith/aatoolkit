package twilio

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// failingWSWriter always fails Write with err, so a test can force
// carrierMediaSink's write path to error deterministically — no real network
// connection, no race against TCP write buffering. Package-local to this
// file rather than added to fakeWSWriter (output_test.go): that fixture's
// whole contract is that Write always succeeds, and every one of its callers
// relies on that.
type failingWSWriter struct{ err error }

func (f *failingWSWriter) Write(context.Context, websocket.MessageType, []byte) error {
	return f.err
}

// TestCarrierMediaSink_WriteFailureNeverDeliversCarrierAudio closes the M6
// gap found by the stage-9 mutation check against telephony/twilio/realtime.go
// (AATK-84): carrierMediaSink.Media and .Clear call deliverCarrierAudio only
// AFTER a successful conn.Write. Mutating them to deliver the record
// unconditionally — regardless of the write error — left the entire suite
// green, because nothing exercised Media/Clear against a carrier write that
// fails.
//
// This constructs carrierMediaSink directly rather than driving it through
// HandleStreamRealtime and a real carrier WebSocket: carrierMediaSink.conn
// is already typed as the unexported wsWriter interface specifically so
// tests can fake it (see output.go's doc comment on wsWriter, and
// output_test.go's fakeWSWriter, which does the same for
// dataPlaneOutput/controlPlaneOutput). HandleStreamRealtime takes the carrier
// connection as a concrete *websocket.Conn, so reaching this path through it
// would mean forcing a real socket write to fail — racy against TCP write
// buffering — or widening HandleStreamRealtime's public signature to accept
// an injectable writer, which is a structural change out of scope here
// (CLAUDE.md #4). Testing carrierMediaSink itself, in-package, needs neither.
func TestCarrierMediaSink_WriteFailureNeverDeliversCarrierAudio(t *testing.T) {
	writeErr := errors.New("carrier write failed")
	ch := make(chan CarrierAudio, 1)
	sink := &carrierMediaSink{
		conn:           &failingWSWriter{err: writeErr},
		streamSID:      "SSfail",
		carrierAudioCh: ch,
	}

	if err := sink.Media(context.Background(), carrierPayloadB64()); !errors.Is(err, writeErr) {
		t.Fatalf("Media: got err %v, want %v", err, writeErr)
	}
	select {
	case rec := <-ch:
		t.Fatalf("Media must not deliver a carrier-audio record when the carrier write failed; got %+v", rec)
	default:
	}

	if err := sink.Clear(context.Background()); !errors.Is(err, writeErr) {
		t.Fatalf("Clear: got err %v, want %v", err, writeErr)
	}
	select {
	case rec := <-ch:
		t.Fatalf("Clear must not deliver a carrier-audio record when the carrier write failed; got %+v", rec)
	default:
	}
}
