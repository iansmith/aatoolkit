package twilio

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"

	"github.com/coder/websocket"
)

// e164Pattern matches E.164-formatted phone numbers: a leading '+', a first
// digit 1-9, then 1-14 more digits (max 15 digits total).
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// StreamHandler is called by ServeStreams for each accepted Twilio Media Streams
// WebSocket connection, after the mandatory start frame has been decoded. The
// handler owns the connection for the duration of the call and returns when done.
type StreamHandler func(ctx context.Context, conn *websocket.Conn, start Frame) error

// Server handles Twilio HTTP webhook requests and WebSocket Media Streams connections.
// Set AuthToken to the Twilio auth token for the account; every inbound webhook
// request is validated against the X-Twilio-Signature header before processing.
// Set HandleStream to handle incoming Media Streams WebSocket connections.
type Server struct {
	AuthToken    string
	HandleStream StreamHandler

	// StreamScheme selects the scheme advertised in the TwiML <Stream url>
	// and, correspondingly, the scheme used to reconstruct the URL for
	// signature validation. "ws" advertises/validates over ws/http; "wss"
	// or the zero value "" advertise/validate over wss/https (secure by
	// default — a directly-constructed Server{} is never accidentally
	// insecure).
	StreamScheme string
}

// streamScheme returns the interpreted advertise scheme ("ws" or "wss") per
// the secure-by-default mapping: only an explicit "ws" advertises ws;
// everything else (including the zero value "") advertises wss.
func (s *Server) streamScheme() string {
	if s.StreamScheme == "ws" {
		return "ws"
	}
	return "wss"
}

// validateScheme returns the scheme used to reconstruct the signature
// validation URL, matching what the client actually POSTs over for the
// advertised stream scheme.
func (s *Server) validateScheme() string {
	if s.streamScheme() == "ws" {
		return "http"
	}
	return "https"
}

// ServeHTTP implements http.Handler. It validates the Twilio request signature and
// returns 403 Forbidden for any request that fails validation (including requests
// with an empty AuthToken). On success it validates the caller's E.164 From number
// (403 on failure), logs From and CallSid, and responds with TwiML instructing
// Twilio to open the Media Streams WebSocket at /streams.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Twilio-Signature")
	rawURL := s.validateScheme() + "://" + r.Host + r.URL.RequestURI()
	if !ValidateSignature(s.AuthToken, rawURL, r.PostForm, sig) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	from := r.PostForm.Get("From")
	callSid := r.PostForm.Get("CallSid")
	if !e164Pattern.MatchString(from) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	log.Printf("twilio: ServeHTTP: From=%s CallSid=%s", from, callSid)

	streamURL := fmt.Sprintf("%s://%s/streams", s.streamScheme(), r.Host)
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<Response><Connect><Stream url="%s"/></Connect></Response>`, html.EscapeString(streamURL))
}

// ServeStreams handles a Twilio Media Streams WebSocket connection. It upgrades
// the HTTP connection to WebSocket, reads the mandatory start frame (optionally
// preceded by a connected frame), then calls HandleStream. If HandleStream is nil,
// inbound frames are read and discarded until the client disconnects.
func (s *Server) ServeStreams(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("twilio: ServeStreams: accept: %v", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		return
	}

	f, err := DecodeFrame(raw)
	if err != nil {
		log.Printf("twilio: ServeStreams: decode first frame: %v", err)
		conn.Close(websocket.StatusUnsupportedData, fmt.Sprintf("decode: %v", err))
		return
	}

	// If the first frame is connected, consume it and read the next frame.
	if f.Event == EventConnected {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}

		f, err = DecodeFrame(raw)
		if err != nil {
			log.Printf("twilio: ServeStreams: decode frame after connected: %v", err)
			conn.Close(websocket.StatusUnsupportedData, fmt.Sprintf("decode: %v", err))
			return
		}
	}

	if f.Event != EventStart {
		log.Printf("twilio: ServeStreams: expected start frame, got %q", f.Event)
		conn.Close(websocket.StatusPolicyViolation, "expected start frame")
		return
	}

	if s.HandleStream != nil {
		s.HandleStream(ctx, conn, f)
		return
	}

	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}
