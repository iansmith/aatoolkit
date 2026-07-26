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

// --- AATK-40: the delivery-status callback ---

// assertEmptyTwiML pins the acknowledgement half of a webhook handler's
// contract: the empty <Response/> plus its content type, which together are
// what stop Twilio adding an automatic reply.
//
// It exists because a status-code check cannot stand in for it.
// httptest.NewRecorder starts life with Code == 200, so `w.Code != 200` passes
// against a handler that wrote nothing at all — every test here would stay
// green with the acknowledgement deleted outright.
func assertEmptyTwiML(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "text/xml" {
		t.Errorf("Content-Type = %q, want text/xml", ct)
	}
	if body := w.Body.String(); body != "<Response></Response>" {
		t.Errorf("body = %q, want empty-ack TwiML", body)
	}
}

// A validated status callback must be parsed into SMSStatus, handed to
// HandleSMSStatus, and acknowledged. This is the only path by which a delivery
// outcome reaches a consumer at all: a 2xx from SendSMS means the provider
// accepted the message, not that a handset received it, so a carrier rejection
// exists nowhere else.
func TestServeSMSStatus_ValidRequestCallsHandler(t *testing.T) {
	var got twilio.SMSStatus
	called := 0
	s := &twilio.Server{
		AuthToken: "authtoken",
		HandleSMSStatus: func(_ context.Context, st twilio.SMSStatus) {
			called++
			got = st
		},
	}
	form := url.Values{
		"MessageSid":    {"SM123"},
		"MessageStatus": {"undelivered"},
		"ErrorCode":     {"30034"},
		"To":            {"+15105551234"},
		"From":          {"+15105550000"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/status", form)
	w := httptest.NewRecorder()

	s.ServeSMSStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	assertEmptyTwiML(t, w)
	if called != 1 {
		t.Fatalf("HandleSMSStatus called %d times, want 1", called)
	}
	if got.MessageSID != "SM123" {
		t.Fatalf("SMSStatus.MessageSID = %q, want %q — without it an outcome cannot be matched to its send", got.MessageSID, "SM123")
	}
	if got.Status != "undelivered" {
		t.Fatalf("SMSStatus.Status = %q, want %q", got.Status, "undelivered")
	}
	if got.ErrorCode != "30034" {
		t.Fatalf("SMSStatus.ErrorCode = %q, want %q", got.ErrorCode, "30034")
	}
	if got.To != "+15105551234" || got.From != "+15105550000" {
		t.Fatalf("parsed SMSStatus = %+v", got)
	}
}

// Most callbacks carry no ErrorCode — every non-failure status omits the field
// entirely. The parsed value must be empty, not a spurious code an operator
// would then go looking up.
func TestServeSMSStatus_AbsentErrorCodeIsEmpty(t *testing.T) {
	var got twilio.SMSStatus
	s := &twilio.Server{
		AuthToken:       "authtoken",
		HandleSMSStatus: func(_ context.Context, st twilio.SMSStatus) { got = st },
	}
	form := url.Values{
		"MessageSid":    {"SM123"},
		"MessageStatus": {"delivered"},
		"To":            {"+15105551234"},
		"From":          {"+15105550000"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/status", form)
	w := httptest.NewRecorder()

	s.ServeSMSStatus(w, req)

	if got.Status != "delivered" {
		t.Fatalf("SMSStatus.Status = %q, want %q", got.Status, "delivered")
	}
	if got.ErrorCode != "" {
		t.Fatalf("SMSStatus.ErrorCode = %q for a callback that carried none, want empty", got.ErrorCode)
	}
}

// The status route is as public as the inbound one, so the signature is the
// same security boundary: a forged POST must be rejected 403 and must never
// reach the handler. Asserting on the dispatch count as well as the status
// code — a 403 that still dispatched would be a side effect an attacker
// controls.
func TestServeSMSStatus_InvalidSignatureRejected(t *testing.T) {
	called := 0
	s := &twilio.Server{
		AuthToken:       "authtoken",
		HandleSMSStatus: func(context.Context, twilio.SMSStatus) { called++ },
	}
	form := url.Values{"MessageSid": {"SM123"}, "MessageStatus": {"failed"}}
	rawURL := "https://webhook.example.com/sms/status"
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "bm90YXJlYWxzaWc=") // not a valid signature
	w := httptest.NewRecorder()

	s.ServeSMSStatus(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if called != 0 {
		t.Fatalf("HandleSMSStatus called %d times on bad signature, want 0", called)
	}
}

// A nil HandleSMSStatus must not panic. A Server{} that predates this field
// still answers the route, so adding the field breaks no existing consumer.
func TestServeSMSStatus_NilHandlerStillAcknowledges(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"} // HandleSMSStatus nil
	form := url.Values{"MessageSid": {"SM123"}, "MessageStatus": {"sent"}}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/status", form)
	w := httptest.NewRecorder()

	s.ServeSMSStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	assertEmptyTwiML(t, w)
}

// The status route must not gate on Authorize — see ServeSMSStatus's doc
// comment for why (From is our own number here, not the caller's).
//
// This is the one way to get the route silently wrong, and no other test can
// see it: every one of them uses a nil Authorize, so copying ServeSMS's gate
// across leaves the whole suite green while production swallows every delivery
// outcome forever. Authorize returns false for everything here, so the test
// fails if the gate exists at all rather than only when a roster happens to
// exclude our own number.
func TestServeSMSStatus_DoesNotGateOnAuthorize(t *testing.T) {
	called := 0
	s := &twilio.Server{
		AuthToken:       "authtoken",
		Authorize:       func(string) bool { return false },
		SMSRejectText:   "not available",
		HandleSMSStatus: func(context.Context, twilio.SMSStatus) { called++ },
	}
	form := url.Values{
		"MessageSid":    {"SM123"},
		"MessageStatus": {"delivered"},
		"To":            {"+15105551234"},
		"From":          {"+15105550000"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/status", form)
	w := httptest.NewRecorder()

	s.ServeSMSStatus(w, req)

	if called != 1 {
		t.Fatalf("HandleSMSStatus called %d times, want 1 — the status route must not run Authorize; From is our own number, so a roster predicate rejects every callback", called)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	// SMSRejectText is set above so this assertion has teeth: a copied
	// Authorize gate answers with <Message>not available</Message>, not the
	// empty ack.
	assertEmptyTwiML(t, w)
}
