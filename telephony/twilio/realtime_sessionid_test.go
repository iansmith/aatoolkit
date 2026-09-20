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

// TestSessionUpdate_SessionIDVariesPerCallThroughOneHandler is the assertion
// a string-only option cannot satisfy, and the reason WithSessionIDFor
// exists. It mirrors TestSessionUpdate_InstructionsVaryPerCallThroughOneHandler
// exactly, because the hazard is the same one.
//
// NewStreamHandler binds its options at construction and reuses them for
// every call it serves, so a plain per-handler string is per-PROCESS on that
// path. For a session identifier that is the precise failure the field exists
// to prevent: two concurrent callers would both announce the same session,
// and a backend keying per-session state on it would serve one caller's
// context to the other. Two calls through ONE handler must carry different
// identifiers.
//
// slopstop:test contract
func TestSessionUpdate_SessionIDVariesPerCallThroughOneHandler(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	calls := 0
	handler := NewStreamHandler(be.url(), WithSessionIDFor(func(start Frame) string {
		calls++
		if calls == 1 {
			return "sess-first"
		}
		return "sess-second"
	}))

	newRealtimeHarnessWith(t, handler)
	waitHandshakes(t, be, 1, 5*time.Second)
	newRealtimeHarnessWith(t, handler)
	waitHandshakes(t, be, 2, 5*time.Second)

	first, second := be.handshake(0), be.handshake(1)
	if !strings.Contains(first, `"client_session_id":"sess-first"`) {
		t.Fatalf("first call must carry its own session ID, got %s", first)
	}
	if !strings.Contains(second, `"client_session_id":"sess-second"`) {
		t.Fatalf("second call through the SAME handler must carry its own session ID, got %s", second)
	}
}

// --- observable behavior 2, frozen-literal form -----------------------------

// TestSessionUpdate_WithEmptySessionIDIsByteIdenticalToToday is the ticket's
// named regression guard at this layer: it is what keeps every existing
// consumer's handshake unchanged. It mirrors
// TestSessionUpdate_WithEmptyVoiceIsByteIdenticalToToday — an explicitly
// empty identifier must produce the same bytes as no option at all, not a
// near-equivalent carrying "".
//
// There is deliberately no no-option twin here. Passing no option at all
// would dial NewStreamHandler(be.url()) with nothing set, which is
// character-for-character what
// TestSessionUpdate_UnsetToolsIsByteIdenticalToToday,
// TestSessionUpdate_WithNoVoiceIsByteIdenticalToToday and
// TestSessionUpdate_WithNoInstructionsIsByteIdenticalToToday already do
// against this same frozen literal: a fourth copy could never fail alone, and
// would give handshakeBaseline a fourth owner to update.
//
// slopstop:test regression — guards: "No option, or an empty string, produces a handshake byte-identical to today's."
func TestSessionUpdate_WithEmptySessionIDIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithSessionID("")))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with an explicitly empty session ID must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}
