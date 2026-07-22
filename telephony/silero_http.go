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

// This is the engine's VAD detector: the Silero VAD model with a fixed I/O
// contract (window sileroWindowSize, state sileroStateShape), but inference
// runs in the VAD sidecar (scripts/vad_server.py) reached over HTTP, so the
// driver stays cgo-free — exactly as STT runs in the whisper sidecar
// (STTClient). Externalizing the model out of the call-handling process is the
// point (SOP-145/147); the in-process ONNX detector was retired.
//
// The sidecar is stateless — it takes the current recurrent state in each
// request and returns the updated state — so this detector owns the state and
// threads it across Detect calls (zeroing it in Reset).

// defaultVADTimeout bounds a single window's inference call. A window is 32ms
// of audio arriving ~31×/s, and the sidecar is warm before traffic flows
// (its /healthz gates on startup warm-up), so a healthy call answers in
// milliseconds. The bound exists only to fail a wedged sidecar fast rather
// than block the VAD goroutine indefinitely — runVAD treats the error as a
// no-event window.
const defaultVADTimeout = 2 * time.Second

// vadInferenceDeadline bounds a single window's inference — retries included —
// and is strictly less than DataPlaneBufferMS (80ms), so a stalled sidecar
// fails the window before the data-plane buffer it feeds would overflow rather
// than blocking the VAD goroutine (SOP-147 / charter R8). It is derived from
// the buffer, not a bare literal, so a buffer retune can't silently push the
// per-inference budget past it — TestSileroPerInferenceDeadline guards the
// inequality.
const vadInferenceDeadline = (DataPlaneBufferMS - 20) * time.Millisecond

// vadRetryBackoff is the pause between inference retries within
// vadInferenceDeadline — small enough that several attempts fit inside the
// deadline, non-zero so a transient sidecar error isn't retried in a tight spin.
const vadRetryBackoff = 5 * time.Millisecond

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
// httpSileroDetector with zeroed state per call — one per session, so no
// session's recurrent state leaks into another's.
func (c *VADClient) Detector() func() (VADDetector, error) {
	return func() (VADDetector, error) {
		d := &httpSileroDetector{client: c}
		d.Reset()
		return d, nil
	}
}

// infer runs one input window through the sidecar, sending the current state and
// returning the speech probability and the updated state. input is the 64-sample
// context ++ the 256-sample chunk = 320 samples (AATK-8; the caller assembles it).
// The wire format is little-endian float32 throughout (matching numpy/struct on
// the Python side):
//
//	request : input (sileroContextSize+sileroWindowSize f32) ++ state (sileroStateElems f32)
//	response: prob (1 f32) ++ new state (sileroStateElems f32)
func (c *VADClient) infer(ctx context.Context, input, state []float32) (float32, []float32, error) {
	if len(input) != sileroContextSize+sileroWindowSize {
		return 0, nil, fmt.Errorf("silero-http: input length %d != %d", len(input), sileroContextSize+sileroWindowSize)
	}

	reqBuf := make([]byte, (len(input)+len(state))*4)
	putFloat32sLE(reqBuf, input)
	putFloat32sLE(reqBuf[len(input)*4:], state)

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
	client  *VADClient
	state   []float32 // sileroStateElems() floats, [2,1,128] flattened C-order
	context []float32 // last sileroContextSize samples of the previous window (AATK-8)
}

// Detect posts one window to the sidecar, threads the returned state, and
// returns the speech probability — the vadDetector contract sileroDetector
// also implements. It prepends the carried 64-sample context to the window
// (context ++ window = 320) and carries this window's tail forward, exactly as
// the in-process sileroDetector does — that parity keeps the two interchangeable.
func (d *httpSileroDetector) Detect(ctx context.Context, window []float32) (float32, error) {
	if len(window) != sileroWindowSize {
		return 0, fmt.Errorf("silero-http: window length %d != %d", len(window), sileroWindowSize)
	}
	// Bound this window's inference — retries included — to vadInferenceDeadline,
	// derived from the caller's ctx so a cancelled session still ends it (SOP-147).
	ictx, cancel := context.WithTimeout(ctx, vadInferenceDeadline)
	defer cancel()

	input := append(append([]float32(nil), d.context...), window...)
	var lastErr error
	for {
		prob, newState, err := d.client.infer(ictx, input, d.state)
		if err == nil {
			d.state = newState
			d.context = append(d.context[:0], window[len(window)-sileroContextSize:]...)
			return prob, nil
		}
		lastErr = err
		// Retry a transient sidecar failure until the deadline, then fail loud —
		// the state is untouched on error, so a retry re-sends the same window.
		select {
		case <-ictx.Done():
			return 0, fmt.Errorf("silero-http: inference failed within %s: %w", vadInferenceDeadline, lastErr)
		case <-time.After(vadRetryBackoff):
		}
	}
}

// Reset zeroes the recurrent state and context, starting a fresh utterance's
// inference history — called by runVAD on every exit, exactly as for sileroDetector.
func (d *httpSileroDetector) Reset() {
	d.state = make([]float32, sileroStateElems())
	d.context = make([]float32, sileroContextSize)
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
