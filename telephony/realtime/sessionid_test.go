package realtime

import (
	"encoding/json"
	"reflect"
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

// TestDial_HandshakeWithNoSessionIDOmitsTheField pins the regression half of
// observable behavior 2 at this layer: a consumer that never calls
// WithSessionID must get the handshake this engine sent before the field
// existed. Checked structurally here; the frozen-literal form of the same
// requirement is at the twilio layer, mirroring how
// TestDial_HandshakeWithNoToolsOmitsTheField splits with
// TestSessionUpdate_UnsetToolsIsByteIdenticalToToday (tools_test.go).
//
// slopstop:test regression — guards: "No option produces a handshake with no such field."
func TestDial_HandshakeWithNoSessionIDOmitsTheField(t *testing.T) {
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

	if strings.Contains(string(evs[0]), `"client_session_id"`) {
		t.Fatalf("handshake with no WithSessionID option must omit the field entirely, got %s", evs[0])
	}
}

// TestDial_HandshakeWithEmptySessionIDOmitsTheField pins the other half of
// observable behavior 2: an explicitly empty identifier is the same case as
// no option at all. This is the case a field without `omitempty` would get
// wrong — it would emit "client_session_id":"" and change every handshake of
// a consumer that passes a sometimes-empty value.
//
// slopstop:test regression — guards: "An empty string produces a handshake with no such field."
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
// in exactly "}}" — sessionSpec's close followed by sessionUpdate's. An
// omitempty field added to sessionSpec keeps that true whether it is emitted
// or not, but only as long as it is not the last field to marshal into
// something that changes the tail. This requires the whole document to be
// valid JSON, both values to sit under "session", and the tools bytes to
// still appear verbatim.
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
		ClientSessionID string          `json:"client_session_id"` // must stay unset: it belongs on session
		Tools           json.RawMessage `json:"tools"`             // likewise
		Session         struct {
			ClientSessionID string          `json:"client_session_id"`
			Tools           json.RawMessage `json:"tools"`
		} `json:"session"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("handshake did not decode: %v\n%s", err, out)
	}
	if decoded.ClientSessionID != "" {
		t.Fatalf("client_session_id must nest under session, not sessionUpdate's own top level: %s", out)
	}
	if decoded.Tools != nil {
		t.Fatalf("tools must nest under session, not sessionUpdate's own top level: %s", out)
	}
	if decoded.Session.ClientSessionID != sessionIDRaw {
		t.Fatalf("session.client_session_id = %q, want %q\nhandshake: %s",
			decoded.Session.ClientSessionID, sessionIDRaw, out)
	}

	var got, want interface{}
	if err := json.Unmarshal(decoded.Session.Tools, &got); err != nil {
		t.Fatalf("session.tools did not decode: %v", err)
	}
	if err := json.Unmarshal(tools, &want); err != nil {
		t.Fatalf("test fixture did not decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session.tools decoded to %#v, want %#v", got, want)
	}
}
