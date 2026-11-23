package twilio_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// --- AATK-168: the webhook's StirVerstat reaches both hooks and the start Frame ---

// The webhook's StirVerstat (Twilio's STIR/SHAKEN verification result) is on
// the context RejectCall and AnswerCall are handed, and on the start Frame of
// the stream the webhook opened. A call with no StirVerstat field gives "" in
// all three, as does a field sent empty.
func TestServeHTTP_StirVerstatReachesHooksAndStartFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"passed A", url.Values{"StirVerstat": {"TN-Validation-Passed-A"}}, "TN-Validation-Passed-A"},
		{"failed B", url.Values{"StirVerstat": {"TN-Validation-Failed-B"}}, "TN-Validation-Failed-B"},
		{"no validation", url.Values{"StirVerstat": {"No-TN-Validation"}}, "No-TN-Validation"},
		{"absent", url.Values{}, ""},
		{"empty", url.Values{"StirVerstat": {""}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var atReject, atAnswer []string
			s := &twilio.Server{
				AuthToken:    "authtoken",
				StreamScheme: "wss",
				RejectCall: func(ctx context.Context, _, _ string) (twilio.RejectReason, bool) {
					atReject = append(atReject, twilio.StirVerstat(ctx))
					return "", false
				},
				AnswerCall: func(ctx context.Context, _, _ string) (string, bool) {
					atAnswer = append(atAnswer, twilio.StirVerstat(ctx))
					return "", false
				},
			}
			form := url.Values{"From": {"+15105550123"}, "To": {"+15105550999"}, "CallSid": {"CA1"}}
			for k, v := range tc.form {
				form[k] = v
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, signedWebhookRequest(t, "authtoken", "https", "example.com", form))
			if w.Code != http.StatusOK {
				t.Fatalf("webhook status = %d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			if len(atReject) != 1 || atReject[0] != tc.want {
				t.Fatalf("RejectCall saw StirVerstat %q, want one call with %q", atReject, tc.want)
			}
			if len(atAnswer) != 1 || atAnswer[0] != tc.want {
				t.Fatalf("AnswerCall saw StirVerstat %q, want one call with %q", atAnswer, tc.want)
			}

			conn, startFrame := captureStartFrame(t, s)
			sendStart(t, conn, "SS1", "CA1")
			if f := startFrame(); f.StirVerstat != tc.want || f.From != "+15105550123" {
				t.Fatalf("Frame StirVerstat/From = %q/%q, want %q/+15105550123", f.StirVerstat, f.From, tc.want)
			}
		})
	}
}

// A context the webhook never touched carries no StirVerstat.
func TestStirVerstat_EmptyOnPlainContext(t *testing.T) {
	if got := twilio.StirVerstat(context.Background()); got != "" {
		t.Fatalf("StirVerstat(Background) = %q, want \"\"", got)
	}
}

// A start frame whose CallSid no webhook recorded has no StirVerstat.
func TestServeStreams_StirVerstatEmptyWhenNoWebhookRecorded(t *testing.T) {
	conn, startFrame := captureStartFrame(t, &twilio.Server{})
	sendStart(t, conn, "SSunknown01", "CAunknown01")
	if f := startFrame(); f.StirVerstat != "" {
		t.Fatalf("Frame StirVerstat = %q, want \"\"", f.StirVerstat)
	}
}
