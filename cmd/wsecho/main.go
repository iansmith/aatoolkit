// Command wsecho is a throwaway WebSocket echo server for local testing of
// twilio-cli. It listens on :9740/streams, accepts any connection, and logs
// each inbound frame. It does not send anything back (the CLI read loop just
// blocks until the connection closes).
//
// Usage:
//
//	go run ./cmd/wsecho          # listen on default :9740
//	go run ./cmd/wsecho -addr :8080
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/coder/websocket"
)

func main() {
	addr := flag.String("addr", ":9740", "listen address")
	flag.Parse()

	http.HandleFunc("/streams", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // no origin check needed for local testing
		})
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		defer conn.CloseNow()

		log.Printf("client connected from %s", r.RemoteAddr)
		ctx := r.Context()
		var frames int
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				log.Printf("connection closed after %d frames: %v", frames, err)
				return
			}
			frames++
			log.Printf("frame %d: %d bytes", frames, len(msg))
		}
	})

	log.Printf("wsecho listening on %s/streams", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
