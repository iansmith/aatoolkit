package telephony_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// TestReplay_SidecarE2E is the end-to-end VAD fidelity gate (SOP-174), the
// sidecar-era successor to the in-process TestReplay_MatchesProduction that
// SOP-147 removed. It replays a committed real recording through the WHOLE
// pipeline — decodeMuLaw → windower → the real VAD sidecar over HTTP → the turn
// state machine — and asserts Replay dispatches exactly one FullPass per golden
// end-of-utterance. Unlike the byte-level fixture tests (SOP-146/147/148), this
// exercises the assembled system: the Go HTTP client, real ONNX Runtime, real
// timing, and real turn-taking, over real audio.
//
// It needs the Python VAD sidecar toolchain, so it is opt-in: set
// AATOOLKIT_VAD_PYTHON to a python with onnxruntime/fastapi/uvicorn (e.g. a
// venv's bin/python). Without it the test skips, so `go test ./...` stays green
// on machines without that toolchain while running the real path where present.
func TestReplay_SidecarE2E(t *testing.T) {
	py := os.Getenv("AATOOLKIT_VAD_PYTHON")
	if py == "" {
		t.Skip("set AATOOLKIT_VAD_PYTHON to a python with onnxruntime/fastapi/uvicorn to run the VAD sidecar integration test")
	}
	url := startVADSidecar(t, py)

	ulaw, err := os.ReadFile(filepath.Join("testdata", "meetings_today.ulaw"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sttClient := fixedTextSTTServer(t, "can you show me my meetings today")

	// Generous: real ONNX Runtime over a 10s recording, one HTTP round-trip per
	// 32ms window, and under -race measurably slower — not a hang, just compute.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	results, err := telephony.Replay(ctx, "CA-sidecar-e2e", bytes.NewReader(ulaw), sttClient,
		telephony.WithVADFactory(telephony.NewVADClient(url).Detector()))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// One FullPass per golden end-of-utterance — derived from the committed
	// golden, not hardcoded, so this stays in lockstep across a VAD retune.
	euWindows := goldenEndOfUtteranceWindows(t)
	if len(results) != len(euWindows) {
		t.Fatalf("got %d FullPass results, want %d (golden end-of-utterance windows %v): %+v", len(results), len(euWindows), euWindows, results)
	}
	if results[0].Text != "can you show me my meetings today" {
		t.Errorf("results[0].Text = %q, want the STT stub's canned reply", results[0].Text)
	}
}

// startVADSidecar launches the real aatoolkit VAD sidecar under py, pointed at
// the vendored model on a free port, and returns its base URL once /healthz
// answers. It registers cleanup to stop the process. Missing model or a sidecar
// that never becomes healthy skips (a toolchain gap, not a code failure).
func startVADSidecar(t *testing.T, py string) string {
	t.Helper()
	// Test CWD is the telephony/ package dir; the script and model are siblings.
	script, err := filepath.Abs(filepath.Join("..", "scripts", "vad_server.py"))
	if err != nil {
		t.Fatalf("resolve sidecar script: %v", err)
	}
	model, err := filepath.Abs(filepath.Join("..", "models", "silero_vad", "silero_vad.onnx"))
	if err != nil {
		t.Fatalf("resolve model: %v", err)
	}
	if _, err := os.Stat(model); err != nil {
		t.Skipf("VAD model not present at %s: %v", model, err)
	}

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, py, script)
	cmd.Env = append(os.Environ(),
		"AATOOLKIT_VAD_MODEL="+model,
		"AATOOLKIT_VAD_HOST=127.0.0.1",
		fmt.Sprintf("AATOOLKIT_VAD_PORT=%d", port),
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // surface startup errors to the test log
	if err := cmd.Start(); err != nil {
		cancel()
		t.Skipf("cannot start VAD sidecar (%s): %v", py, err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second) // model warm-up gates /healthz
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("VAD sidecar did not become healthy at %s within 30s", url)
	return ""
}

// freePort returns a currently-free localhost TCP port. There is an unavoidable
// gap between closing the probe listener and the sidecar binding, but on a test
// host that window is not a real contention risk.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// goldenEndOfUtteranceWindows reads the committed meetings_today events golden
// and returns the window index of every end-of-utterance event — the expected
// FullPass boundaries, read from the fixture rather than hardcoded.
func goldenEndOfUtteranceWindows(t *testing.T) []int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "meetings_today_events.json"))
	if err != nil {
		t.Fatalf("read events golden: %v", err)
	}
	var golden struct {
		Events []struct {
			WindowIndex int    `json:"window_index"`
			Kind        string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse events golden: %v", err)
	}
	var windows []int
	for _, e := range golden.Events {
		if e.Kind == "end-of-utterance" {
			windows = append(windows, e.WindowIndex)
		}
	}
	return windows
}
