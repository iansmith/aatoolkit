package twilio

import (
	"strings"
	"testing"
	"time"
)

// Tests for AATK-136 ("let a consumer put its own session identifier in the
// handshake") at the twilio wiring layer: WithSessionID
// (telephony/twilio/realtime.go) and the round trip it takes to the client
// layer's handshake. The client-layer option and the composition with the
// tools splice live in telephony/realtime/sessionid_test.go.
//
// handshakeBaseline and waitHandshakes are defined in
// realtime_instructions_test.go; these tests reuse that same frozen literal
// rather than freezing a second copy of it.

// wiredSessionID is the consumer-minted identifier these tests send through
// the wiring layer. A distinct literal from the client layer's, so a failure
// message names which layer it came from.
const wiredSessionID = "sess-twilio-7f3a"

// --- observable behavior 4: the wiring layer's option reaches the handshake --

// TestSessionUpdate_CarriesSuppliedSessionID pins observable behavior 4: the
// twilio-layer option must reach the client-layer handshake, the same round
// trip TestSessionUpdate_CarriesDeclaredToolsUnmodified
// (realtime_tools_test.go) pins for tools. WithSessionID existing at this
// layer proves nothing until HandleStreamRealtime actually passes it to
// realtime.Dial.
//
// slopstop:test contract
func TestSessionUpdate_CarriesSuppliedSessionID(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithSessionID(wiredSessionID)))

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	want := `"client_session_id":"` + wiredSessionID + `"`
	if !strings.Contains(got, want) {
		t.Fatalf("handshake must carry the supplied session ID:\n got  %s\nwant substring %s", got, want)
	}
}

// --- observable behavior 2, frozen-literal form -----------------------------

// TestSessionUpdate_WithNoSessionIDIsByteIdenticalToToday is the ticket's
// named regression guard: it is what keeps every existing consumer's
// handshake unchanged. Pinned against the SAME frozen literal
// TestSessionUpdate_UnsetToolsIsByteIdenticalToToday and
// TestSessionUpdate_WithNoVoiceIsByteIdenticalToToday guard, so a field that
// leaks into the handshake fails all three rather than only the new one.
//
// slopstop:test regression — guards: "No option produces a handshake byte-identical to today's."
func TestSessionUpdate_WithNoSessionIDIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url()))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with no session ID must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}

// TestSessionUpdate_WithEmptySessionIDIsByteIdenticalToToday pins the other
// half at this layer, mirroring
// TestSessionUpdate_WithEmptyVoiceIsByteIdenticalToToday: an explicitly empty
// identifier must produce the same bytes as no option at all, not a
// near-equivalent carrying "".
//
// slopstop:test regression — guards: "An empty string produces a handshake byte-identical to today's."
func TestSessionUpdate_WithEmptySessionIDIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithSessionID("")))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with an explicitly empty session ID must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}
