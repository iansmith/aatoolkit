package twilio

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestDecodeFrame_PayloadStillDecodesForExistingConsumers pins the guarantee
// that carrying the encoded form alongside changes no existing field: the tap,
// STT dispatch, and replay all read Payload and must see exactly the bytes they
// see today.
//
// Deliberately not part of the Phase 0 red commit — it is green on current code
// by design. It is a regression guard for AATK-70's change to Frame, not a
// description of new behavior, and freezing a green test as the baseline would
// make every downstream diff clean by construction.
func TestDecodeFrame_PayloadStillDecodesForExistingConsumers(t *testing.T) {
	p := carrierPayloadB64()

	f, err := DecodeFrame(mediaFrameRaw("SS1", p))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if len(f.Payload) != oneFrameBytes {
		t.Fatalf("Payload must be %d decoded bytes, got %d", oneFrameBytes, len(f.Payload))
	}
	want, err := base64.StdEncoding.DecodeString(p)
	if err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !bytes.Equal(f.Payload, want) {
		t.Fatal("Payload must equal the base64-decoded carrier payload")
	}
}

// --- AATK-72: the exported realtime entry point -----------------------------
//
// A consumer that supplies telephony.SessionOptions never calls
// NewStreamHandler — it owns its own StreamHandler body so per-call setup and
// teardown stay bound to that scope. These tests enter the way that consumer
// does: HandleStreamRealtime called directly with the (ctx, conn, start) the
// consumer's own handler received. Entering through NewStreamHandler instead
// would leave the exported name itself uncovered, which is the whole ticket.

// realtimeDirect adapts the exported entry point to a StreamHandler, closing
// over the backend URL the way a consumer branching on its own config would.
func realtimeDirect(url string) StreamHandler {
	return func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, url, 0)
	}
}

// TestHandleStreamRealtime_DialFailureEndsCallAndClosesCarrier pins the failure
// guarantee at the exported entry: a backend that refuses connection ends the
// call with an error inside the dial timeout instead of holding the line open,
// and the carrier socket is closed on the way out rather than left dangling.
func TestHandleStreamRealtime_DialFailureEndsCallAndClosesCarrier(t *testing.T) {
	h := newRealtimeHarnessWith(t, realtimeDirect(deadBackendURL(t)))

	if err := h.waitDone(realtimeDialTimeout); err == nil {
		t.Fatal("a backend that refuses connection must end the call with a non-nil error")
	}

	// The carrier connection must be closed, not merely abandoned: a read from
	// the carrier side has to fail once the handler has returned.
	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := h.conn.Read(readCtx); err == nil {
		t.Fatal("handler must close the carrier connection on the dial-failure path")
	}
}

// TestHandleStreamRealtime_StopFrameEndsCallWithNilError pins the hangup path
// at the exported entry. The dial assertion is not incidental: without it a
// handler that did nothing at all would also return nil here, and the test
// would pass while proving nothing about reaching the realtime transport.
func TestHandleStreamRealtime_StopFrameEndsCallWithNilError(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, realtimeDirect(be.url()))

	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))
	waitForAppends(t, be, 1, 5*time.Second)

	if got := be.connCount(); got != 1 {
		t.Fatalf("exported entry must dial the backend exactly once, got %d", got)
	}

	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))

	if err := h.waitDone(5 * time.Second); err != nil {
		t.Fatalf("a stop frame must end the call cleanly, got error: %v", err)
	}
}
