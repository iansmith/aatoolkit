package smsfake_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
)

// TestDefaults_AreTheValuesTwilioCLIDeclared pins the two exported defaults to
// the values twilio-cli declared before they moved here. They are on the wire
// of every fake round trip — AccountSid and ApiVersion in the signed webhook
// form — so a changed value is a changed signature, not a cosmetic edit.
func TestDefaults_AreTheValuesTwilioCLIDeclared(t *testing.T) {
	if got, want := smsfake.DefaultAccountSID, "ACtwiliocli0000000000000000000000"; got != want {
		t.Errorf("DefaultAccountSID = %q, want %q", got, want)
	}
	if got, want := smsfake.DefaultAPIVersion, "2010-04-01"; got != want {
		t.Errorf("DefaultAPIVersion = %q, want %q", got, want)
	}
}

// TestInboundForm_Fields pins the SMS webhook form field set: MessageSid (plus
// its SmsMessageSid/SmsSid aliases), AccountSid, From, To, Body, ApiVersion —
// built the way twilio-cli builds it, from the two exported defaults.
func TestInboundForm_Fields(t *testing.T) {
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0001", "+15551234567", "+15105559999", "hello there")

	if got := form.Get("MessageSid"); got != "SMtest0001" {
		t.Errorf("MessageSid = %q, want SMtest0001", got)
	}
	if got := form.Get("SmsMessageSid"); got != "SMtest0001" {
		t.Errorf("SmsMessageSid = %q, want SMtest0001", got)
	}
	if got := form.Get("SmsSid"); got != "SMtest0001" {
		t.Errorf("SmsSid = %q, want SMtest0001", got)
	}
	if got := form.Get("From"); got != "+15551234567" {
		t.Errorf("From = %q, want +15551234567", got)
	}
	if got := form.Get("To"); got != "+15105559999" {
		t.Errorf("To = %q, want +15105559999", got)
	}
	if got := form.Get("Body"); got != "hello there" {
		t.Errorf("Body = %q, want %q", got, "hello there")
	}
	if form.Get("AccountSid") == "" {
		t.Error("AccountSid is empty, want a non-empty placeholder")
	}
	if form.Get("ApiVersion") == "" {
		t.Error("ApiVersion is empty, want a non-empty value")
	}
	if got := form.Get("AccountSid"); got != smsfake.DefaultAccountSID {
		t.Errorf("AccountSid = %q, want %q", got, smsfake.DefaultAccountSID)
	}
	if got := form.Get("ApiVersion"); got != smsfake.DefaultAPIVersion {
		t.Errorf("ApiVersion = %q, want %q", got, smsfake.DefaultAPIVersion)
	}
	if got := len(form); got != 8 {
		t.Errorf("form has %d fields, want exactly the eight above: %v", got, form)
	}
}

// TestInboundForm_PlacesAccountSIDAndAPIVersion closes the hole the defaults
// leave in TestInboundForm_Fields: accountSID and apiVersion are adjacent
// string parameters, so a builder that swapped them, or ignored them for the
// defaults, would pass a test that only ever supplies the defaults.
func TestInboundForm_PlacesAccountSIDAndAPIVersion(t *testing.T) {
	const accountSID, apiVersion = "ACplacement0000000000000000000000", "2099-12-31"

	form := smsfake.InboundForm(accountSID, apiVersion, "SMtest0004", "+15551234567", "+15105559999", "hello there")

	if got := form.Get("AccountSid"); got != accountSID {
		t.Errorf("AccountSid = %q, want the accountSID argument %q", got, accountSID)
	}
	if got := form.Get("ApiVersion"); got != apiVersion {
		t.Errorf("ApiVersion = %q, want the apiVersion argument %q", got, apiVersion)
	}
}

// TestPostWebhook_SignatureURLMismatch403s pins the signing rule directly: the
// signature is computed over the exact POST URL, so a signature computed for
// one URL and presented on a POST to a different URL must 403 — isolated from
// routing (both URLs below are otherwise valid, registered paths; only the
// signed-vs-actual URL differs), so this cannot pass merely because an
// unmatched route 404s.
func TestPostWebhook_SignatureURLMismatch403s(t *testing.T) {
	const authToken = "test-auth-token"

	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	realURL := ts.URL + "/sms/inbound"
	wrongURLForSigning := ts.URL + "/sms"
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0002", "+15551234567", "+15105559999", "hello there")
	sig := twilio.ComputeSignature(authToken, wrongURLForSigning, form)

	req, err := http.NewRequest(http.MethodPost, realURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 (signature bound to a different URL than the one posted to), got %d", resp.StatusCode)
	}
}

// TestPostWebhook_SignsTheFormOverTheExactURL pins the ceremony's wire shape
// from the receiving side: the form arrives intact, form-encoded, and carrying
// the X-Twilio-Signature twilio.ComputeSignature yields for the auth token,
// the exact URL posted to, and that form — which is what a real server checks.
func TestPostWebhook_SignsTheFormOverTheExactURL(t *testing.T) {
	const authToken = "test-auth-token"

	type seen struct {
		contentType, signature string
		form                   url.Values
	}
	got := make(chan seen, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse posted form: %v", err)
		}
		got <- seen{r.Header.Get("Content-Type"), r.Header.Get("X-Twilio-Signature"), r.PostForm}
	}))
	defer ts.Close()

	webhookURL := ts.URL + "/sms/inbound"
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0005", "+15551234567", "+15105559999", "hello there")
	if _, _, err := smsfake.PostWebhook(context.Background(), webhookURL, authToken, form); err != nil {
		t.Fatalf("PostWebhook: %v", err)
	}

	s := <-got
	if want := "application/x-www-form-urlencoded"; s.contentType != want {
		t.Errorf("Content-Type = %q, want %q", s.contentType, want)
	}
	if want := twilio.ComputeSignature(authToken, webhookURL, form); s.signature != want {
		t.Errorf("X-Twilio-Signature = %q, want %q (computed over the exact URL posted to)", s.signature, want)
	}
	if !reflect.DeepEqual(s.form, form) {
		t.Errorf("posted form = %v, want %v", s.form, form)
	}
}

// TestPostWebhook_WrongAuthToken403s is the distinct auth-mismatch error
// path: correct path, but the poster signs with a different auth token than
// the server validates against, so the signature never matches.
func TestPostWebhook_WrongAuthToken403s(t *testing.T) {
	srv := &twilio.Server{AuthToken: "server-side-token", StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	webhookURL := ts.URL + "/sms/inbound"
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0003", "+15551234567", "+15105559999", "hello there")
	_, _, err := smsfake.PostWebhook(context.Background(), webhookURL, "wrong-client-token", form)
	if err == nil {
		t.Fatal("expected an error POSTing with a mismatched auth token, got nil")
	}
}
