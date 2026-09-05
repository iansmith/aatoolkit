package driver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingSSEServer answers one SSE delta and records the Authorization
// header of the last request it saw.
func recordingSSEServer(lastAuth *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n")
	}))
}

// newTestHost builds a Host through New -- the production constructor, so the
// client under test is the one production uses -- with the fast tier pointed
// at url.
func newTestHost(url, apiKey string) *Host {
	return New(Config{
		Tiers: map[string]Tier{
			"fast": {URL: url, Model: "test", MaxTokens: 512, APIKey: apiKey},
		},
		Prompt: func() string { return "" },
	})
}

// TestTierAPIKey_IsSentAsBearer (AATK-112): a tier with an APIKey sends it as
// Authorization: Bearer on the chat request; a tier without one sends no
// Authorization header at all -- byte-identical to the request before this
// field existed.
func TestTierAPIKey_IsSentAsBearer(t *testing.T) {
	var lastAuth atomic.Value
	srv := recordingSSEServer(&lastAuth)
	defer srv.Close()

	h := newTestHost(srv.URL+"/v1/chat/completions", "sk-test-123")
	if _, _, err := h.Send([]byte("[]"), "fast"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := lastAuth.Load(); got != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test-123")
	}

	h = newTestHost(srv.URL+"/v1/chat/completions", "")
	if _, _, err := h.Send([]byte("[]"), "fast"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := lastAuth.Load(); got != "" {
		t.Errorf("Authorization = %q with no APIKey, want no header", got)
	}
}

// TestTierAPIKey_DoesNotFollowARedirect (AATK-112): a 30x from the tier's
// upstream is not followed. Go's client keeps Authorization across a redirect
// to the same hostname, a subdomain, or another port, and strips it only for
// an unrelated host; the fleet's standing rule is not to lean on that -- a
// redirect is surfaced as the response it is, and the second host never sees
// a request. Red today: the default client follows the 302 and the second
// server records the hop.
func TestTierAPIKey_DoesNotFollowARedirect(t *testing.T) {
	var hops int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"leaked\"},\"finish_reason\":null}]}\n")
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer first.Close()

	h := newTestHost(first.URL+"/v1/chat/completions", "sk-test-123")
	_, _, err := h.Send([]byte("[]"), "fast")
	if err == nil {
		t.Fatalf("Send followed the redirect and returned content; want the 302 reported as an error")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("error = %v, want it to name status 302", err)
	}
	if n := atomic.LoadInt32(&hops); n != 0 {
		t.Errorf("second host received %d request(s); the redirect must not be followed", n)
	}
}
