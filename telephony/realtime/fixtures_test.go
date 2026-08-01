package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Test fixtures and the one assertion that is green before any implementation:
// the framing arithmetic is a property of base64 and G.711, not of this package.
// It lives here, apart from the behavior tests, so the Phase 0 commit contains
// only tests observed failing against the unimplemented package.

// One 20 ms G.711 frame at 8 kHz: 160 samples, one byte each.
const frameBytes = 160

// frameB64 is a carrier payload as it arrives on the wire — base64, never decoded.
func frameB64() string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", frameBytes)))
}

func TestCarrierFrameEncodesTo216Characters(t *testing.T) {
	if got := len(frameB64()); got != 216 {
		t.Fatalf("160-byte G.711 frame must base64-encode to 216 chars, got %d", got)
	}
}

// fakeBackend is an in-process realtime backend. No live backend, no model
// load: the suite stays fast and hermetic.
type fakeBackend struct {
	srv *httptest.Server

	mu       sync.Mutex
	received []json.RawMessage // client events, in arrival order

	toSend    []any // pushed to the client after the handshake
	closeHard bool  // close the socket right after session.created
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	b := &fakeBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()

		// Read the handshake the client opens with.
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		b.record(data)

		_ = writeJSON(ctx, c, ServerEvent{Type: EventSessionCreated})

		if b.closeHard {
			_ = c.Close(websocket.StatusNormalClosure, "bye")
			return
		}

		for _, ev := range b.toSend {
			if err := writeJSON(ctx, c, ev); err != nil {
				return
			}
		}

		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			b.record(data)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBackend) record(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.received = append(b.received, json.RawMessage(append([]byte(nil), data...)))
}

func (b *fakeBackend) url() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http")
}

func (b *fakeBackend) events() []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]json.RawMessage(nil), b.received...)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}

// recordingSink captures what reaches the carrier.
type recordingSink struct {
	mu     sync.Mutex
	media  []string
	clears int
}

func (s *recordingSink) Media(_ context.Context, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.media = append(s.media, payload)
	return nil
}

func (s *recordingSink) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
	return nil
}

func (s *recordingSink) snapshot() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.media...), s.clears
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
