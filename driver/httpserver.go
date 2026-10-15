// The engine driver serves HTTP (design/aa-server-status.md §7.1, §10):
// aa-server-status needs a mandatory GET /healthz on :9730 to manage it as a
// "source" server (§6.1 — health probe is the authoritative "serving" signal).
// This file is intentionally minimal: the health listener and the
// Twilio-facing listener, nothing more. A local HTTP tester for the driver
// (app-cli) is separate, deferred work (design §11) and does not belong here.
package driver

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// healthzHandler answers the aa-server-status health probe. aa-server-status only
// needs to know the process is alive and listening — no dependency on driver
// state (Yaegi runtime, LLM tiers, etc.) is checked here.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}

// newHTTPMux builds the driver's HTTP routes. http.ServeMux dispatches by
// path only (no method gating), which matches design §6.1: the health probe
// just needs a 2xx GET, and we don't need to reject other methods.
func newHTTPMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	return mux
}

// StartHealthServer launches the health listener in the background and
// returns the *http.Server handle so the caller can Shutdown it on exit. A
// bind failure is reported loudly to stderr but does not crash the driver —
// aa-server-status will simply see the health probe fail, which is itself the
// correct signal (design §6.5: runtime errors abort loudly, they don't take
// the whole program down).
func StartHealthServer(addr string) *http.Server {
	srv := &http.Server{Addr: addr, Handler: newHTTPMux()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "health server: %v\n", err)
		}
	}()
	return srv
}

// TwilioServerOption configures StartTwilioServer (AATK-159). It mirrors
// twilio.RealtimeOption: the zero set of options is today's server exactly, and
// every option is additive, so a caller that passes none gets what a caller got
// before options existed.
type TwilioServerOption func(*twilioServerConfig)

// twilioServerConfig is the resolved option set for StartTwilioServer.
// Unexported: the options are the API, and a struct literal would be a second
// way to build the same value.
type twilioServerConfig struct {
	// wrap, when non-nil, is applied to each route's handler before it is
	// mounted. Nil mounts the handlers as they are.
	wrap func(route string, h http.Handler) http.Handler
}

func resolveTwilioServerConfig(opts []TwilioServerOption) twilioServerConfig {
	var cfg twilioServerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithHandlerWrapper wraps each Twilio route's handler before it is mounted. At
// construction, wrap is called once per route with that route's path
// ("/webhook", "/streams", "/sms/inbound", "/sms/status") and its handler, and
// the mux serves what it returns on that path. It exists so a consumer can put
// request middleware in front of these routes (typically an OpenTelemetry
// server span, so model calls made while handling an inbound message nest
// under it) without rebuilding the route table on a mux of its own.
//
// A nil wrap leaves the routes unwrapped. Given more than once, the last one
// wins, a nil one included. A wrap that returns a nil handler panics at
// construction, as http.ServeMux.Handle does.
//
// Caveats:
//
//   - /streams is the Media Streams WebSocket upgrade, which takes over the
//     connection through the ResponseWriter. The writer the wrapper passes on
//     must implement http.Hijacker, or have an Unwrap() http.ResponseWriter
//     method that leads to one; otherwise the upgrade is answered 501 and every
//     call fails. otelhttp.NewHandler keeps it. A span around /streams lasts the
//     whole call, so a wrapper that wants per-request spans can return h
//     unchanged for "/streams".
//   - The wrapper runs before signature validation, so forged POSTs pass
//     through it too (and get a span). It must not read r.Body or rewrite
//     r.Host or r.URL: validation parses the body itself and reconstructs the
//     signed URL from the host and path.
//   - The ctx HandleSMS receives is the request's context: it carries what the
//     wrapper put there, and it is cancelled when the request has been
//     handled, moments after HandleSMS returns. Work that outlives the
//     handler, such as a model call started in a goroutine, must run under
//     context.WithoutCancel(ctx), which keeps the values (the span parent
//     among them) and drops the cancellation. The same holds for the ctx
//     HandleSMSStatus and AnswerCall receive.
//   - With otelhttp, pass otelhttp.WithPublicEndpointFn returning true
//     (current otelhttp releases no longer have WithPublicEndpoint): these
//     requests come from the provider, not from a traced caller, so a trace
//     context they carry is linked rather than adopted as the parent. The
//     engine adds no span attributes of its own here, From and To included.
func WithHandlerWrapper(wrap func(route string, h http.Handler) http.Handler) TwilioServerOption {
	return func(c *twilioServerConfig) { c.wrap = wrap }
}

// newTwilioMux builds the Twilio-facing routes: /webhook (the Twilio call
// webhook, s.ServeHTTP), /streams (the Media Streams WebSocket upgrade,
// s.ServeStreams), /sms/inbound (the inbound-SMS webhook, s.ServeSMS), and
// /sms/status (the delivery-status callback, s.ServeSMSStatus). They are one
// table, so the path a route is mounted on is the same string a
// WithHandlerWrapper wrapper is handed for it.
//
// /sms/status belongs on this mux rather than a consumer's own because it is
// the URL a consumer puts in OutboundSMS.StatusCallback, and this listener is
// already the one the provider reaches. Left off, a consumer's only options are
// to rebuild this route table on a mux of its own — a copy that stops matching
// the day a route here changes — or to run a second listener on another port.
func newTwilioMux(s *twilio.Server, opts ...TwilioServerOption) *http.ServeMux {
	cfg := resolveTwilioServerConfig(opts)
	mux := http.NewServeMux()
	for _, rt := range []struct {
		path string
		h    http.HandlerFunc
	}{
		{"/webhook", s.ServeHTTP},
		{"/streams", s.ServeStreams},
		{"/sms/inbound", s.ServeSMS},
		{"/sms/status", s.ServeSMSStatus},
	} {
		var h http.Handler = rt.h
		if cfg.wrap != nil {
			h = cfg.wrap(rt.path, h)
		}
		mux.Handle(rt.path, h)
	}
	return mux
}

// StartTwilioServer launches the Twilio-facing HTTP listener in the background,
// serving the routes built by newTwilioMux — which is where they are listed, so
// adding one is not a two-comment edit. opts are passed straight through to
// newTwilioMux; with none, the server is exactly what it was before they
// existed.
// Bind failures are reported to stderr but do not crash the process.
//
// opts was added in AATK-159. Existing calls compile unchanged, but the
// function's type changed, so code holding StartTwilioServer in a variable of
// the old func type must update that type.
func StartTwilioServer(addr string, s *twilio.Server, opts ...TwilioServerOption) *http.Server {
	mux := newTwilioMux(s, opts...)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "twilio server: %v\n", err)
		}
	}()
	return srv
}
