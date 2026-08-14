package twilio

import (
	"bytes"
	"context"
	"errors"
	"log"
	"runtime"
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

// TestCarrierAudioChan_NoGoroutineOutlivesACallLeftUndrained closes the gap the
// stage-10b requirements adversary found in the DoD item "No engine goroutine
// outlives a call whose consumer never reads. Differential measurement, as
// AATK-79, and mutation-checked."
//
// The differential measurement in the frozen
// TestCarrierAudioChan_NoEngineGoroutineOutlivesTheCall is real and its control
// mutation is correctly caught — an unconditionally parked goroutine shows up as
// 15 goroutines over 5 calls against 10 with no consumer. But it is blind to the
// one leak shape this ticket was rewritten to prevent. Its comment claims "the
// consumer is present and permanently behind after its proof read, which is the
// worst case"; the test does not build that. It emits exactly one record, reads
// it, and stops the call, so the channel is EMPTY when the call ends and no send
// is ever left outstanding. Injecting the per-record leak —
//
//	go func() { s.carrierAudioCh <- rec }()
//
// into deliverCarrierAudio leaves that test green, because with nothing pending
// there is nothing for the spawned goroutine to block on.
//
// This test constructs what that comment describes: after the proof read it
// drives further records and never drains them again, so the buffer is full and
// subsequent sends have nowhere to go at the moment the call is stopped. Under
// the correct implementation those sends take the non-blocking default branch
// and are dropped, costing nothing. Under the per-record-goroutine leak each one
// parks a goroutine that outlives the call — which is precisely the hazard the
// ticket's "why not a wrapper" rationale exists to prevent, and the reason
// observation is a channel rather than a synchronous MediaSink wrapper.
//
// The frozen test is not edited; this is a pure addition beside it.
//
// slopstop:test non-interference — paired: the positive half is the proof read,
// which fails first if the option is not genuinely wired, so the goroutine
// measurement cannot pass vacuously against an inert implementation; the
// negative half is that a call whose consumer stopped reading, with records
// still outstanding, costs no goroutine once it ends
func TestCarrierAudioChan_NoGoroutineOutlivesACallLeftUndrained(t *testing.T) {
	const calls = 5
	// Beyond the proof read, and never drained. Buffer 1 absorbs the first;
	// every one after it has nowhere to go at the moment the call is stopped.
	const undrained = 6

	run := func(withConsumer bool) int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		before := runtime.NumGoroutine()

		for i := 0; i < calls; i++ {
			be := newFakeRealtimeBackend(t)
			var opts []RealtimeOption
			var ch chan CarrierAudio
			if withConsumer {
				ch = make(chan CarrierAudio, 1)
				opts = append(opts, WithCarrierAudioChan(ch))
			}
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
			waitBackendReady(t, be, h)
			seen := h.countMediaFrames(t)

			if withConsumer {
				// Positive: the channel is genuinely wired. Drain exactly once.
				be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("consumer never received a carrier-audio record")
				}
			}

			// Negative: keep the engine sending while nothing drains ch again.
			for j := 0; j < undrained; j++ {
				be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
			}
			// Wait for the carrier to have received them, so the sink really ran
			// for each one before the call is stopped — otherwise the call could
			// end before the sends this test is measuring ever happened.
			want := undrained
			if withConsumer {
				want++ // the proof-read frame reached the carrier too
			}
			waitFor(t, 5*time.Second, func() bool { return seen() >= want })

			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			_ = h.waitDone(5 * time.Second)
		}

		// Poll rather than trust one instantaneous reading: a goroutine that is
		// exiting correctly may not have been descheduled yet.
		deadline := time.After(3 * time.Second)
		growth := runtime.NumGoroutine() - before
		for {
			select {
			case <-deadline:
				return growth
			default:
			}
			runtime.GC()
			time.Sleep(25 * time.Millisecond)
			if g := runtime.NumGoroutine() - before; g < growth {
				growth = g
			}
		}
	}

	withConsumer := run(true)
	withNone := run(false)

	if withConsumer > withNone {
		t.Fatalf("a call left with %d undrained carrier-audio records must cost no goroutines once it ends: "+
			"%d over %d calls, against %d with no consumer", undrained, withConsumer, calls, withNone)
	}
}
