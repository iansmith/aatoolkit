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
// Dial has a negotiated session; one that errors has no connection to close.
func Dial(ctx context.Context, url string) (*Client, error) {
	return nil, fmt.Errorf("realtime: Dial not implemented")
}

// AppendAudio forwards one carrier frame's payload to the backend. payload is
// base64 G.711 and is placed on the wire unchanged.
func (c *Client) AppendAudio(ctx context.Context, payload string) error {
	return fmt.Errorf("realtime: AppendAudio not implemented")
}

// Read returns the next server event. Events whose type this package does not
// model are returned with their Type set and the rest zero — Read never fails
// on an unrecognised type. It returns an error when the connection ends.
func (c *Client) Read(ctx context.Context) (ServerEvent, error) {
	return ServerEvent{}, fmt.Errorf("realtime: Read not implemented")
}

// Close shuts the connection down. It is safe to call on an already-closed
// client.
func (c *Client) Close() error {
	return fmt.Errorf("realtime: Close not implemented")
}

// send marshals and writes one client event.
func (c *Client) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("realtime: marshal client event: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}
