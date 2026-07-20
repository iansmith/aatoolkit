package factdb

import "testing"

// TestCompile pins the JSON→openCypher translation for a small but representative
// slice of the "husband's birthday" utterance: one resolved entity (the user), one
// placeholder (the unnamed husband), a reified birthday statement, and a relational
// spouse statement. The golden output must be deterministic (stable ordering,
// escaped literals, MERGE for idempotency) so re-running extraction never dups nodes.
func TestCompile(t *testing.T) {
	r := &Result{
		Entities: []Entity{
			{Handle: "e2", Type: "Person", Mention: "my husband", Resolution: ResolutionPlaceholder, Attrs: map[string]string{"rel": "spouse_of:user"}},
			{Handle: "e1", Type: "Person", Mention: "I", Resolution: ResolutionResolved, Name: "user"},
		},
		Statements: []Statement{
			{Subject: "e2", Predicate: "birthday", Value: "Aug 8", Confidence: 0.9, SourceSpan: "my husband's birthday is Aug 8"},
			{Subject: "e1", Predicate: "spouse", Object: "e2"},
		},
		Followups: []Followup{
			{Gap: "e2.name", Priority: PriorityImmediate, Question: "What's your husband's name?"},
		},
	}

	want := `// entities
MERGE (e1:Person {k:'user'}) SET e1.mention='I', e1.resolution='resolved', e1.name='user';
MERGE (e2:Person {k:'Person:my husband'}) SET e2.mention='my husband', e2.resolution='nil_placeholder', e2.rel='spouse_of:user';
// statements
MERGE (e1)-[:SUBJECT_OF]->(s_e1_spouse:Statement {k:'e1|spouse||e2'}) SET s_e1_spouse.predicate='spouse';
MERGE (s_e1_spouse)-[:ABOUT]->(e2);
MERGE (e2)-[:SUBJECT_OF]->(s_e2_birthday:Statement {k:'e2|birthday|Aug 8|'}) SET s_e2_birthday.predicate='birthday', s_e2_birthday.value='Aug 8', s_e2_birthday.confidence=0.9, s_e2_birthday.source_span='my husband\'s birthday is Aug 8';
// followups (not written to graph; shown for review)
// [immediate] e2.name — What's your husband's name?
`

	got := Compile(r)
	if got != want {
		t.Errorf("Compile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
