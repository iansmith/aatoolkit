package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// readHandshake consumes twilio-cli's two opening frames -- connected, then
// start, the order real Twilio uses -- and returns the start frame's raw
// JSON. Any test reading a later frame must consume both first.
//
// It asserts the order rather than skipping blindly, so every test that talks
// to twilio-cli also holds the opening handshake to the protocol.
func readHandshake(t *testing.T, buf *bufio.ReadWriter) []byte {
	t.Helper()

	connected, err := readWSFrame(buf)
	if err != nil {
		t.Errorf("read connected frame: %v", err)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(connected, &m); err != nil {
		t.Errorf("connected frame is not JSON: %q: %v", connected, err)
		return nil
	}
	if m["event"] != "connected" {
		t.Errorf("first frame event: got %v, want connected — Twilio opens every Media Stream with it before start", m["event"])
	}

	start, err := readWSFrame(buf)
	if err != nil {
		t.Errorf("read start frame: %v", err)
		return nil
	}
	return start
}

// hijackedWSServer is the shape most of twilio-cli's protocol tests give their
// far end: hijack the connection, complete the upgrade, consume the opening
// handshake, and hand the raw conn and buffer to serve.
//
// It owns the server's lifetime as well as the connection's, and joins the
// handler goroutine before releasing either. That join is load-bearing, not
// tidiness. httptest.Server.Close does not wait for a hijacked connection's
// goroutine, so without it two things go wrong on a test whose dial returns
// before the handler has been scheduled: closing the server pulls the socket
// out from under the handler, which surfaces as an intermittent "read
// connected frame: use of closed network connection" -- an error that names
// the handshake and means nothing of the kind -- and the readHandshake call
// below, which takes t, can reach a t whose test has already finished, which
// panics the whole binary with "Log in goroutine after Test has completed".
//
// The conn is closed by this cleanup rather than by trackConn, and only after
// the join has given up. trackConn registers its close from inside the handler
// -- so it is registered LAST and, cleanups being LIFO, would run FIRST, which
// is precisely the close-out-from-under this join exists to prevent: a handler
// still inside readHandshake does not merely unblock, it reports the closed
// socket as a handshake failure. A handler behind on scheduling is waited for;
// only one genuinely wedged past the timeout gets its socket pulled, which is
// the leak trackConn was there to stop.
//
// It exists because that prologue had been copied into fifteen servers across
// three files (dial_test.go x11, playback_test.go x3, record_test.go x1),
// several of which had dropped trackConn along the way and leaked the client's
// dial goroutine on any test that timed out.
//
// Five servers still build their own, each because it has to do something
// before or instead of this prologue: stubWSServer checks the Upgrade header
// first and needs the start frame back, TestDial_SendsConnectedBeforeStart and
// TestDial_PeerClosesBetweenHandshakeFrames read the handshake themselves
// because asserting on it is the point, TestCLI_ServerClose closes without
// reading it at all, and TestDial_ReturnsOnServerClose needs only the conn.
// Two others -- TestDial_NoStopFrameOnServerClose and TestCLI_NoEchoMarks --
// do match this shape and have not been migrated; that is worth doing, and is
// not this change.
func hijackedWSServer(t *testing.T, serve func(conn net.Conn, buf *bufio.ReadWriter)) *httptest.Server {
	t.Helper()
	served := make(chan struct{})
	var mu sync.Mutex
	var hijacked net.Conn
	var started bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started = true
		mu.Unlock()
		defer close(served)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		mu.Lock()
		hijacked = conn
		mu.Unlock()
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // connected + start
		serve(conn, buf)
	}))
	t.Cleanup(func() {
		// Bounded, and it closes either way. A test that failed before it ever
		// dialled has no handler to wait for, and a cleanup is the wrong place
		// to turn that into a second failure on top of the real one -- so the
		// timeout is only a failure when a handler actually started. The wait
		// itself still runs in that case, because "no handler yet" and "no
		// handler ever" are the same observation from here, and the whole
		// point of this join is to give one that is merely behind on
		// scheduling its chance to arrive.
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			// Wedged, not merely behind: pull the socket to unblock whatever
			// read it is parked in, then give it a moment to unwind so it
			// cannot log against a finished t.
			mu.Lock()
			conn := hijacked
			mu.Unlock()
			if conn != nil {
				conn.Close()
			}
			select {
			case <-served:
			case <-time.After(time.Second):
				mu.Lock()
				ran := started
				mu.Unlock()
				if ran {
					t.Error("timed out waiting for the hijacked handler goroutine to finish")
				}
			}
		}
		srv.Close()
	})
	return srv
}

// wsHandshake must be called after hijacking the conn to complete the WebSocket upgrade.
func wsHandshake(conn net.Conn, key string) {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	_, _ = conn.Write([]byte(resp))
}

// readWSFrame unmasks client→server frames per RFC 6455 before returning the payload.
func readWSFrame(buf *bufio.ReadWriter) ([]byte, error) {
	r := buf.Reader
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("read frame header: %w", err)
	}

	masked := header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7f)

	switch payloadLen {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, fmt.Errorf("read 16-bit length: %w", err)
		}
		payloadLen = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, fmt.Errorf("read 64-bit length: %w", err)
		}
		payloadLen = int(ext[0])<<56 | int(ext[1])<<48 | int(ext[2])<<40 | int(ext[3])<<32 |
			int(ext[4])<<24 | int(ext[5])<<16 | int(ext[6])<<8 | int(ext[7])
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return nil, fmt.Errorf("read mask key: %w", err)
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	if masked {
		for i, b := range payload {
			payload[i] = b ^ maskKey[i%4]
		}
	}
	return payload, nil
}

// writeWSFrame writes a single unmasked text frame (server→client direction,
// where RFC 6455 does not require masking) carrying payload.
func writeWSFrame(w io.Writer, payload []byte) error {
	n := len(payload)
	var header []byte
	switch {
	case n <= 125:
		header = []byte{0x81, byte(n)}
	case n <= 65535:
		header = []byte{0x81, 126, byte(n >> 8), byte(n)}
	default:
		return fmt.Errorf("writeWSFrame: payload too large: %d bytes", n)
	}
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("writeWSFrame: write header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("writeWSFrame: write payload: %w", err)
	}
	return nil
}

// writeWSText writes one unmasked server→client text frame. Server frames are
// never masked (RFC 6455 §5.1), which is what makes this the short half of the
// pair with readWSFrame.
//
// Only the two length forms these tests actually produce are handled: a
// payload of 126 bytes or more uses the 16-bit extended length, and anything
// needing the 64-bit form would be a frame no test here sends.
func writeWSText(conn net.Conn, payload []byte) error {
	var header []byte
	switch n := len(payload); {
	case n < 126:
		header = []byte{0x81, byte(n)}
	case n < 1<<16:
		header = []byte{0x81, 126, byte(n >> 8), byte(n)}
	default:
		return fmt.Errorf("writeWSText: payload of %d bytes needs the 64-bit length form", n)
	}
	if _, err := conn.Write(append(header, payload...)); err != nil {
		return fmt.Errorf("writeWSText: %w", err)
	}
	return nil
}
