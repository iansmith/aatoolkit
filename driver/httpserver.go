// The the engine driver is becoming an HTTP server (design design/aa-server-status.md
// §7.1, §10): aa-server-status needs a mandatory GET /healthz on :9730 to manage
// it as a "source" server (§6.1 — health probe is the authoritative "serving"
// signal). This file is intentionally minimal: just the listener + health
// route. The full twilio-cli/HTTP command surface is separate, deferred work
// (design §11) and does not belong here.
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

// newTwilioMux builds the Twilio-facing routes: /webhook (the Twilio call
// webhook, s.ServeHTTP), /streams (the Media Streams WebSocket upgrade,
// s.ServeStreams), /sms/inbound (the inbound-SMS webhook, s.ServeSMS), and
// /sms/status (the delivery-status callback, s.ServeSMSStatus).
//
// /sms/status belongs on this mux rather than a consumer's own because it is
// the URL a consumer puts in OutboundSMS.StatusCallback, and this listener is
// already the one the provider reaches. Left off, a consumer's only options are
// to rebuild this route table on a mux of its own — a copy that stops matching
// the day a route here changes — or to run a second listener on another port.
func newTwilioMux(s *twilio.Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.ServeHTTP)
	mux.HandleFunc("/streams", s.ServeStreams)
	mux.HandleFunc("/sms/inbound", s.ServeSMS)
	mux.HandleFunc("/sms/status", s.ServeSMSStatus)
	return mux
}

// StartTwilioServer launches the Twilio-facing HTTP listener in the background,
// serving the routes built by newTwilioMux — which is where they are listed, so
// adding one is not a two-comment edit.
// Bind failures are reported to stderr but do not crash the process.
func StartTwilioServer(addr string, s *twilio.Server) *http.Server {
	mux := newTwilioMux(s)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "twilio server: %v\n", err)
		}
	}()
	return srv
}
