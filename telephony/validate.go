package telephony

import (
	"fmt"
	"log"
	"math"
	"time"
)

// ValidateVAD is the engine's startup self-test for the VAD pipeline: it
// constructs the production Silero detector and runs one real inference,
// failing hard (a non-nil error) if the model can't load or produces an
// implausible result. Main calls this once before accepting calls and panics
// on a non-nil error — a broken VAD model should never reach production
// silently as a detector that always returns garbage.
func ValidateVAD() error {
	return validateVAD(NewSileroDetector)
}

// validateVAD is ValidateVAD with the detector factory injected, so tests can
// exercise the failure path (e.g. a corrupt model) without touching the real
// embedded asset.
func validateVAD(factory func() (vadDetector, error)) error {
	det, err := factory()
	if err != nil {
		return fmt.Errorf("vad self-test: constructing detector: %w", err)
	}

	// A zero-valued window is a deterministic, dependency-free self-test
	// input — it doesn't need a real audio fixture, only a real model run.
	window := make([]float32, sileroWindowSize)

	start := time.Now()
	prob, err := det.Detect(window)
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
