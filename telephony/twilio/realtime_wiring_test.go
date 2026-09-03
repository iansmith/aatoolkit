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
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// measureCallCPU runs one realtime call for window with opts applied, then
// reports the process CPU time that window burned.
//
// One definition serving every closed-consumer-channel busy-loop guard on this
// path — client event, voice update, mark request — because the measurement is
// the same one three times over and a fourth channel would otherwise copy it
// again (CLAUDE.md #4). What each guard needs is a DIFFERENTIAL: the same
// window with the option supplied and a closed channel, against the same window
// with no option supplied at all, so the caller calls this twice and compares.
//
// CPU rather than liveness, and that part is load-bearing: an implementation
// that returns the channel unchanged on a closed receive spins the select loop
// forever WITHOUT starving its sibling cases — Go's asynchronous preemption
// (since 1.14) keeps them scheduled on time regardless — so the call stays up,
// no extra frame reaches the backend, and every liveness assertion still
// passes. Only the CPU burned can see it.
//
// syscall.Getrusage is not available on Windows; callers skip there.
func measureCallCPU(t *testing.T, window time.Duration, opts ...RealtimeOption) time.Duration {
	t.Helper()

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
	waitBackendReady(t, be, h)

	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	time.Sleep(window)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		t.Fatalf("getrusage: %v", err)
	}

	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
	_ = h.waitDone(5 * time.Second)

	userDelta := time.Duration(after.Utime.Sec-before.Utime.Sec)*time.Second +
		time.Duration(after.Utime.Usec-before.Utime.Usec)*time.Microsecond
	sysDelta := time.Duration(after.Stime.Sec-before.Stime.Sec)*time.Second +
		time.Duration(after.Stime.Usec-before.Stime.Usec)*time.Microsecond
	return userDelta + sysDelta
}

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

	mu         sync.Mutex
	conns      int
	received   []json.RawMessage
	handshakes []json.RawMessage
	conn       *websocket.Conn

	closeAfterHandshake bool

	// emitInterval, when positive, makes the backend emit an audio-delta
	// event on this interval after the handshake completes — active traffic,
	// for tests that must prove an idle timer does not fire while the
	// backend keeps talking. Zero (the default) emits nothing after the
	// handshake, which is the silent-backend case every other test in this
	// file relies on.
	emitInterval time.Duration
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
		b.mu.Lock()
		b.conn = c
		b.mu.Unlock()
		ctx := r.Context()

		// The session.update handshake the client opens with. Recorded, not
		// discarded: it is the only place the negotiated session config is
		// observable, and it is what the handshake tests assert on.
		_, hs, err := c.Read(ctx)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.handshakes = append(b.handshakes, json.RawMessage(append([]byte(nil), hs...)))
		b.mu.Unlock()
		if err := writeRealtimeJSON(ctx, c, map[string]string{"type": "session.created"}); err != nil {
			return
		}

		if b.closeAfterHandshake {
			_ = c.Close(websocket.StatusNormalClosure, "bye")
			return
		}

		if b.emitInterval > 0 {
			done := make(chan struct{})
			defer close(done)
			go b.emitAudioDeltas(ctx, c, done)
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

// emitAudioDeltas writes a response.output_audio.delta event every
// emitInterval until done is closed or a write fails. It stops on its own
// when the connection ends, rather than leaking a goroutine past the request
// handler's return — the handler's own read loop is what detects that end and
// closes done via its defer.
func (b *fakeRealtimeBackend) emitAudioDeltas(ctx context.Context, c *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(b.emitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := writeRealtimeJSON(ctx, c, map[string]string{
				"type":  "response.output_audio.delta",
				"delta": "AAAA",
			}); err != nil {
				return
			}
		}
	}
}

// deadBackendURL is a well-formed backend address certain to refuse connection:
// bind a listener so the port is real, then close it. Shared by every test that
// exercises the dial-failure path, so they all point at the same kind of dead
// address rather than each inventing one.
func deadBackendURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()
	return url
}

// closeNow ends the backend's current connection from the backend side,
// immediately. It is how a test drives the "backend goes away" ending
// deliberately, mid-run, rather than waiting for closeAfterHandshake's
// automatic close right after the handshake.
func (b *fakeRealtimeBackend) closeNow() {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c != nil {
		_ = c.Close(websocket.StatusNormalClosure, "bye")
	}
}

// handshake returns the nth session.update the backend received, as the raw
// bytes that arrived. Tests assert on these rather than on a struct: what goes
// on the wire is the contract.
func (b *fakeRealtimeBackend) handshake(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= len(b.handshakes) {
		return ""
	}
	return string(b.handshakes[n])
}

func (b *fakeRealtimeBackend) handshakeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.handshakes)
}

// emitOnce writes a single event from the backend, now. Safe once the handshake
// has completed — see emitEvery's note on concurrent writers.
func (b *fakeRealtimeBackend) emitOnce(t *testing.T, ev map[string]string) {
	t.Helper()
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		t.Fatal("backend has no connection to emit on — call waitBackendReady first")
	}
	if err := writeRealtimeJSON(context.Background(), c, ev); err != nil {
		t.Fatalf("emit %s: %v", ev["type"], err)
	}
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

// countMediaFrames starts reading the carrier side and counts the media frames
// the handler writes to it, returning a query function.
//
// Opt-in rather than always-on: several tests read h.conn themselves to prove
// the handler closed it, and a background reader would race them for the same
// bytes.
func (h *realtimeHarness) countMediaFrames(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	var n int

	go func() {
		for {
			_, data, err := h.conn.Read(context.Background())
			if err != nil {
				return
			}
			var ev struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(data, &ev) == nil && ev.Event == "media" {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}
	}()

	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
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
	h := newRealtimeHarness(t, deadBackendURL(t))

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
