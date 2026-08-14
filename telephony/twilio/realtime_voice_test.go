package twilio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Voice-selection tests for the realtime handshake (AATK-83).
//
// AATK-74 fenced out every session field but Instructions with one stated
// reason: a general passthrough invites a consumer to reconfigure
// turn-taking, which the engine has opinions about. Voice does not touch
// turn-taking, so this ticket lifts the fence for voice alone and leaves it
// standing for everything else.
//
// These assert on the bytes the backend actually received, mirroring
// realtime_instructions_test.go's discipline: a backend is free to
// distinguish an absent field from an empty one, and — the risk specific to
// this ticket — free to distinguish the input side of the audio spec from
// the output side. newSessionUpdate builds ONE audioChannel literal today and
// assigns it to both Input and Output (events.go), so a Voice field added
// carelessly to that type can leak onto the input side.
//
// handshakeBaseline and waitHandshakes are defined in
// realtime_instructions_test.go (AATK-74), same package — reused here rather
// than redeclared, since the "byte-identical to today" claim is only
// meaningful against the one frozen literal.

// audioSpec decodes only the two keys these tests need to tell apart: the
// input and output sides of the session's audio object. It exists so the
// input/output distinction can be checked precisely without depending on
// whatever field order the real implementation ends up picking, and without
// referencing telephony/realtime's unexported sessionSpec type (a different
// package, and the type this ticket's own regression risk warns against
// asserting against). This decodes the actual bytes the backend received; it
// is not a value the code under test computes.
type audioSpec struct {
	Session struct {
		Audio struct {
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
		} `json:"audio"`
	} `json:"session"`
}

// slopstop:test contract
func TestSessionUpdate_CarriesSuppliedVoice(t *testing.T) {
	const want = "marin"

	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(), WithVoice(want))
	})

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	if !strings.Contains(got, `"voice":"`+want+`"`) {
		t.Fatalf("handshake must carry the supplied voice unmodified, got %s", got)
	}
}

// slopstop:test contract
func TestSessionUpdate_CarriesVoiceThroughNewStreamHandler(t *testing.T) {
	const want = "alloy"

	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoice(want)))

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	if !strings.Contains(got, `"voice":"`+want+`"`) {
		t.Fatalf("NewStreamHandler must forward voice to the handshake, got %s", got)
	}
}

// slopstop:test non-interference — paired: asserts the output side of the audio spec carries the supplied voice
func TestSessionUpdate_VoiceAppearsOnlyOnOutputChannel(t *testing.T) {
	const want = "marin"

	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoice(want)))

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	var parsed audioSpec
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("handshake did not decode as JSON: %v\nraw: %s", err, got)
	}

	// Positive half: the pairing this non-interference test needs so an
	// empty stub cannot satisfy it. A stub that stores voice and never
	// forwards it (today's stub) fails here — nothing reads the option.
	if !strings.Contains(string(parsed.Session.Audio.Output), `"voice":"`+want+`"`) {
		t.Fatalf("output side of the audio spec must carry the supplied voice, got %s", parsed.Session.Audio.Output)
	}
	// Negative half: the risk this test exists for. newSessionUpdate builds
	// one audioChannel literal and assigns it to both Input and Output
	// (events.go) — a Voice field set on that shared literal before the
	// split would leak onto input too, which is a protocol correctness bug
	// no struct-level test would catch (Input and Output share a Go type).
	if strings.Contains(string(parsed.Session.Audio.Input), `"voice"`) {
		t.Fatalf("input side of the audio spec must NOT carry voice — a shared literal leaked it onto input, got %s", parsed.Session.Audio.Input)
	}
}

// slopstop:test regression — guards: "Unset, or empty, omits the field entirely — the handshake is byte-identical to one with no option."
func TestSessionUpdate_WithNoVoiceIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url()))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with no voice option must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}

// slopstop:test regression — guards: "Unset, or empty, omits the field entirely — the handshake is byte-identical to one with no option."
func TestSessionUpdate_WithEmptyVoiceIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoice("")))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with an explicitly empty voice must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}
