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
// The false cases are the ones worth enumerating, and they come in two kinds.
// Malformed frames — unparseable, no output, empty — answer "the turn ended",
// the conservative direction, which leaves a loop stopped rather than playing
// over whatever comes next. Well-formed frames that merely CONTAIN the token
// answer false too, and those are the ones that give the table its teeth: the
// item type is read at response.output[].type and nowhere else, so a
// transcript quoting the words, an item nested a level deeper, and output
// hung at the wrong depth are all false. Without them every case here agrees
// with a plain substring scan for "function_call", which is precisely the
// wrong implementation a second author would write.
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
		// The three cases below are what separate this from a substring scan
		// for "function_call", and they are here because a review measured
		// that the original table did not: every case in it agreed with
		// bytes.Contains(raw, []byte("function_call")), so the table could
		// not tell the real predicate from that one. A table sampled from
		// what an implementation handles proves it self-consistent and
		// nothing else.
		{
			name: "the token nested inside an item's content does not count",
			raw:  `{"type":"response.done","response":{"output":[{"type":"message","content":[{"type":"function_call"}]}]}}`,
			want: false,
		},
		{
			name: "the token at the wrong depth does not count",
			raw:  `{"type":"response.done","output":[{"type":"function_call"}]}`,
			want: false,
		},
		{
			name: "a transcript that merely says the words does not count",
			raw:  `{"type":"response.done","response":{"output":[{"type":"message","transcript":"the type is function_call"}]}}`,
			want: false,
		},
		{
			name: "a frame that is not a response.done does not count",
			raw:  `{"type":"response.created","response":{"output":[{"type":"function_call"}]}}`,
			want: false,
		},
		{
			name: "a frame with no type at all does not count",
			raw:  `{"response":{"output":[{"type":"function_call"}]}}`,
			want: false,
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
