package twilio_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// A valid, signed inbound-SMS webhook must be parsed into InboundSMS, handed to
// HandleSMS, and answered with 200 + empty TwiML so Twilio sends no auto-reply.
func TestServeSMS_ValidRequestCallsHandler(t *testing.T) {
	var got twilio.InboundSMS
	called := 0
	s := &twilio.Server{
		AuthToken:    "authtoken",
		StreamScheme: "wss",
		HandleSMS: func(_ context.Context, msg twilio.InboundSMS) {
			called++
			got = msg
		},
	}
	form := url.Values{
		"MessageSid": {"SM123"},
		"From":       {"+15105551234"},
		"To":         {"+15105550000"},
		"Body":       {"hello there"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/xml" {
		t.Fatalf("Content-Type = %q, want text/xml", ct)
	}
	if called != 1 {
		t.Fatalf("HandleSMS called %d times, want 1", called)
	}
	if got.MessageSID != "SM123" || got.From != "+15105551234" || got.To != "+15105550000" || got.Body != "hello there" {
		t.Fatalf("parsed InboundSMS = %+v", got)
	}
}

// A request whose X-Twilio-Signature does not validate must be rejected 403 and
// must never reach HandleSMS — the signature is the security boundary on a
// public endpoint, so an unsigned/forged POST cannot trigger any side effect.
func TestServeSMS_InvalidSignatureRejected(t *testing.T) {
	called := 0
	s := &twilio.Server{
		AuthToken:    "authtoken",
		StreamScheme: "wss",
		HandleSMS:    func(context.Context, twilio.InboundSMS) { called++ },
	}
	form := url.Values{"From": {"+15105551234"}, "Body": {"hello"}}
	rawURL := "https://webhook.example.com/sms/inbound"
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "bm90YXJlYWxzaWc=") // not a valid signature
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if called != 0 {
		t.Fatalf("HandleSMS called %d times on bad signature, want 0", called)
	}
}

// A nil HandleSMS must not panic: a validated request is acknowledged with 200 +
// empty TwiML even when no consumer handler is wired (mirrors nil HandleStream).
func TestServeSMS_NilHandlerStillAcknowledges(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"} // HandleSMS nil
	form := url.Values{"From": {"+15105551234"}, "Body": {"hi"}}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// --- AATK-19: Authorize gate on the SMS webhook ---

// An unknown/unauthorized sender must be rejected with HTTP 200 and the exact
// rejection TwiML, and HandleSMS must never be invoked.
func TestServeSMS_UnknownSenderRejected(t *testing.T) {
	called := 0
	s := &twilio.Server{
		AuthToken:     "t",
		Authorize:     func(string) bool { return false },
		SMSRejectText: "I'm sorry, this service is not available",
		HandleSMS:     func(context.Context, twilio.InboundSMS) { called++ },
	}
	form := url.Values{"From": {"+15105551234"}, "Body": {"hi"}}
	req := signedTwilioRequest(t, "t", "https", "webhook.example.com", "/sms/inbound", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	want := `<Response><Message>I&#39;m sorry, this service is not available</Message></Response>`
	if got := w.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if called != 0 {
		t.Fatalf("HandleSMS called %d times, want 0", called)
	}
}

// Authorize == nil must reproduce today's behavior exactly (back-compat).
func TestServeSMS_NilAuthorizeAllows(t *testing.T) {
	called := 0
	var got twilio.InboundSMS
	s := &twilio.Server{
		AuthToken: "authtoken",
		HandleSMS: func(_ context.Context, msg twilio.InboundSMS) {
			called++
			got = msg
		},
	} // Authorize left nil
	form := url.Values{
		"MessageSid": {"SM999"},
		"From":       {"+15105551234"},
		"To":         {"+15105550000"},
		"Body":       {"hello"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "<Response></Response>" {
		t.Fatalf("body = %q, want empty-ack TwiML (nil Authorize must not change existing behavior)", body)
	}
	if called != 1 {
		t.Fatalf("HandleSMS called %d times, want 1", called)
	}
	if got.From != "+15105551234" {
		t.Fatalf("InboundSMS.From = %q, want %q", got.From, "+15105551234")
	}
}

// Adversary gap: Authorize returning true (not nil) must allow through — pins
// that the gate checks the predicate's result, not merely whether it's set.
func TestServeSMS_AuthorizeTrueAllows(t *testing.T) {
	called := 0
	s := &twilio.Server{
		AuthToken: "authtoken",
		Authorize: func(string) bool { return true },
		HandleSMS: func(context.Context, twilio.InboundSMS) { called++ },
	}
	form := url.Values{"From": {"+15105551234"}, "Body": {"hi"}}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "<Response></Response>" {
		t.Fatalf("body = %q, want empty-ack TwiML (Authorize returning true must allow through)", body)
	}
	if called != 1 {
		t.Fatalf("HandleSMS called %d times, want 1", called)
	}
}
