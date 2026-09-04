package realtime_test

import (
	"encoding/json"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// TestResponseEndedInFunctionCall pins the question a consumer has to be able
// to ask of a response.done: did the turn actually end, or does the caller's
// wait continue into a tool round trip?
//
// It is exported because more than one thing needs the answer and they must
// not disagree. The relay's filler audio reads it to decide whether to keep
// the hold loop running or stop it; a consumer recording what the relay did
// reads it to decide where one wait ends and the next begins. Two
// reimplementations of this shape drift, and the drift is invisible: both
// answers look plausible on their own, and the disagreement only shows up as a
// wait that is reported as two waits, or a loop that stops in the middle of
// one.
//
// The false cases are the ones worth enumerating. Every one of them is a frame
// this could plausibly be handed, and the conservative answer to all of them
// is "the turn ended" — which leaves the loop stopped rather than playing over
// whatever comes next.
func TestResponseEndedInFunctionCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "a function call means the wait continues",
			raw:  `{"type":"response.done","response":{"output":[{"type":"function_call","name":"whatever"}]}}`,
			want: true,
		},
		{
			name: "a function call alongside speech still continues it",
			raw:  `{"type":"response.done","response":{"output":[{"type":"message"},{"type":"function_call"}]}}`,
			want: true,
		},
		{
			name: "speech alone ends the turn",
			raw:  `{"type":"response.done","response":{"output":[{"type":"message"}]}}`,
			want: false,
		},
		{
			name: "an empty output list ends the turn",
			raw:  `{"type":"response.done","response":{"output":[]}}`,
			want: false,
		},
		{
			name: "a response object with no output at all ends the turn",
			raw:  `{"type":"response.done","response":{}}`,
			want: false,
		},
		{
			name: "a frame that does not parse ends the turn",
			raw:  `{"type":"response.done",`,
			want: false,
		},
		{
			name: "an empty frame ends the turn",
			raw:  ``,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := realtime.ResponseEndedInFunctionCall(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("ResponseEndedInFunctionCall(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
