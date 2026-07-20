package telephony

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/assets"
)

// sileroGoldens mirrors internal/telephony/testdata/silero_goldens.json — a
// copy of third_party/gonnx's onnxruntime-produced golden data for the
// vendored silero_vad.onnx model.
type sileroGoldens struct {
	SampleRate int           `json:"sample_rate"`
	WindowSize int           `json:"window_size"`
	Frames     []sileroFrame `json:"frames"`
}

type sileroFrame struct {
	Index  int       `json:"index"`
	Kind   string    `json:"kind"`
	Input  []float32 `json:"input"`
	Output float32   `json:"output"`
}

func loadSileroGoldens(t *testing.T) sileroGoldens {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "silero_goldens.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	var g sileroGoldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}
	return g
}

// TestSileroDetectorGolden feeds the detector the same frame sequence
// onnxruntime was run against (state threaded internally, frame to frame) and
// asserts each per-frame probability agrees within the 1e-3 conformance delta
// used throughout the gonnx effort.
func TestSileroDetectorGolden(t *testing.T) {
	const delta = 1e-3
	g := loadSileroGoldens(t)
	if len(g.Frames) < 10 {
		t.Fatalf("golden fixture must cover at least 10 frames, got %d", len(g.Frames))
	}

	det, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}

	for _, f := range g.Frames {
		got, err := det.Detect(f.Input)
		if err != nil {
			t.Fatalf("frame %d (%s): Detect: %v", f.Index, f.Kind, err)
		}
		diff := got - f.Output
		if diff < 0 {
			diff = -diff
		}
		if diff > delta {
			t.Errorf("frame %d (%s): got prob %v, want %v (delta %v > %v)", f.Index, f.Kind, got, f.Output, diff, delta)
		}
	}
}

// TestSileroDetectorRejectsWrongWindowSize asserts Detect refuses a window
// whose length doesn't match the model's fixed window size, rather than
// silently misbehaving against the ONNX runtime.
func TestSileroDetectorRejectsWrongWindowSize(t *testing.T) {
	det, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}

	if _, err := det.Detect(make([]float32, 100)); err == nil {
		t.Error("want error for a too-short window, got nil")
	}
	if _, err := det.Detect(make([]float32, 512)); err == nil {
		t.Error("want error for a too-long window, got nil")
	}
}

// TestModelDriftGuard pins the embedded model copy to the fork's
// vendored copy so the two can never silently diverge.
func TestModelDriftGuard(t *testing.T) {
	embedded := sha256.Sum256(assets.SileroVADONNX)

	forkPath := filepath.Join("..", "third_party", "gonnx", "sample_models", "onnx_models", "silero_vad.onnx")
	forkBytes, err := os.ReadFile(forkPath)
	if err != nil {
		t.Fatalf("reading fork copy %s: %v", forkPath, err)
	}
	forkSum := sha256.Sum256(forkBytes)

	got := hex.EncodeToString(embedded[:])
	want := hex.EncodeToString(forkSum[:])
	if got != want {
		t.Errorf("embedded model sha256 %s != fork copy sha256 %s", got, want)
	}
}

// BenchmarkSileroDetect measures steady-state per-frame inference latency
// with state threaded across calls, mirroring the real runVAD hot loop.
func BenchmarkSileroDetect(b *testing.B) {
	g := sileroGoldens{}
	raw, err := os.ReadFile(filepath.Join("testdata", "silero_goldens.json"))
	if err != nil {
		b.Fatalf("reading golden fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		b.Fatalf("unmarshalling golden fixture: %v", err)
	}
	if len(g.Frames) == 0 {
		b.Fatal("golden fixture has no frames")
	}

	det, err := NewSileroDetector()
	if err != nil {
		b.Fatalf("NewSileroDetector: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := g.Frames[i%len(g.Frames)]
		if _, err := det.Detect(f.Input); err != nil {
			b.Fatalf("Detect: %v", err)
		}
	}
}
