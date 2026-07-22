package telephony

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SOP-147. These tests pin the HTTP VAD detector against silero_wire_fixture.json
// (SOP-146) — the byte-exact request/response oracle from real ONNX Runtime
// inference — plus the failure policy (retry within a sub-buffer deadline, loud
// error on exhaustion). The detector runs the model out-of-process in the VAD
// sidecar; these tests need no live sidecar, only the committed fixture.

type wireFrame struct {
	Index       int    `json:"index"`
	RequestHex  string `json:"request_hex"`
	ResponseHex string `json:"response_hex"`
}

type wireFixture struct {
	ModelSHA256   string `json:"model_sha256"`
	MeetingsToday struct {
		Frames []wireFrame `json:"frames"`
	} `json:"meetings_today"`
}

func loadWireFixture(t *testing.T) wireFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "silero_wire_fixture.json"))
	if err != nil {
		t.Fatalf("reading wire fixture: %v", err)
	}
	var f wireFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing wire fixture: %v", err)
	}
	return f
}

// windowFromRequest recovers the 256-sample window from a fixture request, whose
// leading floats are (context ++ window): the window is the [context, context+window)
// slice. Feeding these windows back drives the detector to reconstruct the same
// context/state threading the fixture captured.
func windowFromRequest(t *testing.T, requestHex string) []float32 {
	t.Helper()
	b, err := hex.DecodeString(requestHex)
	if err != nil {
		t.Fatalf("decode request hex: %v", err)
	}
	floats := float32sFromLE(b)
	return append([]float32(nil), floats[sileroContextSize:sileroContextSize+sileroWindowSize]...)
}

func responseProb(t *testing.T, responseHex string) float32 {
	t.Helper()
	b, err := hex.DecodeString(responseHex)
	if err != nil {
		t.Fatalf("decode response hex: %v", err)
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b[:4]))
}

// TestSileroHTTPEncodeDecode drives the detector with the fixture's windows and
// asserts, byte-for-byte, that the request it sends equals the fixture's
// request_hex — proving the wire encoding and the context/state threading match
// the oracle — and that it decodes the replayed response's probability. Frames
// 0..4 cover the zero-init first frame and the threading chain.
func TestSileroHTTPEncodeDecode(t *testing.T) {
	fx := loadWireFixture(t)
	frames := fx.MeetingsToday.Frames
	if len(frames) < 5 {
		t.Fatalf("fixture has %d meetings_today frames, want >= 5", len(frames))
	}
	frames = frames[:5]

	var i int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := hex.EncodeToString(body); got != frames[i].RequestHex {
			t.Errorf("frame %d: request bytes do not match fixture request_hex", i)
		}
		resp, _ := hex.DecodeString(frames[i].ResponseHex)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(resp)
		i++
	}))
	defer srv.Close()

	det, err := NewVADClient(srv.URL).Detector()()
	if err != nil {
		t.Fatalf("Detector(): %v", err)
	}
	for n := range frames {
		prob, err := det.Detect(context.Background(), windowFromRequest(t, frames[n].RequestHex))
		if err != nil {
			t.Fatalf("frame %d Detect: %v", n, err)
		}
		if want := responseProb(t, frames[n].ResponseHex); prob != want {
			t.Errorf("frame %d prob = %v, want %v", n, prob, want)
		}
	}
	if i != len(frames) {
		t.Errorf("sidecar saw %d requests, want %d", i, len(frames))
	}
}

// TestSileroRetry: a transient 503 on the first attempt is retried within the
// deadline and succeeds on the second — no frame lost to a blip.
func TestSileroRetry(t *testing.T) {
	fx := loadWireFixture(t)
	f0 := fx.MeetingsToday.Frames[0]
	resp, _ := hex.DecodeString(f0.ResponseHex)

	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(resp)
	}))
	defer srv.Close()

	det, _ := NewVADClient(srv.URL).Detector()()
	prob, err := det.Detect(context.Background(), windowFromRequest(t, f0.RequestHex))
	if err != nil {
		t.Fatalf("Detect should retry past a transient 503: %v", err)
	}
	if want := responseProb(t, f0.ResponseHex); prob != want {
		t.Errorf("prob = %v, want %v", prob, want)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want >= 2 (the 503 was not retried)", attempts)
	}
}

// TestSileroDeadlineExceeded: a sidecar that never answers must make Detect
// return a non-nil error once the per-inference deadline elapses — the silent
// `continue` is gone (SOP-147).
func TestSileroDeadlineExceeded(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block) // LIFO: unblock the handler before srv.Close() waits on it

	det, _ := NewVADClient(srv.URL).Detector()()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := det.Detect(ctx, make([]float32, sileroWindowSize)); err == nil {
		t.Fatal("expected a deadline error from a stalled sidecar, got nil")
	}
}

// TestSileroPerInferenceDeadline guards charter R8: the per-inference budget
// must stay strictly inside the data-plane buffer it feeds, so a stalled
// inference fails the window before the buffer would overflow.
func TestSileroPerInferenceDeadline(t *testing.T) {
	if vadInferenceDeadline >= DataPlaneBufferMS*time.Millisecond {
		t.Fatalf("vadInferenceDeadline %s must be < DataPlaneBufferMS (%dms)", vadInferenceDeadline, DataPlaneBufferMS)
	}
}
