package twilio

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Session-instruction tests for the realtime handshake (AATK-74).
//
// On this path the backend owns the conversation and today it is given nothing
// to own it with: newSessionUpdate carries Type and Audio and nothing else, so
// every call is answered by whatever persona the backend defaults to.
//
// These assert on the bytes the backend actually received, not on a struct.
// What goes on the wire is the contract, and a backend that distinguishes an
// absent field from an empty one is not hypothetical.

// handshakeBaseline is the exact session.update this engine sends today, with
// no instructions supplied. Frozen here as a literal: the "unsupplied is
// byte-identical to today" requirement is meaningless if the expected value is
// computed by the same code under test.
const handshakeBaseline = `{"type":"session.update","session":{"type":"realtime","audio":{"input":{"format":{"type":"audio/pcmu"}},"output":{"format":{"type":"audio/pcmu"}}}}}`

// waitHandshakes blocks until the backend has accepted n handshakes.
func waitHandshakes(t *testing.T, be *fakeRealtimeBackend, n int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if be.handshakeCount() >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("backend received %d handshake(s), want %d", be.handshakeCount(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSessionUpdate_WithNoInstructionsIsByteIdenticalToToday pins the
// no-regression half. An option that defaults to an empty field rather than an
// absent one would pass a struct comparison and fail this.
func TestSessionUpdate_WithNoInstructionsIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url()))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with no instructions must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}

// TestSessionUpdate_CarriesSuppliedInstructions covers the direct entry point —
// a consumer that owns its own handler body.
func TestSessionUpdate_CarriesSuppliedInstructions(t *testing.T) {
	const want = "You are a laconic dispatcher."

	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, be.url(), WithInstructions(want))
	})

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	if !strings.Contains(got, `"instructions":"`+want+`"`) {
		t.Fatalf("handshake must carry the supplied instructions unmodified, got %s", got)
	}
}

// TestSessionUpdate_CarriesInstructionsThroughNewStreamHandler covers the other
// entry point. Both must reach it; solving only one is called out in the ticket.
func TestSessionUpdate_CarriesInstructionsThroughNewStreamHandler(t *testing.T) {
	const want = "You are a terse receptionist."

	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithInstructions(want)))

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	if !strings.Contains(got, `"instructions":"`+want+`"`) {
		t.Fatalf("NewStreamHandler must forward instructions to the handshake, got %s", got)
	}
}

// TestSessionUpdate_InstructionsVaryPerCallThroughOneHandler is the assertion
// the ticket exists to force, and the one a string-only option cannot satisfy.
//
// NewStreamHandler binds its options at construction and reuses them for every
// call, so a plain per-handler string is per-*process* on that path. The text
// has to be resolved when the call arrives. Two calls through ONE handler must
// therefore carry different instructions.
func TestSessionUpdate_InstructionsVaryPerCallThroughOneHandler(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	calls := 0
	handler := NewStreamHandler(be.url(), WithInstructionsFor(func(start Frame) string {
		calls++
		if calls == 1 {
			return "first caller"
		}
		return "second caller"
	}))

	newRealtimeHarnessWith(t, handler)
	waitHandshakes(t, be, 1, 5*time.Second)
	newRealtimeHarnessWith(t, handler)
	waitHandshakes(t, be, 2, 5*time.Second)

	first, second := be.handshake(0), be.handshake(1)
	if !strings.Contains(first, `"instructions":"first caller"`) {
		t.Fatalf("first call must carry its own instructions, got %s", first)
	}
	if !strings.Contains(second, `"instructions":"second caller"`) {
		t.Fatalf("second call through the SAME handler must carry its own instructions, got %s", second)
	}
}
