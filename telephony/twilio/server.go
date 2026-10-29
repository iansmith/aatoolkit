package twilio

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/coder/websocket"
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

// SMSStatus is the parsed subset of a Twilio delivery-status callback — the POST
// a message's OutboundSMS.StatusCallback URL receives once the provider knows
// what became of it.
//
// It is the other half of SendSMS's contract. A 2xx from the send means the
// message was *accepted for delivery*, not delivered; the real outcome, carrier
// rejections included, only ever arrives here. MessageSID is what ties the two
// together: it is the same id SendSMS returned, and without it a caller has an
// outcome it cannot attribute to any particular send.
//
// Field names mirror InboundSMS, with the direction reversed — on a callback
// From is the number the message was sent *from* (ours) and To is the
// recipient, the opposite of an inbound webhook.
type SMSStatus struct {
	MessageSID string // Twilio MessageSid — the id SendSMS returned for this message
	Status     string // MessageStatus: queued, sent, delivered, undelivered, failed
	// ErrorCode is the provider's own error code, empty when the callback
	// carried none — which is every non-failure status, since the field is
	// omitted rather than sent blank.
	//
	// A string, not an int, on purpose. Parsing would need somewhere to put its
	// own failure on a route whose whole job is to hand the outcome over without
	// editorializing, and an operator looking a code up wants the characters the
	// provider actually sent. It also keeps the struct all-strings, like
	// InboundSMS.
	ErrorCode string
	To        string // recipient, E.164
	From      string // the Twilio number it was sent from, E.164
}

// SMSStatusHandler is called by ServeSMSStatus for each validated
// delivery-status callback. Fire-and-forget for the same reason as SMSHandler:
// the route always acknowledges, and the provider has no use for a reply.
//
// What to do with an outcome — log it, alert on it, ignore it — is the
// consumer's decision, not the engine's. In particular the engine draws no
// conclusion from a callback that never arrives: many carriers never send a
// delivery receipt, so a perfectly fine message simply stops at "sent", and
// reading absence as failure would manufacture failures that did not happen.
type SMSStatusHandler func(ctx context.Context, status SMSStatus)

// RejectReason is the reason attribute of TwiML's <Reject> verb, which
// ServeHTTP writes when a voice call is refused unanswered (see
// Server.RejectCall and Server.VoiceRejectReason). With <Reject> as the first
// verb Twilio does not answer the call, so it is not billed; it still appears
// in the call log. Twilio takes two reasons: RejectReasonRejected, its
// default, and RejectReasonBusy.
//
// Once a call is refused unanswered, only an exact RejectReasonBusy writes
// <Reject reason="busy"/>. Every other value — RejectReasonRejected, anything
// outside the two, or "" returned by RejectCall alongside reject=true — writes
// a plain <Reject/>, so no other value can reach the document. A typo such as
// "Busy" is therefore still refused unanswered, as a plain rejection, and
// nothing reports it.
//
// On Server.VoiceRejectReason, "" (the zero value) does not mean a plain
// <Reject/>: it means Authorize's refusal is not unanswered at all and stays
// the spoken <Say>…</Say><Hangup/>. Set RejectReasonRejected there for a plain
// <Reject/>.
type RejectReason string

const (
	// RejectReasonRejected is Twilio's default reason. It writes a plain
	// <Reject/>: the default needs no attribute, so this value never reaches
	// the document.
	RejectReasonRejected RejectReason = "rejected"

	// RejectReasonBusy writes <Reject reason="busy"/>.
	RejectReasonBusy RejectReason = "busy"
)

// Server handles Twilio HTTP webhook requests and WebSocket Media Streams connections.
// Set AuthToken to the Twilio auth token for the account; every inbound webhook
// request is validated against the X-Twilio-Signature header before processing.
// Set HandleStream to handle incoming Media Streams WebSocket connections,
// HandleSMS to handle inbound SMS webhooks, and HandleSMSStatus to handle
// delivery-status callbacks for messages sent via SendSMS.
type Server struct {
	AuthToken    string
	HandleStream StreamHandler
	HandleSMS    SMSHandler

	// HandleSMSStatus receives each validated delivery-status callback. Nil
	// means the route still answers and nothing is dispatched, so a Server
	// built before this field existed behaves exactly as it did.
	HandleSMSStatus SMSStatusHandler

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
	// the consumer — the engine never hard-codes rejection copy. The voice
	// refusal is spoken (<Say>, then <Hangup/>), which answers the call;
	// VoiceRejectReason refuses it unanswered instead.
	VoiceRejectText string
	SMSRejectText   string

	// VoiceRejectReason, if set, makes Authorize's refusal of a voice call
	// unanswered: ServeHTTP writes <Reject/> (<Reject reason="busy"/> for
	// RejectReasonBusy; see RejectReason) as the first and only verb, and
	// VoiceRejectText is not used. The zero value "" keeps the spoken
	// VoiceRejectText refusal byte for byte, so a Server built before this
	// field existed behaves exactly as it did. It is read only when Authorize
	// refuses a voice call; with Authorize nil, or admitting the caller, it
	// does nothing. Voice only: ServeSMS never reads it, and an SMS refusal
	// is still SMSRejectText.
	VoiceRejectReason RejectReason

	// AnswerCall, if set, decides per call whether to answer with a spoken
	// refusal instead of opening a stream. It is handed the caller (from) and
	// the dialed number (to; "" when the webhook's To is missing or not
	// E.164), so a consumer serving several numbers can answer differently on
	// each. Returning refuse=true answers <Say>say</Say><Hangup/> and opens
	// no stream; say is XML-escaped, and anything XML 1.0 cannot carry
	// (invalid UTF-8, U+0000-U+001F other than tab, LF and CR, U+FFFE and
	// U+FFFF) is dropped. That refusal is spoken, so it answers the call;
	// RejectCall refuses one unanswered. AnswerCall runs only for a call that
	// passed signature validation, the E.164 From check, Authorize and
	// RejectCall; an Authorize refusal or a RejectCall rejection wins and
	// AnswerCall is never consulted. Nil keeps the stream answer unchanged.
	// Voice only: ServeSMS never calls it, because InboundSMS already
	// carries To. The engine holds no wording and no identity here; both
	// belong to the consumer, as with Authorize.
	AnswerCall func(ctx context.Context, from, to string) (say string, refuse bool)

	// RejectCall, if set, decides per call whether to refuse it unanswered.
	// It is handed the request's ctx and the same from and to as AnswerCall
	// (to is "" when the webhook's To is missing or not E.164). Returning
	// reject=true writes <Reject/> (<Reject reason="busy"/> for
	// RejectReasonBusy; see RejectReason) as the first and only verb and
	// opens no stream; AnswerCall is not consulted. Returning reject=false
	// ignores reason, and the call goes on to AnswerCall. It runs only for a
	// call that passed signature validation, the E.164 From check and
	// Authorize; an Authorize refusal wins and RejectCall is never consulted.
	// The webhook waits on it, so it should return promptly. Nil changes
	// nothing. Voice only: ServeSMS never calls it. The engine holds no
	// identity here; the decision belongs to the consumer, as with Authorize.
	RejectCall func(ctx context.Context, from, to string) (reason RejectReason, reject bool)

	fromMu    sync.Mutex
	fromBySID map[string]callParties // CallSid -> From/To, bounded by maxPendingFrom
	fromOrder []string               // insertion order, for evict-oldest
}

// callParties is what the voice webhook records under a CallSid for
// ServeStreams to thread onto the start Frame.
type callParties struct {
	from, to string
}

// maxPendingFrom bounds the CallSid -> From/To map ServeHTTP records for
// ServeStreams to thread onto the start Frame.
//
// ServeHTTP records only a call it answers with a stream; a call refused at
// the webhook takes no entry, whether the signature or E.164 From check
// refused it (403) or Authorize, RejectCall or AnswerCall did (spoken or
// unanswered). Only eviction removes an entry: lookupCall reads it and
// leaves it in place, so every recorded call holds its entry until it is
// evicted.
// Recording a new CallSid while the map holds maxPendingFrom evicts the
// oldest, so a stream whose start frame arrives after maxPendingFrom newer
// calls were recorded finds no entry, and its start Frame carries "" for
// From and To. Without the bound the map would grow for the whole of a
// long-running server's lifetime.
const maxPendingFrom = 1024

// recordCall remembers from and to under callSid for a later ServeStreams
// lookup. A blank callSid is never recorded — it cannot uniquely key a later
// start frame, so keeping it would just waste an eviction slot. Recording a
// callSid already held replaces its from and to in place: it takes no new
// slot and keeps its place in the eviction order.
func (s *Server) recordCall(callSid, from, to string) {
	if callSid == "" {
		return
	}
	s.fromMu.Lock()
	defer s.fromMu.Unlock()
	if s.fromBySID == nil {
		s.fromBySID = make(map[string]callParties)
	}
	if _, exists := s.fromBySID[callSid]; !exists {
		s.fromOrder = append(s.fromOrder, callSid)
		if len(s.fromOrder) > maxPendingFrom {
			delete(s.fromBySID, s.fromOrder[0])
			s.fromOrder = s.fromOrder[1:]
		}
	}
	s.fromBySID[callSid] = callParties{from: from, to: to}
}

// lookupCall returns the From and To recorded for callSid, or "" for both if
// none is held: never recorded, or since evicted. It leaves the entry in
// place.
func (s *Server) lookupCall(callSid string) callParties {
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
// /streams — unless the call is refused. Authorize, RejectCall and AnswerCall
// are consulted in that order; the first to refuse answers the webhook, and
// the rest are not consulted:
//
//   - Authorize refuses the caller: VoiceRejectText spoken as
//     <Say>…</Say><Hangup/>, or, with VoiceRejectReason set, <Reject/>.
//   - RejectCall rejects the call: <Reject/>.
//   - AnswerCall refuses the call: its own say text, as <Say>…</Say><Hangup/>.
//
// A spoken refusal answers the call. <Reject/> (reason="busy" for
// RejectReasonBusy) is written as the first and only verb, so Twilio does not
// answer the call. The dialed To reaches RejectCall, AnswerCall and the start
// Frame only when it is E.164; otherwise it is "". Only a call answered with
// the stream has its From and To recorded for ServeStreams (see
// maxPendingFrom); a refused call records nothing.
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

	to := r.PostForm.Get("To")
	if !e164Pattern.MatchString(to) {
		to = ""
	}

	if s.Authorize != nil && !s.Authorize(from) {
		if s.VoiceRejectReason != "" {
			writeVoiceReject(w, s.VoiceRejectReason)
		} else {
			writeVoiceRefusal(w, s.VoiceRejectText)
		}
		return
	}

	if s.RejectCall != nil {
		if reason, reject := s.RejectCall(r.Context(), from, to); reject {
			writeVoiceReject(w, reason)
			return
		}
	}

	if s.AnswerCall != nil {
		if say, refuse := s.AnswerCall(r.Context(), from, to); refuse {
			writeVoiceRefusal(w, say)
			return
		}
	}

	// Recorded only now, once the call is sure to be answered with a stream:
	// a refused call opens no stream, so nothing would ever read its entry.
	// Recorded before the TwiML is written, so the entry is in place before
	// Twilio can open the stream that looks it up.
	s.recordCall(callSid, from, to)

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
		fmt.Fprintf(w, `<Response><Message>%s</Message></Response>`, escapeTwiML(s.SMSRejectText))
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

	writeEmptyTwiML(w)
}

// writeVoiceRefusal answers a voice webhook by speaking text (escaped by
// escapeTwiML) and hanging up, opening no stream; the call is answered. One
// definition for both spoken refusals in ServeHTTP — Authorize's
// VoiceRejectText and AnswerCall's say — so the two cannot drift into
// different documents. The unanswered refusal is writeVoiceReject.
func writeVoiceRefusal(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<Response><Say>%s</Say><Hangup/></Response>`, escapeTwiML(text))
}

// rejectTwiML and rejectBusyTwiML are the only two documents an unanswered
// refusal can write: <Reject> as the first and only verb, with no reason
// attribute for Twilio's default reason, or with reason="busy". Both are
// constants, so no caller value can reach the document.
const (
	rejectTwiML     = `<Response><Reject/></Response>`
	rejectBusyTwiML = `<Response><Reject reason="` + string(RejectReasonBusy) + `"/></Response>`
)

// writeVoiceReject answers a voice webhook with <Reject>, so Twilio does not
// answer the call, and opens no stream. One definition for both unanswered
// refusals in ServeHTTP — Authorize's VoiceRejectReason and RejectCall's
// reason. Only an exact RejectReasonBusy writes rejectBusyTwiML; every other
// reason writes rejectTwiML. Like writeVoiceRefusal it sets Content-Type
// text/xml before writing and writes no XML prolog.
func writeVoiceReject(w http.ResponseWriter, reason RejectReason) {
	doc := rejectTwiML
	if reason == RejectReasonBusy {
		doc = rejectBusyTwiML
	}
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprint(w, doc)
}

// escapeTwiML makes text safe as the content of a TwiML element.
// html.EscapeString covers the XML metacharacters but passes through what
// XML 1.0 cannot carry at all: invalid UTF-8, the C0 controls other than tab,
// LF and CR, and U+FFFE/U+FFFF. One such character makes the whole document
// unparseable, so they are dropped first. Valid text, tab, LF and CR
// included, escapes to the same bytes as html.EscapeString alone.
func escapeTwiML(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(text, ""))
	return html.EscapeString(text)
}

// writeEmptyTwiML acknowledges a webhook without asking Twilio to do anything.
//
// One definition, because it is one decision: the content type and the empty
// <Response/> together are what stop Twilio adding an automatic reply of its
// own. Answering with a blank body or a non-XML content type instead is not
// silently fine — Twilio logs it as a 11200/12300 document-parse warning and
// the consumer never sees it.
func writeEmptyTwiML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprint(w, "<Response></Response>")
}

// ServeSMSStatus handles a Twilio delivery-status callback. It validates the
// request signature (403 on failure, including an empty AuthToken), parses the
// outcome into SMSStatus, hands it to HandleSMSStatus if set, and acknowledges
// with empty TwiML.
//
// It deliberately does not run Authorize, and copying that gate across from
// ServeSMS would break every callback. On an inbound message From is the
// sender, so gating on it is the roster check. Here From is the account's own
// number and To is the recipient — so a consumer whose Authorize answers "is
// this caller someone I know" says no to every delivery outcome it will ever
// receive, silently, while the send side keeps working. The signature is the
// security boundary on this route; the caller's identity is not in question,
// because the only party that POSTs here is the provider we just sent through.
func (s *Server) ServeSMSStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) {
		return
	}

	if s.HandleSMSStatus != nil {
		s.HandleSMSStatus(r.Context(), SMSStatus{
			MessageSID: r.PostForm.Get("MessageSid"),
			Status:     r.PostForm.Get("MessageStatus"),
			ErrorCode:  r.PostForm.Get("ErrorCode"),
			To:         r.PostForm.Get("To"),
			From:       r.PostForm.Get("From"),
		})
	}

	writeEmptyTwiML(w)
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

	parties := s.lookupCall(f.CallSID)
	f.From, f.To = parties.from, parties.to

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
