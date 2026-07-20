package factdb

import (
	"path/filepath"
	"testing"
)

// TestParseResult confirms the parser recovers the JSON object even when a small
// model wraps it in a code fence and prose — the realistic failure mode.
func TestParseResult(t *testing.T) {
	content := "Sure! Here are the facts:\n```json\n" +
		`{"entities":[{"handle":"e1","type":"Person","mention":"my husband","resolution":"nil_placeholder"}],` +
		`"statements":[{"subject":"e1","predicate":"birthday","value":"Aug 8"}],` +
		`"followups":[{"gap":"e1.name","priority":"immediate","question":"What's his name?"}]}` +
		"\n```\nHope that helps!"
	r, err := ParseResult(content)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if len(r.Entities) != 1 || r.Entities[0].Handle != "e1" {
		t.Errorf("entities: %+v", r.Entities)
	}
	if len(r.Statements) != 1 || r.Statements[0].Value != "Aug 8" {
		t.Errorf("statements: %+v", r.Statements)
	}
	if len(r.Followups) != 1 || r.Followups[0].Priority != PriorityImmediate {
		t.Errorf("followups: %+v", r.Followups)
	}
}

// TestConsolidate checks that a name learned in a later turn fills the earlier
// placeholder, its now-satisfied "immediate" name follow-up drops out, and
// duplicate facts collapse.
func TestConsolidate(t *testing.T) {
	turn1 := &Result{
		Entities:   []Entity{{Handle: "e1", Type: "Person", Mention: "my husband", Resolution: ResolutionPlaceholder}},
		Statements: []Statement{{Subject: "e1", Predicate: "birthday", Value: "Aug 8"}},
		Followups:  []Followup{{Gap: "e1.name", Priority: PriorityImmediate, Question: "His name?"}},
	}
	turn2 := &Result{
		Entities:   []Entity{{Handle: "e1", Type: "Person", Mention: "Dave", Resolution: ResolutionResolved, Name: "Dave"}},
		Statements: []Statement{{Subject: "e1", Predicate: "birthday", Value: "Aug 8"}}, // dup
	}
	c := Consolidate([]*Result{turn1, turn2})

	if len(c.Entities) != 1 || c.Entities[0].Name != "Dave" || c.Entities[0].Resolution != ResolutionResolved {
		t.Errorf("entity not filled by later turn: %+v", c.Entities)
	}
	if len(c.Statements) != 1 {
		t.Errorf("duplicate statement not collapsed: %+v", c.Statements)
	}
	if len(c.Followups) != 0 {
		t.Errorf("name follow-up should drop once entity is named: %+v", c.Followups)
	}
}

// TestGrade checks statement recall and immediate-follow-up recall scoring.
func TestGrade(t *testing.T) {
	gold := &Result{
		Statements: []Statement{
			{Subject: "e1", Predicate: "birthday", Value: "Aug 8"},
			{Subject: "e2", Predicate: "birthday", Value: "Sep 22"},
		},
		Followups: []Followup{{Gap: "e1.name", Priority: PriorityImmediate}},
	}
	got := &Result{
		Statements: []Statement{
			{Subject: "x", Predicate: "Birthday", Value: "aug 8"}, // case-insensitive match
			{Subject: "y", Predicate: "hobby", Value: "chess"},    // extra
		},
		Followups: []Followup{{Gap: "person.name", Priority: PriorityImmediate}}, // matches on "name"
	}
	rep := Grade(got, gold)
	if rep.StmtFound != 1 || rep.StmtGold != 2 {
		t.Errorf("stmt recall: found %d/%d", rep.StmtFound, rep.StmtGold)
	}
	if rep.StmtExtra != 1 {
		t.Errorf("expected 1 extra, got %d", rep.StmtExtra)
	}
	if rep.ImmediateHit != 1 || rep.ImmediateGold != 1 {
		t.Errorf("immediate recall: %d/%d", rep.ImmediateHit, rep.ImmediateGold)
	}
}

// TestFixtureRoundTrip confirms a fixture survives save→load unchanged.
func TestFixtureRoundTrip(t *testing.T) {
	f := &Fixture{
		Name:  "husband-birthday",
		Model: "gemma",
		Turns: []Turn{{
			Speaker: "user",
			Text:    "my husband's birthday is Aug 8",
			Recorded: &Result{
				Entities:   []Entity{{Handle: "e1", Type: "Person", Mention: "my husband", Resolution: ResolutionPlaceholder}},
				Statements: []Statement{{Subject: "e1", Predicate: "birthday", Value: "Aug 8"}},
			},
		}},
		Gold: &Result{Statements: []Statement{{Subject: "e1", Predicate: "birthday", Value: "Aug 8"}}},
	}
	path := filepath.Join(t.TempDir(), "fx.json")
	if err := SaveFixture(path, f); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Name != f.Name || len(got.Turns) != 1 || got.Turns[0].Recorded.Statements[0].Value != "Aug 8" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Gold == nil || len(got.Gold.Statements) != 1 {
		t.Errorf("gold lost in round-trip: %+v", got.Gold)
	}
}
