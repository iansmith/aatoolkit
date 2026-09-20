package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// AATK-136: what a consumer sees when the backend REFUSES the handshake and
// says why.
//
// Dial's loop reads until session.created and ignores every other frame. A
// backend that rejects the handshake — because it has not been taught an
// engine extension like client_session_id, or because it dislikes any other
// field — answers with an error frame naming the problem and then simply
// waits. The loop discarded that frame, so the dial failed with nothing but
// the caller's own deadline, and the one sentence identifying the cause was
// dropped on the floor.
//
// This is the first-run experience for a consumer turning on a session field
// their backend does not know, so the frame has to survive into the error.

// rejectingBackend answers the handshake with an error frame and keeps the
// socket OPEN, which is the hard case: a backend that closes surfaces
// promptly as a read error, while one that stays open leaves Dial blocked
// until the caller's context ends, with nothing said about why.
//
// It is a local server rather than a knob on the shared fakeBackend because
// that one always sends session.created, which is the one thing this backend
// must never do.
func rejectingBackend(t *testing.T, message string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()

		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = writeJSON(ctx, c, map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "invalid_request_error", "message": message},
		})
		// Say nothing further, and do not close: block until the client
		// gives up, which is what the real service does.
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestDial_RejectedHandshakeSurfacesTheBackendsLastFrame pins that the reason
// reaches the caller. Without it the error names only the deadline, and a
// consumer has no way to learn which field the backend refused short of
// packet capture.
//
// slopstop:test contract
func TestDial_RejectedHandshakeSurfacesTheBackendsLastFrame(t *testing.T) {
	const reason = "Unknown parameter: 'session.client_session_id'."
	url := rejectingBackend(t, reason)

	// Short bound: this package's Dial has no timer of its own, so the
	// caller's context is what ends the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	c, err := Dial(ctx, url, WithSessionID("sess-rejected"))
	if err == nil {
		c.Close()
		t.Fatal("Dial must fail when the backend never acknowledges the session")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("Dial's error must carry the backend's own explanation.\n got  %v\nwant substring %q", err, reason)
	}
}

// TestDial_RejectedHandshakeWithNoFrameStillReportsTheDeadline pins the other
// half: a backend that says NOTHING at all must still produce the error it
// always did, with no empty "last frame" noise appended to it.
//
// slopstop:test regression — guards: "A silent backend's dial error is unchanged."
func TestDial_RejectedHandshakeWithNoFrameStillReportsTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	c, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err == nil {
		c.Close()
		t.Fatal("Dial must fail when the backend never acknowledges the session")
	}
	if !strings.Contains(err.Error(), EventSessionCreated) {
		t.Fatalf("a silent backend's error must still name what was awaited, got %v", err)
	}
	if strings.Contains(err.Error(), "last frame") {
		t.Fatalf("no frame arrived, so the error must not mention one: %v", err)
	}
}
