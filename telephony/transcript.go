package telephony

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// defaultAgentLabel is the generic role name for the agent's response line in
// the transcript summary. This engine is agent-agnostic, so the consumer's
// product identity is never embedded here (the boundary rule); a consumer that
// wants its own name injects it via WithTranscriptAgentLabel.
const defaultAgentLabel = "agent"

// formatTranscript renders a call's per-turn utterance lists as a human-readable
// conversation view (SOP-168): each user turn's utterances bracketed on one
// line, with a blank agent response line between consecutive user turns (the
// agent has no spoken response yet -- the blank brackets are the placeholder).
// Its purpose is to gauge the quality of what a human would hear/read -- STT
// accuracy and blank responses -- not audio quality. Empty input renders "".
//
// agentLabel names the response role; the "user" and agent prefixes are padded
// to the same width so the bracketed content lines up in a terminal.
func formatTranscript(turns [][]string, agentLabel string) string {
	labelW := len("user")
	if len(agentLabel) > labelW {
		labelW = len(agentLabel)
	}
	userPrefix := fmt.Sprintf("%-*s --> ", labelW, "user")
	agentLine := fmt.Sprintf("%-*s ->  []\n", labelW, agentLabel)

	var b strings.Builder
	for i, turn := range turns {
		b.WriteString(userPrefix)
		for j, utt := range turn {
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteByte('[')
			b.WriteString(utt)
			b.WriteByte(']')
		}
		b.WriteByte('\n')
		if i < len(turns)-1 {
			b.WriteString(agentLine)
		}
	}
	return b.String()
}

// WithTranscriptOutput enables the end-of-call conversation transcript summary
// (SOP-168). At Close the session renders each turn's utterances bracketed and:
// prints the summary to live (nil = no print), and writes <sid>.transcript.txt
// into dir (dir == "" = no file). Live and replay wiring pass the same dir/sid
// as the audio tap / decision record so the transcript sits beside them.
func WithTranscriptOutput(dir, sid string, live io.Writer) SessionOption {
	return func(s *Session) {
		s.transcriptDir = dir
		s.transcriptSID = sid
		s.transcriptLive = live
	}
}

// WithTranscriptAgentLabel sets the response role label the transcript summary
// prints (SOP-168). Empty or unset falls back to the generic "agent"; a consumer
// injects its own product name here so the engine never embeds it.
func WithTranscriptAgentLabel(label string) SessionOption {
	return func(s *Session) { s.transcriptAgentLabel = label }
}

// emitTranscript renders the accumulated per-turn transcript and writes it to
// the configured live writer and/or <sid>.transcript.txt file (SOP-168). Called
// from Close after the sequencer has drained. Clears the accumulator so a second
// Close is a no-op.
func (s *Session) emitTranscript() {
	if len(s.transcriptTurns) == 0 {
		return
	}
	label := s.transcriptAgentLabel
	if label == "" {
		label = defaultAgentLabel
	}
	out := formatTranscript(s.transcriptTurns, label)
	s.transcriptTurns = nil

	if s.transcriptLive != nil {
		fmt.Fprint(s.transcriptLive, out)
	}
	if s.transcriptDir != "" && s.transcriptSID != "" {
		path := filepath.Join(s.transcriptDir, s.transcriptSID+".transcript.txt")
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			log.Printf("telephony: session %s: transcript write: %v", s.CallSID, err)
		}
	}
}
