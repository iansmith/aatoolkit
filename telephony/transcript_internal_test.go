package telephony

import "testing"

// TestFormatTranscript pins the conversation transcript summary's exact shape
// (SOP-168): each user turn's utterances bracketed on one line, with a blank
// agent response line between consecutive user turns. This is the format the
// human reads at end of call/replay to gauge STT + segmentation quality.
func TestFormatTranscript(t *testing.T) {
	turns := [][]string{
		{"Hello there", "how are you today"},
		{"I need some help", "with scheduling a trip"},
	}
	want := "" +
		"user  --> [Hello there] [how are you today]\n" +
		"agent ->  []\n" +
		"user  --> [I need some help] [with scheduling a trip]\n"
	if got := formatTranscript(turns, "agent"); got != want {
		t.Errorf("formatTranscript mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatTranscript_CustomLabel proves the agent label is injectable and the
// user/agent prefixes stay column-aligned when the label is a different width.
func TestFormatTranscript_CustomLabel(t *testing.T) {
	want := "" +
		"user --> [a]\n" +
		"bot  ->  []\n" +
		"user --> [b]\n"
	if got := formatTranscript([][]string{{"a"}, {"b"}}, "bot"); got != want {
		t.Errorf("formatTranscript custom-label mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatTranscript_SingleTurn: one turn, no agent line (nothing followed).
func TestFormatTranscript_SingleTurn(t *testing.T) {
	want := "user  --> [just this]\n"
	if got := formatTranscript([][]string{{"just this"}}, "agent"); got != want {
		t.Errorf("formatTranscript single-turn mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatTranscript_Empty: no turns renders nothing.
func TestFormatTranscript_Empty(t *testing.T) {
	if got := formatTranscript(nil, "agent"); got != "" {
		t.Errorf("formatTranscript(nil): got %q, want empty", got)
	}
}

// TestFlushTurnTranscripts_AccumulatesForTranscript pins the capture hook: a
// completed turn's per-utterance list is copied into transcriptTurns (before the
// join) so Close can render it, and an empty flush appends nothing (SOP-168).
func TestFlushTurnTranscripts_AccumulatesForTranscript(t *testing.T) {
	s := &Session{turnTranscripts: []string{"hello there", "how are you"}}
	s.flushTurnTranscripts(TriggerSilenceTurnEnd)

	if len(s.transcriptTurns) != 1 {
		t.Fatalf("transcriptTurns: got %d turns, want 1", len(s.transcriptTurns))
	}
	got := s.transcriptTurns[0]
	if len(got) != 2 || got[0] != "hello there" || got[1] != "how are you" {
		t.Errorf("captured utterances: got %v, want [hello there, how are you]", got)
	}
	if s.turnTranscripts != nil {
		t.Errorf("turnTranscripts not cleared after flush: %v", s.turnTranscripts)
	}

	// An empty flush is a no-op -- it must not append an empty turn.
	s.flushTurnTranscripts(TriggerSilenceTurnEnd)
	if len(s.transcriptTurns) != 1 {
		t.Errorf("empty flush appended a turn: transcriptTurns len %d, want 1", len(s.transcriptTurns))
	}
}
