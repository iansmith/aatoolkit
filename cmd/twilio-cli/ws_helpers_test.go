package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
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
