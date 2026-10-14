package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
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

// TestWebhookForm_AccountSidAndApiVersionAreTheDefaults pins the two voice-form
// fields read from smsfake's exported defaults. The two constants sit side by
// side under one package prefix, and the receiving server accepts the signed
// POST whatever they hold, so a swap or a wrong value would otherwise reach the
// wire unnoticed. smsfake's own tests pin the constants' literal values.
func TestWebhookForm_AccountSidAndApiVersionAreTheDefaults(t *testing.T) {
	form := webhookForm("CAdefaults0001", "+15551234567", "+15559876543")

	if got := form.Get("AccountSid"); got != smsfake.DefaultAccountSID {
		t.Errorf("AccountSid = %q, want smsfake.DefaultAccountSID %q", got, smsfake.DefaultAccountSID)
	}
	if got := form.Get("ApiVersion"); got != smsfake.DefaultAPIVersion {
		t.Errorf("ApiVersion = %q, want smsfake.DefaultAPIVersion %q", got, smsfake.DefaultAPIVersion)
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

	startMsg, err := twilio.EncodeStart("SStest0001", callSid, smsfake.DefaultAccountSID, 1)
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

// TestWebhookCeremony_ReturnsTheAnsweredStreamURL pins the success path
// exactly: the stream URL fetchStreamURL returns is the one the server's TwiML
// answered with, not merely one shaped like it.
func TestWebhookCeremony_ReturnsTheAnsweredStreamURL(t *testing.T) {
	const want = "wss://media.example.test/streams"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<Response><Connect><Stream url="` + want + `"/></Connect></Response>`))
	}))
	defer ts.Close()

	got, err := fetchStreamURL(context.Background(), ts.URL+"/webhook", "test-auth-token", "CAtest0002", "+15551234567", "+15559876543")
	if err != nil {
		t.Fatalf("fetchStreamURL: %v", err)
	}
	if got != want {
		t.Errorf("stream URL = %q, want %q", got, want)
	}
}

// TestWebhookCeremony_ErrorTextsNameTheWebhook pins the voice webhook's error
// text at each stage that can fail before the TwiML is parsed. twilio-cli
// prints that text as its fatal error, and
// TestTwilioCLIRecordSentMustNotClobberTheAudioItReplays reads "post webhook"
// out of the output to tell whether a run reached the network, so the wording
// is part of the tool's output, not a detail of how it was produced.
func TestWebhookCeremony_ErrorTextsNameTheWebhook(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("signature mismatch"))
	}))
	defer rejecting.Close()

	truncating := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	defer truncating.Close()

	// The URL will not parse, so the request is never built. The parse error
	// comes from url.Parse rather than being written out, so the test pins the
	// wording around it and not net/url's.
	const unbuildable = "http://[::1/webhook"
	_, parseErr := url.Parse(unbuildable)
	if parseErr == nil {
		t.Fatalf("url.Parse(%q) succeeded; the build case below would not fail", unbuildable)
	}
	// Port 1 is privileged, so no test process can be listening on it.
	const unanswered = "http://127.0.0.1:1/webhook"
	rejectingURL := rejecting.URL + "/webhook"

	for _, tc := range []struct {
		name, webhookURL string
		want             string // the whole error text, unless prefixOnly
		prefixOnly       bool   // the rest is the dial error, whose wording is the OS's
	}{
		{"build", unbuildable, "build webhook request: " + parseErr.Error(), false},
		{"post", unanswered, `post webhook: Post "` + unanswered + `": `, true},
		{"non-200", rejectingURL, "webhook " + rejectingURL + " returned status 403: signature mismatch", false},
		{"read", truncating.URL + "/webhook", "read webhook response: " + io.ErrUnexpectedEOF.Error(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchStreamURL(context.Background(), tc.webhookURL, "test-auth-token", "CAtest0003", "+15551234567", "+15559876543")
			if err == nil {
				t.Fatal("fetchStreamURL succeeded, want an error")
			}
			if got := err.Error(); tc.prefixOnly && !strings.HasPrefix(got, tc.want) {
				t.Errorf("error = %q, want it to start %q", got, tc.want)
			} else if !tc.prefixOnly && got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}
