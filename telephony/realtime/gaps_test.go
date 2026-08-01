package realtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Phase 0 adversary gap tests (plan Step 0f). Both are grounded in the ticket's
// own wording rather than new contract.

// G1 — Behavior 2 says "Each inbound carrier media frame is forwarded as one
// input_audio_buffer.append": a 1:1 mapping that preserves order. A single-frame
// test cannot distinguish that from a bridge which batches, drops, or reorders.
func TestEachFrameBecomesOneAppendInOrder(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	b := NewBridge(c, &recordingSink{})

	// Distinct payloads so order is observable, each a valid carrier frame.
	payloads := []string{frameB64(), "AAAA", "BBBB", frameB64(), "CCCC"}
	for i, p := range payloads {
		if err := b.Forward(ctx, p); err != nil {
			t.Fatalf("Forward(%d): %v", i, err)
		}
	}

	var appends []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		appends = appends[:0]
		for _, raw := range be.events() {
			var ev struct {
				Type  string `json:"type"`
				Audio string `json:"audio"`
			}
			if json.Unmarshal(raw, &ev) == nil && ev.Type == EventAudioAppend {
				appends = append(appends, ev.Audio)
			}
		}
		if len(appends) >= len(payloads) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(appends) != len(payloads) {
		t.Fatalf("got %d appends for %d frames, want one each", len(appends), len(payloads))
	}
	for i := range payloads {
		if appends[i] != payloads[i] {
			t.Errorf("append %d = %q, want %q (order and verbatim payload)", i, appends[i], payloads[i])
		}
	}
}

// G2 — the DoD requires failures to surface "rather than blocking, panicking, or
// silently reconnecting". Mid-session close is covered by the Phase 0 suite; a
// backend that was never reachable is not, and a Dial that hands back a
// usable-looking Client defers the error to the first frame of a live call.
//
// Added during implementation, not frozen at Phase 0: against the stub, Dial
// errored for every input, so this assertion was vacuously green — the
// false-negative shape attack vector 5 exists to catch. It is only falsifiable
// once Dial can succeed, which is now.
func TestDialAgainstUnreachableBackendReturnsError(t *testing.T) {
	// A well-formed address with nothing listening: stand a backend up to claim
	// a port, then close it before dialling.
	be := newFakeBackend(t)
	url := be.url()
	be.srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c, err := Dial(ctx, url)
	if err == nil {
		if c != nil {
			_ = c.Close()
		}
		t.Fatal("Dial against an unreachable backend returned nil error")
	}
	if c != nil {
		t.Errorf("Dial returned a non-nil Client alongside an error: %+v", c)
	}
}
