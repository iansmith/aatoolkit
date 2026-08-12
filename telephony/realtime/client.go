package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"

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
func Dial(ctx context.Context, url string, opts ...DialOption) (*Client, error) {
	var cfg dialConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// The handshake response never needs closing — coder/websocket owns it.
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		// redactUserinfo keeps credentials out of logs: a ws:// URL may carry
		// user:password, and this error is the one thing guaranteed to be logged.
		return nil, fmt.Errorf("realtime: dial %s: %w", redactUserinfo(url), err)
	}
	c := &Client{conn: conn}

	if err := c.send(ctx, newSessionUpdate(cfg.instructions)); err != nil {
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

// DialOption configures the session negotiated by Dial. Supplying none
// produces the handshake this package sent before options existed.
type DialOption func(*dialConfig)

type dialConfig struct {
	instructions string
}

// WithInstructions sets the session persona the backend is told to adopt. The
// protocol's session object defines this field; a backend that builds its
// runtime config from the session request reads it and prepends it as a system
// message. Empty omits the field rather than sending "".
func WithInstructions(s string) DialOption {
	return func(c *dialConfig) { c.instructions = s }
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
		return ServerEvent{}, fmt.Errorf("realtime: reading server event: %w", err)
	}
	var ev ServerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return ServerEvent{}, fmt.Errorf("realtime: decoding server event: %w", err)
	}
	// Raw must be the verbatim wire bytes, not a re-marshal of ev: a re-marshal
	// would re-order keys and drop fields this package does not model. Copy
	// defensively — data is the connection's read buffer and must not be
	// retained past this call.
	ev.Raw = append([]byte(nil), data...)
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

// redactUserinfo strips any credentials from a URL so it is safe to log. A URL
// that will not parse is reported as-is: it cannot contain structured userinfo,
// and hiding a malformed address makes the error harder to act on.
func redactUserinfo(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = neturl.User("redacted")
	return u.String()
}

// send marshals and writes one client event.
func (c *Client) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("realtime: marshal client event: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}
