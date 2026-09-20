package realtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Additional pins for AATK-85's buildSessionUpdate byte-splice (events.go),
// beyond the frozen substring-based tests in tools_test.go.
//
// tools_test.go proves the declared tools bytes appear *somewhere* in the
// handshake via strings.Contains. That check cannot tell a well-formed
// handshake from one where the splice landed at the wrong brace depth, or
// where the leading separator between the previous field and "tools" went
// missing: both malformed variants still contain the literal substring
// `"tools":<raw>` and so still satisfy a Contains check, even though
// neither produces valid JSON a real backend could parse. These tests
// instead require the full handshake to decode as valid JSON with "tools"
// nested directly under the session object — not merged into a
// neighbouring field, and not attached to sessionUpdate's own top level.

// TestBuildSessionUpdate_ToolsSpliceProducesWellFormedNestedJSON pins that
// the spliced handshake is valid JSON and that tools lands at the correct
// nesting level: a child of "session", not of the top-level message.
func TestBuildSessionUpdate_ToolsSpliceProducesWellFormedNestedJSON(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","name":"lookup_weather"}]`)
	out, err := buildSessionUpdate("be helpful", "nova", "", tools)
	if err != nil {
		t.Fatalf("buildSessionUpdate: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("buildSessionUpdate output is not valid JSON: %s", out)
	}

	var decoded struct {
		Type    string          `json:"type"`
		Tools   json.RawMessage `json:"tools"` // must stay unset: tools belongs on session, not here
		Session struct {
			Type  string          `json:"type"`
			Tools json.RawMessage `json:"tools"`
		} `json:"session"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("buildSessionUpdate output did not decode: %v\n%s", err, out)
	}
	if decoded.Tools != nil {
		t.Fatalf("tools must nest under session, not sessionUpdate's own top level: %s", out)
	}
	if len(decoded.Session.Tools) == 0 {
		t.Fatalf("session.tools must be present: %s", out)
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

// TestBuildSessionUpdate_EmptyNonNilToolsOmitsTheField pins what counts as
// "non-empty" for the tools splice. The frozen tests in tools_test.go cover
// only the two ends of the spectrum WithTools' zero value produces in
// practice: nil (WithTools never called) and a populated declaration. This
// covers the case in between that a `tools == nil` check (rather than
// buildSessionUpdate's actual `len(tools) == 0`) would get wrong: an
// explicitly empty but non-nil json.RawMessage must still omit the field,
// not splice in an empty value.
func TestBuildSessionUpdate_EmptyNonNilToolsOmitsTheField(t *testing.T) {
	out, err := buildSessionUpdate("", "", "", json.RawMessage{})
	if err != nil {
		t.Fatalf("buildSessionUpdate: %v", err)
	}
	if strings.Contains(string(out), `"tools"`) {
		t.Fatalf("an empty (non-nil) tools value must omit the field entirely, got %s", out)
	}
}

// TestBuildSessionUpdate_SessionSpecCannotMarshalEmpty pins the one condition
// the tools splice actually depends on, against the REAL sessionSpec rather
// than a description of it (AATK-136).
//
// The splice strips a fixed two-byte suffix, so it needs the session object
// to have at least one field. If sessionSpec could ever marshal to "{}", base
// would end `"session":{}}`, the suffix check would still pass, and the
// splice would emit `"session":{,"tools":…` — invalid JSON, reported by
// nothing, discovered as a dial that never completes.
//
// What this catches, stated as what it actually catches: sessionSpec having
// NO unconditionally-emitted field. Two fields supply that today and either
// one suffices — Type (a string without omitempty is emitted whatever its
// value) and Audio (a struct, on which omitempty is a no-op). So silencing
// one alone leaves the splice safe and correctly leaves this test GREEN:
// dropping Audio, or putting omitzero on it, passes. Only silencing both —
// omitempty/omitzero on Type together with omitzero on Audio — reds it.
//
// That precision is the point. Three successive versions of
// buildSessionUpdate's comment stated the rule wrongly, and an earlier
// version of THIS comment named two mutations it claimed to catch and did
// not. A test doc that overstates its own reach is the failure it exists to
// prevent.
//
// slopstop:test regression — guards: "sessionSpec must never marshal to an empty object."
func TestBuildSessionUpdate_SessionSpecCannotMarshalEmpty(t *testing.T) {
	// The all-zero spec is the worst case: every omitempty field omitted.
	base, err := json.Marshal(sessionSpec{})
	if err != nil {
		t.Fatalf("marshal sessionSpec: %v", err)
	}
	if string(base) == "{}" {
		t.Fatalf("sessionSpec must never marshal to an empty object — the tools splice emits invalid JSON if it can: %s", base)
	}

	// And prove the consequence end to end rather than trusting the reasoning:
	// an all-zero session still splices to something a backend could parse.
	out, err := buildSessionUpdate("", "", "", json.RawMessage(`[{"name":"x"}]`))
	if err != nil {
		t.Fatalf("buildSessionUpdate: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("splice over an all-zero session produced invalid JSON: %s", out)
	}
}

// TestBuildSessionUpdate_MalformedToolsIsRejected pins that a syntactically
// invalid tools value is reported as an error rather than spliced in blind.
// The splice below is plain byte concatenation with no parser of its own, so
// without this check a truncated or unbalanced fragment would silently
// produce an invalid handshake instead of a local, actionable error.
func TestBuildSessionUpdate_MalformedToolsIsRejected(t *testing.T) {
	_, err := buildSessionUpdate("", "", "", json.RawMessage(`[{"type":"function"`))
	if err == nil {
		t.Fatal("buildSessionUpdate must reject syntactically invalid tools JSON, got nil error")
	}
}
