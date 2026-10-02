package realtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Tests for AATK-141: a server event over coder/websocket's default read
// limit must reach Client.Read intact. See readLimit for why.

// TestRead_SessionUpdatedLargerThanDefaultReadLimit pushes a ~64 KiB
// session.updated — twice the default limit — through Client.Read after a
// successful Dial, and requires the verbatim frame back.
//
// slopstop:test contract
func TestRead_SessionUpdatedLargerThanDefaultReadLimit(t *testing.T) {
	const defaultReadLimit = 32768 // coder/websocket's default, the limit being lifted
	frame := []byte(`{"type":"session.updated","session":{"instructions":"` +
		strings.Repeat("x", 2*defaultReadLimit) + `"}}`)

	be := newFakeBackend(t)
	be.toSend = []any{json.RawMessage(frame)}
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ev, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read of a %d-byte session.updated: %v", len(frame), err)
	}
	if ev.Type != "session.updated" {
		t.Fatalf("Type = %q, want session.updated", ev.Type)
	}
	if !bytes.Equal(ev.Raw, frame) {
		t.Fatalf("Raw is %d bytes, want the %d-byte frame verbatim", len(ev.Raw), len(frame))
	}
}
