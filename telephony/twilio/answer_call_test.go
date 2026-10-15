package twilio_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// Frame.To carries the webhook's dialed number onto the start frame.
func TestServeStreams_ToThreadedFromWebhook(t *testing.T) {
	const from, to = "+15105550123", "+15105550999"
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {from}, "To": {to}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d", w.Code)
	}

	received := make(chan twilio.Frame, 1)
	s.HandleStream = func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
		received <- f
		return nil
	}
	conn := dialStreams(t, streamsServer(t, s))
	sendStart(t, conn, "SS1", "CA1")

	select {
	case f := <-received:
		if f.To != to || f.From != from {
			t.Fatalf("Frame From/To = %q/%q, want %q/%q", f.From, f.To, from, to)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called within 2s")
	}
}

// Frame.To is "" when the webhook's To was not E.164: the value recorded for
// the start frame is the validated one, not the raw form field. From is checked
// too, so a pass cannot come from the CallSid simply not being found.
func TestServeStreams_ToEmptyWhenWebhookToInvalid(t *testing.T) {
	const from = "+15105550123"
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {from}, "To": {"client:alice"}, "CallSid": {"CA1"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d", w.Code)
	}

	received := make(chan twilio.Frame, 1)
	s.HandleStream = func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
		received <- f
		return nil
	}
	conn := dialStreams(t, streamsServer(t, s))
	sendStart(t, conn, "SS1", "CA1")

	select {
	case f := <-received:
		if f.From != from || f.To != "" {
			t.Fatalf("Frame From/To = %q/%q, want %q/\"\"", f.From, f.To, from)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called within 2s")
	}
}

// Frame.To is "" when no webhook recorded the CallSID.
func TestServeStreams_ToEmptyWhenNoWebhookRecorded(t *testing.T) {
	s := &twilio.Server{}
	received := make(chan twilio.Frame, 1)
	s.HandleStream = func(ctx context.Context, conn *websocket.Conn, f twilio.Frame) error {
		received <- f
		return nil
	}
	conn := dialStreams(t, streamsServer(t, s))
	sendStart(t, conn, "SS2", "CAunknown")

	select {
	case f := <-received:
		if f.To != "" {
			t.Fatalf("Frame.To = %q, want \"\"", f.To)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called within 2s")
	}
}
