package twilio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// --- AATK-157: the pending-call map ServeHTTP fills for ServeStreams ---

// pendingSID and pendingFrom name the i-th call these tests record, so every
// entry carries values of its own and a lookup cannot pass by finding some
// other call's entry.
func pendingSID(i int) string  { return fmt.Sprintf("CA%05d", i) }
func pendingFrom(i int) string { return fmt.Sprintf("+1510555%04d", i) }

const pendingTo = "+14155550000"

// requirePendingSize fails unless the map and its eviction order both hold
// exactly want entries. Checking both catches a slot taken in one and not
// the other.
func requirePendingSize(t *testing.T, s *Server, want int) {
	t.Helper()
	if len(s.fromBySID) != want || len(s.fromOrder) != want {
		t.Fatalf("pending map holds %d entries, eviction order %d; want %d of each",
			len(s.fromBySID), len(s.fromOrder), want)
	}
}

// requirePending fails unless callSid looks up to exactly from and to.
func requirePending(t *testing.T, s *Server, callSid, from, to string) {
	t.Helper()
	if got := s.lookupCall(callSid); got.from != from || got.to != to {
		t.Fatalf("lookupCall(%q) = From %q To %q, want From %q To %q", callSid, got.from, got.to, from, to)
	}
}

// One CallSid past maxPendingFrom evicts the oldest, and only the oldest: its
// lookup comes back "" for both From and To, every later call is still
// there with its own values, and the map never holds more than the bound.
func TestRecordCall_EvictsOldestPastBound(t *testing.T) {
	s := &Server{}
	for i := 0; i <= maxPendingFrom; i++ {
		s.recordCall(pendingSID(i), pendingFrom(i), pendingTo)
		if n := len(s.fromBySID); n > maxPendingFrom {
			t.Fatalf("after recording %d calls the pending map holds %d entries, want at most %d",
				i+1, n, maxPendingFrom)
		}
	}

	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), "", "")
	for i := 1; i <= maxPendingFrom; i++ {
		requirePending(t, s, pendingSID(i), pendingFrom(i), pendingTo)
	}
}

// Recording a CallSid that is already pending replaces its From and To in
// place. With the map full, re-recording the second-oldest call evicts
// nothing: it takes no new slot. It also keeps its place in the eviction
// order, so the next two new calls evict the oldest and then it, not the
// call recorded after it.
func TestRecordCall_ReRecordUpdatesInPlace(t *testing.T) {
	s := &Server{}
	for i := 0; i < maxPendingFrom; i++ {
		s.recordCall(pendingSID(i), pendingFrom(i), pendingTo)
	}

	const newFrom, newTo = "+15105559999", "+15105558888"
	s.recordCall(pendingSID(1), newFrom, newTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), pendingFrom(0), pendingTo)
	requirePending(t, s, pendingSID(1), newFrom, newTo)

	s.recordCall(pendingSID(maxPendingFrom), pendingFrom(maxPendingFrom), pendingTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(0), "", "")
	requirePending(t, s, pendingSID(1), newFrom, newTo)

	s.recordCall(pendingSID(maxPendingFrom+1), pendingFrom(maxPendingFrom+1), pendingTo)
	requirePendingSize(t, s, maxPendingFrom)
	requirePending(t, s, pendingSID(1), "", "")
	requirePending(t, s, pendingSID(2), pendingFrom(2), pendingTo)
}

// signedVoiceWebhook builds a voice webhook POST for ServeHTTP, signed for
// authToken over https://example.com/webhook.
func signedVoiceWebhook(authToken string, form url.Values) *http.Request {
	const rawURL = "https://example.com/webhook"
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", ComputeSignature(authToken, rawURL, form))
	return req
}

// A call ServeHTTP does not answer with a stream takes no pending slot. No
// stream opens for it, so nothing would ever read the entry, and holding one
// would only push a call that will stream toward eviction. Each refusal path
// is checked on a Server whose map is empty, and the refusal itself is
// checked too, so a row cannot pass by never reaching its path.
func TestServeHTTP_RefusedCallTakesNoPendingSlot(t *testing.T) {
	const say = "not this number"
	for _, tc := range []struct {
		name      string
		server    func(asked *int) *Server
		form      url.Values
		signWith  string
		wantCode  int
		wantBody  string
		wantAsked int
	}{
		{
			name:     "bad signature",
			server:   func(*int) *Server { return &Server{AuthToken: "authtoken"} },
			form:     url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith: "wrongtoken",
			wantCode: http.StatusForbidden,
			wantBody: "Forbidden\n",
		},
		{
			name:     "From not E.164",
			server:   func(*int) *Server { return &Server{AuthToken: "authtoken"} },
			form:     url.Values{"From": {"anonymous"}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith: "authtoken",
			wantCode: http.StatusForbidden,
			wantBody: "Forbidden\n",
		},
		{
			name: "Authorize refuses",
			server: func(*int) *Server {
				return &Server{
					AuthToken:       "authtoken",
					Authorize:       func(string) bool { return false },
					VoiceRejectText: "no",
				}
			},
			form:     url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith: "authtoken",
			wantCode: http.StatusOK,
			wantBody: `<Response><Say>no</Say><Hangup/></Response>`,
		},
		{
			name: "AnswerCall refuses",
			server: func(asked *int) *Server {
				return &Server{
					AuthToken: "authtoken",
					AnswerCall: func(context.Context, string, string) (string, bool) {
						*asked++
						return say, true
					},
				}
			},
			form:      url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith:  "authtoken",
			wantCode:  http.StatusOK,
			wantBody:  `<Response><Say>` + say + `</Say><Hangup/></Response>`,
			wantAsked: 1,
		},
		// AATK-162: an unanswered refusal takes no slot either. In these rows
		// asked counts RejectCall.
		{
			name: "RejectCall rejects",
			server: func(asked *int) *Server {
				return &Server{
					AuthToken: "authtoken",
					RejectCall: func(context.Context, string, string) (RejectReason, bool) {
						*asked++
						return "", true
					},
				}
			},
			form:      url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith:  "authtoken",
			wantCode:  http.StatusOK,
			wantBody:  `<Response><Reject/></Response>`,
			wantAsked: 1,
		},
		{
			name: "RejectCall rejects busy",
			server: func(asked *int) *Server {
				return &Server{
					AuthToken: "authtoken",
					RejectCall: func(context.Context, string, string) (RejectReason, bool) {
						*asked++
						return RejectReasonBusy, true
					},
				}
			},
			form:      url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith:  "authtoken",
			wantCode:  http.StatusOK,
			wantBody:  `<Response><Reject reason="busy"/></Response>`,
			wantAsked: 1,
		},
		{
			name: "Authorize refuses with VoiceRejectReason",
			server: func(asked *int) *Server {
				return &Server{
					AuthToken:         "authtoken",
					Authorize:         func(string) bool { return false },
					VoiceRejectText:   "no",
					VoiceRejectReason: RejectReasonRejected,
					RejectCall: func(context.Context, string, string) (RejectReason, bool) {
						*asked++
						return "", true
					},
				}
			},
			form:     url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}},
			signWith: "authtoken",
			wantCode: http.StatusOK,
			wantBody: `<Response><Reject/></Response>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked int
			s := tc.server(&asked)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedVoiceWebhook(tc.signWith, tc.form))

			if w.Code != tc.wantCode || w.Body.String() != tc.wantBody || asked != tc.wantAsked {
				t.Fatalf("status %d body %q, hook asked %d times; want %d %q, %d",
					w.Code, w.Body.String(), asked, tc.wantCode, tc.wantBody, tc.wantAsked)
			}
			requirePendingSize(t, s, 0)
			requirePending(t, s, pendingSID(1), "", "")
		})
	}
}

// The control for the refusal rows above: a call answered with a stream,
// with Authorize and AnswerCall both set and both letting it through, does
// take a slot, holding its From and To. Without this, the refusal rows would
// pass on a ServeHTTP that never recorded anything.
func TestServeHTTP_StreamedCallTakesPendingSlot(t *testing.T) {
	s := &Server{
		AuthToken: "authtoken",
		Authorize: func(string) bool { return true },
		AnswerCall: func(context.Context, string, string) (string, bool) {
			return "", false
		},
	}
	form := url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedVoiceWebhook("authtoken", form))

	want := `<Response><Connect><Stream url="wss://example.com/streams"/></Connect></Response>`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status %d body %q, want 200 %q", w.Code, w.Body.String(), want)
	}
	requirePendingSize(t, s, 1)
	requirePending(t, s, pendingSID(1), pendingFrom(1), pendingTo)
}

// The control for the unanswered refusal rows above: with VoiceRejectReason
// set and RejectCall letting the call through, a call answered with a stream
// still takes a slot, holding its From and To. Without this, those rows would
// pass on a ServeHTTP that skipped recording whenever a reject option was set.
func TestServeHTTP_RejectOptionsSetStreamedCallTakesPendingSlot(t *testing.T) {
	s := &Server{
		AuthToken:         "authtoken",
		Authorize:         func(string) bool { return true },
		VoiceRejectReason: RejectReasonBusy,
		RejectCall: func(context.Context, string, string) (RejectReason, bool) {
			return RejectReasonBusy, false
		},
		AnswerCall: func(context.Context, string, string) (string, bool) {
			return "", false
		},
	}
	form := url.Values{"From": {pendingFrom(1)}, "To": {pendingTo}, "CallSid": {pendingSID(1)}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, signedVoiceWebhook("authtoken", form))

	want := `<Response><Connect><Stream url="wss://example.com/streams"/></Connect></Response>`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status %d body %q, want 200 %q", w.Code, w.Body.String(), want)
	}
	requirePendingSize(t, s, 1)
	requirePending(t, s, pendingSID(1), pendingFrom(1), pendingTo)
}
