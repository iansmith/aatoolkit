package twilio_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// computeSig is the reference Twilio HMAC-SHA1 algorithm used to produce
// known-good values for the acceptance tests below.
func computeSig(authToken, rawURL string, params url.Values) string {
	h := hmac.New(sha1.New, []byte(authToken))
	h.Write([]byte(rawURL))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(params[k][0]))
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Edge: empty signature is always rejected.
func TestValidateSignature_EmptySignatureRejected(t *testing.T) {
	if twilio.ValidateSignature("authtoken", "https://example.com/webhook", nil, "") {
		t.Fatal("empty signature must be rejected")
	}
}

// Edge: URL with query params contributes to the HMAC (not stripped).
func TestValidateSignature_URLWithQueryParamsValidates(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook?foo=bar"
	sig := computeSig(token, rawURL, nil)
	if !twilio.ValidateSignature(token, rawURL, nil, sig) {
		t.Fatal("correct signature over URL with query params must validate")
	}
}

// Rejection: garbled signature bytes are rejected.
func TestValidateSignature_WrongSignatureRejected(t *testing.T) {
	if twilio.ValidateSignature("authtoken", "https://example.com/webhook", nil, "bm90YXJlYWxzaWc=") {
		t.Fatal("wrong signature must be rejected")
	}
}

// Rejection: correct signature against a different auth token is rejected.
func TestValidateSignature_WrongTokenRejected(t *testing.T) {
	sig := computeSig("correcttoken", "https://example.com/webhook", nil)
	if twilio.ValidateSignature("wrongtoken", "https://example.com/webhook", nil, sig) {
		t.Fatal("signature valid for a different token must be rejected")
	}
}

// Happy: correct HMAC-SHA1 with no POST params validates.
func TestValidateSignature_CorrectSignatureAccepted(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	sig := computeSig(token, rawURL, nil)
	if !twilio.ValidateSignature(token, rawURL, nil, sig) {
		t.Fatal("correct HMAC-SHA1 signature must validate")
	}
}

// Happy: correct HMAC-SHA1 with sorted POST params validates.
func TestValidateSignature_CorrectSignatureWithParamsAccepted(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	params := url.Values{"CallSid": {"CA123"}, "From": {"+15005550006"}}
	sig := computeSig(token, rawURL, params)
	if !twilio.ValidateSignature(token, rawURL, params, sig) {
		t.Fatal("correct HMAC-SHA1 with POST params must validate")
	}
}

// Happy: ComputeSignature's output is accepted by ValidateSignature, and a
// signature computed with a different token is rejected.
func TestComputeSignature_AcceptedByValidate(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	params := url.Values{"CallSid": {"CA123"}, "From": {"+15005550006"}}

	sig := twilio.ComputeSignature(token, rawURL, params)
	if !twilio.ValidateSignature(token, rawURL, params, sig) {
		t.Fatal("ComputeSignature output must be accepted by ValidateSignature")
	}

	wrongSig := twilio.ComputeSignature("wrongtoken", rawURL, params)
	if twilio.ValidateSignature(token, rawURL, params, wrongSig) {
		t.Fatal("signature computed with a different token must be rejected")
	}
}

// statusCallback is the what these tests pass PostSignedForm: a webhook Twilio
// signs like any other, under a label none of PostSignedForm's callers use, so
// an error text naming a caller's label in place of what fails here.
const statusCallback = "status callback"

// The request, from the receiving side: POSTed to webhookURL, form-encoded,
// the form intact, and carrying the X-Twilio-Signature ComputeSignature yields
// over that same URL. The URL carries a query string because the signature
// covers it, so a poster that signed the URL without its query, or POSTed
// without it, would fail here. The URL posted to is rebuilt the way Server
// rebuilds the one it validates a signature over.
func TestPostSignedForm_SignsTheFormOverTheExactURL(t *testing.T) {
	const token = "authtoken"
	type seen struct {
		target, method, contentType, signature string
		form                                   url.Values
	}
	got := make(chan seen, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		got <- seen{"http://" + r.Host + r.URL.RequestURI(), r.Method, r.Header.Get("Content-Type"), r.Header.Get("X-Twilio-Signature"), r.PostForm}
	}))
	defer ts.Close()

	rawURL := ts.URL + "/sms/status?ref=abc"
	form := url.Values{"MessageSid": {"SM123"}, "MessageStatus": {"delivered"}}
	if _, _, err := twilio.PostSignedForm(context.Background(), statusCallback, rawURL, token, form); err != nil {
		t.Fatalf("PostSignedForm: %v", err)
	}

	var s seen
	select {
	case s = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no request reached the server")
	}
	if s.target != rawURL {
		t.Errorf("POSTed to %q, want %q, the URL signed over", s.target, rawURL)
	}
	if s.method != http.MethodPost {
		t.Errorf("method = %q, want %q", s.method, http.MethodPost)
	}
	if want := "application/x-www-form-urlencoded"; s.contentType != want {
		t.Errorf("Content-Type = %q, want %q", s.contentType, want)
	}
	if want := twilio.ComputeSignature(token, rawURL, form); s.signature != want {
		t.Errorf("X-Twilio-Signature = %q, want %q (computed over the exact URL posted to)", s.signature, want)
	}
	if !reflect.DeepEqual(s.form, form) {
		t.Errorf("posted form = %v, want %v", s.form, form)
	}
}

// Each outcome PostSignedForm's doc names: the status, the body and the error
// text, which names what at every stage. A nil wantBody means nil, not merely
// empty.
func TestPostSignedForm_ReturnsWhatTheClientHandedBack(t *testing.T) {
	serve := func(h http.HandlerFunc) string {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts.URL + "/sms/status"
	}
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	redirectTo := func(target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		}
	}
	// Port 1 is privileged, so no test process can be listening on it.
	const unanswered = "http://127.0.0.1:1/sms/status"

	okURL := serve(answer(http.StatusOK, "<Response></Response>"))
	forbiddenURL := serve(answer(http.StatusForbidden, "signature mismatch"))
	createdURL := serve(answer(http.StatusCreated, "created, which is still not 200"))
	truncatedURL := serve(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// A Content-Length the body never reaches, then a dropped connection.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		_ = buf.Flush()
	})
	loopURL := serve(redirectTo("/sms/status"))
	redirectsToUnansweredURL := serve(redirectTo(unanswered))
	badLocationURL := serve(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://[::1/sms/status")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	for _, tc := range []struct {
		name, webhookURL string
		wantStatus       int
		wantBody         []byte
		wantErr          string // the whole error text, unless prefixOnly; "" for none
		prefixOnly       bool   // the rest is net/http's own error
	}{
		{"200", okURL, http.StatusOK, []byte("<Response></Response>"), "", false},
		{"403", forbiddenURL, http.StatusForbidden, []byte("signature mismatch"),
			statusCallback + " " + forbiddenURL + " returned status 403: signature mismatch", false},
		{"201", createdURL, http.StatusCreated, []byte("created, which is still not 200"),
			statusCallback + " " + createdURL + " returned status 201: created, which is still not 200", false},
		{"body cut short", truncatedURL, http.StatusOK, []byte("partial"),
			"read " + statusCallback + " response: unexpected EOF", false},
		{"redirect policy gave up", loopURL, http.StatusTemporaryRedirect, nil, "post " + statusCallback + ": ", true},
		{"unbuildable request", "http://[::1/sms/status", 0, nil, "build " + statusCallback + " request: ", true},
		{"nobody listening", unanswered, 0, nil, "post " + statusCallback + ": ", true},
		{"redirect to nobody listening", redirectsToUnansweredURL, 0, nil, "post " + statusCallback + ": ", true},
		{"redirect Location that will not parse", badLocationURL, 0, nil, "post " + statusCallback + ": ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, err := twilio.PostSignedForm(context.Background(), statusCallback, tc.webhookURL, "authtoken", url.Values{"MessageSid": {"SM123"}})
			got := ""
			if err != nil {
				got = err.Error()
			}
			if tc.prefixOnly && !strings.HasPrefix(got, tc.wantErr) {
				t.Errorf("error = %q, want it to start %q", got, tc.wantErr)
			} else if !tc.prefixOnly && got != tc.wantErr {
				t.Errorf("error = %q, want %q", got, tc.wantErr)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if !reflect.DeepEqual(body, tc.wantBody) {
				t.Errorf("body = %q (nil: %t), want %q (nil: %t)", body, body == nil, tc.wantBody, tc.wantBody == nil)
			}
		})
	}
}

// ctx is on the request. http.DefaultClient has no timeout of its own, so
// without it a server that takes the POST and never answers would hold
// PostSignedForm forever. The cancel lands only once the server holds the
// request, so it is the in-flight POST that ctx abandons, not one never sent.
func TestPostSignedForm_CancellingCtxAbandonsTheInFlightPOST(t *testing.T) {
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
	done := make(chan error, 1)
	go func() {
		_, _, err := twilio.PostSignedForm(ctx, statusCallback, ts.URL+"/sms/status", "authtoken", url.Values{"MessageSid": {"SM123"}})
		done <- err
	}()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the POST never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want one wrapping context.Canceled", err)
		}
		if want := "post " + statusCallback + ": "; err != nil && !strings.HasPrefix(err.Error(), want) {
			t.Errorf("error = %q, want it to start %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PostSignedForm still running 2s after its ctx was cancelled mid-POST")
	}
}
