package twilio_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- AATK-143: the dialed number (To) at the voice webhook ---

// answerCallRecorder is an AnswerCall hook that records every (from, to) it
// is asked about and answers with a fixed decision.
type answerCallRecorder struct {
	say    string
	refuse bool
	asked  [][2]string
}

func (a *answerCallRecorder) hook(_ context.Context, from, to string) (string, bool) {
	a.asked = append(a.asked, [2]string{from, to})
	return a.say, a.refuse
}

// AnswerCall nil keeps v0.5.0's stream TwiML byte for byte.
func TestServeHTTP_AnswerCallNilStreams(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

	want := `<Response><Connect><Stream url="wss://example.com/streams"/></Connect></Response>`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status %d body %q, want 200 %q", w.Code, w.Body.String(), want)
	}
}

// A refusing hook answers exactly Say(escaped)+Hangup and opens no stream.
func TestServeHTTP_AnswerCallRefuseSaysAndHangsUp(t *testing.T) {
	rec := &answerCallRecorder{say: "Call A & B", refuse: true}
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

	want := `<Response><Say>Call A &amp; B</Say><Hangup/></Response>`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status %d body %q, want 200 %q", w.Code, w.Body.String(), want)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/xml" {
		t.Fatalf("Content-Type = %q, want text/xml", ct)
	}
}

// The refusal stays well-formed XML whatever say holds. Characters XML 1.0
// cannot carry (invalid UTF-8, C0 controls other than tab, LF and CR, and
// U+FFFE/U+FFFF) are dropped; valid text, tab and newline included, is
// escaped to the same bytes as before.
func TestServeHTTP_AnswerCallRefusalIsWellFormedXML(t *testing.T) {
	for _, tc := range []struct{ say, wantSay string }{
		{"vt\x0bhere", "vthere"},
		{"nul\x00here", "nulhere"},
		{"esc\x1bhere", "eschere"},
		{"bad\xffutf8", "badutf8"},
		{"non￾char", "nonchar"},
		{"non￿char", "nonchar"},
		{"tab\there\nnext", "tab\there\nnext"},
		{"Call A & B", "Call A &amp; B"},
	} {
		rec := &answerCallRecorder{say: tc.say, refuse: true}
		s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
		form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

		want := "<Response><Say>" + tc.wantSay + "</Say><Hangup/></Response>"
		if got := w.Body.String(); got != want {
			t.Errorf("say %q: body %q, want %q", tc.say, got, want)
		}
		var doc struct {
			Say string `xml:"Say"`
		}
		if err := xml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Errorf("say %q: body %q is not well-formed XML: %v", tc.say, w.Body.String(), err)
		}
	}
}

// The hook is handed the webhook's From and To, in that order — not From twice.
func TestServeHTTP_AnswerCallSeesPostedTo(t *testing.T) {
	rec := &answerCallRecorder{}
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	s.ServeHTTP(httptest.NewRecorder(), signedWebhookRequest(t, "authtoken", "https", "example.com", form))

	if len(rec.asked) != 1 || rec.asked[0] != [2]string{"+15105551234", "+15105550000"} {
		t.Fatalf("AnswerCall asked %v, want [[+15105551234 +15105550000]]", rec.asked)
	}
}

// The hook is handed the webhook request's own context, so a consumer lookup
// that honours ctx sees the request's values and stops when the request is
// cancelled. The request context here carries a value and is already
// cancelled; context.Background() in its place has neither.
func TestServeHTTP_AnswerCallGetsRequestContext(t *testing.T) {
	type ctxKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "request"))
	cancel()

	var gotValue any
	var gotErr error
	s := &twilio.Server{
		AuthToken:    "authtoken",
		StreamScheme: "wss",
		AnswerCall: func(ctx context.Context, _, _ string) (string, bool) {
			gotValue, gotErr = ctx.Value(ctxKey{}), ctx.Err()
			return "", false
		},
	}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form).WithContext(ctx)
	s.ServeHTTP(httptest.NewRecorder(), req)

	if gotValue != "request" || !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("AnswerCall ctx value %v, err %v; want %q and context.Canceled (the request's context)",
			gotValue, gotErr, "request")
	}
}

// A To that is missing or not E.164 reaches the hook as "".
func TestServeHTTP_AnswerCallInvalidToIsEmpty(t *testing.T) {
	for _, to := range []string{"", "client:alice", "5105550000"} {
		rec := &answerCallRecorder{}
		s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
		form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA1"}}
		if to != "" {
			form.Set("To", to)
		}
		s.ServeHTTP(httptest.NewRecorder(), signedWebhookRequest(t, "authtoken", "https", "example.com", form))
		if len(rec.asked) != 1 || rec.asked[0][1] != "" {
			t.Fatalf("To=%q: AnswerCall asked %v, want to == \"\"", to, rec.asked)
		}
	}
}

// A hook that declines leaves the stream TwiML unchanged.
func TestServeHTTP_AnswerCallFalseStreams(t *testing.T) {
	rec := &answerCallRecorder{say: "ignored", refuse: false}
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

	want := `<Response><Connect><Stream url="wss://example.com/streams"/></Connect></Response>`
	if w.Body.String() != want {
		t.Fatalf("body %q, want %q", w.Body.String(), want)
	}
}

// A forged request is refused before the hook is consulted.
func TestServeHTTP_BadSignatureNeverAsksAnswerCall(t *testing.T) {
	rec := &answerCallRecorder{say: "x", refuse: true}
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/webhook",
		strings.NewReader("From=%2B15105551234&To=%2B15105550000"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "bm90YXJlYWxzaWc=")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || len(rec.asked) != 0 {
		t.Fatalf("status %d, hook asked %v; want 403 and no hook call", w.Code, rec.asked)
	}
}

// An Authorize refusal wins: VoiceRejectText, and the hook is never asked.
func TestServeHTTP_AuthorizeFalseNeverAsksAnswerCall(t *testing.T) {
	rec := &answerCallRecorder{say: "x", refuse: true}
	s := &twilio.Server{
		AuthToken:       "authtoken",
		Authorize:       func(string) bool { return false },
		VoiceRejectText: "no",
		AnswerCall:      rec.hook,
	}
	form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

	if got, want := w.Body.String(), `<Response><Say>no</Say><Hangup/></Response>`; got != want || len(rec.asked) != 0 {
		t.Fatalf("body %q, hook asked %v; want %q and no hook call", got, rec.asked, want)
	}
}

// A From that is not E.164 is refused with 403 before the hook is consulted,
// so a refusing hook never turns it into Say+Hangup.
func TestServeHTTP_BadFromNeverAsksAnswerCall(t *testing.T) {
	for _, from := range []string{"5105551234", "anonymous"} {
		rec := &answerCallRecorder{say: "x", refuse: true}
		s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", AnswerCall: rec.hook}
		form := url.Values{"From": {from}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

		if w.Code != http.StatusForbidden || len(rec.asked) != 0 {
			t.Fatalf("From=%q: status %d, hook asked %v; want 403 and no hook call", from, w.Code, rec.asked)
		}
	}
}

// AnswerCall is voice only: an inbound SMS never consults it, and even with a
// hook that would refuse, the text still reaches HandleSMS and is acknowledged
// with empty TwiML.
func TestServeSMS_NeverAsksAnswerCall(t *testing.T) {
	rec := &answerCallRecorder{say: "x", refuse: true}
	handled := 0
	s := &twilio.Server{
		AuthToken:  "authtoken",
		AnswerCall: rec.hook,
		HandleSMS:  func(context.Context, twilio.InboundSMS) { handled++ },
	}
	form := url.Values{"MessageSid": {"SM1"}, "From": {"+15105551234"}, "To": {"+15105550000"}, "Body": {"hi"}}
	w := httptest.NewRecorder()
	s.ServeSMS(w, signedTwilioRequest(t, "authtoken", "https", "example.com", "/sms/inbound", form))

	if len(rec.asked) != 0 || handled != 1 || w.Body.String() != "<Response></Response>" {
		t.Fatalf("hook asked %v, HandleSMS called %d, body %q; want no hook call, 1, %q",
			rec.asked, handled, w.Body.String(), "<Response></Response>")
	}
}
