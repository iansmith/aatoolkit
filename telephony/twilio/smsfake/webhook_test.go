package smsfake_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
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
// from the receiving side: the form arrives intact, form-encoded, POSTed, and
// carrying the X-Twilio-Signature twilio.ComputeSignature yields for the auth
// token, the exact URL posted to, and that form — which is what a real server
// checks.
//
// The method is asserted here because nothing downstream would notice it
// change: twilio.Server.ServeSMS reads the form whatever the method, so a PUT
// passes every test that goes through it, and only a consuming server that
// routes by method would answer it 405.
func TestPostWebhook_SignsTheFormOverTheExactURL(t *testing.T) {
	const authToken = "test-auth-token"

	type seen struct {
		method, contentType, signature string
		form                           url.Values
	}
	got := make(chan seen, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse posted form: %v", err)
		}
		got <- seen{r.Method, r.Header.Get("Content-Type"), r.Header.Get("X-Twilio-Signature"), r.PostForm}
	}))
	defer ts.Close()

	webhookURL := ts.URL + "/sms/inbound"
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0005", "+15551234567", "+15105559999", "hello there")
	if _, _, err := smsfake.PostWebhook(context.Background(), webhookURL, authToken, form); err != nil {
		t.Fatalf("PostWebhook: %v", err)
	}

	s := smsfake.ReceiveWithin(t, got, "request at the webhook server")
	if s.method != http.MethodPost {
		t.Errorf("method = %q, want %q", s.method, http.MethodPost)
	}
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

// TestPostWebhook_AcceptedByRealServerReturnsStatusAndBody drives the ceremony
// against the real inbound-SMS handler: a correctly signed post is accepted,
// the handler sees the message the form carried, and PostWebhook hands back
// the 200 and the body the server answered with rather than discarding them.
func TestPostWebhook_AcceptedByRealServerReturnsStatusAndBody(t *testing.T) {
	const authToken = "test-auth-token"

	got := make(chan twilio.InboundSMS, 1)
	srv := &twilio.Server{
		AuthToken:    authToken,
		StreamScheme: "ws",
		HandleSMS:    func(_ context.Context, msg twilio.InboundSMS) { got <- msg },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sms/inbound", srv.ServeSMS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0006", "+15551234567", "+15105559999", "hello there")
	status, body, err := smsfake.PostWebhook(context.Background(), ts.URL+"/sms/inbound", authToken, form)
	if err != nil {
		t.Fatalf("PostWebhook: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if want := "<Response></Response>"; string(body) != want {
		t.Errorf("body = %q, want the server's empty TwiML %q", body, want)
	}

	msg := smsfake.ReceiveWithin(t, got, "message passed to HandleSMS")
	if msg.MessageSID != "SMtest0006" || msg.From != "+15551234567" || msg.To != "+15105559999" || msg.Body != "hello there" {
		t.Errorf("server parsed %+v, want MessageSID SMtest0006, From +15551234567, To +15105559999, Body %q", msg, "hello there")
	}
}

// TestPostWebhook_NonOKReturnsStatusAndBodyWithTheError pins the rejection
// path: anything but 200 — a 2xx included — is an error naming the URL, the
// status and the body, and PostWebhook also returns that status and body, so a
// caller can act on the server's answer without parsing the error text.
func TestPostWebhook_NonOKReturnsStatusAndBodyWithTheError(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusForbidden, "signature mismatch"},
		{http.StatusCreated, "created, which is still not 200"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			webhookURL := ts.URL + "/sms/inbound"
			form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0007", "+15551234567", "+15105559999", "hello there")
			status, body, err := smsfake.PostWebhook(context.Background(), webhookURL, "test-auth-token", form)
			if err == nil {
				t.Fatalf("PostWebhook answered %d succeeded, want an error", tc.status)
			}
			if want := fmt.Sprintf("SMS webhook %s returned status %d: %s", webhookURL, tc.status, tc.body); err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if status != tc.status {
				t.Errorf("status = %d, want %d alongside the error", status, tc.status)
			}
			if string(body) != tc.body {
				t.Errorf("body = %q, want %q alongside the error", body, tc.body)
			}
		})
	}
}

// TestPostWebhook_TruncatedResponseReturnsStatusAndWhatArrived pins the read
// failure: the status line arrived, so its code comes back, together with the
// part of the body that was read before the connection dropped.
func TestPostWebhook_TruncatedResponseReturnsStatusAndWhatArrived(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// A Content-Length the body never reaches, then a dropped connection.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		_ = buf.Flush()
	}))
	defer ts.Close()

	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0008", "+15551234567", "+15105559999", "hello there")
	status, body, err := smsfake.PostWebhook(context.Background(), ts.URL+"/sms/inbound", "test-auth-token", form)
	if err == nil {
		t.Fatal("PostWebhook succeeded on a truncated response, want a read error")
	}
	if got := err.Error(); !strings.HasPrefix(got, "read SMS webhook response: ") {
		t.Errorf("error = %q, want the read-failure text", got)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d — the status line arrived before the body broke", status, http.StatusOK)
	}
	if string(body) != "partial" {
		t.Errorf("body = %q, want the %q that arrived", body, "partial")
	}
}

// TestPostWebhook_RedirectLoopReturnsTheLastStatus pins the one failure in
// which net/http hands back a response together with an error: its redirect
// policy gave up on the chain. That last redirect's status comes back; the
// body is nil, because the client has already closed it.
func TestPostWebhook_RedirectLoopReturnsTheLastStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sms/inbound", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0010", "+15551234567", "+15105559999", "hello there")
	status, body, err := smsfake.PostWebhook(context.Background(), ts.URL+"/sms/inbound", "test-auth-token", form)
	if err == nil {
		t.Fatal("PostWebhook succeeded against a redirect loop, want an error")
	}
	if got := err.Error(); !strings.HasPrefix(got, "post SMS webhook: ") {
		t.Errorf("error = %q, want the post-failure text", got)
	}
	if status != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d — the server answered, with a redirect the client stopped following", status, http.StatusTemporaryRedirect)
	}
	if body != nil {
		t.Errorf("body = %q, want nil — the client closed it before handing the response back", body)
	}
}

// TestPostWebhook_NoResponseReturnsNoStatus pins the failures in which the
// client hands no response back — a request that cannot be built, one nobody
// answers, and a redirect whose next hop nobody answers: an error carrying the
// matching text, a zero status and a nil body. In the last case the server did
// answer, with a 307, but net/http discards that response when the next hop
// fails, so a zero status means no response was handed back, not that nobody
// answered.
func TestPostWebhook_NoResponseReturnsNoStatus(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL + "/sms/inbound"
	closed.Close()

	redirectsToClosed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, closedURL, http.StatusTemporaryRedirect)
	}))
	defer redirectsToClosed.Close()

	for _, tc := range []struct {
		name, webhookURL, wantPrefix string
	}{
		{"unbuildable request", "http://[::1/sms/inbound", "build SMS webhook request: "},
		{"nobody listening", closedURL, "post SMS webhook: "},
		{"redirect to nobody listening", redirectsToClosed.URL + "/sms/inbound", "post SMS webhook: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0009", "+15551234567", "+15105559999", "hello there")
			status, body, err := smsfake.PostWebhook(context.Background(), tc.webhookURL, "test-auth-token", form)
			if err == nil {
				t.Fatal("PostWebhook succeeded, want an error")
			}
			if got := err.Error(); !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("error = %q, want it to start %q", got, tc.wantPrefix)
			}
			if status != 0 {
				t.Errorf("status = %d, want 0 when no response was handed back", status)
			}
			if body != nil {
				t.Errorf("body = %q, want nil when no response was handed back", body)
			}
		})
	}
}

// TestPostWebhook_CancellingCtxAbandonsTheInFlightPOST pins that ctx bounds
// the request itself. http.DefaultClient has no timeout of its own, so a
// webhook server that takes the request and never answers would hold
// PostWebhook forever if ctx were not on the request. The cancel lands only
// once the server holds the request, so it is the in-flight POST that ctx
// abandons, not one that was never sent.
func TestPostWebhook_CancellingCtxAbandonsTheInFlightPOST(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(arrived)
		select { // never answers; released only so ts.Close can return
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release) // runs before ts.Close, which waits for the handler

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, "SMtest0011", "+15551234567", "+15105559999", "hello there")
		status, body, err := smsfake.PostWebhook(ctx, ts.URL+"/sms/inbound", "test-auth-token", form)
		done <- result{status, body, err}
	}()

	smsfake.ReceiveWithin(t, arrived, "POST at the webhook server")
	cancel()

	r := smsfake.ReceiveWithin(t, done, "return from PostWebhook after its ctx was cancelled mid-POST")
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("error = %v, want one wrapping context.Canceled", r.err)
	}
	if r.err != nil && !strings.HasPrefix(r.err.Error(), "post SMS webhook: ") {
		t.Errorf("error = %q, want the post-failure text", r.err)
	}
	if r.status != 0 {
		t.Errorf("status = %d, want 0 — no response was handed back", r.status)
	}
	if r.body != nil {
		t.Errorf("body = %q, want nil — no response was handed back", r.body)
	}
}
