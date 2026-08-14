package realtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for AATK-85 ("Declare tools in the realtime handshake and route
// function calls both directions") at the realtime package level: the Tools
// field on sessionSpec (events.go) and the WithTools DialOption (client.go).
// The twilio-level WithTools option and the function-call round trip live in
// telephony/twilio/realtime_tools_test.go.
//
// Tools is carried as json.RawMessage, not a typed struct: this package
// models nothing about what a tool is (out of scope per the ticket), and a
// typed struct would re-encode the consumer's bytes on the way to the wire —
// reordering keys, dropping unknown fields, re-escaping '<', '>', '&'. The
// DoD requires tools to appear "unmodified", so the field's Go type is what
// decides whether that holds.

// declaredToolsRaw is a consumer's tool declaration, deliberately containing
// characters json.Marshal rewrites when it re-encodes a json.RawMessage (<,
// >, &) plus insignificant whitespace, mirroring rawClientEvent's technique
// in client_send_test.go — so a test can tell verbatim wire delivery from a
// re-marshalled approximation.
//
// The shape itself is invented for this test: the real protocol's tool
// declaration schema is modelled nowhere in this codebase, and the ticket
// puts "what a tool is" entirely on the consumer.
const declaredToolsRaw = `[{"type":  "function","name":"lookup_weather","description":"<desc>&more"}]`

// --- declared tools reach the wire unmodified -------------------------------

// TestDial_HandshakeCarriesDeclaredToolsUnmodified pins observable behavior 1:
// a consumer can declare tool definitions in the handshake, and they arrive
// at the backend byte-for-byte.
//
// slopstop:test contract
func TestDial_HandshakeCarriesDeclaredToolsUnmodified(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url(), WithTools(json.RawMessage(declaredToolsRaw)))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	evs := be.events()
	if len(evs) == 0 {
		t.Fatal("backend received no client event")
	}

	got := string(evs[0])
	want := `"tools":` + declaredToolsRaw
	if !strings.Contains(got, want) {
		t.Fatalf("handshake must carry the declared tools unmodified:\n got  %s\nwant substring %s", got, want)
	}
}

// --- unset is byte-identical to today ---------------------------------------

// TestDial_HandshakeWithNoToolsOmitsTheField pins observable behavior 1's
// other half: "Unset omits the field entirely, byte-identical to today."
// Checked structurally (the "tools" key must be entirely absent) rather than
// against a full frozen literal: the frozen-literal form of this same
// requirement lives in telephony/twilio/realtime_instructions_test.go's
// handshakeBaseline, which this package's Dial output feeds directly, and is
// exercised again from that layer by
// TestSessionUpdate_UnsetToolsIsByteIdenticalToToday
// (telephony/twilio/realtime_tools_test.go).
//
// slopstop:test regression — guards: "Unset omits the field entirely, byte-identical to today."
func TestDial_HandshakeWithNoToolsOmitsTheField(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	evs := be.events()
	if len(evs) == 0 {
		t.Fatal("backend received no client event")
	}

	if strings.Contains(string(evs[0]), `"tools"`) {
		t.Fatalf("handshake with no WithTools option must omit the tools field entirely, got %s", evs[0])
	}
}
