// Package ftest is a probe harness for measuring how well the local LLM extracts
// storable facts from everyday conversation. It is deliberately separate from the
// live assistant: the point is to see, on a curated set of conversations, what the
// model pulls out (entities, temporal facts, placeholders for unknown people) and
// whether it flags the high-value gaps — names and familial relationships — for an
// immediate follow-up.
//
// The data model mirrors the fact-database design in docs/fact-database-research.md:
// entities carry a resolution status (resolved vs. NIL placeholder), statements are
// reified facts with bi-temporal validity and provenance, and follow-ups are the
// knowledge gaps the model wants filled, each with a priority (immediate | banked).
//
// Extraction emits structured JSON (this package's types), NOT AGE Cypher, so the
// model's job is pure semantics; a deterministic compiler (compile.go) turns the
// JSON into openCypher for Apache AGE. That split keeps "did it understand the
// facts" measurable independently of "did it emit valid Cypher".
package factdb

// Fixture is one recorded conversation used as an extraction test case. `record`
// mode produces it live; `run` mode replays Turns through the extractor and, when
// Gold is present, grades the consolidated result against it.
type Fixture struct {
	Name  string  `json:"name"`
	Notes string  `json:"notes,omitempty"`
	Model string  `json:"model,omitempty"` // model that produced the Recorded fields
	Turns []Turn  `json:"turns"`
	Gold  *Result `json:"gold,omitempty"` // hand-authored expected consolidated facts
}

// Turn is one utterance. For user turns, Recorded holds the extraction the model
// produced at record time, given all prior turns as context (incremental). Assistant
// turns carry no extraction — they exist so the model sees realistic dialogue context.
type Turn struct {
	Speaker  string  `json:"speaker"` // "user" | "assistant"
	Text     string  `json:"text"`
	Recorded *Result `json:"recorded,omitempty"`
}

// Result is the structured extraction for one turn — or, after Consolidate, for a
// whole conversation. It is exactly what the model is asked to emit as JSON.
type Result struct {
	Entities   []Entity    `json:"entities"`
	Statements []Statement `json:"statements"`
	Followups  []Followup  `json:"followups"`
}

// Entity is a person/group/event/activity mentioned in the conversation. Handle is
// a conversation-local id ("e1", "e2") the model assigns and reuses across turns;
// the compiler maps handles to stable MERGE keys so the model never invents graph
// ids. Resolution records whether the mention was pinned to a known entity or left
// as a placeholder to be filled by a later answer.
type Entity struct {
	Handle     string            `json:"handle"`          // "e1"
	Type       string            `json:"type"`            // Person | Group | Event | Activity
	Mention    string            `json:"mention"`         // "my husband"
	Resolution string            `json:"resolution"`      // "resolved" | "nil_placeholder"
	Name       string            `json:"name,omitempty"`  // known canonical name, if any
	Attrs      map[string]string `json:"attrs,omitempty"` // e.g. {"rel":"spouse_of:user"}
}

// Statement is a reified fact: subject --predicate--> (value | object), with optional
// bi-temporal validity, recurrence, confidence, and the source text span it came from.
// Object names an entity handle when the object is itself an entity (n-ary/relational
// facts); Value holds a literal when it is not.
type Statement struct {
	Subject    string  `json:"subject"`               // entity handle
	Predicate  string  `json:"predicate"`             // "birthday", "has_activity", "prefers_pronoun"...
	Value      string  `json:"value,omitempty"`       // literal object
	Object     string  `json:"object,omitempty"`      // entity-handle object (relational fact)
	ValidFrom  string  `json:"valid_from,omitempty"`  // ISO-8601 or ""; "" = unknown/open
	ValidUntil string  `json:"valid_until,omitempty"` // when the fact expires (e.g. Christmas)
	Recurrence string  `json:"recurrence,omitempty"`  // iCalendar RRULE, e.g. "FREQ=WEEKLY;BYDAY=MO"
	Confidence float64 `json:"confidence,omitempty"`
	SourceSpan string  `json:"source_span,omitempty"`
}

// Followup is a knowledge gap the model wants filled. Priority is the crux of the
// probe: "immediate" means the caller should ask this turn (high-value — names, familial
// relationships), "banked" means queue it for later so she isn't an interrogation.
type Followup struct {
	Gap      string `json:"gap"`      // "e1.name"
	Priority string `json:"priority"` // "immediate" | "banked"
	Reason   string `json:"reason,omitempty"`
	Question string `json:"question"`
}

// Priority values.
const (
	PriorityImmediate = "immediate"
	PriorityBanked    = "banked"
)

// Resolution values.
const (
	ResolutionResolved    = "resolved"
	ResolutionPlaceholder = "nil_placeholder"
)
