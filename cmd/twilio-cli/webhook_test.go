package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

func TestWebhookCeremony_AcceptedByRealServer(t *testing.T) {
	const authToken = "test-auth-token"
	var gotForm url.Values
	mux := http.NewServeMux()
	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		gotForm = r.PostForm
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const from, to = "+15551234567", "+15559876543"
	webhookURL := ts.URL + "/webhook"
	streamURL, err := fetchStreamURL(context.Background(), webhookURL, authToken, "CAtest0001", from, to)
	if err != nil {
		t.Fatalf("fetchStreamURL: %v", err)
	}

	if !strings.HasPrefix(streamURL, "ws://") || !strings.HasSuffix(streamURL, "/streams") {
		t.Errorf("streamURL = %q, want ws://.../streams", streamURL)
	}

	for _, field := range []string{"CallSid", "AccountSid", "From", "To", "CallStatus", "Direction", "ApiVersion"} {
		if gotForm.Get(field) == "" {
			t.Errorf("webhook POST missing field %q", field)
		}
	}
	if got := gotForm.Get("CallSid"); got != "CAtest0001" {
		t.Errorf("CallSid = %q, want CAtest0001", got)
	}
	if got := gotForm.Get("From"); got != from {
		t.Errorf("From = %q, want %q", got, from)
	}
}

// TestWebhookForm_Aliases pins AATK-16 observable behavior 3: the webhook form
// carries Twilio's caller-id aliases Caller (= From) and Called (= To) plus a
// non-empty CallerName, in addition to the From/To it already sent.
func TestWebhookForm_Aliases(t *testing.T) {
	const from, to = "+15551234567", "+15559876543"
	form := webhookForm("CAaliases0001", from, to)

	if got := form.Get("Caller"); got != from {
		t.Errorf("Caller = %q, want %q (= From)", got, from)
	}
	if got := form.Get("Called"); got != to {
		t.Errorf("Called = %q, want %q (= To)", got, to)
	}
	if form.Get("CallerName") == "" {
		t.Error("CallerName is empty, want a non-empty placeholder")
	}
	// The raw From/To fields remain, unaffected by the aliases.
	if got := form.Get("From"); got != from {
		t.Errorf("From = %q, want %q", got, from)
	}
	if got := form.Get("To"); got != to {
		t.Errorf("To = %q, want %q", got, to)
	}
}

func TestWebhookCeremony_ExtractsStreamURL(t *testing.T) {
	t.Run("good TwiML extracts stream URL", func(t *testing.T) {
		twiml := []byte(`<Response><Connect><Stream url="ws://example.com/streams"/></Connect></Response>`)
		got, err := parseStreamURL(twiml)
		if err != nil {
			t.Fatalf("parseStreamURL: %v", err)
		}
		if want := "ws://example.com/streams"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("bad TwiML yields clear error", func(t *testing.T) {
		twiml := []byte(`<Response><Say>Hello</Say></Response>`)
		_, err := parseStreamURL(twiml)
		if err == nil {
			t.Fatal("expected error for non-Connect/Stream TwiML, got nil")
		}
		if !strings.Contains(err.Error(), "Connect") && !strings.Contains(err.Error(), "Stream") {
			t.Errorf("error %q does not name what was expected", err)
		}
	})

	t.Run("unparseable XML yields clear error", func(t *testing.T) {
		_, err := parseStreamURL([]byte("not xml at all"))
		if err == nil {
			t.Fatal("expected error for unparseable TwiML, got nil")
		}
	})
}

func TestCallSid_SingleSourcedAcrossWebhookAndStart(t *testing.T) {
	const authToken = "test-auth-token"
	const callSid = "CAsingle0001"

	srv := &twilio.Server{AuthToken: authToken, StreamScheme: "ws"}
	mux := http.NewServeMux()
	var gotWebhookCallSid string
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		gotWebhookCallSid = r.PostForm.Get("CallSid")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	webhookURL := ts.URL + "/webhook"
	if _, err := fetchStreamURL(context.Background(), webhookURL, authToken, callSid, "+15551234567", "+15559876543"); err != nil {
		t.Fatalf("fetchStreamURL: %v", err)
	}
	if gotWebhookCallSid != callSid {
		t.Fatalf("webhook POST CallSid = %q, want %q", gotWebhookCallSid, callSid)
	}

	startMsg, err := twilio.EncodeStart("SStest0001", callSid, defaultAccountSid, 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	f, err := twilio.DecodeFrame(startMsg)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.CallSID != callSid {
		t.Fatalf("start frame CallSID = %q, want %q", f.CallSID, callSid)
	}

	if gotWebhookCallSid != f.CallSID {
		t.Fatalf("CallSid mismatch: webhook=%q start=%q", gotWebhookCallSid, f.CallSID)
	}
}
