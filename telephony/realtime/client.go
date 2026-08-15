package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"

	"github.com/coder/websocket"
)

// Client is one websocket connection to a realtime voice backend. It owns the
// connection's lifetime only in the sense that Close shuts it down; it never
// reconnects. Reconnect policy belongs to whatever owns the call, which knows
// whether a dropped backend should end the call or be retried.
type Client struct {
	conn *websocket.Conn

	// writeSem serialises every client-originated write (AppendAudio, Send, the
	// Dial handshake) through one chokepoint. coder/websocket's own Conn
	// already guarantees safe concurrent Write calls per its documented
	// contract (conn.go, backed by write.go's writeFrameMu), so this is
	// defensive redundancy against a future library change rather than
	// something load-bearing today — kept anyway because relying solely on a
	// dependency's internal locking is fragile.
	//
	// A one-slot channel rather than a sync.Mutex, and THAT part is
	// load-bearing: acquisition has to honour ctx. Callers do NOT all carry a
	// deadline, and that asymmetry is the whole hazard — a consumer event is
	// written under a bounded context, while carrier audio is written on the
	// call's own context, which has none. coder/websocket's own write locks
	// are context-aware for exactly that reason (conn.go's mu.lock selects on
	// ctx.Done()). sync.Mutex.Lock cannot be cancelled, so a plain mutex here
	// would put an UNBOUNDED wait in front of a bounded write and silently
	// void the caller's deadline — measured: with the carrier pump parked in
	// conn.Write on the unbounded call context, a consumer Send carrying a 5 s
	// deadline was still blocked 9 s later, wedging the select loop that
	// observes every way a call can end.
	writeSem chan struct{}
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
	var dialOpts *websocket.DialOptions
	if cfg.httpClient != nil {
		dialOpts = &websocket.DialOptions{HTTPClient: cfg.httpClient}
	}
	conn, _, err := websocket.Dial(ctx, url, dialOpts)
	if err != nil {
		// redactUserinfo keeps credentials out of logs: a ws:// URL may carry
		// user:password, and this error is the one thing guaranteed to be logged.
		return nil, fmt.Errorf("realtime: dial %s: %w", redactUserinfo(url), err)
	}
	c := &Client{conn: conn, writeSem: make(chan struct{}, 1)}

	// buildSessionUpdate, not c.send(newSessionUpdate(...)): cfg.tools must
	// reach the wire unmodified, and c.send's json.Marshal path re-escapes
	// and re-compacts a json.RawMessage field — see buildSessionUpdate's doc
	// comment for the measured failure this avoids.
	handshake, err := buildSessionUpdate(cfg.instructions, cfg.voice, cfg.tools)
	if err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("realtime: building %s: %w", EventSessionUpdate, err)
	}
	if err := c.write(ctx, handshake); err != nil {
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
	voice        string
	tools        json.RawMessage
	httpClient   *http.Client
}

// WithInstructions sets the session persona the backend is told to adopt. The
// protocol's session object defines this field; a backend that builds its
// runtime config from the session request reads it and prepends it as a system
// message. Empty omits the field rather than sending "".
func WithInstructions(s string) DialOption {
	return func(c *dialConfig) { c.instructions = s }
}

// WithVoice sets the backend's output voice for the session. The protocol's
// session object defines this field on the output side of the audio spec; a
// backend that supports multiple voices reads it and answers in the named
// one. This package does not validate or normalize the name — the backend
// owns which names it accepts, so whatever is supplied here reaches the wire
// unmodified. Empty omits the field rather than sending "".
func WithVoice(name string) DialOption {
	return func(c *dialConfig) { c.voice = name }
}

// WithTools declares the consumer's tool definitions for this session
// (AATK-85). This package models nothing about what a tool is — the
// consumer owns tool schemas entirely — so tools is carried as
// json.RawMessage rather than a typed struct: a typed struct would
// re-encode the consumer's bytes on the way to the wire (reordering keys,
// dropping unknown fields, re-escaping '<', '>', '&'), and the declared
// tools must reach the backend unmodified. Nil (the default) omits the
// field entirely, producing the handshake this package sent before tools
// existed. A tools value that is not syntactically valid JSON makes Dial
// return an error rather than sending a malformed handshake.
func WithTools(tools json.RawMessage) DialOption {
	return func(c *dialConfig) { c.tools = tools }
}

// WithHTTPClient overrides the HTTP client used for the WebSocket dial's
// handshake, giving control over the underlying transport.
//
// This option exists FOR TESTING ONLY — specifically, to inject a
// transport-level fault (e.g. a connection whose Write calls can be made to
// fail on demand while Read keeps working) so a test can drive send-failure
// handling deterministically. Nil (the default) uses coder/websocket's own
// default transport. Production callers should never need this option; if
// you find yourself reaching for it outside a test, that is a sign this
// package needs a different seam.
func WithHTTPClient(hc *http.Client) DialOption {
	return func(c *dialConfig) { c.httpClient = hc }
}

// AppendAudio forwards one carrier frame's payload to the backend. payload is
// base64 G.711 and is placed on the wire unchanged.
func (c *Client) AppendAudio(ctx context.Context, payload string) error {
	return c.send(ctx, audioAppend{Type: EventAudioAppend, Audio: payload})
}

// Send writes one consumer-supplied client event to the backend, verbatim.
// This package does not model, validate, or interpret event — the consumer
// owns its own protocol correctness (session.update, conversation.item.create,
// response.create, function-call output, or anything else the backend
// accepts).
//
// event's bytes go on the wire exactly as supplied: unlike send(), this does
// NOT go through json.Marshal, which would re-escape '<', '>', '&' and
// recompact whitespace on a json.RawMessage. It goes through the same write
// chokepoint as AppendAudio, so the two are safe to call concurrently.
func (c *Client) Send(ctx context.Context, event json.RawMessage) error {
	return c.write(ctx, event)
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
	// would re-order keys and drop fields this package does not model.
	//
	// The copy is insurance, not a fix for a live aliasing bug: websocket.Conn.Read
	// returns a freshly allocated slice per message (io.ReadAll on the frame
	// reader), so data is not a shared buffer today. It is one alloc per event on
	// a path that already allocates one, and it means Raw survives any future
	// pooling on the transport rather than turning into a silent corruption.
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
	return c.write(ctx, b)
}

// write is the single chokepoint every client-originated write funnels
// through: the Dial handshake, AppendAudio (via send), and Send. Serialising
// here means AppendAudio and Send are safe to call concurrently.
//
// The wait for the slot is taken under ctx, so a caller's deadline covers the
// whole write — the queueing behind a slower writer as well as the write
// itself. See writeSem for why that is not optional.
func (c *Client) write(ctx context.Context, b []byte) error {
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("realtime: awaiting write slot: %w", ctx.Err())
	}
	defer func() { <-c.writeSem }()
	return c.conn.Write(ctx, websocket.MessageText, b)
}
