package twilio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Wiring tests for the realtime voice transport (AATK-70). The property under
// test is that carrier audio crosses this package to the backend as the
// **base64 string it arrived as** — never decoded and re-encoded. Every
// assertion below therefore compares the encoded form; decoding both sides
// first would pass against a round-tripping re-encode, which is the exact
// defect this wiring exists to avoid.

// oneFrameBytes is one 20 ms G.711 frame at 8 kHz: 160 samples, one byte each.
const oneFrameBytes = 160

// carrierPayloadB64 is a carrier media payload as it arrives on the wire.
func carrierPayloadB64() string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", oneFrameBytes)))
}

// mediaFrameRaw builds the raw Twilio media message carrying payloadB64
// verbatim, so a test controls the exact string that arrives on the wire.
func mediaFrameRaw(streamSID, payloadB64 string) []byte {
	return []byte(fmt.Sprintf(
		`{"event":"media","streamSid":%q,"media":{"payload":%q,"chunk":1,"timestamp":"0"}}`,
		streamSID, payloadB64))
}

// --- the non-transcoding property, at the demux seam ------------------------

func TestDecodeFrame_CarriesEncodedPayloadVerbatim(t *testing.T) {
	p := carrierPayloadB64()
	if len(p) != 216 {
		t.Fatalf("160-byte G.711 frame must base64-encode to 216 chars, got %d", len(p))
	}

	f, err := DecodeFrame(mediaFrameRaw("SS1", p))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.EncodedPayload != p {
		t.Fatalf("EncodedPayload must be the carrier's payload verbatim:\n got %q\nwant %q",
			f.EncodedPayload, p)
	}
}

// --- the fake backend ------------------------------------------------------

// fakeRealtimeBackend is an in-process realtime voice backend: it counts
// connection attempts and records the client events it receives.
type fakeRealtimeBackend struct {
	srv *httptest.Server

	mu       sync.Mutex
	conns    int
	received []json.RawMessage

	closeAfterHandshake bool
}

func newFakeRealtimeBackend(t *testing.T) *fakeRealtimeBackend {
	t.Helper()
	b := &fakeRealtimeBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.conns++
		b.mu.Unlock()

		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()

		// The session.update handshake the client opens with.
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		if err := writeRealtimeJSON(ctx, c, map[string]string{"type": "session.created"}); err != nil {
			return
		}

		if b.closeAfterHandshake {
			_ = c.Close(websocket.StatusNormalClosure, "bye")
			return
		}

		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			b.mu.Lock()
			b.received = append(b.received, json.RawMessage(append([]byte(nil), data...)))
			b.mu.Unlock()
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeRealtimeBackend) url() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http")
}

func (b *fakeRealtimeBackend) connCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conns
}

// appendedAudio returns the audio field of every input_audio_buffer.append
// received, in arrival order, as the raw base64 strings they arrived as.
func (b *fakeRealtimeBackend) appendedAudio() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, raw := range b.received {
		var ev struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		if ev.Type == "input_audio_buffer.append" {
			out = append(out, ev.Audio)
		}
	}
	return out
}

func writeRealtimeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}

// --- the carrier-side harness ----------------------------------------------

// realtimeHarness drives NewStreamHandler over a real carrier WebSocket, so
// the switch is exercised exactly where a live call would reach it.
type realtimeHarness struct {
	t         *testing.T
	conn      *websocket.Conn
	streamSID string
	done      chan error
}

func newRealtimeHarness(t *testing.T, realtimeURL string) *realtimeHarness {
	t.Helper()
	return newRealtimeHarnessWith(t, NewStreamHandler(realtimeURL))
}

// newRealtimeHarnessWith is the same carrier-side rig driving an arbitrary
// StreamHandler, so a test can enter by the exported realtime handler directly
// (AATK-72) exactly as a consumer with its own session options would, rather
// than only through NewStreamHandler's switch.
func newRealtimeHarnessWith(t *testing.T, handle StreamHandler) *realtimeHarness {
	t.Helper()
	h := &realtimeHarness{
		t:         t,
		streamSID: "SS" + t.Name(),
		done:      make(chan error, 1),
	}

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			err := handle(ctx, conn, start)
			h.done <- err
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial carrier: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	h.conn = conn

	msg, err := EncodeStart(h.streamSID, "CA"+t.Name(), "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}
	return h
}

func (h *realtimeHarness) sendRaw(msg []byte) {
	h.t.Helper()
	if err := h.conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// waitDone waits for the handler to return, failing the test on timeout — a
// hung session is the failure mode the error paths below exist to rule out.
func (h *realtimeHarness) waitDone(d time.Duration) error {
	h.t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(d):
		h.t.Fatal("stream handler never returned — session is hung")
		return nil
	}
}

// --- behavior 3: byte-identical carrier audio reaches the backend -----------

func TestRealtime_CarrierAudioReachesBackendVerbatim(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())

	p := carrierPayloadB64()
	h.sendRaw(mediaFrameRaw(h.streamSID, p))

	deadline := time.After(5 * time.Second)
	for {
		got := be.appendedAudio()
		if len(got) > 0 {
			// String equality on the encoded form: the whole point.
			if got[0] != p {
				t.Fatalf("backend must receive the carrier payload verbatim:\n got %q\nwant %q", got[0], p)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("backend never received an input_audio_buffer.append")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- behavior 1: the default is genuinely off ------------------------------

func TestRealtime_SwitchUnsetAttemptsNoDial(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	// Switch unset: the existing VAD/STT/TTS path runs and no realtime client
	// is constructed, so the backend sees zero connection attempts.
	h := newRealtimeHarness(t, "")
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))

	time.Sleep(250 * time.Millisecond)

	if got := be.connCount(); got != 0 {
		t.Fatalf("switch unset must attempt no dial, got %d connection attempt(s)", got)
	}
}

// --- behavior 5: failure paths end the call, never hang --------------------

func TestRealtime_DialFailureEndsCallWithError(t *testing.T) {
	// A backend that refuses connection: bind a listener, then close it, so the
	// address is well-formed and certain to be dead.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()

	h := newRealtimeHarness(t, url)

	if err := h.waitDone(5 * time.Second); err == nil {
		t.Fatal("a backend that refuses connection must end the call with a non-nil error")
	}
}

func TestRealtime_BackendDropMidCallEndsCallWithError(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	be.closeAfterHandshake = true

	h := newRealtimeHarness(t, be.url())
	h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))

	if err := h.waitDone(5 * time.Second); err == nil {
		t.Fatal("a backend that drops mid-call must end the call with a non-nil error")
	}
}
