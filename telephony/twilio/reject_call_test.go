package twilio_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- AATK-162: refusing a voice call unanswered (<Reject>) ---

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
		name            string
		authorize       func(string) bool
		voiceRejectText string
		answer          *answerCallRecorder // nil leaves AnswerCall unset
		wantAnswerAsked int
		want            string
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &twilio.Server{
				AuthToken:       "authtoken",
				StreamScheme:    "wss",
				Authorize:       tc.authorize,
				VoiceRejectText: tc.voiceRejectText,
			}
			if tc.answer != nil {
				s.AnswerCall = tc.answer.hook
			}
			form := url.Values{"From": {"+15105551234"}, "To": {"+15105550000"}, "CallSid": {"CA1"}}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))

			requireTwiML(t, w, tc.want)
			if tc.answer != nil && len(tc.answer.asked) != tc.wantAnswerAsked {
				t.Fatalf("AnswerCall asked %v, want %d call(s)", tc.answer.asked, tc.wantAnswerAsked)
			}
		})
	}
}
