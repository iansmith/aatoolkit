package twilio_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// TestRESTClient_SendSMS_RequestShape locks the request shape SendSMS must produce:
// path, form fields, and the API-key-pair Basic-auth header (D11 "prefer the API-key
// pair"). The expected auth blob is computed in-test from the constants, never
// hardcoded, so the test documents the encoding rather than duplicating a literal.
func TestRESTClient_SendSMS_RequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotFrom, gotTo, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotFrom = r.PostForm.Get("From")
		gotTo = r.PostForm.Get("To")
		gotBody = r.PostForm.Get("Body")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client := testClient(srv.URL)

	_, err := client.SendSMS(context.Background(), testSMS())
	if err != nil {
		t.Fatalf("SendSMS returned error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/2010-04-01/Accounts/AC123/Messages.json"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// Against the constants testSMS is built from, not repeated literals: this
	// is the one test that reads those values, so a drift between what is sent
	// and what is expected has to fail here rather than pass on both sides.
	if gotFrom != testFrom {
		t.Errorf("From = %q, want %q", gotFrom, testFrom)
	}
	if gotTo != testTo {
		t.Errorf("To = %q, want %q", gotTo, testTo)
	}
	if gotBody != testBody {
		t.Errorf("Body = %q, want %q", gotBody, testBody)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("SK1:sekret"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
}

// TestRESTClient_SendSMS_ContentType asserts the request is form-encoded per the
// ticket's observable behavior 2 ("Content-Type: application/x-www-form-urlencoded").
// Added by Phase-0 adversary review: the RequestShape test parses the form via
// r.ParseForm but never pins the Content-Type header itself, so a client that sent
// e.g. JSON with matching field names could pass it unnoticed.
func TestRESTClient_SendSMS_ContentType(t *testing.T) {
	var gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client := testClient(srv.URL)

	if _, err := client.SendSMS(context.Background(), testSMS()); err != nil {
		t.Fatalf("SendSMS returned error: %v", err)
	}

	if want := "application/x-www-form-urlencoded"; !strings.HasPrefix(gotContentType, want) {
		t.Errorf("Content-Type = %q, want prefix %q", gotContentType, want)
	}
}

// TestRESTClient_SendSMS_ErrorOnNon2xx asserts non-2xx responses surface a non-nil
// error carrying the status code, and that the error text never leaks the credential
// secret or the basic-auth blob (ticket: "Credentials are never logged").
func TestRESTClient_SendSMS_ErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code": 21211, "message": "Invalid 'To' Phone Number"}`))
	}))
	defer srv.Close()

	client := testClient(srv.URL)

	_, err := client.SendSMS(context.Background(), testSMS())
	if err == nil {
		t.Fatal("SendSMS returned nil error for a 400 response, want non-nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q does not contain status code 400", err.Error())
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("SK1:sekret"))
	if strings.Contains(err.Error(), "sekret") {
		t.Errorf("error %q leaks the KeySecret", err.Error())
	}
	if strings.Contains(err.Error(), wantAuth) {
		t.Errorf("error %q leaks the basic-auth blob", err.Error())
	}
}

// --- AATK-35: the message id, and the delivery-status callback ---

// testSMS is the message every test in this file sends. Only RequestShape reads
// its values; everywhere else the two numbers and the body are incidental.
func testSMS() twilio.OutboundSMS {
	return twilio.OutboundSMS{From: testFrom, To: testTo, Body: testBody}
}

const (
	testFrom = "+15550001111"
	testTo   = "+15550002222"
	testBody = "hello"
)

// wantMessageSID is the id the stub answers with, and messagesResource is built
// around it rather than repeating it — one definition, so an edit cannot leave
// the fixture and the expectation disagreeing and read as a client bug.
const wantMessageSID = "SM1f9a0b7c2d3e4f5061728394a5b6c7d8"

// messagesResource is the shape Twilio answers a successful send with, kept to
// the fields this test is about. Fields echoing the request (to, from, body)
// are deliberately left out: present, they would look like they must track
// testSMS, and nothing would notice when they stopped.
var messagesResource = fmt.Sprintf(`{
  "sid": %q,
  "account_sid": "AC123",
  "status": "queued",
  "error_code": null,
  "error_message": null
}`, wantMessageSID)

// testClient points a client at srv with the credentials the auth assertions in
// this file are written against.
func testClient(baseURL string) twilio.RESTClient {
	return twilio.RESTClient{AccountSID: "AC123", KeySID: "SK1", KeySecret: "sekret", BaseURL: baseURL}
}

// The id must come from the response body. Asserting it equals the stub's sid,
// rather than merely that some non-empty string came back, is what rules out a
// client returning a fabricated or hardcoded value.
func TestRESTClient_SendSMS_ReturnsProviderMessageSID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(messagesResource))
	}))
	defer srv.Close()

	client := testClient(srv.URL)

	sid, err := client.SendSMS(context.Background(), testSMS())
	if err != nil {
		t.Fatalf("SendSMS returned error: %v", err)
	}
	if sid != wantMessageSID {
		t.Errorf("message SID = %q, want %q", sid, wantMessageSID)
	}
}

// The callback is what makes the id useful: every StatusCallback POST carries
// the MessageSid, so the id is the correlator that matches a terminal status
// back to the send it belongs to.
func TestRESTClient_SendSMS_StatusCallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		callback string
		wantSent bool
	}{
		{"supplied", "https://example.test/sms/status?ref=abc", true},
		{"omitted", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var present bool

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm: %v", err)
					return
				}
				// Distinguish "absent" from "present but empty" — sending the
				// field with an empty value is not the same request, and only
				// the map lookup can tell them apart.
				_, present = r.PostForm["StatusCallback"]
				got = r.PostForm.Get("StatusCallback")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(messagesResource))
			}))
			defer srv.Close()

			client := testClient(srv.URL)
			msg := testSMS()
			msg.StatusCallback = tc.callback

			if _, err := client.SendSMS(context.Background(), msg); err != nil {
				t.Fatalf("SendSMS returned error: %v", err)
			}

			if present != tc.wantSent {
				t.Errorf("StatusCallback present = %v, want %v", present, tc.wantSent)
			}
			if tc.wantSent && got != tc.callback {
				t.Errorf("StatusCallback = %q, want %q verbatim — a caller may carry its own correlation id in the query string", got, tc.callback)
			}
		})
	}
}

// A 2xx means the message was accepted, and that stays true even if we cannot
// find an id in the response. Returning an error here would tell the caller the
// send failed when it did not, and the obvious reaction to a failed send is to
// send again — so a parsing problem would turn into a duplicate SMS to a real
// handset. The send succeeded; only traceability was lost, and an empty id says
// exactly that.
func TestRESTClient_SendSMS_UnparseableSuccessBodyIsNotASendFailure(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "OK"},
		{"json without sid", `{"status":"queued"}`},
		{"empty body", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := testClient(srv.URL)

			sid, err := client.SendSMS(context.Background(), testSMS())
			if err != nil {
				t.Errorf("SendSMS error = %v, want nil — the provider accepted the message", err)
			}
			if sid != "" {
				t.Errorf("message SID = %q, want empty when the response carries none", sid)
			}
		})
	}
}
