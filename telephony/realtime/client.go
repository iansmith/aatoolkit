package realtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/websocket"
)

// Client is one websocket connection to a realtime voice backend. It owns the
// connection's lifetime only in the sense that Close shuts it down; it never
// reconnects. Reconnect policy belongs to whatever owns the call, which knows
// whether a dropped backend should end the call or be retried.
type Client struct {
	conn *websocket.Conn
}

// Dial connects to url, sends the session.update handshake, and waits for the
// backend's session.created before returning. A Client that comes back from
// Dial has a negotiated session; on any failure the connection is torn down and
// the returned Client is nil, so a half-open Client can never escape and defer
// its error to the first frame of a live call.
func Dial(ctx context.Context, url string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("realtime: dial %s: %w", url, err)
	}
	c := &Client{conn: conn}

	if err := c.send(ctx, newSessionUpdate()); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("realtime: sending %s: %w", EventSessionUpdate, err)
	}

	// Read until the session is acknowledged. A backend is free to emit other
	// events first; only session.created ends the handshake.
	for {
		ev, err := c.Read(ctx)
		if err != nil {
			conn.CloseNow()
			return nil, fmt.Errorf("realtime: awaiting %s: %w", EventSessionCreated, err)
		}
		if ev.Type == EventSessionCreated {
			return c, nil
		}
	}
}

// AppendAudio forwards one carrier frame's payload to the backend. payload is
// base64 G.711 and is placed on the wire unchanged.
func (c *Client) AppendAudio(ctx context.Context, payload string) error {
	return c.send(ctx, audioAppend{Type: EventAudioAppend, Audio: payload})
}

// Read returns the next server event. Events whose type this package does not
// model decode with their Type set and the rest zero — Read never fails on an
// unrecognised type, because the protocol is larger than the subset used here
// and a backend may legitimately emit more of it. It returns an error when the
// connection ends.
func (c *Client) Read(ctx context.Context) (ServerEvent, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return ServerEvent{}, err
	}
	var ev ServerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return ServerEvent{}, fmt.Errorf("realtime: decoding server event: %w", err)
	}
	return ev, nil
}

// Close shuts the connection down. It is safe on a nil Client and on one whose
// connection has already gone away.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

// send marshals and writes one client event.
func (c *Client) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("realtime: marshal client event: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}
