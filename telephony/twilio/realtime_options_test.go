package twilio

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// Option-reachability tests for the realtime path (AATK-78).
//
// AATK-76 delivered the idle bound as a positional parameter on
// HandleStreamRealtime, and NewStreamHandler hardcoded 0 — so the bound existed
// but was unreachable for anyone entering through NewStreamHandler, which is the
// path driver.Config.StreamHandler() takes. These two tests pin the half of the
// contract that was missing rather than wrong: that an option supplied at
// NewStreamHandler actually reaches the call, and that supplying none still
// arms nothing.

// TestNewStreamHandler_WithIdleTimeoutBoundsASilentBackend is the assertion that
// could not be written before options existed. Same silent-backend fixture the
// direct-entry tests use, but entered through NewStreamHandler.
func TestNewStreamHandler_WithIdleTimeoutBoundsASilentBackend(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithIdleTimeout(idleGapTimeout)))

	err := h.waitDone(idleGapTimeout + 500*time.Millisecond)
	if err == nil {
		t.Fatal("an idle timeout supplied to NewStreamHandler must bound a silent backend")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("error must name the idle timeout, got: %v", err)
	}
}

// TestNewStreamHandler_WithNoOptionsLeavesASilentCallRunning pins the other
// half: the zero-option path is the behaviour NewStreamHandler had before this
// existed. A caller who supplies nothing must not acquire a bound by accident.
func TestNewStreamHandler_WithNoOptionsLeavesASilentCallRunning(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url()))

	time.Sleep(idleGapTimeout * 5)

	assertStillRunning(t, h, "NewStreamHandler with no options must arm no timer")
}

// TestNewStreamHandler_OptionsOnTheDefaultPathAreInert pins the documented
// no-op: options supplied alongside an empty realtimeURL, the default, are
// ignored rather than routing the call to a realtime backend. Asserted by the
// backend seeing no connection, the same signal
// TestRealtime_SwitchUnsetAttemptsNoDial uses.
//
// That signal cannot tell an ignored option from a handler that tried to dial
// the empty URL and failed, which reaches no backend either;
// TestNewStreamHandler_EmptyURLEndsTheCallAndDialsNothing counts dials at the
// dialRealtime seam for that. Deliberately no assertion here about how the
// call ends: that is the same test's subject, not an option's.
func TestNewStreamHandler_OptionsOnTheDefaultPathAreInert(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	h := newRealtimeHarnessWith(t, NewStreamHandler("", WithIdleTimeout(idleGapTimeout), WithCallerSilenceTimeout(time.Second)))
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))

	time.Sleep(idleGapTimeout * 2)

	if got := be.connCount(); got != 0 {
		t.Fatalf("an option on the default path must not construct a realtime client, got %d dial(s)", got)
	}
}

// TestNewStreamHandler_EmptyURLEndsTheCallAndDialsNothing pins what an empty
// realtimeURL does to a call — and so what driver.Config{}.StreamHandler()
// does, since it passes an unset RealtimeURL straight through: the handler
// returns a non-nil error promptly, which ends the call, and never reaches the
// realtime dial.
//
// The dial is counted at the dialRealtime seam rather than at a backend,
// because a handler that fell through to the realtime path with an empty URL
// would fail to dial and never reach any backend, so a backend's connection
// count reads zero for that handler too. Nothing is sent after the start
// frame: the call must end on its own, not because carrier traffic arrived.
//
// No t.Parallel: dialRealtime is process-global.
func TestNewStreamHandler_EmptyURLEndsTheCallAndDialsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []RealtimeOption
	}{
		{"no options", nil},
		{"with options", []RealtimeOption{WithIdleTimeout(idleGapTimeout), WithCallerSilenceTimeout(time.Second)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Int32
			orig := dialRealtime
			dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
				dials.Add(1)
				return nil, errors.New("dial refused by the test")
			}
			t.Cleanup(func() { dialRealtime = orig })

			h := newRealtimeHarnessWith(t, NewStreamHandler("", tc.opts...))

			if err := h.waitDone(2 * time.Second); err == nil {
				t.Fatal("an empty realtimeURL must end the call with a non-nil error")
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("an empty realtimeURL must not dial a realtime backend, got %d dial(s)", got)
			}
		})
	}
}

// TestNewStreamHandler_EmptyURLLogsWhyTheCallEnded pins the operator's only
// signal for why a call ended this way. Server.ServeStreams does not log the
// error a StreamHandler returns, so unless the handler itself logs, a
// deployment with no realtime URL ends every call with nothing in the log
// saying why. The CallSid ties the line to the call it ended.
func TestNewStreamHandler_EmptyURLLogsWhyTheCallEnded(t *testing.T) {
	logs := captureLog(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(""))

	if err := h.waitDone(2 * time.Second); err == nil {
		t.Fatal("an empty realtimeURL must end the call with a non-nil error")
	}

	got := logs.String()
	if !strings.Contains(got, "no realtime URL") {
		t.Errorf("log must say the realtime URL is missing, got:\n%s", got)
	}
	if callSID := "CA" + t.Name(); !strings.Contains(got, callSID) {
		t.Errorf("log must name the call (%s), got:\n%s", callSID, got)
	}
}

// --- WithFillerAudio's inert cases (AATK-108) ------------------------------
//
// The filler option carries a value rather than a channel, so its "asked for
// nothing" cases are not a nil channel but a zero field: a Delay of zero, or
// an empty Loop. Neither may write anything to the carrier —
// pinned here, beside the idle timeout's own zero case, rather than in the
// filler suite, because what they pin is the option's reachability and not
// the loop's behaviour.

// TestWithFillerAudio_ZeroDelayWritesNothing pins the documented off switch:
// Delay zero means never start, not start immediately.
func TestWithFillerAudio_ZeroDelayWritesNothing(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Loop: fillerTestLoop()})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	time.Sleep(fillerTestDelay * 3)

	if got := wire(); len(got) != 0 {
		t.Fatalf("a zero Delay must never start the loop, got %+v", got)
	}
}

// TestWithFillerAudio_EmptyLoopWritesNothing pins the other half: a delay with
// nothing to play must not write empty frames, and must not wedge looking for
// a frame in a zero-length ring.
func TestWithFillerAudio_EmptyLoopWritesNothing(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := fillerHarness(t, be.url(), FillerConfig{Delay: fillerTestDelay})
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	armFiller(t, be)
	time.Sleep(fillerTestDelay * 3)

	if got := wire(); len(got) != 0 {
		t.Fatalf("an empty Loop must never write to the carrier, got %+v", got)
	}
	assertStillRunning(t, h, "an empty Loop must not end the call")
}
