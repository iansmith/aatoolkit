package twilio

import (
	"strings"
	"testing"
	"time"
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
// no-op: an empty realtimeURL returns DefaultHandleStream, which never reaches
// the realtime transport, so an option supplied alongside it is ignored rather
// than routing the call. Asserted by the backend seeing no dial at all, which is
// the same signal TestRealtime_SwitchUnsetAttemptsNoDial uses for the switch
// itself.
//
// Deliberately no assertion about how the call ends. On the default path it ends
// with "no VAD factory wired", because this harness has no sidecars — a property
// of the fixture, not of the option, and asserting on it would pin the default
// path's unrelated behaviour inside an options test.
func TestNewStreamHandler_OptionsOnTheDefaultPathAreInert(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	h := newRealtimeHarnessWith(t, NewStreamHandler("", WithIdleTimeout(idleGapTimeout)))
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))

	time.Sleep(idleGapTimeout * 2)

	if got := be.connCount(); got != 0 {
		t.Fatalf("an option on the default path must not construct a realtime client, got %d dial(s)", got)
	}
}
