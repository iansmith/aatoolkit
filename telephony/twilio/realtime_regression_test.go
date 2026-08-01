package twilio

import (
	"bytes"
	"encoding/base64"
	"testing"
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
