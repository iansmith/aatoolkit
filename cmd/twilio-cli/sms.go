package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// smsForm builds the standard Twilio inbound-SMS webhook field set for
// messageSid, from, to, and body, including the SmsMessageSid/SmsSid aliases
// real Twilio inbound-SMS webhooks send alongside MessageSid (mirroring
// webhookForm's Caller/Called aliases for the voice webhook).
func smsForm(messageSid, from, to, body string) url.Values {
	return url.Values{
		"MessageSid":    {messageSid},
		"SmsMessageSid": {messageSid},
		"SmsSid":        {messageSid},
		"AccountSid":    {defaultAccountSid},
		"From":          {from},
		"To":            {to},
		"Body":          {body},
		"ApiVersion":    {defaultAPIVersion},
	}
}

// postSMSWebhook performs the signed Twilio SMS-webhook ceremony: it POSTs the
// standard inbound-SMS field set (signed over webhookURL, the exact URL
// posted to) and returns an error if the request fails or the server rejects
// it (e.g. a 403 from a signature mismatch caused by a wrong path or auth
// token).
func postSMSWebhook(ctx context.Context, webhookURL, authToken, messageSid, from, to, body string) error {
	form := smsForm(messageSid, from, to, body)
	sig := twilio.ComputeSignature(authToken, webhookURL, form)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build SMS webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post SMS webhook: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read SMS webhook response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SMS webhook %s returned status %d: %s", webhookURL, resp.StatusCode, respBody)
	}

	return nil
}

// captureBaseURLEnv renders the environment assignment that points a server's
// outbound Twilio REST client at captureURL. Single-sourced here because two
// places have to agree on it exactly: the guidance the operator copies before
// launching the server, and the timeout error reminding them they didn't.
func captureBaseURLEnv(captureURL string) string {
	return "TWILIO_API_BASE_URL=" + captureURL
}

// capturedSMS is one outbound Messages.json POST recorded by smsCaptureServer.
type capturedSMS struct {
	AccountSID string
	From       string
	To         string
	Body       string
}

// smsCaptureServer implements the Twilio Messages API shape used by
// RESTClient.SendSMS (POST /2010-04-01/Accounts/{sid}/Messages.json -> 201 +
// minimal JSON), recording every call it receives. It stands in for a real
// Twilio account so a server's outbound SMS reply can be captured and
// asserted on without one.
type smsCaptureServer struct {
	*httptest.Server

	mu   sync.Mutex
	msgs []capturedSMS

	// notify carries a wake-up on every recorded message, so a waiter is woken
	// by the arrival itself rather than by polling for it.
	//
	// It signals "something arrived", not "your message arrived" — the waiter
	// re-reads the recorded count to decide. That is what lets a second runSMS
	// against the same server wait for its own reply instead of re-reading the
	// first one. Buffered by one and sent to without blocking: a dropped send
	// only ever means a wake-up is already pending, and recording a message
	// must never block the handler on nobody listening.
	notify chan struct{}
}

// newSMSCaptureServer starts a new capture server listening on the requested
// port of 127.0.0.1, returning an error naming that port if the bind fails.
//
// The bind is deliberately explicit rather than ephemeral: the server whose
// reply is being captured reads TWILIO_API_BASE_URL once at startup, so the
// capture URL has to be nameable before this process runs at all. It keeps the
// embedded *httptest.Server — only the listener is swapped — so URL and Close
// behave exactly as callers already expect, and a bind failure returns an error
// instead of the panic httptest.NewServer would raise.
func newSMSCaptureServer(port int) (*smsCaptureServer, error) {
	// Port 0 is the one value net.Listen would accept while defeating the whole
	// point: it binds an ephemeral port, so the URL is again unknowable before
	// this process runs, and the guidance would print a :0 the operator cannot
	// use. Everything else out of range net.Listen rejects for us.
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("SMS capture port %d is not a usable port (want 1-65535; -capture-port 0 would bind an ephemeral port, which the server cannot be pointed at in advance)", port)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bind SMS capture server on port %d: %w (pick a free port with -capture-port)", port, err)
	}

	c := &smsCaptureServer{notify: make(chan struct{}, 1)}
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
func (c *smsCaptureServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	msg := capturedSMS{
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
		"sid":    newSID("SM"),
		"status": "queued",
	})
}

// captured returns a snapshot of every Messages.json POST recorded so far.
func (c *smsCaptureServer) captured() []capturedSMS {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedSMS, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// defaultCaptureWait bounds how long runSMS waits for the outbound reply. It
// must exceed the responder's own 30s delivery deadline, because a turn that
// hits that deadline still emits a failure-report SMS — a shorter wait would
// report "no reply" for a turn still legitimately in flight.
const defaultCaptureWait = 35 * time.Second

// smsOptions configures optional runSMS behavior.
type smsOptions struct {
	captureWait time.Duration
}

// smsOption configures smsOptions.
type smsOption func(*smsOptions)

// withCaptureWait overrides defaultCaptureWait, so a test can exercise the
// wait-expiry path without waiting out the production bound.
func withCaptureWait(d time.Duration) smsOption {
	return func(o *smsOptions) { o.captureWait = d }
}

// runSMS performs the SMS fake-mode round trip: it posts a signed inbound-SMS
// webhook to webhookURL, then waits up to defaultCaptureWait for the outbound
// REST reply to land on capture and returns the first one that does.
//
// The wait is required, not defensive. The server answers the webhook from a
// buffered queue and runs the turn on a detached goroutine, so the outbound
// Messages.json POST lands after postSMSWebhook has already returned. It is
// signalled by the capture server rather than polled, so the reply is returned
// the moment it arrives. The caller is responsible for having launched the
// target server with TWILIO_API_BASE_URL pointed at capture.URL.
func runSMS(ctx context.Context, webhookURL, authToken, from, to, body string, capture *smsCaptureServer, opts ...smsOption) (capturedSMS, error) {
	cfg := smsOptions{captureWait: defaultCaptureWait}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Anything already recorded belongs to an earlier call, so this run waits
	// for the count to grow past it rather than for "any message at all". That
	// is what makes a second runSMS against the same capture server return its
	// own reply instead of re-reporting the first one.
	before := len(capture.captured())

	messageSid := newSID("SM")
	if err := postSMSWebhook(ctx, webhookURL, authToken, messageSid, from, to, body); err != nil {
		return capturedSMS{}, err
	}

	timeout := time.NewTimer(cfg.captureWait)
	defer timeout.Stop()

	for {
		select {
		case <-capture.notify:
			if msgs := capture.captured(); len(msgs) > before {
				return msgs[before], nil
			}
		case <-timeout.C:
			return capturedSMS{}, fmt.Errorf("no reply reached the capture server within %s — check the server was launched with %s", cfg.captureWait, captureBaseURLEnv(capture.URL))
		case <-ctx.Done():
			return capturedSMS{}, fmt.Errorf("waiting for the reply: %w", ctx.Err())
		}
	}
}
