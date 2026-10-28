package twilio_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- AATK-162: refusing a voice call unanswered (<Reject>) ---

// The only two documents an unanswered refusal may write, as literals.
const (
	wantReject     = `<Response><Reject/></Response>`
	wantRejectBusy = `<Response><Reject reason="busy"/></Response>`
)

// rejectCallRecorder is a RejectCall hook that records every (from, to) it
// is asked about and answers with a fixed decision.
type rejectCallRecorder struct {
	reason twilio.RejectReason
	reject bool
	asked  [][2]string
}

func (r *rejectCallRecorder) hook(_ context.Context, from, to string) (twilio.RejectReason, bool) {
	r.asked = append(r.asked, [2]string{from, to})
	return r.reason, r.reject
}

// requireOnlyVerb fails unless body parses as a <Response> whose one and
// only verb is named verb. It is a second witness beside the exact-bytes
// check: it shows the expected literal itself is well-formed TwiML with
// nothing before or after that verb.
func requireOnlyVerb(t *testing.T, body []byte, verb string) {
	t.Helper()
	var doc struct {
		XMLName xml.Name `xml:"Response"`
		Verbs   []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body %q is not well-formed XML: %v", body, err)
	}
	if len(doc.Verbs) != 1 || doc.Verbs[0].XMLName.Local != verb {
		t.Fatalf("body %q has verbs %v, want only <%s>", body, doc.Verbs, verb)
	}
}

// requireTwiML fails unless the webhook answered 200 with exactly want and
// Content-Type text/xml. The content type is read from Result(), the header
// snapshot taken at the first write, so a writer that sets it only after
// writing the body fails here even though the live w.Header() map would
// show it.
func requireTwiML(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status %d body %q, want 200 %q", w.Code, w.Body.String(), want)
	}
	if ct := w.Result().Header.Get("Content-Type"); ct != "text/xml" {
		t.Fatalf("Content-Type = %q, want text/xml", ct)
	}
}

// Both spoken refusals keep the exact bytes and content type they had before
// an unanswered refusal existed. The expected documents are literals of the
// output before this change, not built from the code under test. The
// Authorize rows also pin the content type of that path, which no other test
// checks.
func TestServeHTTP_SpokenRefusalBytesUnchanged(t *testing.T) {
	const sorry = "I'm sorry, this service is not available"
	refuse := func(string) bool { return false }
	for _, tc := range []struct {
		name              string
		authorize         func(string) bool
		voiceRejectText   string
		answer            *answerCallRecorder // nil leaves AnswerCall unset
		wantAnswerAsked   int
		voiceRejectReason twilio.RejectReason
		reject            *rejectCallRecorder // nil leaves RejectCall unset
		wantRejectAsked   int
		want              string
	}{
		{
			name:            "Authorize refuses",
			authorize:       refuse,
			voiceRejectText: sorry,
			want:            `<Response><Say>I&#39;m sorry, this service is not available</Say><Hangup/></Response>`,
		},
		{
			name:      "Authorize refuses, VoiceRejectText unset",
			authorize: refuse,
			want:      `<Response><Say></Say><Hangup/></Response>`,
		},
		{
			name:            "Authorize refuses, percent in text",
			authorize:       refuse,
			voiceRejectText: "100% sure",
			want:            `<Response><Say>100% sure</Say><Hangup/></Response>`,
		},
		{
			name:            "AnswerCall refuses",
			answer:          &answerCallRecorder{say: "Call A & B", refuse: true},
			wantAnswerAsked: 1,
			want:            `<Response><Say>Call A &amp; B</Say><Hangup/></Response>`,
		},
		{
			name:            "AnswerCall refuses with empty say",
			answer:          &answerCallRecorder{say: "", refuse: true},
			wantAnswerAsked: 1,
			want:            `<Response><Say></Say><Hangup/></Response>`,
		},
		{
			name:            "AnswerCall refuses, percent in say",
			answer:          &answerCallRecorder{say: "100% sure", refuse: true},
			wantAnswerAsked: 1,
			want:            `<Response><Say>100% sure</Say><Hangup/></Response>`,
		},
		// The new options set, but not firing.
		{
			name:            "RejectCall lets the call through, AnswerCall refuses",
			reject:          &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: false},
			wantRejectAsked: 1,
			answer:          &answerCallRecorder{say: "Call A & B", refuse: true},
			wantAnswerAsked: 1,
			want:            `<Response><Say>Call A &amp; B</Say><Hangup/></Response>`,
		},
		{
			name:              "VoiceRejectReason set, Authorize admits, AnswerCall refuses",
			authorize:         func(string) bool { return true },
			voiceRejectReason: twilio.RejectReasonBusy,
			answer:            &answerCallRecorder{say: "wrong number", refuse: true},
			wantAnswerAsked:   1,
			want:              `<Response><Say>wrong number</Say><Hangup/></Response>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &twilio.Server{
				AuthToken:         "authtoken",
				StreamScheme:      "wss",
				Authorize:         tc.authorize,
				VoiceRejectText:   tc.voiceRejectText,
				VoiceRejectReason: tc.voiceRejectReason,
			}
			if tc.answer != nil {
				s.AnswerCall = tc.answer.hook
			}
			if tc.reject != nil {
				s.RejectCall = tc.reject.hook
			}
			form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

			requireTwiML(t, w, tc.want)
			if tc.answer != nil && len(tc.answer.asked) != tc.wantAnswerAsked {
				t.Fatalf("AnswerCall asked %v, want %d call(s)", tc.answer.asked, tc.wantAnswerAsked)
			}
			if tc.reject != nil && len(tc.reject.asked) != tc.wantRejectAsked {
				t.Fatalf("RejectCall asked %v, want %d call(s)", tc.reject.asked, tc.wantRejectAsked)
			}
		})
	}
}

// injectedReason is a reason that would break out of the attribute and add
// a <Say> to the document, if a reason value were ever written into it.
const injectedReason = twilio.RejectReason(`busy"/><Say>x</Say><Reject reason="busy`)

// A rejecting RejectCall writes <Reject> as the first and only verb: the
// plain document for every reason except an exact RejectReasonBusy, which
// alone writes reason="busy". No other value reaches the document. AnswerCall
// would refuse with text of its own, so any spoken bytes would show, and it
// is never asked.
func TestServeHTTP_RejectCallWritesRejectDocument(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason twilio.RejectReason
		want   string
	}{
		{"empty", "", wantReject},
		{"RejectReasonRejected", twilio.RejectReasonRejected, wantReject},
		{"RejectReasonBusy", twilio.RejectReasonBusy, wantRejectBusy},
		{"Busy", "Busy", wantReject},
		{"leading space", " busy", wantReject},
		{"unknown", "failed", wantReject},
		{"attribute injection", injectedReason, wantReject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reject := &rejectCallRecorder{reason: tc.reason, reject: true}
			answer := &answerCallRecorder{say: "Call A & B", refuse: true}
			s := &twilio.Server{
				AuthToken:    "authtoken",
				StreamScheme: "wss",
				Authorize:    func(string) bool { return true },
				RejectCall:   reject.hook,
				AnswerCall:   answer.hook,
			}
			form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

			requireTwiML(t, w, tc.want)
			requireOnlyVerb(t, w.Body.Bytes(), "Reject")
			if len(reject.asked) != 1 || reject.asked[0] != [2]string{"+15105551234", "+15105550000"} {
				t.Fatalf("RejectCall asked %v, want [[+15105551234 +15105550000]]", reject.asked)
			}
			if len(answer.asked) != 0 {
				t.Fatalf("AnswerCall asked %v, want no call", answer.asked)
			}
		})
	}
}

// With VoiceRejectReason set, an Authorize refusal is unanswered: the same
// two documents as RejectCall, under the same rule for the reason, in place
// of the spoken VoiceRejectText. Authorize's refusal still wins, so neither
// RejectCall nor AnswerCall is asked, though both would refuse.
func TestServeHTTP_VoiceRejectReasonRejectsAuthorizeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason twilio.RejectReason
		want   string
	}{
		{"RejectReasonRejected", twilio.RejectReasonRejected, wantReject},
		{"RejectReasonBusy", twilio.RejectReasonBusy, wantRejectBusy},
		{"Busy", "Busy", wantReject},
		{"attribute injection", injectedReason, wantReject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reject := &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: true}
			answer := &answerCallRecorder{say: "Call A & B", refuse: true}
			s := &twilio.Server{
				AuthToken:         "authtoken",
				StreamScheme:      "wss",
				Authorize:         func(string) bool { return false },
				VoiceRejectText:   "no",
				VoiceRejectReason: tc.reason,
				RejectCall:        reject.hook,
				AnswerCall:        answer.hook,
			}
			form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

			requireTwiML(t, w, tc.want)
			requireOnlyVerb(t, w.Body.Bytes(), "Reject")
			if len(reject.asked) != 0 || len(answer.asked) != 0 {
				t.Fatalf("RejectCall asked %v, AnswerCall asked %v; want neither asked", reject.asked, answer.asked)
			}
		})
	}
}

// The hooks are consulted in order — Authorize, RejectCall, AnswerCall — and
// the first refusal answers the webhook. A RejectCall that lets the call
// through changes nothing, and VoiceRejectReason does nothing unless
// Authorize refuses.
func TestServeHTTP_RejectCallPrecedence(t *testing.T) {
	const stream = `<Response><Connect><Stream url="wss://example.com/streams"/></Connect></Response>`
	for _, tc := range []struct {
		name              string
		authorize         func(string) bool
		voiceRejectReason twilio.RejectReason
		reject            *rejectCallRecorder // nil leaves RejectCall unset
		answer            *answerCallRecorder // nil leaves AnswerCall unset
		want              string
		wantRejectAsked   int
		wantAnswerAsked   int
	}{
		{
			name:      "Authorize refusal wins over RejectCall and AnswerCall",
			authorize: func(string) bool { return false },
			reject:    &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: true},
			answer:    &answerCallRecorder{say: "x", refuse: true},
			want:      `<Response><Say>no</Say><Hangup/></Response>`,
		},
		{
			name:            "RejectCall wins over AnswerCall",
			authorize:       func(string) bool { return true },
			reject:          &rejectCallRecorder{reason: "", reject: true},
			answer:          &answerCallRecorder{say: "x", refuse: true},
			want:            wantReject,
			wantRejectAsked: 1,
		},
		{
			name:            "RejectCall lets through, no AnswerCall",
			reject:          &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: false},
			want:            stream,
			wantRejectAsked: 1,
		},
		{
			name:            "RejectCall lets through, AnswerCall lets through",
			reject:          &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: false},
			answer:          &answerCallRecorder{say: "ignored", refuse: false},
			want:            stream,
			wantRejectAsked: 1,
			wantAnswerAsked: 1,
		},
		{
			name:              "VoiceRejectReason without an Authorize refusal",
			voiceRejectReason: twilio.RejectReasonBusy,
			want:              stream,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &twilio.Server{
				AuthToken:         "authtoken",
				StreamScheme:      "wss",
				Authorize:         tc.authorize,
				VoiceRejectText:   "no",
				VoiceRejectReason: tc.voiceRejectReason,
			}
			if tc.reject != nil {
				s.RejectCall = tc.reject.hook
			}
			if tc.answer != nil {
				s.AnswerCall = tc.answer.hook
			}
			form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

			requireTwiML(t, w, tc.want)
			if tc.reject != nil && len(tc.reject.asked) != tc.wantRejectAsked {
				t.Fatalf("RejectCall asked %v, want %d call(s)", tc.reject.asked, tc.wantRejectAsked)
			}
			if tc.answer != nil && len(tc.answer.asked) != tc.wantAnswerAsked {
				t.Fatalf("AnswerCall asked %v, want %d call(s)", tc.answer.asked, tc.wantAnswerAsked)
			}
		})
	}
}

// RejectCall is handed what AnswerCall is: the webhook's From and To, in that
// order, with a To that is missing or not E.164 as "", and the request's own
// context. The request context here carries a value and is already
// cancelled; context.Background() in its place has neither.
func TestServeHTTP_RejectCallGetsRequestContextFromAndTo(t *testing.T) {
	t.Run("From and To", func(t *testing.T) {
		rec := &rejectCallRecorder{}
		s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", RejectCall: rec.hook}
		form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
		s.ServeHTTP(httptest.NewRecorder(), signedWebhookRequest(t, "authtoken", "https", "example.com", form))

		if len(rec.asked) != 1 || rec.asked[0] != [2]string{"+15105551234", "+15105550000"} {
			t.Fatalf("RejectCall asked %v, want [[+15105551234 +15105550000]]", rec.asked)
		}
	})

	t.Run("To not E.164", func(t *testing.T) {
		for _, to := range []string{"", "client:alice", "5105550000"} {
			rec := &rejectCallRecorder{}
			s := &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", RejectCall: rec.hook}
			form := url.Values{"From": {"+15105551234"}, "CallSid": {"CA1"}}
			if to != "" {
				form.Set("To", to)
			}
			s.ServeHTTP(httptest.NewRecorder(), signedWebhookRequest(t, "authtoken", "https", "example.com", form))
			if len(rec.asked) != 1 || rec.asked[0] != [2]string{"+15105551234", ""} {
				t.Fatalf("To=%q: RejectCall asked %v, want [[+15105551234 ]]", to, rec.asked)
			}
		}
	})

	t.Run("request context", func(t *testing.T) {
		type ctxKey struct{}
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "request"))
		cancel()

		asked := 0
		var gotValue any
		var gotErr error
		s := &twilio.Server{
			AuthToken:    "authtoken",
			StreamScheme: "wss",
			RejectCall: func(ctx context.Context, _, _ string) (twilio.RejectReason, bool) {
				asked++
				gotValue, gotErr = ctx.Value(ctxKey{}), ctx.Err()
				return "", false
			},
		}
		form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
		req := signedWebhookRequest(t, "authtoken", "https", "example.com", form).WithContext(ctx)
		s.ServeHTTP(httptest.NewRecorder(), req)

		if asked != 1 || gotValue != "request" || !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("RejectCall asked %d times, ctx value %v, err %v; want once, %q and context.Canceled (the request's context)",
				asked, gotValue, gotErr, "request")
		}
	})
}

// Signature validation and the E.164 From check run before either reject
// option: a forged request, or one whose From is withheld or not E.164, is
// still answered 403 and neither Authorize nor RejectCall is asked, so
// neither option can turn it into a 200 TwiML document.
func TestServeHTTP_GatesRunBeforeRejectOptions(t *testing.T) {
	servers := []struct {
		name  string
		build func(rec *rejectCallRecorder, authorized *int) *twilio.Server
	}{
		{"RejectCall", func(rec *rejectCallRecorder, _ *int) *twilio.Server {
			return &twilio.Server{AuthToken: "authtoken", StreamScheme: "wss", RejectCall: rec.hook}
		}},
		{"VoiceRejectReason", func(_ *rejectCallRecorder, authorized *int) *twilio.Server {
			return &twilio.Server{
				AuthToken:         "authtoken",
				StreamScheme:      "wss",
				Authorize:         func(string) bool { *authorized++; return false },
				VoiceRejectReason: twilio.RejectReasonBusy,
			}
		}},
	}
	requests := []struct{ name, signWith, from string }{
		{"bad signature", "wrongtoken", "+15105551234"},
		{"From anonymous", "authtoken", "anonymous"},
		{"From not E.164", "authtoken", "5105551234"},
	}
	for _, srv := range servers {
		for _, req := range requests {
			t.Run(srv.name+"/"+req.name, func(t *testing.T) {
				rec := &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: true}
				authorized := 0
				s := srv.build(rec, &authorized)
				form := url.Values{"From": {req.from}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, signedWebhookRequest(t, req.signWith, "https", "example.com", form))

				if w.Code != http.StatusForbidden || w.Body.String() != "Forbidden\n" || len(rec.asked) != 0 || authorized != 0 {
					t.Fatalf("status %d body %q, RejectCall asked %v, Authorize asked %d; want 403 %q and neither asked",
						w.Code, w.Body.String(), rec.asked, authorized, "Forbidden\n")
				}
			})
		}
	}
}

// A control, since the behaviour pinned is an absence: neither reject option
// reaches an inbound SMS. With both set, an Authorize refusal is still the
// exact <Message> SMSRejectText document and an admitted text still reaches
// HandleSMS with empty TwiML; RejectCall is never asked.
func TestServeSMS_RejectOptionsLeaveSMSUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		authorize   bool
		want        string
		wantHandled int
	}{
		{"Authorize refuses", false, `<Response><Message>I&#39;m sorry, this service is not available</Message></Response>`, 0},
		{"Authorize admits", true, `<Response></Response>`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &rejectCallRecorder{reason: twilio.RejectReasonBusy, reject: true}
			handled := 0
			s := &twilio.Server{
				AuthToken:         "authtoken",
				Authorize:         func(string) bool { return tc.authorize },
				SMSRejectText:     "I'm sorry, this service is not available",
				VoiceRejectReason: twilio.RejectReasonBusy,
				RejectCall:        rec.hook,
				HandleSMS:         func(context.Context, twilio.InboundSMS) { handled++ },
			}
			form := url.Values{"MessageSid": {"SM1"}, "From": {"+15105551234"}, "To": {"+15105550000"}, "Body": {"hi"}}
			w := httptest.NewRecorder()
			s.ServeSMS(w, signedTwilioRequest(t, "authtoken", "https", "example.com", "/sms/inbound", form))

			requireTwiML(t, w, tc.want)
			if handled != tc.wantHandled || len(rec.asked) != 0 {
				t.Fatalf("HandleSMS called %d times, RejectCall asked %v; want %d and no RejectCall",
					handled, rec.asked, tc.wantHandled)
			}
		})
	}
}
