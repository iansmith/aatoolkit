package telephony

import (
	"math"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestHTTPVADDetector_ParityWithInProcess is the end-to-end proof that the VAD
// sidecar (scripts/vad_server.py) and the in-process gonnx sileroDetector are
// interchangeable: fed the same windows with the same threaded state, they
// return the same speech probabilities. This is what "the sidecar has the same
// signaling as the baked-in VAD" actually means, and it can only be checked
// against a real running sidecar.
//
// Gated like transcribe_integration_test.go: set AATOOLKIT_VAD_URL to a running
// vad_server.py (e.g. http://127.0.0.1:7790) to run it; skipped otherwise so
// the standalone suite needs no Python/onnxruntime.
//
// Tolerance is 5e-3: gonnx and onnxruntime agree to ≤1e-3 on the Silero goldens
// (SOP-80), and this leaves margin without letting a real format mismatch (which
// would diverge wildly) pass.
func TestHTTPVADDetector_ParityWithInProcess(t *testing.T) {
	url := os.Getenv("AATOOLKIT_VAD_URL")
	if url == "" {
		t.Skip("set AATOOLKIT_VAD_URL to a running vad_server.py to run the parity test")
	}
	if !sidecarReachable(url) {
		t.Skipf("vad sidecar not reachable at %s", url)
	}

	inproc, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}
	remote, err := NewVADClient(url).Detector()()
	if err != nil {
		t.Fatalf("Detector: %v", err)
	}

	// A handful of distinct windows so the recurrent state actually evolves and
	// the comparison covers more than a single zeroed-state inference.
	const tol = 5e-3
	for i, win := range parityWindows() {
		wantProb, err := inproc.Detect(win)
		if err != nil {
			t.Fatalf("window %d: in-process Detect: %v", i, err)
		}
		gotProb, err := remote.Detect(win)
		if err != nil {
			t.Fatalf("window %d: sidecar Detect: %v", i, err)
		}
		if diff := math.Abs(float64(gotProb - wantProb)); diff > tol {
			t.Errorf("window %d: sidecar prob %v vs in-process %v (diff %g > %g) — the two detectors disagree",
				i, gotProb, wantProb, diff, tol)
		}
	}
}

// parityWindows returns a deterministic sequence of distinct inference windows.
func parityWindows() [][]float32 {
	windows := make([][]float32, 8)
	for w := range windows {
		win := make([]float32, sileroWindowSize)
		for i := range win {
			// A varying, bounded PCM-like signal; the exact shape is irrelevant,
			// only that it differs per window so state evolves.
			win[i] = float32(math.Sin(float64(i+w*7)*0.05)) * 0.6
		}
		windows[w] = win
	}
	return windows
}

func sidecarReachable(url string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
