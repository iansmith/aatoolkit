package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// freeTCPPort returns a port that was bindable on 127.0.0.1 a moment ago, by
// binding one and closing it again. Used to exercise an *explicit* bind
// without hardcoding a port number the machine may already be using.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// TestTwilioCLI_SMSRoundTrip pins AATK-23's observable behaviors: the SMS mode
// posts a correctly-signed inbound-SMS webhook to the server's /sms/inbound
// route, the server's stub HandleSMS replies via a RESTClient pointed at the
// CLI's capture server, and the capture server records exactly one outbound
// Messages.json POST whose To/Body match what the server's reply carried. The
// expected To is the <FROM> arg — the server replies to the sender.
func TestTwilioCLI_SMSRoundTrip(t *testing.T) {
	const authToken = "test-auth-token"
	const from, to, body = "+15551234567", "+15105559999", "hello there"
	const replyBody = "hi back"

	capture, err := newSMSCaptureServer(freeTCPPort(t))
	if err != nil {
		t.Fatalf("newSMSCaptureServer: %v", err)
	}
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

// TestSMSForm_Fields pins the SMS webhook form field set: MessageSid (plus
// its SmsMessageSid/SmsSid aliases), AccountSid, From, To, Body, ApiVersion.
func TestSMSForm_Fields(t *testing.T) {
	form := smsForm("SMtest0001", "+15551234567", "+15105559999", "hello there")

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
}

// TestPostSMSWebhook_SignatureURLMismatch403s pins observable behavior 1's
// warning directly: the signature is computed over the exact POST URL, so a
// signature computed for one URL and presented on a POST to a different URL
// must 403 — isolated from routing (both URLs below are otherwise valid,
// registered paths; only the signed-vs-actual URL differs), so this cannot
// pass merely because an unmatched route 404s.
func TestPostSMSWebhook_SignatureURLMismatch403s(t *testing.T) {
	const authToken = "test-auth-token"

	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	realURL := ts.URL + "/sms/inbound"
	wrongURLForSigning := ts.URL + "/sms"
	form := smsForm("SMtest0002", "+15551234567", "+15105559999", "hello there")
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

// TestNewSMSCaptureServer_BindsRequestedPort pins AATK-31 observable behavior
// 1: the capture server binds the port it was asked for, rather than an
// ephemeral one. The whole point is that the operator must be able to name the
// URL before the CLI runs — an ephemeral port makes that impossible, because
// the server reads TWILIO_API_BASE_URL once at startup.
func TestNewSMSCaptureServer_BindsRequestedPort(t *testing.T) {
	port := freeTCPPort(t)

	capture, err := newSMSCaptureServer(port)
	if err != nil {
		t.Fatalf("newSMSCaptureServer(%d): %v", port, err)
	}
	defer capture.Close()

	want := fmt.Sprintf(":%d", port)
	if !strings.Contains(capture.URL, want) {
		t.Errorf("capture server URL = %q, want it to contain %q (an explicit bind, not an ephemeral port)", capture.URL, want)
	}
}

// TestNewSMSCaptureServer_PortInUseNamesThePort is the error/rejection edge
// case for observable behavior 4: an occupied port fails immediately with an
// error the operator can act on — it names the port and the flag that changes
// it — rather than panicking the way httptest.NewServer does.
func TestNewSMSCaptureServer_PortInUseNamesThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	capture, err := newSMSCaptureServer(port)
	if err == nil {
		capture.Close()
		t.Fatalf("newSMSCaptureServer(%d) succeeded against a held port, want a bind error", port)
	}
	if got := err.Error(); !strings.Contains(got, strconv.Itoa(port)) {
		t.Errorf("bind error %q does not name the port %d", got, port)
	}
	if got := err.Error(); !strings.Contains(got, "-capture-port") {
		t.Errorf("bind error %q does not mention -capture-port, the flag that changes it", got)
	}
}

// TestRunSMS_CapturesReplyPostedAfterWebhookReturns pins AATK-31 observable
// behaviors 2 and 3, and is the test that fails against a no-wait runSMS.
//
// It mirrors the real responder rather than the convenient case: the webhook
// handler returns immediately (the responder only enqueues), and the outbound
// Messages.json POST is made from a separate goroutine *after* ServeSMS has
// answered. A version that posts synchronously from inside the handler would
// pass against the racy implementation and prove nothing.
func TestRunSMS_CapturesReplyPostedAfterWebhookReturns(t *testing.T) {
	const authToken = "test-auth-token"
	const from, to, body = "+15551234567", "+15105559999", "hello there"
	const replyBody = "hi back, later"

	// A margin over the loopback webhook round trip: an implementation that
	// checks the capture the instant postSMSWebhook returns must observe zero
	// messages, even though the reply is on its way.
	const replyDelay = 200 * time.Millisecond

	capture, err := newSMSCaptureServer(freeTCPPort(t))
	if err != nil {
		t.Fatalf("newSMSCaptureServer: %v", err)
	}
	defer capture.Close()

	// Closed once ServeSMS has returned, so the reply provably leaves after
	// the webhook has been answered — not merely "probably later".
	webhookReturned := make(chan struct{})
	sendErr := make(chan error, 1)

	srv := &twilio.Server{
		AuthToken:    authToken,
		StreamScheme: "ws",
		HandleSMS: func(_ context.Context, msg twilio.InboundSMS) {
			go func() {
				<-webhookReturned
				time.Sleep(replyDelay)
				rest := &twilio.RESTClient{AccountSID: defaultAccountSid, BaseURL: capture.URL}
				sendErr <- rest.SendSMS(context.Background(), to, msg.From, replyBody)
			}()
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", func(w http.ResponseWriter, r *http.Request) {
		srv.ServeSMS(w, r)
		close(webhookReturned)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	msg, err := runSMS(context.Background(), ts.URL+"/sms/inbound", authToken, from, to, body, capture)
	if err != nil {
		t.Fatalf("runSMS: %v (a reply posted after the webhook returned must still be captured)", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("async reply SendSMS: %v", err)
	}

	if got := msg.To; got != from {
		t.Errorf("captured To = %q, want %q (the FROM arg — the server replies to the sender)", got, from)
	}
	if got := msg.Body; got != replyBody {
		t.Errorf("captured Body = %q, want %q", got, replyBody)
	}
}

// TestRunSMS_TimesOutWithActionableError pins observable behavior 4's other
// half: when no reply ever arrives, the wait expires with an error that names
// the bound it waited and points at the env var the operator most likely
// forgot — and never the old count-based phrasing, which described the symptom
// rather than the cause.
func TestRunSMS_TimesOutWithActionableError(t *testing.T) {
	const authToken = "test-auth-token"
	const from, to, body = "+15551234567", "+15105559999", "hello there"

	// The production bound is defaultCaptureWait; the seam exists so this
	// path is exercised without waiting it out.
	const wait = 50 * time.Millisecond

	capture, err := newSMSCaptureServer(freeTCPPort(t))
	if err != nil {
		t.Fatalf("newSMSCaptureServer: %v", err)
	}
	defer capture.Close()

	// A server that accepts the webhook and never replies.
	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	_, err = runSMS(context.Background(), ts.URL+"/sms/inbound", authToken, from, to, body, capture, withCaptureWait(wait))
	if err == nil {
		t.Fatal("runSMS succeeded with no reply ever posted, want a wait-expiry error")
	}
	got := err.Error()
	if !strings.Contains(got, wait.String()) {
		t.Errorf("timeout error %q does not name the wait bound %s it waited", got, wait)
	}
	if !strings.Contains(got, "TWILIO_API_BASE_URL") {
		t.Errorf("timeout error %q does not mention TWILIO_API_BASE_URL, the most likely cause", got)
	}
	if strings.Contains(got, "got 0 Messages.json POSTs") {
		t.Errorf("timeout error %q still uses the old count phrasing, which named the symptom rather than the cause", got)
	}
}

// TestRunSMSMode_PrintsActionableBaseURLGuidance pins observable behavior 5:
// the guidance names the exact environment assignment the operator has to make
// before launching the server, not an internal Go field they have no way to
// set. The literal below is the contract — the default port and the shape of
// the line the operator copies.
func TestRunSMSMode_PrintsActionableBaseURLGuidance(t *testing.T) {
	const want = "TWILIO_API_BASE_URL=http://127.0.0.1:9750"

	got := smsCaptureGuidance(defaultCapturePort)

	if !strings.Contains(got, want) {
		t.Errorf("guidance = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "RESTClient.BaseURL") {
		t.Errorf("guidance = %q, still names RESTClient.BaseURL — an internal field the operator cannot set", got)
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
