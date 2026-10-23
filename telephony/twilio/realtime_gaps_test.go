package twilio

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Adversary gap tests for the AATK-70 wiring (Phase 0, Step 0f). Each covers a
// case the first pass left open; fixtures come from realtime_wiring_test.go.

// TestRealtime_EachCarrierFrameBecomesOneAppendInOrder is the hard case the
// single-frame test leaves open: the bridge's contract is one frame in, one
// append out, with no batching and no reordering. A wiring that coalesced or
// resequenced frames would still pass a one-frame test.
func TestRealtime_EachCarrierFrameBecomesOneAppendInOrder(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())

	// Distinct payloads so ordering is observable, each a full 20 ms frame.
	want := []string{
		encodeFrameOf(t, 0x01),
		encodeFrameOf(t, 0x02),
		encodeFrameOf(t, 0x03),
	}
	for _, p := range want {
		h.sendRaw(mediaFrameRaw(h.streamSID, p))
	}

	got := waitForAppends(t, be, len(want), 5*time.Second)
	if len(got) != len(want) {
		t.Fatalf("want %d appends, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("append %d must be the carrier payload verbatim:\n got %q\nwant %q",
				i, got[i], want[i])
		}
	}
}

// TestRealtime_StopFrameEndsCall covers the control plane on the realtime path.
// The happy-path tests only ever send media, so a wiring that routed media
// correctly but never terminated on a stop frame would pass all of them and
// hang a real call at hangup.
func TestRealtime_StopFrameEndsCall(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())

	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))
	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))

	if err := h.waitDone(5 * time.Second); err != nil {
		t.Fatalf("a stop frame must end the call cleanly, got error: %v", err)
	}
}

// TestRealtime_SwitchSetDialsExactlyOnce is the other half of
// TestRealtime_SwitchUnsetAttemptsNoDial. On its own, the unset test is
// vacuous: with no URL the harness could not reach that backend however the
// switch behaved, so a zero count proves nothing about the switch. This pins
// that the same stub does record a connection when the switch IS set, which is
// what makes the pair evidence rather than a tautology.
func TestRealtime_SwitchSetDialsExactlyOnce(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())

	// Drive one frame so the session is unambiguously live before counting.
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 1, 5*time.Second)

	if got := be.connCount(); got != 1 {
		t.Fatalf("switch set must dial the backend exactly once, got %d", got)
	}
}

// encodeFrameOf builds one full 20 ms G.711 frame of a single repeated byte,
// base64-encoded as the carrier would deliver it.
func encodeFrameOf(t *testing.T, b byte) string {
	t.Helper()
	buf := make([]byte, oneFrameBytes)
	for i := range buf {
		buf[i] = b
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// waitForAppends polls until n appends have arrived or the deadline passes,
// failing the test rather than returning a short slice a caller might misread
// as an ordering failure.
func waitForAppends(t *testing.T, be *fakeRealtimeBackend, n int, d time.Duration) []string {
	t.Helper()
	got, err := awaitAppends(t, be, n, d)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// awaitAppends is waitForAppends' poll loop, returning the failure instead of
// failing t, so a test can read the message. t is only the key that finds the
// harness's recorded handler errors.
func awaitAppends(t *testing.T, be *fakeRealtimeBackend, n int, d time.Duration) ([]string, error) {
	deadline := time.After(d)
	for {
		if got := be.appendedAudio(); len(got) >= n {
			return got, nil
		}
		select {
		case <-deadline:
			return nil, errors.New(appendWaitFailure(len(be.appendedAudio()), n, be.connCount(), harnessHandlerErrs(t)))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// appendWaitFailure builds awaitAppends' failure text from what the backend and
// the harness's handler observed.
func appendWaitFailure(got, want, conns int, errs []error) string {
	return fmt.Sprintf("backend received %d of %d expected appends before deadline", got, want)
}
