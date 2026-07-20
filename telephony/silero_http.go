package telephony

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// This is the out-of-process counterpart to silero.go's in-process gonnx
// sileroDetector: the same Silero VAD model, the same fixed I/O contract
// (window sileroWindowSize, state sileroStateShape, sr sileroSampleRate), but
// inference runs in the VAD sidecar (scripts/vad_server.py) reached over HTTP,
// so the driver stays cgo-free — exactly as STT runs in the whisper sidecar
// (STTClient). Externalizing the model is the point (SOP-145): it moves the
// ONNX runtime out of the call-handling process.
//
// The sidecar is stateless — it takes the current recurrent state in each
// request and returns the updated state — so this detector owns the state and
// threads it across Detect calls, mirroring how sileroDetector threads d.state
// = stateN and zeroes it in Reset. That parity is what makes the two detectors
// interchangeable behind vadDetector.

// defaultVADTimeout bounds a single window's inference call. A window is 32ms
// of audio arriving ~31×/s, and the sidecar is warm before traffic flows
// (its /healthz gates on startup warm-up), so a healthy call answers in
// milliseconds. The bound exists only to fail a wedged sidecar fast rather
// than block the VAD goroutine indefinitely — runVAD treats the error as a
// no-event window.
const defaultVADTimeout = 2 * time.Second

// VADClient posts inference windows to the VAD sidecar's octet-stream endpoint
// (scripts/vad_server.py). It holds one keep-alive http.Client so a call's
// stream of windows reuses a single connection, mirroring STTClient.
type VADClient struct {
	url    string
	client *http.Client
}

// NewVADClient returns a client posting to baseURL (e.g. http://127.0.0.1:7790).
func NewVADClient(baseURL string) *VADClient {
	return &VADClient{
		url: strings.TrimSuffix(baseURL, "/"),
		client: &http.Client{
			Timeout: defaultVADTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
			},
		},
	}
}

// Detector returns a vadFactory (the WithVADFactory shape) that builds a fresh
// httpSileroDetector with zeroed state per call — one per session, matching
// NewSileroDetector handing each session its own gonnx state.
func (c *VADClient) Detector() func() (VADDetector, error) {
	return func() (VADDetector, error) {
		d := &httpSileroDetector{client: c}
		d.Reset()
		return d, nil
	}
}

// infer runs one window through the sidecar, sending the current state and
// returning the speech probability and the updated state. The wire format is
// little-endian float32 throughout (matching numpy/struct on the Python side):
//
//	request : window (sileroWindowSize f32) ++ state (sileroStateElems f32)
//	response: prob (1 f32) ++ new state (sileroStateElems f32)
func (c *VADClient) infer(ctx context.Context, window, state []float32) (float32, []float32, error) {
	if len(window) != sileroWindowSize {
		return 0, nil, fmt.Errorf("silero-http: window length %d != %d", len(window), sileroWindowSize)
	}

	reqBuf := make([]byte, (len(window)+len(state))*4)
	putFloat32sLE(reqBuf, window)
	putFloat32sLE(reqBuf[len(window)*4:], state)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/", bytes.NewReader(reqBuf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling vad sidecar %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("vad sidecar returned %d: %.200s", resp.StatusCode, raw)
	}

	wantLen := 4 + len(state)*4
	if len(raw) != wantLen {
		return 0, nil, fmt.Errorf("silero-http: response length %d != %d", len(raw), wantLen)
	}
	prob := math.Float32frombits(binary.LittleEndian.Uint32(raw[:4]))
	return prob, float32sFromLE(raw[4:]), nil
}

// httpSileroDetector is a vadDetector backed by the VAD sidecar over HTTP. It
// owns the recurrent state threaded across Detect calls; it is not safe for
// concurrent use (one per session, like sileroDetector).
type httpSileroDetector struct {
	client *VADClient
	state  []float32 // sileroStateElems() floats, [2,1,128] flattened C-order
}

// Detect posts one window to the sidecar, threads the returned state, and
// returns the speech probability — the vadDetector contract sileroDetector
// also implements.
func (d *httpSileroDetector) Detect(window []float32) (float32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultVADTimeout)
	defer cancel()
	prob, newState, err := d.client.infer(ctx, window, d.state)
	if err != nil {
		return 0, err
	}
	d.state = newState
	return prob, nil
}

// Reset zeroes the recurrent state, starting a fresh utterance's inference
// history — called by runVAD on every exit, exactly as for sileroDetector.
func (d *httpSileroDetector) Reset() {
	d.state = make([]float32, sileroStateElems())
}

// putFloat32sLE writes xs as little-endian float32 into dst, which must have
// room for len(xs)*4 bytes.
func putFloat32sLE(dst []byte, xs []float32) {
	for i, x := range xs {
		binary.LittleEndian.PutUint32(dst[i*4:], math.Float32bits(x))
	}
}

// float32sFromLE decodes little-endian float32s from b (len(b) a multiple of 4).
func float32sFromLE(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
