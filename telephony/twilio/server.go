package twilio

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"sync"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// e164Pattern matches E.164-formatted phone numbers: a leading '+', a first
// digit 1-9, then 1-14 more digits (max 15 digits total).
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// StreamHandler is called by ServeStreams for each accepted Twilio Media Streams
// WebSocket connection, after the mandatory start frame has been decoded. The
// handler owns the connection for the duration of the call and returns when done.
type StreamHandler func(ctx context.Context, conn *websocket.Conn, start Frame) error

// InboundSMS is the parsed subset of a Twilio inbound-message webhook. Additional
// fields (NumMedia, MediaUrl0…, NumSegments, …) remain available on the raw form
// if a consumer needs them later.
type InboundSMS struct {
	MessageSID string // Twilio MessageSid
	From       string // sender, E.164
	To         string // the Twilio number that received it, E.164
	Body       string // message text
}

// SMSHandler is called by ServeSMS for each validated inbound-SMS webhook. It is
// fire-and-forget: ServeSMS always answers Twilio with empty TwiML regardless, so
// a synchronous reply is not modeled here (a future reply path is a separate change).
type SMSHandler func(ctx context.Context, msg InboundSMS)

// Server handles Twilio HTTP webhook requests and WebSocket Media Streams connections.
// Set AuthToken to the Twilio auth token for the account; every inbound webhook
// request is validated against the X-Twilio-Signature header before processing.
// Set HandleStream to handle incoming Media Streams WebSocket connections, and
// HandleSMS to handle inbound SMS webhooks.
type Server struct {
	AuthToken    string
	HandleStream StreamHandler
	HandleSMS    SMSHandler

	// StreamScheme selects the scheme advertised in the TwiML <Stream url>
	// and, correspondingly, the scheme used to reconstruct the URL for
	// signature validation. "ws" advertises/validates over ws/http; "wss"
	// or the zero value "" advertise/validate over wss/https (secure by
	// default — a directly-constructed Server{} is never accidentally
	// insecure).
	StreamScheme string

	// Authorize decides whether a caller/sender is allowed through the voice
	// and SMS webhooks. A nil Authorize means authorize-all (back-compat: a
	// Server that never sets it behaves exactly as before this field existed).
	// The engine ships only this injected predicate — it never resolves or
	// holds any identity/roster itself.
	Authorize func(from string) bool

	// VoiceRejectText and SMSRejectText are the caller-facing rejection
	// wording for an unauthorized voice call / SMS, respectively. Injected by
	// the consumer — the engine never hard-codes rejection copy.
	VoiceRejectText string
	SMSRejectText   string

	// ReplyRouter, if set, is threaded onto each stream's context before
	// HandleStream is called, letting a caller outside this package reach a
	// live session by CallSID/StreamSID and route reply frames through its
	// already-wired data-plane output (AATK-22). Nil means no registration —
	// the engine never requires a router to operate.
	ReplyRouter *telephony.ReplyRouter

	fromMu    sync.Mutex
	fromBySID map[string]string // CallSid -> From, bounded by maxPendingFrom
	fromOrder []string          // insertion order, for evict-oldest
}

// maxPendingFrom bounds the CallSid->From map recorded by ServeHTTP for
// ServeStreams to thread onto the start Frame. Mirrors the maxAbandoned
// evict-oldest discipline in telephony/stt.go: a call whose stream never
// opens (or is rejected at the webhook) would otherwise leave a permanent
// entry, growing unboundedly over a long-running server's lifetime.
const maxPendingFrom = 1024

// recordFrom remembers from under callSid for a later ServeStreams lookup.
// A blank callSid is never recorded — it cannot uniquely key a later start
// frame, so keeping it would just waste an eviction slot.
func (s *Server) recordFrom(callSid, from string) {
	if callSid == "" {
		return
	}
	s.fromMu.Lock()
	defer s.fromMu.Unlock()
	if s.fromBySID == nil {
		s.fromBySID = make(map[string]string)
	}
	if _, exists := s.fromBySID[callSid]; !exists {
		s.fromOrder = append(s.fromOrder, callSid)
		if len(s.fromOrder) > maxPendingFrom {
			delete(s.fromBySID, s.fromOrder[0])
			s.fromOrder = s.fromOrder[1:]
		}
	}
	s.fromBySID[callSid] = from
}

// lookupFrom returns the From recorded for callSid, or "" if none was ever
// recorded (unknown/missing case).
func (s *Server) lookupFrom(callSid string) string {
	s.fromMu.Lock()
	defer s.fromMu.Unlock()
	return s.fromBySID[callSid]
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

// authenticate parses the request body and validates the Twilio signature over
// the reconstructed public URL (scheme from validateScheme, host from r.Host as
// preserved by the reverse proxy). It writes the error response and returns false
// on any failure; on success the parsed body is available in r.PostForm.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return false
	}

	sig := r.Header.Get("X-Twilio-Signature")
	rawURL := s.validateScheme() + "://" + r.Host + r.URL.RequestURI()
	if !ValidateSignature(s.AuthToken, rawURL, r.PostForm, sig) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// ServeHTTP implements http.Handler for the Twilio call webhook. It validates the
// request signature (403 on failure, including an empty AuthToken), then validates
// the caller's E.164 From number (403 on failure), logs From and CallSid, and
// responds with TwiML instructing Twilio to open the Media Streams WebSocket at
// /streams.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) {
		return
	}

	from := r.PostForm.Get("From")
	callSid := r.PostForm.Get("CallSid")
	if !e164Pattern.MatchString(from) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	log.Printf("twilio: ServeHTTP: From=%s CallSid=%s", from, callSid)

	s.recordFrom(callSid, from)

	if s.Authorize != nil && !s.Authorize(from) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<Response><Say>%s</Say><Hangup/></Response>`, html.EscapeString(s.VoiceRejectText))
		return
	}

	streamURL := fmt.Sprintf("%s://%s/streams", s.streamScheme(), r.Host)
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<Response><Connect><Stream url="%s"/></Connect></Response>`, html.EscapeString(streamURL))
}

// ServeSMS handles a Twilio inbound-SMS webhook. It validates the request
// signature (403 on failure, including an empty AuthToken), parses the message
// fields into InboundSMS, hands them to HandleSMS if set, and answers with empty
// TwiML so Twilio sends no automatic reply.
func (s *Server) ServeSMS(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) {
		return
	}

	from := r.PostForm.Get("From")
	if s.Authorize != nil && !s.Authorize(from) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<Response><Message>%s</Message></Response>`, html.EscapeString(s.SMSRejectText))
		return
	}

	msg := InboundSMS{
		MessageSID: r.PostForm.Get("MessageSid"),
		From:       from,
		To:         r.PostForm.Get("To"),
		Body:       r.PostForm.Get("Body"),
	}
	if s.HandleSMS != nil {
		s.HandleSMS(r.Context(), msg)
	}

	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprint(w, "<Response></Response>")
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

	f.From = s.lookupFrom(f.CallSID)

	if s.ReplyRouter != nil {
		ctx = ContextWithReplyRouter(ctx, s.ReplyRouter)
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
