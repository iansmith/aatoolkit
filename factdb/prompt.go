package factdb

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BuildMessages assembles the OpenAI-style messages array for one extraction call:
// the ontology's system prompt (with its predicate vocabulary appended), then a
// user message carrying prior turns as context, the known entity handles to reuse,
// and the current utterance to extract. The system prompt must instruct the model
// to emit ONLY the JSON this package parses (entities/statements/followups).
func (o Ontology) BuildMessages(priorTurns []Turn, known []Entity, utterance string) ([]byte, error) {
	var ctx strings.Builder
	if len(priorTurns) > 0 {
		ctx.WriteString("Conversation so far:\n")
		for _, t := range priorTurns {
			fmt.Fprintf(&ctx, "  %s: %s\n", t.Speaker, t.Text)
		}
		ctx.WriteString("\n")
	}
	if len(known) > 0 {
		ctx.WriteString("Known entities (reuse these handles):\n")
		for _, e := range known {
			label := e.Mention
			if e.Name != "" {
				label = e.Name
			}
			fmt.Fprintf(&ctx, "  %s = %s (%s)\n", e.Handle, label, e.Resolution)
		}
		ctx.WriteString("\n")
	}
	fmt.Fprintf(&ctx, "Current user turn to extract:\n  %s", utterance)

	sys := o.SystemPrompt + "\n\nUse ONLY these predicate values (pick the closest match): " +
		strings.Join(o.Predicates, ", ") + "."
	msgs := []map[string]string{
		{"role": "system", "content": sys},
		{"role": "user", "content": ctx.String()},
	}
	return json.Marshal(msgs)
}
