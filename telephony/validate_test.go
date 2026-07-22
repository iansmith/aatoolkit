package telephony

import (
	"context"
	"errors"
	"testing"
)

// probeDetector is a minimal vadDetector for exercising the self-test without a
// live sidecar: Detect returns a fixed probability, Reset is a no-op.
type probeDetector struct{ prob float32 }

func (d probeDetector) Detect(context.Context, []float32) (float32, error) { return d.prob, nil }
func (d probeDetector) Reset()                                             {}

// TestValidateVAD_HealthyDetector asserts the startup self-test passes when the
// injected factory yields a detector that returns a plausible probability.
func TestValidateVAD_HealthyDetector(t *testing.T) {
	factory := func() (VADDetector, error) { return probeDetector{prob: 0.42}, nil }
	if err := ValidateVAD(factory); err != nil {
		t.Errorf("ValidateVAD with a healthy detector: got error %v, want nil", err)
	}
}

// TestValidateVAD_ConstructionFailure asserts the self-test fails hard (rather
// than panicking or silently succeeding) when the factory can't build a
// detector — the seam a consumer relies on to refuse to start with a broken VAD.
func TestValidateVAD_ConstructionFailure(t *testing.T) {
	factory := func() (vadDetector, error) { return nil, errors.New("sidecar unreachable") }
	if err := validateVAD(factory); err == nil {
		t.Error("validateVAD with a failing factory: got nil error, want non-nil")
	}
}

// TestValidateVAD_ImplausibleProbability asserts an out-of-range probability
// (e.g. a garbage detector) is rejected, not accepted as a valid self-test.
func TestValidateVAD_ImplausibleProbability(t *testing.T) {
	factory := func() (vadDetector, error) { return probeDetector{prob: 7}, nil }
	if err := validateVAD(factory); err == nil {
		t.Error("validateVAD with prob=7: got nil error, want non-nil (probability out of [0,1])")
	}
}
