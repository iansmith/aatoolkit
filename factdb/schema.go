package factdb

import (
	"encoding/json"
	"fmt"
	"os"
)

// Ontology is the domain-specific extraction contract a caller supplies to the
// harness: the controlled predicate vocabulary, the entity-type enum, and the
// system prompt that instructs the model. Constraining the model to these values
// (via the response_format JSON schema AND the prompt) is the fix for vocabulary
// drift — a model understands the facts but invents predicate names ("schedule",
// "attends") instead of the graph's canonical ones unless it is pinned. Keep the
// predicate list and the gold fixtures in sync: a fact can only be graded if both
// sides name the predicate the same way.
type Ontology struct {
	Predicates   []string `json:"predicates"`
	EntityTypes  []string `json:"entity_types"`
	SystemPrompt string   `json:"system_prompt"`
}

// LoadOntology reads an Ontology from a JSON file (predicates, entity_types,
// system_prompt) and validates that none of the three is empty.
func LoadOntology(path string) (Ontology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Ontology{}, err
	}
	var o Ontology
	if err := json.Unmarshal(b, &o); err != nil {
		return Ontology{}, fmt.Errorf("parsing ontology %s: %w", path, err)
	}
	if len(o.Predicates) == 0 {
		return Ontology{}, fmt.Errorf("ontology %s: no predicates declared", path)
	}
	if len(o.EntityTypes) == 0 {
		return Ontology{}, fmt.Errorf("ontology %s: no entity_types declared", path)
	}
	if o.SystemPrompt == "" {
		return Ontology{}, fmt.Errorf("ontology %s: empty system_prompt", path)
	}
	return o, nil
}

// ResultSchema returns the JSON Schema for a Result, with the key fields locked to
// enums: predicate to the ontology's controlled vocabulary, entity type, resolution,
// and follow-up priority. Emitted as response_format.json_schema so the model's
// output is constrained at generation time rather than fuzzy-matched afterward.
func (o Ontology) ResultSchema() map[string]any {
	str := map[string]any{"type": "string"}
	enum := func(vals []string) map[string]any {
		return map[string]any{"type": "string", "enum": vals}
	}

	entity := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle":     str,
			"type":       enum(o.EntityTypes),
			"mention":    str,
			"resolution": enum([]string{ResolutionResolved, ResolutionPlaceholder}),
			"name":       str,
			"attrs":      map[string]any{"type": "object", "additionalProperties": str},
		},
		"required": []string{"handle", "type", "mention", "resolution"},
	}

	statement := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subject":     str,
			"predicate":   enum(o.Predicates),
			"value":       str,
			"object":      str,
			"valid_from":  str,
			"valid_until": str,
			"recurrence":  str,
			"confidence":  map[string]any{"type": "number"},
			"source_span": str,
		},
		"required": []string{"subject", "predicate"},
	}

	followup := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"gap":      str,
			"priority": enum([]string{PriorityImmediate, PriorityBanked}),
			"reason":   str,
			"question": str,
		},
		"required": []string{"gap", "priority", "question"},
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entities":   map[string]any{"type": "array", "items": entity},
			"statements": map[string]any{"type": "array", "items": statement},
			"followups":  map[string]any{"type": "array", "items": followup},
		},
		"required": []string{"entities", "statements", "followups"},
	}
}
