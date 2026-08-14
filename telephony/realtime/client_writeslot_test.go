package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Write-chokepoint tests for AATK-82. Client.Send made AppendAudio's write path
// a shared one, and writeSem is the chokepoint that serialises the two. These
// pin the property that makes the chokepoint safe to put in front of a bounded
// write: acquiring the slot must itself honour the caller's context.
//
// Without that, the slot is an unbounded wait in front of a bounded write, and
// the caller's deadline is silently void — which is fatal on the twilio path,
// where a consumer event is written from HandleStreamRealtime's own select
// loop and a wait that never ends stops backendDone, carrierDone and the idle
// timer from ever being observed.

// TestWriteSlotAcquisition_HonoursCallerContext pins that a caller whose
// context expires while the single write slot is held by somebody else gets
// its deadline back, rather than waiting for the slot holder.
//
// The slot is taken and never released, standing in for the real case
// writeSem's doc comment records: the carrier pump parked inside conn.Write on
// the call's own unbounded context, holding the slot for as long as the
// backend refuses to read.
//
// This is what a plain sync.Mutex could not give — Mutex.Lock cannot be
// cancelled — and it is why the chokepoint is a one-slot channel selected on
// alongside ctx.Done().
//
// slopstop:test regression — guards: "acquiring the write slot honours the caller's context, so a bounded write cannot be preceded by an unbounded wait"
func TestWriteSlotAcquisition_HonoursCallerContext(t *testing.T) {
	// No conn: a Send that correctly gives up on the slot never reaches one,
	// so needing a live connection here would only be able to hide the bug.
	c := &Client{writeSem: make(chan struct{}, 1)}

	// Occupy the slot for the rest of the test and never give it back.
	c.writeSem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Send(ctx, json.RawMessage(`{"type":"response.create"}`))
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a Send that gives up on the write slot must return its context's error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send must abandon the write slot when its context expires; it was still blocked 5s after a 100ms deadline")
	}
}
