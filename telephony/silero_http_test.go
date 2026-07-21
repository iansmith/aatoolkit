package telephony

import (
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wire protocol under test (must match scripts/vad_server.py):
//   request : sileroWindowSize float32 (window) ++ stateElems float32 (state),
//             little-endian == 2048 bytes for the 256/256 contract.
//   response: 1 float32 (speech prob) ++ stateElems float32 (updated state),
//             little-endian == 1028 bytes.
// These encode/decode helpers are deliberately independent of the production
// putFloat32sLE/float32sFromLE so the test is a real oracle for the format, not
// a mirror of the code it checks.

func leEncode(xs []float32) []byte {
	out := make([]byte, len(xs)*4)
	for i, x := range xs {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

func leDecode(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// recordingSidecar is a fake vad_server.py: it records each request's decoded
// window and state, and answers with a fixed probability and a new state
// derived from the received one (so state threading is observable across
// calls).
type recordingSidecar struct {
	srv        *httptest.Server
	gotWindows [][]float32
	gotStates  [][]float32
	prob       float32
	status     int // 0 => 200
}

func newRecordingSidecar(prob float32) *recordingSidecar {
	rs := &recordingSidecar{prob: prob}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Input is the 64-sample context ++ 256-sample window = 320 floats (AATK-8);
		// the state is the trailing remainder.
		inputBytes := (sileroContextSize + sileroWindowSize) * 4
		stateBytes := len(body) - inputBytes
		if stateBytes <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rs.gotWindows = append(rs.gotWindows, leDecode(body[:inputBytes]))
		gotState := leDecode(body[inputBytes:])
		rs.gotStates = append(rs.gotStates, gotState)

		if rs.status != 0 {
			w.WriteHeader(rs.status)
			return
		}
		// New state = received state + 1 per element, so a later request's
		// state reveals whether the client threaded the previous response.
		newState := make([]float32, len(gotState))
		for i := range newState {
			newState[i] = gotState[i] + 1
		}
		resp := make([]byte, 4+len(newState)*4)
		binary.LittleEndian.PutUint32(resp[:4], math.Float32bits(rs.prob))
		copy(resp[4:], leEncode(newState))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(resp)
	}))
	return rs
}

func (rs *recordingSidecar) close() { rs.srv.Close() }

func newTestWindow(v float32) []float32 {
	win := make([]float32, sileroWindowSize)
	for i := range win {
		win[i] = v
	}
	return win
}

// TestHTTPVADDetector_DetectDecodesProbAndSendsWindow verifies the happy path:
// the detector POSTs the exact window bytes, and returns the decoded prob.
func TestHTTPVADDetector_DetectDecodesProbAndSendsWindow(t *testing.T) {
	rs := newRecordingSidecar(0.73)
	defer rs.close()

	det, err := NewVADClient(rs.srv.URL).Detector()()
	if err != nil {
		t.Fatalf("Detector(): %v", err)
	}

	win := newTestWindow(0.5)
	prob, err := det.Detect(win)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prob != 0.73 {
		t.Errorf("prob = %v, want 0.73", prob)
	}
	if len(rs.gotWindows) != 1 {
		t.Fatalf("sidecar saw %d requests, want 1", len(rs.gotWindows))
	}
	// The input is the 64-sample context ++ the 256-sample window (AATK-8). On the
	// first frame the context is zero-init; the window carries the 0.5 samples.
	input := rs.gotWindows[0]
	if len(input) != sileroContextSize+sileroWindowSize {
		t.Fatalf("request input length %d, want %d (context+window)", len(input), sileroContextSize+sileroWindowSize)
	}
	for i := 0; i < sileroContextSize; i++ {
		if input[i] != 0 {
			t.Fatalf("request context[%d] = %v, want 0 (zero-init on the first frame)", i, input[i])
		}
	}
	for i := sileroContextSize; i < len(input); i++ {
		if input[i] != 0.5 {
			t.Fatalf("request window[%d] = %v, want 0.5 (window bytes not sent faithfully)", i-sileroContextSize, input[i])
		}
	}
	// Initial state must be all zeros, matching sileroDetector's zeroed start.
	for i, s := range rs.gotStates[0] {
		if s != 0 {
			t.Fatalf("initial request state[%d] = %v, want 0", i, s)
			break
		}
	}
}

// TestHTTPVADDetector_ThreadsState is the semantic parity with sileroDetector:
// the state returned by one inference must be fed to the next, exactly as
// silero.go threads d.state = stateN across Detect calls.
func TestHTTPVADDetector_ThreadsState(t *testing.T) {
	rs := newRecordingSidecar(0.5)
	defer rs.close()

	det, _ := NewVADClient(rs.srv.URL).Detector()()

	if _, err := det.Detect(newTestWindow(0.1)); err != nil {
		t.Fatalf("Detect #1: %v", err)
	}
	if _, err := det.Detect(newTestWindow(0.2)); err != nil {
		t.Fatalf("Detect #2: %v", err)
	}
	// The sidecar returned (received state + 1). The first request's state was
	// zeros, so the second request must carry all-ones.
	for i, s := range rs.gotStates[1] {
		if s != 1 {
			t.Fatalf("second request state[%d] = %v, want 1 — state was not threaded from the first response", i, s)
			break
		}
	}
}

// TestHTTPVADDetector_ResetZeroesState mirrors sileroDetector.Reset: after
// Reset the next request must carry zeroed state again.
func TestHTTPVADDetector_ResetZeroesState(t *testing.T) {
	rs := newRecordingSidecar(0.5)
	defer rs.close()

	det, _ := NewVADClient(rs.srv.URL).Detector()()
	det.Detect(newTestWindow(0.1)) // advances state to ones on the sidecar side
	det.Reset()
	if _, err := det.Detect(newTestWindow(0.1)); err != nil {
		t.Fatalf("Detect after Reset: %v", err)
	}
	for i, s := range rs.gotStates[len(rs.gotStates)-1] {
		if s != 0 {
			t.Fatalf("post-Reset request state[%d] = %v, want 0", i, s)
			break
		}
	}
}

// TestHTTPVADDetector_RejectsWrongWindow mirrors sileroDetector.Detect's
// length guard and must not make an HTTP call for a malformed window.
func TestHTTPVADDetector_RejectsWrongWindow(t *testing.T) {
	rs := newRecordingSidecar(0.5)
	defer rs.close()

	det, _ := NewVADClient(rs.srv.URL).Detector()()
	if _, err := det.Detect(make([]float32, sileroWindowSize-1)); err == nil {
		t.Fatal("expected error for short window, got nil")
	}
	if len(rs.gotWindows) != 0 {
		t.Errorf("a malformed window still hit the sidecar (%d requests)", len(rs.gotWindows))
	}
}

// TestHTTPVADDetector_ServerError surfaces a non-200 as an error, so runVAD
// treats the window as no-event rather than silently trusting garbage.
func TestHTTPVADDetector_ServerError(t *testing.T) {
	rs := newRecordingSidecar(0.5)
	rs.status = http.StatusInternalServerError
	defer rs.close()

	det, _ := NewVADClient(rs.srv.URL).Detector()()
	if _, err := det.Detect(newTestWindow(0.1)); err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
}

// TestHTTPVADDetector_FreshStatePerSession ensures the factory hands each
// session its own detector state — one session's threading must not leak into
// another's, matching NewSileroDetector building a fresh gonnx state per call.
func TestHTTPVADDetector_FreshStatePerSession(t *testing.T) {
	rs := newRecordingSidecar(0.5)
	defer rs.close()

	factory := NewVADClient(rs.srv.URL).Detector()
	detA, _ := factory()
	detB, _ := factory()

	detA.Detect(newTestWindow(0.1)) // A advances its state
	if _, err := detB.Detect(newTestWindow(0.1)); err != nil {
		t.Fatalf("detB.Detect: %v", err)
	}
	// B's first request must carry zeros, unaffected by A.
	last := rs.gotStates[len(rs.gotStates)-1]
	for i, s := range last {
		if s != 0 {
			t.Fatalf("detB first request state[%d] = %v, want 0 — sessions share state", i, s)
			break
		}
	}
}
