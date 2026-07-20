package driver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// The health handler is what aa-server-status polls (design design/aa-server-status.md
// §7.1: health = { port = 9730, path = "/healthz" }) to decide the "source"
// server is up. It must be reachable with a plain GET and return 2xx with no
// dependency on the rest of the driver (Yaegi runtime, LLM tiers, etc.) being
// initialized — aa-server-status only cares that the process is alive and
// listening.
func TestHealthzHandlerReturns2xx(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("GET /healthz status = %d, want 2xx", rec.Code)
	}
}

// Cross-feature: the handler must be reachable through the real mux this
// package wires up (newHTTPServer), not just as a bare function — otherwise a
// routing mistake (wrong path, wrong method restriction) would pass the test
// above but still leave aa-server-status unable to reach it.
func TestHealthzServedThroughMux(t *testing.T) {
	srv := httptest.NewServer(newHTTPMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET /healthz status = %d, want 2xx", resp.StatusCode)
	}
}

// Adversary gap: a POST (or any non-GET) must not be treated as a routing
// miss (404) that would misleadingly read as "server down" to aa-server-status;
// a mandatory health probe (design §6.1) should still resolve through the mux
// rather than 404ing on method, so a strict health checker doesn't confuse a
// method mismatch with a dead server. http.ServeMux dispatches by path only
// (no method restriction) unless we explicitly add one, so this pins that we
// haven't accidentally restricted the route to GET only.
func TestHealthzMuxIgnoresMethod(t *testing.T) {
	srv := httptest.NewServer(newHTTPMux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /healthz: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST /healthz status = %d, want 2xx (mux should not gate on method)", resp.StatusCode)
	}
}

// /webhook must route to (*twilio.Server).ServeHTTP — today StartTwilioServer
// only mounts /streams, leaving the webhook orphaned. We hit the mux directly
// (via newTwilioMux) rather than starting a real listener, mirroring the
// newHTTPMux tests above. A signature-gated 403 (not a 404) proves the route
// reached ServeHTTP rather than falling through to the mux's default handler.
func TestWebhookRoutedToServeHTTP(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"}
	srv := httptest.NewServer(newTwilioMux(s))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /webhook: unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("POST /webhook status = 404, want route to reach ServeHTTP (e.g. 403 for missing signature)")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /webhook status = %d, want 403 (no signature supplied)", resp.StatusCode)
	}
}
