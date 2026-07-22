package twilio_test

import (
	"bytes"
	"encoding/xml"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// Compile-time assertion: *Server must implement http.Handler.
var _ http.Handler = (*twilio.Server)(nil)

// --- helpers ---

// signedWebhookRequest builds a POST /webhook request whose X-Twilio-Signature
// is a valid signature over the given scheme+host+path for authToken, so tests
// can exercise ServeHTTP's post-signature logic (TwiML shape, From validation).
func signedWebhookRequest(t *testing.T, authToken, scheme, host string, form url.Values) *http.Request {
	t.Helper()
	return signedTwilioRequest(t, authToken, scheme, host, "/webhook", form)
}

// signedTwilioRequest builds a POST to path whose X-Twilio-Signature is a valid
// signature over scheme+host+path for authToken, so tests can exercise any
// webhook handler's post-signature logic.
func signedTwilioRequest(t *testing.T, authToken, scheme, host, path string, form url.Values) *http.Request {
	t.Helper()
	rawURL := scheme + "://" + host + path
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", computeSig(authToken, rawURL, form))
	return req
}

// twiMLStream unmarshals the <Connect><Stream url="..."/></Connect> element
// out of a TwiML response body.
type twiMLStream struct {
	XMLName xml.Name `xml:"Response"`
	Connect struct {
		Stream struct {
			URL string `xml:"url,attr"`
		} `xml:"Stream"`
	} `xml:"Connect"`
}

// --- behavior 1: valid request returns Connect/Stream TwiML ---

func TestServeHTTP_ValidRequestReturnsStreamTwiML(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA123"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/xml" {
		t.Fatalf("Content-Type = %q, want %q", ct, "text/xml")
	}
	var tw twiMLStream
	if err := xml.Unmarshal(w.Body.Bytes(), &tw); err != nil {
		t.Fatalf("unmarshal TwiML body %q: %v", w.Body.String(), err)
	}
	if want := "wss://example.com/streams"; tw.Connect.Stream.URL != want {
		t.Fatalf("Stream url = %q, want %q", tw.Connect.Stream.URL, want)
	}
}

// --- behaviors 2-3: scheme flag drives both advertised scheme and validation scheme ---

func TestServeHTTP_SchemeFromFlag(t *testing.T) {
	tests := []struct {
		name          string
		streamScheme  string
		signingScheme string
		wantStreamURL string
	}{
		{"ws advertises ws and validates over http", "ws", "http", "ws://example.com/streams"},
		{"wss advertises wss and validates over https", "wss", "https", "wss://example.com/streams"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &twilio.Server{AuthToken: "authtoken", StreamScheme: tt.streamScheme}
			form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA123"}}
			req := signedWebhookRequest(t, "authtoken", tt.signingScheme, "example.com", form)
			w := httptest.NewRecorder()

			s.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			var tw twiMLStream
			if err := xml.Unmarshal(w.Body.Bytes(), &tw); err != nil {
				t.Fatalf("unmarshal TwiML body %q: %v", w.Body.String(), err)
			}
			if tw.Connect.Stream.URL != tt.wantStreamURL {
				t.Fatalf("Stream url = %q, want %q", tw.Connect.Stream.URL, tt.wantStreamURL)
			}
		})
	}
}

// A request signed over the "other" scheme (mismatched with StreamScheme's
// derived validation scheme) must be rejected — proves the validation scheme
// really is derived from StreamScheme, not hardcoded.
func TestServeHTTP_SchemeFromFlag_MismatchedSigningSchemeRejected(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "ws"}
	form := url.Values{"From": {"+15105551234"}}
	// Server expects http (StreamScheme=ws), but the request is signed over https.
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (signature signed over wrong scheme)", w.Code)
	}
}

// --- zero value is secure ---

func TestServeHTTP_ZeroSchemeIsSecure(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"} // StreamScheme left unset ("")
	form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA123"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var tw twiMLStream
	if err := xml.Unmarshal(w.Body.Bytes(), &tw); err != nil {
		t.Fatalf("unmarshal TwiML body %q: %v", w.Body.String(), err)
	}
	if want := "wss://example.com/streams"; tw.Connect.Stream.URL != want {
		t.Fatalf("zero-value StreamScheme: Stream url = %q, want %q (secure-by-default)", tw.Connect.Stream.URL, want)
	}
}

// --- behavior 4: missing/malformed From rejected with 403 ---

func TestServeHTTP_RejectsBadFrom(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		hasFrom bool
		wantOK  bool
	}{
		{"absent From field", "", false, false},
		{"empty From", "", true, false},
		{"no leading plus", "12345", true, false},
		{"bare plus", "+", true, false},
		{"valid E.164 control", "+15105551234", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
			form := url.Values{"CallSid": {"CA123"}}
			if tt.hasFrom {
				form.Set("From", tt.from)
			}
			req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
			w := httptest.NewRecorder()

			s.ServeHTTP(w, req)

			if tt.wantOK {
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
				}
			} else {
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 for From=%q (present=%v)", w.Code, tt.from, tt.hasFrom)
				}
			}
		})
	}
}

// --- adversary gap: CallSid is logged but not a precondition for 200 ---

// Ticket behavior 1 states CallSid arriving is logged but is NOT a
// precondition for the 200 — a request with a valid From but no CallSid at
// all must still succeed.
func TestServeHTTP_MissingCallSidStillSucceeds(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {"+15105551234"}} // no CallSid field at all
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (CallSid must not be a precondition for success)", w.Code)
	}
}

// --- adversary gap: E.164 leading-digit boundary ---

// The ticket names the exact pattern ^\+[1-9]\d{1,14}$ — a leading zero after
// the plus is a distinct rejection case from "no plus at all" or "bare plus",
// and pins the [1-9] (not [0-9]) first-digit requirement specifically.
func TestServeHTTP_RejectsFromWithLeadingZero(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {"+05105551234"}, "CallSid": {"CA123"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for From with leading zero after '+'", w.Code)
	}
}

// --- signature still validated first ---

// A bad signature must still 403 before any From/TwiML logic runs — the
// signature.go adversary tests already cover this for the pre-existing
// behavior; this pins that the new From/TwiML logic did not move the
// signature check later in the handler.
func TestServeHTTP_BadSignatureRejectedBeforeFromLogic(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/webhook", strings.NewReader("From=not-e164"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "bm90YXJlYWxzaWc=")
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for bad signature", w.Code)
	}
}

// --- log output ---

func TestServeHTTP_LogsFromAndCallSid(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss"}
	form := url.Values{"From": {"+15105551234"}, "CallSid": {"CAlogtest01"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	logged := buf.String()
	if !strings.Contains(logged, "+15105551234") {
		t.Fatalf("log output %q does not contain From value", logged)
	}
	if !strings.Contains(logged, "CAlogtest01") {
		t.Fatalf("log output %q does not contain CallSid value", logged)
	}
}

// --- behavior: authorized-caller webhook gate (AATK-19) ---

// Test that authorized callers can make voice calls through the webhook.
func TestServeHTTP_AuthorizedCallerSucceeds(t *testing.T) {
	authorizedCallers := []string{"+15105551234", "+12025559876"}
	s := &twilio.Server{
		AuthToken:         "authtoken",
		StreamScheme:      "wss",
		AuthorizedCallers: authorizedCallers,
	}
	form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA123"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authorized caller: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// Test that unauthorized callers are rejected with 403.
func TestServeHTTP_UnauthorizedCallerRejected(t *testing.T) {
	authorizedCallers := []string{"+15105551234", "+12025559876"}
	s := &twilio.Server{
		AuthToken:         "authtoken",
		StreamScheme:      "wss",
		AuthorizedCallers: authorizedCallers,
	}
	form := url.Values{"From": {"+15555555555"}, "CallSid": {"CA123"}}
	req := signedWebhookRequest(t, "authtoken", "https", "example.com", form)
	w := httptest.NewRecorder()

	s.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized caller: status = %d, want 403", w.Code)
	}
}

// Test that SMS from authorized callers succeeds.
func TestServeSMS_AuthorizedCallerSucceeds(t *testing.T) {
	authorizedCallers := []string{"+15105551234", "+12025559876"}
	s := &twilio.Server{
		AuthToken:         "authtoken",
		AuthorizedCallers: authorizedCallers,
	}
	form := url.Values{
		"MessageSid": {"SM123"},
		"From":       {"+15105551234"},
		"To":         {"+15105559999"},
		"Body":       {"hello"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "example.com", "/sms", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authorized SMS: status = %d, want 200", w.Code)
	}
}

// Test that SMS from unauthorized callers is rejected with 403.
func TestServeSMS_UnauthorizedCallerRejected(t *testing.T) {
	authorizedCallers := []string{"+15105551234", "+12025559876"}
	s := &twilio.Server{
		AuthToken:         "authtoken",
		AuthorizedCallers: authorizedCallers,
	}
	form := url.Values{
		"MessageSid": {"SM123"},
		"From":       {"+15555555555"},
		"To":         {"+15105559999"},
		"Body":       {"hello"},
	}
	req := signedTwilioRequest(t, "authtoken", "https", "example.com", "/sms", form)
	w := httptest.NewRecorder()

	s.ServeSMS(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized SMS: status = %d, want 403", w.Code)
	}
}
