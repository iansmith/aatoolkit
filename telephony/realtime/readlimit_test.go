package realtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Tests for AATK-141: a server event larger than coder/websocket's default
// read limit (32 KiB) must reach the read loop intact. A backend echoes the
// whole session in session.updated, so long instructions or many tool
// declarations push that one frame past the default, and exceeding it is
// fatal — the transport closes the connection, ending the call.

// TestRead_SessionUpdatedLargerThanDefaultReadLimit pushes a session.updated
// of about 64 KiB — twice the default limit — through Client.Read after a
// successful Dial, and requires the verbatim frame back.
//
// slopstop:test contract
func TestRead_SessionUpdatedLargerThanDefaultReadLimit(t *testing.T) {
	const defaultReadLimit = 32768 // coder/websocket's default, the limit being lifted
	big := map[string]any{
		"type":    "session.updated",
		"session": map[string]any{"instructions": strings.Repeat("x", 2*defaultReadLimit)},
	}
	want, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= defaultReadLimit {
		t.Fatalf("fixture is %d bytes; it must exceed %d to test anything", len(want), defaultReadLimit)
	}

	be := newFakeBackend(t)
	be.toSend = []any{big}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ev, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read of a %d-byte session.updated: %v", len(want), err)
	}
	if ev.Type != "session.updated" {
		t.Fatalf("Type = %q, want session.updated", ev.Type)
	}
	if !bytes.Equal(ev.Raw, want) {
		t.Fatalf("Raw is %d bytes, want the %d-byte frame verbatim", len(ev.Raw), len(want))
	}
}
