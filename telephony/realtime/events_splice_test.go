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
	out, err := buildSessionUpdate("be helpful", "nova", tools)
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
	out, err := buildSessionUpdate("", "", json.RawMessage{})
	if err != nil {
		t.Fatalf("buildSessionUpdate: %v", err)
	}
	if strings.Contains(string(out), `"tools"`) {
		t.Fatalf("an empty (non-nil) tools value must omit the field entirely, got %s", out)
	}
}
