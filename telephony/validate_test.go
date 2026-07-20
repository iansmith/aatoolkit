package telephony

import "testing"

// TestValidateVAD_RealModel asserts the startup self-test passes against the
// real embedded model.
func TestValidateVAD_RealModel(t *testing.T) {
	if err := ValidateVAD(); err != nil {
		t.Errorf("ValidateVAD() with real model: got error %v, want nil", err)
	}
}

// TestValidateVAD_CorruptedModel asserts the self-test fails hard (rather
// than panicking or silently succeeding) when the detector can't be
// constructed — the seam Main relies on to refuse to start with a broken VAD.
func TestValidateVAD_CorruptedModel(t *testing.T) {
	corrupted := func() (vadDetector, error) {
		return newSileroDetectorFromBytes(nil)
	}
	if err := validateVAD(corrupted); err == nil {
		t.Error("validateVAD with a zero-byte model: got nil error, want non-nil")
	}
}
