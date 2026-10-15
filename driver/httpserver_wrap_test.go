package driver

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// twilioRoutePaths is every path newTwilioMux mounts, in route-table order.
// Spelled out here rather than read from the table: these strings are the
// contract a wrapper is written against.
var twilioRoutePaths = []string{"/webhook", "/streams", "/sms/inbound", "/sms/status"}

// wrapTagHeader is the response header a test wrapper sets, so a response
// shows which wrapper (and which of its calls) served it.
const wrapTagHeader = "X-Test-Wrap-Tag"

// routeTagger returns a WithHandlerWrapper function that counts each route it
// is asked to wrap in seen, and tags every response its handler serves with
// tag(route) before calling the route's own handler.
func routeTagger(seen map[string]int, tag func(route string) string) func(string, http.Handler) http.Handler {
	return func(route string, h http.Handler) http.Handler {
		seen[route]++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(wrapTagHeader, tag(route))
			h.ServeHTTP(w, r)
		})
	}
}

// postRoute sends an unsigned form POST to base+path and returns the status
// and the wrapper tag on the response ("" when no wrapper served it).
func postRoute(t *testing.T, client *http.Client, base, path string) (status int, tag string) {
	t.Helper()
	resp, err := client.Post(base+path, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(wrapTagHeader)
}

// postInboundSMS serves a twilio.Server whose HandleSMS is handle through
// StartTwilioServer with opts -- the exported entry point, so the options have
// to survive the pass-through to newTwilioMux as well as be applied there --
// and posts it one inbound SMS from from with body, signed the way the
// provider signs it. It fails the test unless the answer is 200. HandleSMS
// runs inside ServeSMS, so when this returns, the handler has returned.
func postInboundSMS(t *testing.T, handle func(context.Context, twilio.InboundSMS), from, body string, opts ...TwilioServerOption) {
	t.Helper()
	s := &twilio.Server{
		AuthToken:    "authtoken",
		StreamScheme: "ws", // so the signature is reconstructed over http, matching httptest
		HandleSMS:    handle,
	}
	hs := StartTwilioServer("127.0.0.1:0", s, opts...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	})
	ts := httptest.NewServer(hs.Handler)
	t.Cleanup(ts.Close)

	form := url.Values{
		"MessageSid": {"SM123"},
		"From":       {from},
		"Body":       {body},
	}
	rawURL := ts.URL + "/sms/inbound"
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilio.ComputeSignature("authtoken", rawURL, form))

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /sms/inbound: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sms/inbound status = %d, want 200", resp.StatusCode)
	}
}

// allRoutesOnce is the wrapper-call count a wrapper must see: every route,
// exactly once.
func allRoutesOnce() map[string]int {
	want := make(map[string]int, len(twilioRoutePaths))
	for _, p := range twilioRoutePaths {
		want[p] = 1
	}
	return want
}

// The wrapper is called once per route, with that route's own path, and what
// it returns is what the mux serves on that path. Both halves matter: a route
// left out of the loop is served unwrapped (no span, silently), and a wrapper
// handed the wrong path names every span after the wrong route.
func TestWithHandlerWrapper_WrapsEachRouteOnceWithItsPath(t *testing.T) {
	seen := map[string]int{}
	wrap := routeTagger(seen, func(route string) string { return route })
	srv := httptest.NewServer(newTwilioMux(&twilio.Server{AuthToken: "authtoken"}, WithHandlerWrapper(wrap)))
	defer srv.Close()

	if want := allRoutesOnce(); !maps.Equal(seen, want) {
		t.Errorf("wrapper calls by route = %v, want %v", seen, want)
	}
	for _, path := range twilioRoutePaths {
		status, tag := postRoute(t, srv.Client(), srv.URL, path)
		if tag != path {
			t.Errorf("POST %s: status %d, wrapper tag %q, want %q — the handler served on %s is not the one the wrapper returned for it", path, status, tag, path, path)
		}
	}
}

// wrapperCtxKey is the context key a test wrapper stores its value under.
type wrapperCtxKey struct{}

// The point of the option: what a wrapper puts on the request context (a
// server span, in production) reaches the consumer's HandleSMS. Driven through
// StartTwilioServer, the exported entry point, so the options have to survive
// the pass-through to newTwilioMux as well as be applied there.
//
// It also checks the caveat WithHandlerWrapper documents: that ctx is the
// request's, ended when the handler returns, and context.WithoutCancel keeps
// its values while dropping that cancellation.
func TestWithHandlerWrapper_ContextReachesSMSHandler(t *testing.T) {
	got := make(chan context.Context, 1)
	wrap := func(route string, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), wrapperCtxKey{}, route)))
		})
	}
	postInboundSMS(t, func(ctx context.Context, _ twilio.InboundSMS) { got <- ctx }, "+15551234567", "hello", WithHandlerWrapper(wrap))

	var ctx context.Context
	select {
	case ctx = <-got:
	default:
		t.Fatal("HandleSMS was not called")
	}
	if v := ctx.Value(wrapperCtxKey{}); v != "/sms/inbound" {
		t.Fatalf("HandleSMS ctx value = %v, want %q — the wrapper's request context did not reach the handler", v, "/sms/inbound")
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Errorf("HandleSMS ctx not cancelled after ServeSMS returned — the WithoutCancel caveat in WithHandlerWrapper's doc no longer holds")
	}
	detached := context.WithoutCancel(ctx)
	if detached.Err() != nil {
		t.Errorf("context.WithoutCancel(ctx).Err() = %v, want nil", detached.Err())
	}
	if v := detached.Value(wrapperCtxKey{}); v != "/sms/inbound" {
		t.Errorf("context.WithoutCancel(ctx) value = %v, want %q", v, "/sms/inbound")
	}
}

// A nil wrapper is the same as no option, including when it follows a real
// one: given more than once, the last WithHandlerWrapper wins, nil included.
func TestWithHandlerWrapper_NilIsNoOpAndLastWins(t *testing.T) {
	s := &twilio.Server{AuthToken: "authtoken"}
	plain := httptest.NewServer(newTwilioMux(s))
	defer plain.Close()

	t.Run("nil", func(t *testing.T) {
		seenA := map[string]int{}
		a := routeTagger(seenA, func(string) string { return "a" })
		for name, opts := range map[string][]TwilioServerOption{
			"alone":         {WithHandlerWrapper(nil)},
			"after wrapper": {WithHandlerWrapper(a), WithHandlerWrapper(nil)},
		} {
			t.Run(name, func(t *testing.T) {
				srv := httptest.NewServer(newTwilioMux(s, opts...))
				defer srv.Close()
				for _, path := range twilioRoutePaths {
					wantStatus, _ := postRoute(t, plain.Client(), plain.URL, path)
					status, tag := postRoute(t, srv.Client(), srv.URL, path)
					if status != wantStatus || tag != "" {
						t.Errorf("POST %s: status %d, wrapper tag %q; want status %d (as with no option) and no tag", path, status, tag, wantStatus)
					}
				}
			})
		}
		if len(seenA) != 0 {
			t.Errorf("wrapper a called for %v; a later WithHandlerWrapper(nil) must replace it", seenA)
		}
	})

	t.Run("last wins", func(t *testing.T) {
		seenA, seenB := map[string]int{}, map[string]int{}
		a := routeTagger(seenA, func(string) string { return "a" })
		b := routeTagger(seenB, func(string) string { return "b" })
		srv := httptest.NewServer(newTwilioMux(s, WithHandlerWrapper(a), WithHandlerWrapper(b)))
		defer srv.Close()

		if len(seenA) != 0 {
			t.Errorf("first wrapper called for %v, want never: the last WithHandlerWrapper wins", seenA)
		}
		if want := allRoutesOnce(); !maps.Equal(seenB, want) {
			t.Errorf("last wrapper calls by route = %v, want %v", seenB, want)
		}
		for _, path := range twilioRoutePaths {
			if status, tag := postRoute(t, srv.Client(), srv.URL, path); tag != "b" {
				t.Errorf("POST %s: status %d, wrapper tag %q, want %q", path, status, tag, "b")
			}
		}
	})
}

// unwrappingWriter hides the ResponseWriter's other methods but offers it
// back through Unwrap, the way well-behaved middleware writers do.
type unwrappingWriter struct{ http.ResponseWriter }

func (u unwrappingWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// hidingWriter hides the ResponseWriter's Hijack and offers no Unwrap: only
// Header, Write and WriteHeader are promoted from the embedded interface.
type hidingWriter struct{ http.ResponseWriter }

// upgradeStatus sends a WebSocket upgrade request to rawURL using the standard
// library only, and returns the response status. It closes the connection
// either way, which ends a ServeStreams waiting for its first frame.
func upgradeStatus(t *testing.T, client *http.Client, rawURL string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") // RFC 6455's sample nonce: 16 bytes, base64
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s (upgrade): %v", rawURL, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// /streams is a WebSocket upgrade, which takes over the connection through the
// ResponseWriter's http.Hijacker. A wrapper that passes the writer on as it
// came, or wraps it in one that still offers Unwrap, keeps the upgrade
// working; one that hides both turns every call into a 501. This is the
// caveat WithHandlerWrapper documents for /streams.
func TestWithHandlerWrapper_StreamsUpgradeNeedsHijacker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wrapWriter func(http.ResponseWriter) http.ResponseWriter
		want       int
	}{
		{"passthrough", func(w http.ResponseWriter) http.ResponseWriter { return w }, http.StatusSwitchingProtocols},
		{"writer with Unwrap", func(w http.ResponseWriter) http.ResponseWriter { return unwrappingWriter{w} }, http.StatusSwitchingProtocols},
		{"writer hiding Hijacker and Unwrap", func(w http.ResponseWriter) http.ResponseWriter { return hidingWriter{w} }, http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrap := func(route string, h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					h.ServeHTTP(tc.wrapWriter(w), r)
				})
			}
			srv := httptest.NewServer(newTwilioMux(&twilio.Server{}, WithHandlerWrapper(wrap)))
			defer srv.Close()

			if got := upgradeStatus(t, srv.Client(), srv.URL+"/streams"); got != tc.want {
				t.Errorf("upgrade on /streams status = %d, want %d", got, tc.want)
			}
		})
	}
}
