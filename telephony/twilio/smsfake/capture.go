package smsfake

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Message is one outbound Messages.json POST recorded by CaptureServer.
type Message struct {
	AccountSID string
	From       string
	To         string
	Body       string
}

// CaptureServer implements the Twilio Messages API shape used by
// RESTClient.SendSMS (POST /2010-04-01/Accounts/{sid}/Messages.json -> 201 +
// minimal JSON), recording every call it receives. It stands in for a real
// Twilio account so a server's outbound SMS reply can be captured and
// asserted on without one.
type CaptureServer struct {
	*httptest.Server

	mu   sync.Mutex
	msgs []Message

	// notify carries a wake-up on every recorded message, so a waiter is woken
	// by the arrival itself rather than by polling for it.
	//
	// It signals "something arrived", not "your message arrived" — the waiter
	// re-reads the recorded count to decide. That is what lets a later waiter
	// against the same server (twilio-cli's runSMS, for one), once the earlier
	// one has finished, wait for its own reply instead of re-reading the first
	// one. Buffered by one and sent to without blocking: a dropped send only
	// ever means a wake-up is already pending, and recording a message must
	// never block the handler on nobody listening.
	notify chan struct{}
}

// NewCaptureServer starts a new capture server listening on the requested
// port of 127.0.0.1, returning an error naming that port if the bind fails.
//
// The bind is deliberately explicit rather than ephemeral, so the capture URL
// can be named before the capture server exists. The server whose reply is
// being captured reads TWILIO_API_BASE_URL once at startup, so when it is
// launched first (twilio-cli's flow, where the operator starts it before the
// CLI) it has to be given that URL in advance. It keeps the
// embedded *httptest.Server — only the listener is swapped — so URL and Close
// behave exactly as callers already expect, and a bind failure returns an error
// instead of the panic httptest.NewServer would raise.
//
// The port-range and bind errors name -capture-port, the flag twilio-cli
// exposes this port as. A caller without such a flag should reword them rather
// than pass the hint on.
func NewCaptureServer(port int) (*CaptureServer, error) {
	// Port 0 is the one value net.Listen would accept while defeating the whole
	// point: it binds an ephemeral port, so a URL named in advance from the
	// requested port (as twilio-cli's guidance names it) would end in a :0
	// nothing can be pointed at. Everything else out of range net.Listen
	// rejects for us.
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("SMS capture port %d is not a usable port (want 1-65535; -capture-port 0 would bind an ephemeral port, which the server cannot be pointed at in advance)", port)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bind SMS capture server on port %d: %w (pick a free port with -capture-port)", port, err)
	}

	c := &CaptureServer{notify: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/2010-04-01/Accounts/", c.handleMessages)

	srv := httptest.NewUnstartedServer(mux)
	// NewUnstartedServer has already bound an ephemeral listener of its own;
	// discard it in favour of the one on the requested port.
	if err := srv.Listener.Close(); err != nil {
		ln.Close()
		return nil, fmt.Errorf("discard the default capture listener: %w", err)
	}
	srv.Listener = ln
	srv.Start()

	c.Server = srv
	return c, nil
}

// handleMessages records the outbound SMS-send POST and answers with the
// minimal 201 JSON shape SendSMS treats as success.
//
// The "sid" key is a contract, not decoration: RESTClient.SendSMS parses it to
// return the message id, and a 2xx carrying none yields an empty id rather than
// an error — so dropping it here would go unnoticed.
// TestCaptureServer_SendSMSGetsTheSIDBack asserts the id makes it back, which
// is what keeps the two ends honest.
func (c *CaptureServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	msg := Message{
		AccountSID: strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/2010-04-01/Accounts/"), "/Messages.json"),
		From:       r.PostForm.Get("From"),
		To:         r.PostForm.Get("To"),
		Body:       r.PostForm.Get("Body"),
	}

	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default: // a wake-up is already pending; the waiter re-reads the count
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"sid":    NewSID("SM"),
		"status": "queued",
	})
}

// Captured returns a snapshot of every Messages.json POST recorded so far.
func (c *CaptureServer) Captured() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// Notify returns the server's one wake-up channel, so a waiter is woken by an
// arrival itself rather than by polling for it. Every recorded message sends a
// wake-up on it without blocking; it is buffered by one, so arrivals while a
// wake-up is already pending coalesce into that one. A wake-up signals
// "something arrived", not "your message arrived", so a waiter re-reads
// Captured to decide. It is receive-only to callers; only the capture server
// sends on it.
//
// It is single-consumer: every caller gets the same channel and each wake-up
// reaches one receiver only, so wait on it from one goroutine at a time. A
// second concurrent waiter can miss the wake-up for a message that was
// recorded, and block until a later arrival or its own timeout. Waiters that
// take turns, each re-reading Captured, are fine.
func (c *CaptureServer) Notify() <-chan struct{} {
	return c.notify
}
