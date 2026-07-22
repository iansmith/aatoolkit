package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// TestTwilioCLI_SMSRoundTrip pins AATK-23's observable behaviors: the SMS mode
// posts a correctly-signed inbound-SMS webhook to the server's /sms/inbound
// route, the server's stub HandleSMS replies via a RESTClient pointed at the
// CLI's capture server, and the capture server records exactly one outbound
// Messages.json POST whose To/Body match what Sophie's reply carried. The
// expected To is the <FROM> arg — the server replies to the sender.
func TestTwilioCLI_SMSRoundTrip(t *testing.T) {
	const authToken = "test-auth-token"
	const from, to, body = "+15551234567", "+15105559999", "hello there"
	const replyBody = "hi back"

	capture := newSMSCaptureServer()
	defer capture.Close()

	rest := &twilio.RESTClient{
		AccountSID: defaultAccountSid,
		BaseURL:    capture.URL,
	}

	srv := &twilio.Server{
		AuthToken:    authToken,
		StreamScheme: "ws",
		HandleSMS: func(ctx context.Context, msg twilio.InboundSMS) {
			if err := rest.SendSMS(ctx, to, msg.From, replyBody); err != nil {
				t.Errorf("stub HandleSMS: SendSMS: %v", err)
			}
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	webhookURL := ts.URL + "/sms/inbound"
	if _, err := runSMS(context.Background(), webhookURL, authToken, from, to, body, capture); err != nil {
		t.Fatalf("runSMS: %v", err)
	}

	msgs := capture.captured()
	if len(msgs) != 1 {
		t.Fatalf("capture server: got %d Messages.json POSTs, want 1", len(msgs))
	}
	if got := msgs[0].To; got != from {
		t.Errorf("captured To = %q, want %q (the FROM arg — server replies to sender)", got, from)
	}
	if got := msgs[0].Body; got != replyBody {
		t.Errorf("captured Body = %q, want %q", got, replyBody)
	}
}

// TestSMSForm_Fields pins the SMS webhook form field set: MessageSid,
// AccountSid, From, To, Body, ApiVersion.
func TestSMSForm_Fields(t *testing.T) {
	form := smsForm("SMtest0001", "+15551234567", "+15105559999", "hello there")

	if got := form.Get("MessageSid"); got != "SMtest0001" {
		t.Errorf("MessageSid = %q, want SMtest0001", got)
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
}

// TestPostSMSWebhook_WrongPath403s pins observable behavior 1's warning: the
// signature is computed over the exact POST URL, so posting to the wrong path
// (e.g. "/sms" instead of the registered "/sms/inbound") 403s.
func TestPostSMSWebhook_WrongPath403s(t *testing.T) {
	const authToken = "test-auth-token"

	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// postSMSWebhook signs over the URL it's given; giving it the wrong path
	// means the signature won't match what the server reconstructs from
	// r.URL.RequestURI(), so the server 403s and postSMSWebhook must error.
	wrongURL := ts.URL + "/sms"
	err := postSMSWebhook(context.Background(), wrongURL, authToken, "SMtest0002", "+15551234567", "+15105559999", "hello there")
	if err == nil {
		t.Fatal("expected an error POSTing to the wrong path, got nil")
	}
}

// TestPostSMSWebhook_WrongAuthToken403s is the distinct auth-mismatch error
// path: correct path, but the CLI signs with a different auth token than the
// server validates against, so the signature never matches.
func TestPostSMSWebhook_WrongAuthToken403s(t *testing.T) {
	srv := &twilio.Server{AuthToken: "server-side-token", StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	webhookURL := ts.URL + "/sms/inbound"
	err := postSMSWebhook(context.Background(), webhookURL, "wrong-client-token", "SMtest0003", "+15551234567", "+15105559999", "hello there")
	if err == nil {
		t.Fatal("expected an error POSTing with a mismatched auth token, got nil")
	}
}
