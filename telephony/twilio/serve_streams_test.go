package twilio_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- helpers ---

// streamsServer returns a test HTTP server that routes all requests to s.ServeStreams.
func streamsServer(t *testing.T, s *twilio.Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.ServeStreams))
	t.Cleanup(srv.Close)
	return srv
}

// dialStreams dials the test server's WebSocket endpoint.
func dialStreams(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// writeText writes a text WebSocket message to conn.
func writeText(t *testing.T, conn *websocket.Conn, msg []byte) {
	t.Helper()
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// sendStart encodes and writes a Twilio start frame.
func sendStart(t *testing.T, conn *websocket.Conn, streamSID, callSID string) {
	t.Helper()
	msg, err := twilio.EncodeStart(streamSID, callSID, "ACtest", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	writeText(t, conn, msg)
}

// --- edge / boundary ---

// Edge: WebSocket upgrade must succeed — the server performs the HTTP→WebSocket
// handshake and does not immediately close the connection.
func TestServeStreams_AcceptsWebSocketUpgrade(t *testing.T) {
	done := make(chan struct{})
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			close(done)
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, "SS01", "CA01")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("handler not called within 2s after start frame")
	}
}

// Boundary: start frame is the first message on every Twilio connection; its
// StreamSID and CallSID must be decoded and passed to HandleStream exactly.
func TestServeStreams_HandlerReceivesStartFrame(t *testing.T) {
	const (
		wantStreamSID = "SSabc123def456"
		wantCallSID   = "CAghi789jkl012"
	)
	received := make(chan twilio.Frame, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
			received <- f
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, wantStreamSID, wantCallSID)

	select {
	case f := <-received:
		if f.Event != twilio.EventStart {
			t.Errorf("Event = %q, want %q", f.Event, twilio.EventStart)
		}
		if f.StreamSID != wantStreamSID {
			t.Errorf("StreamSID = %q, want %q", f.StreamSID, wantStreamSID)
		}
		if f.CallSID != wantCallSID {
			t.Errorf("CallSID = %q, want %q", f.CallSID, wantCallSID)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler not called within 2s")
	}
}

// Boundary: if the first frame is not a start event, it is a protocol violation.
// The server must close the connection without calling HandleStream.
func TestServeStreams_RejectsNonStartFirstFrame(t *testing.T) {
	handlerCalled := make(chan struct{}, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			handlerCalled <- struct{}{}
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	// Send a media frame as the first message — violates the protocol.
	media, err := twilio.EncodeMedia("SS01", make([]byte, 160))
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, conn, media)

	// Server must close the connection; a subsequent read must fail.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	if readErr == nil {
		t.Error("expected connection to be closed after non-start first frame, but Read succeeded")
	}

	// HandleStream must not have been called.
	select {
	case <-handlerCalled:
		t.Error("HandleStream was called despite protocol violation (non-start first frame)")
	default:
	}
}

// --- error / rejection ---

// Error: nil HandleStream — the server must read and discard all frames until
// the client closes the connection without crashing.
func TestServeStreams_NilHandler_ReadsUntilClose(t *testing.T) {
	s := &twilio.Server{} // HandleStream is nil
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	sendStart(t, conn, "SSnil01", "CAnil01")

	// Send several media frames; server must not crash or stall.
	for i := 0; i < 3; i++ {
		media, err := twilio.EncodeMedia("SSnil01", make([]byte, 160))
		if err != nil {
			t.Fatal(err)
		}
		writeText(t, conn, media)
	}

	// Client closes normally — server must exit cleanly.
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// Error: handler returns an error — the connection must be closed and the error
// must not cause a panic or goroutine leak.
func TestServeStreams_HandlerError_ConnectionClosed(t *testing.T) {
	handlerErr := context.DeadlineExceeded // arbitrary sentinel
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			return handlerErr
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, "SSerr01", "CAerr01")

	// Connection must be closed after the handler returns with an error.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Error("expected connection to be closed after handler error, but Read succeeded")
	}
}

// --- cross-feature interaction ---

// Cross-feature: handler can write outbound media frames that the client receives
// and can decode. Tests that ServeStreams' conn is the same connection the client
// is reading from.
func TestServeStreams_HandlerCanWriteMedia(t *testing.T) {
	const streamSID = "SSwrite01"
	wantPayload := make([]byte, 160)
	for i := range wantPayload {
		wantPayload[i] = byte(i % 256)
	}

	handlerSent := make(chan struct{})
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			msg, err := twilio.EncodeMedia(start.StreamSID, wantPayload)
			if err != nil {
				return err
			}
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				return err
			}
			close(handlerSent)
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, streamSID, "CAwrite01")

	select {
	case <-handlerSent:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not send frame within 2s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	f, err := twilio.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != twilio.EventMedia {
		t.Errorf("Event = %q, want %q", f.Event, twilio.EventMedia)
	}
	if f.StreamSID != streamSID {
		t.Errorf("StreamSID = %q, want %q", f.StreamSID, streamSID)
	}
	if len(f.Payload) != 160 {
		t.Errorf("payload len = %d, want 160", len(f.Payload))
	}
}

// Cross-feature: when the client disconnects, conn.Read inside the handler must
// return an error promptly so the handler can exit — no goroutine leak.
func TestServeStreams_ClientDisconnect_HandlerExits(t *testing.T) {
	handlerExited := make(chan error, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			_, _, err := conn.Read(ctx)
			handlerExited <- err
			return err
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, "SSdisc01", "CAdisc01")

	// Close from the client side immediately after start.
	conn.CloseNow()

	select {
	case err := <-handlerExited:
		if err == nil {
			t.Error("expected handler to receive error after client disconnect, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("handler did not exit within 2s after client disconnect")
	}
}

// --- adversary gap tests ---

// Gap: start frame with an empty StreamSID is technically valid JSON — the
// server must still call HandleStream (the SID may be populated by Twilio later
// in production, but the server should not gate on it).
func TestServeStreams_EmptyStreamSID_HandlerStillCalled(t *testing.T) {
	called := make(chan twilio.Frame, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
			called <- f
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	// Manually encode a start frame with empty StreamSID.
	msg := []byte(`{"event":"start","streamSid":"","start":{"callSid":"CA_empty"}}`)
	writeText(t, conn, msg)

	select {
	case f := <-called:
		if f.Event != twilio.EventStart {
			t.Errorf("Event = %q, want %q", f.Event, twilio.EventStart)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler not called for empty-StreamSID start frame within 2s")
	}
}

// Gap: malformed JSON as the first frame — DecodeFrame returns an error; the
// server must close the connection without calling HandleStream. Failing to
// handle this leaves the conn open with an unconsumed parse error.
func TestServeStreams_MalformedFirstFrame_ConnectionClosed(t *testing.T) {
	handlerCalled := make(chan struct{}, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			handlerCalled <- struct{}{}
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	writeText(t, conn, []byte(`not valid json {{{`))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	if readErr == nil {
		t.Error("expected connection closed after malformed first frame, but Read succeeded")
	}
	select {
	case <-handlerCalled:
		t.Error("HandleStream was called despite malformed first frame")
	default:
	}
}

// Gap: handler can read subsequent media frames from the conn after being called.
// ServeStreams must not consume the inbound stream itself — the handler owns it.
func TestServeStreams_HandlerReadsMediaFramesFromConn(t *testing.T) {
	const streamSID = "SSmedia01"
	const frameCount = 5
	frames := make(chan twilio.Frame, frameCount)

	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					return nil
				}
				f, err := twilio.DecodeFrame(raw)
				if err != nil {
					return err
				}
				frames <- f
			}
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, streamSID, "CAmedia01")

	// Send frameCount media frames.
	for i := 0; i < frameCount; i++ {
		payload := make([]byte, 160)
		payload[0] = byte(i)
		media, err := twilio.EncodeMedia(streamSID, payload)
		if err != nil {
			t.Fatal(err)
		}
		writeText(t, conn, media)
	}

	for i := 0; i < frameCount; i++ {
		select {
		case f := <-frames:
			if f.Event != twilio.EventMedia {
				t.Errorf("frame[%d]: Event = %q, want %q", i, f.Event, twilio.EventMedia)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only received %d/%d frames within 2s", i, frameCount)
		}
	}
}

// sendConnected encodes and writes a Twilio connected frame.
func sendConnected(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	msg, err := twilio.EncodeConnected()
	if err != nil {
		t.Fatalf("EncodeConnected: %v", err)
	}
	writeText(t, conn, msg)
}

// --- SOP-141: Optional connected frame ---

// Behavior: connected then start — the server accepts a connected frame followed by
// a start frame, consumes the connected silently, and passes the start frame to
// HandleStream exactly as if connected were not present.
func TestServeStreams_ConnectedThenStart(t *testing.T) {
	const (
		wantStreamSID = "SSconnected01"
		wantCallSID   = "CAconnected01"
	)
	received := make(chan twilio.Frame, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
			received <- f
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	// Send connected then start.
	sendConnected(t, conn)
	sendStart(t, conn, wantStreamSID, wantCallSID)

	// Handler must receive the start frame unchanged.
	select {
	case f := <-received:
		if f.Event != twilio.EventStart {
			t.Errorf("Event = %q, want %q", f.Event, twilio.EventStart)
		}
		if f.StreamSID != wantStreamSID {
			t.Errorf("StreamSID = %q, want %q", f.StreamSID, wantStreamSID)
		}
		if f.CallSID != wantCallSID {
			t.Errorf("CallSID = %q, want %q", f.CallSID, wantCallSID)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler not called within 2s after connected+start frames")
	}
}

// Regression: start first — the server still accepts start as the first frame
// (no connected before it) without error and passes it to HandleStream.
func TestServeStreams_StartFirst_StillWorks(t *testing.T) {
	const (
		wantStreamSID = "SSnofirst01"
		wantCallSID   = "CAnofirst01"
	)
	received := make(chan twilio.Frame, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
			received <- f
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	// Send start directly (no connected).
	sendStart(t, conn, wantStreamSID, wantCallSID)

	// Handler must receive the start frame unchanged.
	select {
	case f := <-received:
		if f.Event != twilio.EventStart {
			t.Errorf("Event = %q, want %q", f.Event, twilio.EventStart)
		}
		if f.StreamSID != wantStreamSID {
			t.Errorf("StreamSID = %q, want %q", f.StreamSID, wantStreamSID)
		}
		if f.CallSID != wantCallSID {
			t.Errorf("CallSID = %q, want %q", f.CallSID, wantCallSID)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler not called within 2s after start frame")
	}
}

// --- AATK-19: caller From threaded from the voice webhook to the stream ---

// Frame.From is populated for an authorized voice call whose webhook fired
// first: ServeHTTP records From keyed by CallSid, and ServeStreams looks it up
// by the start frame's CallSID before calling HandleStream.
func TestServeStreams_FromThreadedFromWebhook(t *testing.T) {
	const from = "+15105550123"
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}

	form := url.Values{"From": {from}, "CallSid": {"CA1"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	received := make(chan twilio.Frame, 1)
	s.HandleStream = func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
		received <- f
		return nil
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)
	sendStart(t, conn, "SS1", "CA1")

	select {
	case f := <-received:
		if f.From != from {
			t.Fatalf("Frame.From = %q, want %q", f.From, from)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called within 2s")
	}
}

// Error: connected then non-start — if connected is followed by something other
// than start, the server must close with StatusPolicyViolation.
func TestServeStreams_ConnectedThenNonStart_Rejects(t *testing.T) {
	handlerCalled := make(chan struct{}, 1)
	s := &twilio.Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			handlerCalled <- struct{}{}
			return nil
		},
	}
	srv := streamsServer(t, s)
	conn := dialStreams(t, srv)

	// Send connected then media (not start) — violates the protocol.
	sendConnected(t, conn)
	media, err := twilio.EncodeMedia("SSconnectedbad", make([]byte, 160))
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, conn, media)

	// Server must close the connection with StatusPolicyViolation.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	if readErr == nil {
		t.Error("expected connection to be closed after connected+non-start, but Read succeeded")
	}

	// HandleStream must not have been called.
	select {
	case <-handlerCalled:
		t.Error("HandleStream was called despite protocol violation (connected then non-start)")
	default:
	}
}
