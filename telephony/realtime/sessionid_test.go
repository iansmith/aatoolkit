package realtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for AATK-136 ("let a consumer put its own session identifier in the
// handshake") at the realtime package level: the ClientSessionID field on
// sessionSpec (events.go) and the WithSessionID DialOption (client.go). The
// twilio-level WithSessionID option and its round trip to this layer live in
// telephony/twilio/realtime_sessionid_test.go.
//
// Unlike tools, the identifier IS an ordinary struct field marshalled by
// encoding/json rather than a byte splice. The splice exists because the
// encoder rewrites raw JSON bytes (see buildSessionUpdate's doc comment); a
// string carries no such hazard, and the escaping the encoder applies to a
// string is exactly correct string marshalling. So these tests assert the
// value arrives unmodified, not that it bypassed the encoder.

// sessionIDRaw is a consumer-minted identifier. It is a plain opaque token
// rather than one containing '<', '>' or '&': this engine does not promise a
// string field escapes the encoder untouched, and asserting it did would pin
// behaviour the ticket explicitly says not to copy from tools.
const sessionIDRaw = "sess-01HQZX-abc123"

// --- observable behavior 1: a supplied identifier reaches the handshake ------

// TestDial_HandshakeCarriesSuppliedSessionID pins observable behavior 1: the
// consumer's identifier reaches the session object carrying exactly that
// value.
//
// slopstop:test contract
func TestDial_HandshakeCarriesSuppliedSessionID(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url(), WithSessionID(sessionIDRaw))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	evs := be.events()
	if len(evs) == 0 {
		t.Fatal("backend received no client event")
	}

	var decoded struct {
		Session struct {
			ClientSessionID string `json:"client_session_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(evs[0], &decoded); err != nil {
		t.Fatalf("handshake did not decode: %v\n%s", err, evs[0])
	}
	if decoded.Session.ClientSessionID != sessionIDRaw {
		t.Fatalf("session.client_session_id = %q, want %q\nhandshake: %s",
			decoded.Session.ClientSessionID, sessionIDRaw, evs[0])
	}
}

// --- observable behavior 2: absent and empty both omit the field ------------

// TestDial_HandshakeWithEmptySessionIDOmitsTheField pins observable
// behavior 2 at this layer. It covers BOTH halves the ticket names — "no
// option" and "an empty string" — because they are one code path: the option
// assigns the zero value either way, so a separate no-option test could only
// fail when this one already has. This is the stronger of the two, since it
// also exercises the option constructor.
//
// This is the case a field without `omitempty` would get wrong — it would
// emit "client_session_id":"" and change every handshake of a consumer that
// passes a sometimes-empty value. The frozen-literal form of the same
// requirement is at the twilio layer, mirroring how
// TestDial_HandshakeWithNoToolsOmitsTheField (tools_test.go, this package)
// splits with TestSessionUpdate_UnsetToolsIsByteIdenticalToToday
// (telephony/twilio/realtime_tools_test.go).
//
// slopstop:test regression — guards: "No option, or an empty string, produces a handshake with no such field."
func TestDial_HandshakeWithEmptySessionIDOmitsTheField(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url(), WithSessionID(""))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	evs := be.events()
	if len(evs) == 0 {
		t.Fatal("backend received no client event")
	}

	if strings.Contains(string(evs[0]), `"client_session_id"`) {
		t.Fatalf("handshake with an explicitly empty session ID must omit the field entirely, got %s", evs[0])
	}
}

// --- observable behavior 3: it composes with the tools splice ---------------

// TestBuildSessionUpdate_SessionIDComposesWithToolsSplice pins observable
// behavior 3, and it is the case most likely to be broken by a careless
// implementation: the identifier is marshalled by encoding/json while tools
// is concatenated in afterwards by buildSessionUpdate's byte splice, so the
// two mechanisms meet inside one document.
//
// The splice finds its insertion point by asserting the marshalled bytes end
// in exactly "}}" — sessionSpec's close followed by sessionUpdate's. Adding a
// field to sessionSpec cannot disturb that, wherever it sits and whether or
// not it is emitted: the two closing braces are the objects' own, not any
// field's. See buildSessionUpdate for the two conditions that CAN break the
// splice, and TestBuildSessionUpdate_SessionSpecCannotMarshalEmpty for the
// one of them a test can reach.
//
// This test requires the whole document to be valid JSON, both values to sit
// under "session", and the tools bytes to still appear verbatim.
//
// slopstop:test contract
func TestBuildSessionUpdate_SessionIDComposesWithToolsSplice(t *testing.T) {
	tools := json.RawMessage(declaredToolsRaw)
	out, err := buildSessionUpdate("be helpful", "nova", sessionIDRaw, tools)
	if err != nil {
		t.Fatalf("buildSessionUpdate: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("handshake carrying both a session ID and spliced tools is not valid JSON: %s", out)
	}

	// The tools bytes must still reach the wire unmodified — the whole reason
	// the splice exists. A session ID marshalled beside them must not have
	// moved them through the encoder.
	if want := `"tools":` + declaredToolsRaw; !strings.Contains(string(out), want) {
		t.Fatalf("spliced tools must survive verbatim alongside a session ID:\n got  %s\nwant substring %s", out, want)
	}

	var decoded struct {
		// Tools must stay unset here, and this check is reachable: a splice
		// that strips ONE brace and appends one — a coherent mis-edit, not a
		// typo — lands tools at sessionUpdate's top level in output that is
		// still valid JSON and still contains the substring checked above.
		// Measured: that mutation reds this assertion and nothing else in
		// either splice test. There is deliberately no matching check for
		// client_session_id: it is an ordinary struct field on sessionSpec, so
		// encoding/json cannot emit it anywhere but inside "session", and such
		// a check could never fail.
		Tools   json.RawMessage `json:"tools"`
		Session struct {
			ClientSessionID string          `json:"client_session_id"`
			Tools           json.RawMessage `json:"tools"`
		} `json:"session"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("handshake did not decode: %v\n%s", err, out)
	}
	if decoded.Tools != nil {
		t.Fatalf("tools must nest under session, not sessionUpdate's own top level: %s", out)
	}
	if len(decoded.Session.Tools) == 0 {
		t.Fatalf("session.tools must be present alongside the session ID: %s", out)
	}
	if decoded.Session.ClientSessionID != sessionIDRaw {
		t.Fatalf("session.client_session_id = %q, want %q\nhandshake: %s",
			decoded.Session.ClientSessionID, sessionIDRaw, out)
	}
}
