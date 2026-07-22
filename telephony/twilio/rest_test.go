package twilio_test

import (
	"context"
	"encoding/base64"
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

	client := twilio.RESTClient{
		AccountSID: "AC123",
		KeySID:     "SK1",
		KeySecret:  "sekret",
		BaseURL:    srv.URL,
	}

	err := client.SendSMS(context.Background(), "+15550001111", "+15550002222", "hello")
	if err != nil {
		t.Fatalf("SendSMS returned error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/2010-04-01/Accounts/AC123/Messages.json"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotFrom != "+15550001111" {
		t.Errorf("From = %q, want +15550001111", gotFrom)
	}
	if gotTo != "+15550002222" {
		t.Errorf("To = %q, want +15550002222", gotTo)
	}
	if gotBody != "hello" {
		t.Errorf("Body = %q, want hello", gotBody)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("SK1:sekret"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
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

	client := twilio.RESTClient{
		AccountSID: "AC123",
		KeySID:     "SK1",
		KeySecret:  "sekret",
		BaseURL:    srv.URL,
	}

	err := client.SendSMS(context.Background(), "+15550001111", "+15550002222", "hello")
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
