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

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// smsForm builds the standard Twilio inbound-SMS webhook field set for
// messageSid, from, to, and body. Mirrors webhookForm's ceremony for the
// voice webhook.
func smsForm(messageSid, from, to, body string) url.Values {
	return url.Values{
		"MessageSid": {messageSid},
		"AccountSid": {defaultAccountSid},
		"From":       {from},
		"To":         {to},
		"Body":       {body},
		"ApiVersion": {defaultAPIVersion},
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

// newSMSCaptureServer starts a new capture server listening on a local
// ephemeral port.
func newSMSCaptureServer() *smsCaptureServer {
	c := &smsCaptureServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/2010-04-01/Accounts/", c.handleMessages)
	c.Server = httptest.NewServer(mux)
	return c
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

// runSMS performs the SMS fake-mode round trip: it starts a local capture
// server implementing the Twilio Messages API shape, posts a signed inbound-
// SMS webhook to webhookURL, and returns the outbound REST reply the server
// sent back. Server.ServeSMS calls HandleSMS synchronously before answering
// the webhook POST, so any REST call the handler makes has already landed on
// the capture server by the time postSMSWebhook returns — no polling needed.
// The caller (a test, or the CLI) is responsible for pointing the target
// server's RESTClient.BaseURL at capture.URL before calling runSMS.
func runSMS(ctx context.Context, webhookURL, authToken, from, to, body string, capture *smsCaptureServer) (capturedSMS, error) {
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
