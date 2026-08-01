package realtime

import (
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

// G2 (unreachable-backend Dial) was identified by the same pass but is NOT here:
// against the unimplemented stub, Dial errors for every input, so "Dial returns an
// error" asserts what the stub already does — vacuously green, the false-negative
// shape attack vector 5 hunts for. It only becomes falsifiable once Dial can
// succeed, so it is added during implementation rather than frozen as a Phase 0
// baseline that proves nothing.
