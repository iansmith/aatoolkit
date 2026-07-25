package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

// newSMSCaptureServer starts a new capture server listening on the requested
// port of 127.0.0.1, returning an error naming that port if the bind fails.
// The port must be fixed and known before the CLI runs, because the server
// whose reply is being captured reads TWILIO_API_BASE_URL once at startup.
//
// TODO(AATK-31): Phase 0 stub — still binds an ephemeral port and never
// reports a bind failure.
func newSMSCaptureServer(port int) (*smsCaptureServer, error) {
	c := &smsCaptureServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/2010-04-01/Accounts/", c.handleMessages)
	c.Server = httptest.NewServer(mux)
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
// REST reply to land on capture and returns it.
//
// The wait is required, not defensive. The server answers the webhook from a
// buffered queue and runs the turn on a detached goroutine, so the outbound
// Messages.json POST lands after postSMSWebhook has already returned. The
// caller is responsible for having launched the target server with
// TWILIO_API_BASE_URL pointed at capture.URL.
//
// TODO(AATK-31): Phase 0 stub — still checks the capture immediately, so the
// wait bound is accepted and ignored.
func runSMS(ctx context.Context, webhookURL, authToken, from, to, body string, capture *smsCaptureServer, opts ...smsOption) (capturedSMS, error) {
	cfg := smsOptions{captureWait: defaultCaptureWait}
	for _, opt := range opts {
		opt(&cfg)
	}

	messageSid := newSID("SM")
	if err := postSMSWebhook(ctx, webhookURL, authToken, messageSid, from, to, body); err != nil {
		return capturedSMS{}, err
	}

	msgs := capture.captured()
	if len(msgs) != 1 {
		return capturedSMS{}, fmt.Errorf("capture server: got %d Messages.json POSTs, want 1", len(msgs))
	}
	return msgs[0], nil
}
