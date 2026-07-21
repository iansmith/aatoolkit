package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// stubWSServer uses raw HTTP hijacking so no external WS library is needed in
// the test. The returned channel is closed once *received has been assigned
// (on every return path, including early errors) — the handler runs in its
// own goroutine (httptest.Server's connection goroutine), so a caller that
// reads *received without waiting on this channel has no happens-before
// relationship with the write and races the handler under -race.
func stubWSServer(t *testing.T, received *[]byte) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected WebSocket upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		// received is the start frame: readHandshake consumes the connected
		// frame that precedes it and asserts that order.
		*received = readHandshake(t, buf)
	}))
	return srv, done
}

// waitStubWSServer blocks until the given stubWSServer handler goroutine has
// finished (and thus has safely published *received), or fails the test on
// timeout rather than hanging forever on a genuine handler bug.
func waitStubWSServer(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stubWSServer handler to finish")
	}
}

func TestDial_SendsStartEvent(t *testing.T) {
	var received []byte
	srv, done := stubWSServer(t, &received)
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")
	callSid := newSID("CA")

	if err := dial(context.Background(), callSid, addr); err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitStubWSServer(t, done)

	var m map[string]any
	if err := json.Unmarshal(received, &m); err != nil {
		t.Fatalf("server received non-JSON: %q: %v", received, err)
	}
	if m["event"] != "start" {
		t.Errorf("first message event: got %v, want start", m["event"])
	}
	streamSID, ok := m["streamSid"].(string)
	if !ok || streamSID == "" {
		t.Errorf("streamSid: got %v, want non-empty string", m["streamSid"])
	}
	startObj, ok := m["start"].(map[string]any)
	if !ok {
		t.Fatalf("start field missing or wrong type")
	}
	callSID, ok := startObj["callSid"].(string)
	if !ok || callSID != callSid {
		t.Errorf("start.callSid: got %v, want %q", startObj["callSid"], callSid)
	}
}

// TestDial_SendsConnectedBeforeStart pins twilio-cli's opening handshake to
// Twilio's: a connected frame, then start. the server tolerates a missing
// connected frame (ServeStreams consumes one only if the first frame is one),
// so this gap cost nothing at runtime and went unnoticed — but twilio-cli
// exists to prove the server speaks the protocol, and a fake that skips a frame
// the real Twilio always sends cannot prove that.
func TestDial_SendsConnectedBeforeStart(t *testing.T) {
	frames := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		for i := 0; i < 2; i++ {
			msg, err := readWSFrame(buf)
			if err != nil {
				t.Errorf("read frame %d: %v", i+1, err)
				return
			}
			frames <- msg
		}
		close(frames)
	}))
	defer srv.Close()

	if err := dial(context.Background(), newSID("CA"), "ws"+strings.TrimPrefix(srv.URL, "http")); err != nil {
		t.Fatalf("dial: %v", err)
	}

	want := []string{"connected", "start"}
	for i, wantEvent := range want {
		var msg []byte
		select {
		case msg = <-frames:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for frame %d (want %s)", i+1, wantEvent)
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			t.Fatalf("frame %d is not JSON: %q: %v", i+1, msg, err)
		}
		if m["event"] != wantEvent {
			t.Errorf("frame %d: event = %v, want %s (Twilio's opening order is %v)", i+1, m["event"], wantEvent, want)
		}
	}
}

// TestDial_ConnectedFrameMatchesTwilioShape: the connected frame carries the
// protocol/version fields Twilio sends, not just the right event name.
func TestDial_ConnectedFrameMatchesTwilioShape(t *testing.T) {
	raw, err := twilio.EncodeConnected()
	if err != nil {
		t.Fatalf("EncodeConnected: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("connected frame is not JSON: %q: %v", raw, err)
	}
	for field, want := range map[string]string{
		"event":    "connected",
		"protocol": "Call",
		"version":  "1.0.0",
	} {
		if m[field] != want {
			t.Errorf("connected.%s = %v, want %q", field, m[field], want)
		}
	}
}

func TestDial_ReturnsOnServerClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		conn.Close()
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	if err := dial(context.Background(), newSID("CA"), addr); err != nil {
		t.Errorf("dial after server close: %v", err)
	}
}

func TestDial_RefusedConnectionReturnsError(t *testing.T) {
	err := dial(context.Background(), newSID("CA"), "ws://127.0.0.1:1")
	if err == nil {
		t.Error("dial to refused port must return an error")
	}
}

func TestDial_CancelledContextReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := dial(ctx, newSID("CA"), "ws://127.0.0.1:1")
	if err == nil {
		t.Error("dial with cancelled context must return an error")
	}
}

// TestDial_StartEventSIDsAreDistinct asserts distinct streamSIDs are minted
// across separate dials, now that CallSid is caller-supplied (single-sourced
// per PRD D12) rather than internally minted by dial.
func TestDial_StartEventSIDsAreDistinct(t *testing.T) {
	callSid := newSID("CA")

	var received1 []byte
	srv1, done1 := stubWSServer(t, &received1)
	defer srv1.Close()
	if err := dial(context.Background(), callSid, "ws"+strings.TrimPrefix(srv1.URL, "http")); err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	waitStubWSServer(t, done1)

	var received2 []byte
	srv2, done2 := stubWSServer(t, &received2)
	defer srv2.Close()
	if err := dial(context.Background(), callSid, "ws"+strings.TrimPrefix(srv2.URL, "http")); err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	waitStubWSServer(t, done2)

	var m1, m2 map[string]any
	if err := json.Unmarshal(received1, &m1); err != nil {
		t.Fatalf("json.Unmarshal 1: %v", err)
	}
	if err := json.Unmarshal(received2, &m2); err != nil {
		t.Fatalf("json.Unmarshal 2: %v", err)
	}
	streamSID1, _ := m1["streamSid"].(string)
	streamSID2, _ := m2["streamSid"].(string)
	if streamSID1 == "" || streamSID2 == "" {
		t.Fatalf("expected non-empty streamSIDs, got %q and %q", streamSID1, streamSID2)
	}
	if streamSID1 == streamSID2 {
		t.Errorf("streamSIDs across separate dials must be distinct, both are %q", streamSID1)
	}
}

// TestDial_SendsStopFrameOnCancel asserts that cancelling ctx (as SIGINT
// does) causes dial to send a stop frame before closing the WebSocket.
func TestDial_SendsStopFrameOnCancel(t *testing.T) {
	startReceived := make(chan struct{})
	stopReceived := make(chan []byte, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		readHandshake(t, buf) // connected + start
		close(startReceived)

		msg, err := readWSFrame(buf)
		if err != nil {
			t.Errorf("read stop frame: %v", err)
			return
		}
		stopReceived <- msg
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	dialErrCh := make(chan error, 1)
	go func() {
		dialErrCh <- dial(ctx, newSID("CA"), addr)
	}()

	select {
	case <-startReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start frame")
	}

	cancel()

	var stopMsg []byte
	select {
	case stopMsg = <-stopReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stop frame")
	}

	<-dialErrCh

	var m map[string]any
	if err := json.Unmarshal(stopMsg, &m); err != nil {
		t.Fatalf("server received non-JSON: %q: %v", stopMsg, err)
	}
	if m["event"] != "stop" {
		t.Errorf("frame after cancel: event = %v, want stop", m["event"])
	}
}

// --- SOP-126 ---

// withFakeMic overrides the package-level streamMic seam for the duration of
// the test, restoring the original afterward. Real mic capture (ffmpeg +
// avfoundation) is environment-dependent (device permissions, hardware) —
// these protocol-level tests should not depend on its timing.
func withFakeMic(t *testing.T, fn func(ctx context.Context, conn *websocket.Conn, streamSID string) error) {
	t.Helper()
	original := streamMic
	streamMic = fn
	t.Cleanup(func() { streamMic = original })
}

// TestDial_NoStopFrameOnServerClose asserts that a SERVER-initiated close (the
// farewell / idle-timeout hangup) does NOT make twilio-cli send a stop frame — the
// server already closed the socket, so the write only draws a "broken pipe". AATK-7:
// a regression from AATK-2, which sends stop unconditionally when the mic goroutine
// returns, including when the read loop's cancelMic ends it on a server close. The bug
// is a goroutine race (the stop goroutine's select can pick micStopped or readCtx.Done),
// so this asserts on the log with a short settle; run under -count to exercise the race.
func TestDial_NoStopFrameOnServerClose(t *testing.T) {
	// The read loop's cancelMic ends the mic on a server close; a fake mic that runs
	// until its context is cancelled reproduces that (streamMic returns via
	// cancellation, i.e. naturalEnd=false — NOT on its own).
	withFakeMic(t, func(ctx context.Context, _ *websocket.Conn, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // consume connected + start
		conn.Close()          // server-initiated close
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	out := captureLog(t, func() {
		if err := dial(context.Background(), newSID("CA"), addr); err != nil {
			t.Errorf("dial after server close: %v", err)
		}
		time.Sleep(100 * time.Millisecond) // let any stray stop goroutine (mis)fire into the captured log
	})

	if strings.Contains(out, "send stop") {
		t.Errorf("twilio-cli sent a stop frame after a server-initiated close (broken pipe):\n%s", out)
	}
}

// blockingMic simulates a long-running capture that only stops when ctx is
// cancelled — mirrors real streamMicFrames' shape without touching hardware.
func blockingMic(ctx context.Context, _ *websocket.Conn, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// trackConn registers a hijacked net.Conn for forced close at test end,
// regardless of pass/fail/timeout. Without this, a red test that times out
// via t.Fatal leaves its server-side goroutine (and the client's dial()
// goroutine) blocked on a Read that will never complete, leaking sockets
// and goroutines into later tests in the same process.
func trackConn(t *testing.T, conn net.Conn) {
	t.Helper()
	t.Cleanup(func() { conn.Close() })
}

// dialCtx returns a context bounded to timeout, cancelled at test end. Used
// instead of context.Background() so a hung dial() (and its mic goroutine)
// cannot outlive the test that started it.
func dialCtx(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// TestCLI_MarkEcho covers observable behavior 1: dial() estimates the
// playout duration of preceding downlink audio from byte count (8 kHz
// μ-law) and echoes the mark back to the server after that delay.
func TestCLI_MarkEcho(t *testing.T) {
	withFakeMic(t, blockingMic)

	echoReceived := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		readHandshake(t, buf) // connected + start

		const streamSID = "SS_markecho"
		mediaMsg, err := twilio.EncodeMedia(streamSID, make([]byte, muLawFrame20ms))
		if err != nil {
			t.Errorf("EncodeMedia: %v", err)
			return
		}
		if err := writeWSFrame(conn, mediaMsg); err != nil {
			t.Errorf("write media frame: %v", err)
			return
		}

		markMsg, err := twilio.EncodeMark(streamSID, "mark1")
		if err != nil {
			t.Errorf("EncodeMark: %v", err)
			return
		}
		if err := writeWSFrame(conn, markMsg); err != nil {
			t.Errorf("write mark frame: %v", err)
			return
		}

		echoRaw, err := readWSFrame(buf)
		if err != nil {
			t.Errorf("read mark echo: %v", err)
			return
		}
		echoReceived <- echoRaw
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx := dialCtx(t, 5*time.Second)
	dialErrCh := make(chan error, 1)
	go func() { dialErrCh <- dial(ctx, newSID("CA"), addr) }()

	select {
	case echoRaw := <-echoReceived:
		var m map[string]any
		if err := json.Unmarshal(echoRaw, &m); err != nil {
			t.Fatalf("echo not JSON: %q: %v", echoRaw, err)
		}
		if m["event"] != "mark" {
			t.Errorf("echo event = %v, want mark", m["event"])
		}
		markObj, ok := m["mark"].(map[string]any)
		if !ok {
			t.Fatalf("echo mark field missing")
		}
		if markObj["name"] != "mark1" {
			t.Errorf("echo mark name = %v, want mark1", markObj["name"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for mark echo")
	}

	select {
	case err := <-dialErrCh:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dial: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dial to return after server close")
	}
}

// TestCLI_MarkEchoRepeats is an adversary gap test (Step 0f): a real call
// exchanges many marks, not one. TestCLI_MarkEcho alone would still pass a
// broken implementation that only echoes the first mark it ever sees (e.g.
// a stray sync.Once guard, or a byte counter that's never reset between
// marks). This sends two marks, each preceded by its own media frame, and
// asserts both are echoed back by name, in order.
func TestCLI_MarkEchoRepeats(t *testing.T) {
	withFakeMic(t, blockingMic)

	echoReceived := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		readHandshake(t, buf) // connected + start

		const streamSID = "SS_repeatmark"
		sendMarkedMedia := func(frames int, markName string) {
			mediaMsg, err := twilio.EncodeMedia(streamSID, make([]byte, frames*muLawFrame20ms))
			if err != nil {
				t.Errorf("EncodeMedia: %v", err)
				return
			}
			if err := writeWSFrame(conn, mediaMsg); err != nil {
				t.Errorf("write media frame: %v", err)
				return
			}
			markMsg, err := twilio.EncodeMark(streamSID, markName)
			if err != nil {
				t.Errorf("EncodeMark: %v", err)
				return
			}
			if err := writeWSFrame(conn, markMsg); err != nil {
				t.Errorf("write mark frame: %v", err)
				return
			}
			echoRaw, err := readWSFrame(buf)
			if err != nil {
				t.Errorf("read echo for %s: %v", markName, err)
				return
			}
			echoReceived <- echoRaw
		}

		sendMarkedMedia(1, "mark1")
		sendMarkedMedia(2, "mark2")
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx := dialCtx(t, 5*time.Second)
	dialErrCh := make(chan error, 1)
	go func() { dialErrCh <- dial(ctx, newSID("CA"), addr) }()

	wantNames := []string{"mark1", "mark2"}
	for i, want := range wantNames {
		select {
		case echoRaw := <-echoReceived:
			var m map[string]any
			if err := json.Unmarshal(echoRaw, &m); err != nil {
				t.Fatalf("echo[%d] not JSON: %q: %v", i, echoRaw, err)
			}
			if m["event"] != "mark" {
				t.Errorf("echo[%d] event = %v, want mark", i, m["event"])
			}
			markObj, ok := m["mark"].(map[string]any)
			if !ok {
				t.Fatalf("echo[%d] mark field missing", i)
			}
			if markObj["name"] != want {
				t.Errorf("echo[%d] mark name = %v, want %s", i, markObj["name"], want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for echo[%d] (%s)", i, want)
		}
	}

	select {
	case err := <-dialErrCh:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dial: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dial to return after server close")
	}
}

// TestCLI_ServerClose covers observable behavior 2 (server-initiated
// hangup): when the server closes the WebSocket, dial() must return without
// error. This behavior already exists from SOP-125 (see
// TestDial_ReturnsOnServerClose) — this test locks it in under the ticket's
// required name since dial.go's read loop is modified further by this
// ticket (mark echo, clear handling) and could regress it.
func TestCLI_ServerClose(t *testing.T) {
	withFakeMic(t, blockingMic)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		trackConn(t, conn)
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		// Drain the start frame before closing: closing a socket while
		// unread data still sits in its receive buffer can trigger an OS
		// RST instead of a clean FIN, which would surface as a spurious
		// read error in dial() and make this test flaky under load.
		_, _ = readWSFrame(buf) // connected
		_, _ = readWSFrame(buf) // start
		conn.Close()
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx := dialCtx(t, 5*time.Second)
	dialErrCh := make(chan error, 1)
	go func() { dialErrCh <- dial(ctx, newSID("CA"), addr) }()

	select {
	case err := <-dialErrCh:
		if err != nil {
			t.Errorf("dial after server close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dial to exit after server close")
	}
}

// TestCLI_CallerHangup covers observable behavior 2 (caller-initiated
// hangup via capture EOF, distinct from Ctrl-C/ctx-cancel, already covered
// by TestDial_SendsStopFrameOnCancel): when the mic capture stream ends on
// its own, dial() must send a stop frame before closing — not sit blocked
// waiting for the next server message forever.
func TestCLI_CallerHangup(t *testing.T) {
	withFakeMic(t, func(ctx context.Context, conn *websocket.Conn, streamSID string) error {
		// Give the start frame a moment to go out before "capture" ends, so
		// the wire order (start, then stop) is deterministic.
		time.Sleep(50 * time.Millisecond)
		return nil // simulates the capture stream ending cleanly (EOF)
	})

	stopReceived := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		readHandshake(t, buf) // connected + start
		msg, err := readWSFrame(buf)
		if err != nil {
			t.Errorf("read stop frame: %v", err)
			return
		}
		stopReceived <- msg
	}))
	defer srv.Close()
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx := dialCtx(t, 5*time.Second)
	dialErrCh := make(chan error, 1)
	go func() { dialErrCh <- dial(ctx, newSID("CA"), addr) }()

	var stopMsg []byte
	select {
	case stopMsg = <-stopReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stop frame after capture EOF")
	}

	var m map[string]any
	if err := json.Unmarshal(stopMsg, &m); err != nil {
		t.Fatalf("server received non-JSON: %q: %v", stopMsg, err)
	}
	if m["event"] != "stop" {
		t.Errorf("frame after capture EOF: event = %v, want stop", m["event"])
	}

	select {
	case err := <-dialErrCh:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dial: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dial to return")
	}
}

// TestCLI_NoEchoMarks covers observable behavior 5: --no-echo-marks
// suppresses mark echoing. Verified differentially: the same mark, sent to
// two separate dial() calls, is echoed when the flag is absent and is NOT
// echoed when the flag is present — proving the flag actually changes
// behavior, rather than merely observing that echoing never happens to be
// implemented at all.
func TestCLI_NoEchoMarks(t *testing.T) {
	withFakeMic(t, blockingMic)

	runMarkScenario := func(t *testing.T, opts ...dialOption) (echoed bool) {
		t.Helper()
		gotEcho := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			trackConn(t, conn)
			defer conn.Close()
			wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

			readHandshake(t, buf) // connected + start

			const streamSID = "SS_noecho"
			markMsg, err := twilio.EncodeMark(streamSID, "mark1")
			if err != nil {
				t.Errorf("EncodeMark: %v", err)
				return
			}
			if err := writeWSFrame(conn, markMsg); err != nil {
				t.Errorf("write mark frame: %v", err)
				return
			}

			_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := readWSFrame(buf); err == nil {
				gotEcho <- struct{}{}
			}
		}))
		defer srv.Close()
		addr := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx := dialCtx(t, 5*time.Second)
		dialErrCh := make(chan error, 1)
		go func() { dialErrCh <- dial(ctx, newSID("CA"), addr, opts...) }()

		select {
		case <-gotEcho:
			echoed = true
		case <-time.After(1 * time.Second):
			echoed = false
		}
		<-dialErrCh
		return echoed
	}

	if !runMarkScenario(t) {
		t.Error("without --no-echo-marks: expected mark to be echoed, but it was not")
	}
	if runMarkScenario(t, withNoEchoMarks()) {
		t.Error("with --no-echo-marks: expected mark NOT to be echoed, but it was")
	}
}

// TestDial_PeerClosesBetweenHandshakeFrames: a peer that hangs up after the
// connected frame and before the start frame has ended the call, not failed
// it, so dial returns nil.
//
// The server reads the connected frame before closing, which also pins that
// the frame is really sent and really consumable -- the close then lands
// between the two writes by construction rather than by timing.
//
// It does NOT pin the EPIPE path specifically, and deliberately does not try
// to. Whether a hung-up peer surfaces to the writer as EPIPE or to the reader
// as EOF depends on whether the kernel has already accepted the pending write
// -- isCallEnded's own comment calls that a race. Forcing EPIPE here would
// mean controlling socket buffering from a test, which would pin the
// operating system rather than dial. The behaviour above is what callers
// depend on either way; the predicate itself is covered exhaustively by
// TestIsCallEnded, which is where EPIPE is asserted.
func TestDial_PeerClosesBetweenHandshakeFrames(t *testing.T) {
	gotConnected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))

		msg, err := readWSFrame(buf)
		if err != nil {
			t.Errorf("read connected frame: %v", err)
			conn.Close()
			return
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil || m["event"] != "connected" {
			t.Errorf("first frame: got %q, want a connected frame", msg)
		}
		close(gotConnected)
		conn.Close() // hang up before the start frame is written
	}))
	defer srv.Close()

	err := dial(context.Background(), newSID("CA"), "ws"+strings.TrimPrefix(srv.URL, "http"))

	select {
	case <-gotConnected:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the connected frame")
	}
	if err != nil {
		t.Errorf("a peer hanging up mid-handshake is a call ending, not a failure: dial returned %v", err)
	}
}
