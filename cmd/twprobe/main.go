// twprobe — minimal listener to prove Twilio's voice webhook actually
// reaches this box. Twilio's "A call comes in" webhook is a plain HTTP POST,
// so all we need is an HTTP server that (a) dumps whatever arrives so you can
// see it, and (b) answers with valid TwiML so the call doesn't error out.
//
// Wire it up:
//   1. Run this on the box behind 146.75.248.146.
//   2. Console -> Phone Numbers -> Active numbers -> (218) 376-7443
//      -> Voice Configuration -> "A call comes in" = Webhook
//      -> URL: http://146.75.248.146:9750/voice   Method: HTTP POST -> Save
//   3. If on a trial account, verify the phone you'll dial from first.
//   4. Call (218) 376-7443. Watch stdout; you should also hear the <Say>.
//
// Plain http:// is fine for this webhook test. (The media stream you build
// later needs wss:// on a TLS endpoint — that's a separate step.)

package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"time"
)

func main() {
	// Catch-all handler: logs every request regardless of path, so it doesn't
	// matter whether you set the webhook to /voice, /, or anything else.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dump, err := httputil.DumpRequest(r, true) // method, path, headers, body
		if err != nil {
			log.Printf("dump error: %v", err)
		}
		log.Printf("\n===== %s  hit from %s =====\n%s\n============================\n",
			time.Now().Format(time.RFC3339), r.RemoteAddr, dump)

		// Answer with valid TwiML so Twilio logs success and the caller hears it.
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Response><Say>The probe received your call.</Say></Response>`)
	})

	// Bind all interfaces so the request arriving on the public IP is accepted
	// (works whether the box holds 146.75.248.146 directly or via your NAT/port
	// forwarding).
	const addr = "0.0.0.0:9750"
	log.Printf("listening on %s — waiting for Twilio...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
