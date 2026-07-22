package telephony

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// ValidateVAD is the engine's startup self-test for the VAD pipeline: it
// constructs a detector from the given factory and runs one real inference,
// failing hard (a non-nil error) if the detector can't be built or produces an
// implausible result. A consumer calls this once before accepting calls,
// passing the same factory it wires via WithVADFactory (e.g.
// NewVADClient(url).Detector()) — since SOP-147 the detector is the out-of-
// process sidecar, so the self-test also confirms the sidecar answers.
func ValidateVAD(factory func() (VADDetector, error)) error {
	return validateVAD(factory)
}

// validateVAD is ValidateVAD's implementation, taking the unexported detector
// type so tests can inject a fake factory to exercise the failure path.
func validateVAD(factory func() (vadDetector, error)) error {
	det, err := factory()
	if err != nil {
		return fmt.Errorf("vad self-test: constructing detector: %w", err)
	}

	// A zero-valued window is a deterministic, dependency-free self-test
	// input — it doesn't need a real audio fixture, only a real inference.
	window := make([]float32, sileroWindowSize)

	start := time.Now()
	prob, err := det.Detect(context.Background(), window)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("vad self-test: inference: %w", err)
	}

	if math.IsNaN(float64(prob)) || math.IsInf(float64(prob), 0) || prob < 0 || prob > 1 {
		return fmt.Errorf("vad self-test: implausible speech probability %v", prob)
	}

	log.Printf("telephony: VAD self-test ok: probability=%v inference_time=%s", prob, elapsed)
	return nil
}
