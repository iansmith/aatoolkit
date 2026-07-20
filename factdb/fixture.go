package factdb

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// LoadFixture reads a fixture JSON file.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// SaveFixture writes a fixture as pretty JSON (stable, diff-friendly on disk).
func SaveFixture(path string, f *Fixture) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Consolidate merges a sequence of per-turn results into one conversation-level
// Result — the view grading compares against gold, and the view the compiler turns
// into a graph. Entities union by handle (later mentions win on name/resolution and
// merge attrs, modeling a placeholder being filled by a later turn). Statements
// dedup by key. Follow-ups dedup by gap, keeping the strongest priority and
// dropping any gap that a later turn resolved into a named entity.
func Consolidate(results []*Result) *Result {
	entByHandle := map[string]Entity{}
	var entOrder []string
	for _, r := range results {
		if r == nil {
			continue
		}
		for _, e := range r.Entities {
			prev, seen := entByHandle[e.Handle]
			if !seen {
				entOrder = append(entOrder, e.Handle)
				entByHandle[e.Handle] = e
				continue
			}
			entByHandle[e.Handle] = mergeEntity(prev, e)
		}
	}

	stmtByKey := map[string]Statement{}
	var stmtOrder []string
	for _, r := range results {
		if r == nil {
			continue
		}
		for _, s := range r.Statements {
			k := statementKey(s)
			if _, seen := stmtByKey[k]; !seen {
				stmtOrder = append(stmtOrder, k)
			}
			stmtByKey[k] = s // later turn's version wins (e.g. added validity)
		}
	}

	fuByGap := map[string]Followup{}
	var fuOrder []string
	for _, r := range results {
		if r == nil {
			continue
		}
		for _, f := range r.Followups {
			prev, seen := fuByGap[f.Gap]
			if !seen {
				fuOrder = append(fuOrder, f.Gap)
				fuByGap[f.Gap] = f
				continue
			}
			if f.Priority == PriorityImmediate {
				prev.Priority = PriorityImmediate
			}
			fuByGap[f.Gap] = prev
		}
	}

	out := &Result{}
	for _, h := range entOrder {
		out.Entities = append(out.Entities, entByHandle[h])
	}
	for _, k := range stmtOrder {
		out.Statements = append(out.Statements, stmtByKey[k])
	}
	// Drop follow-ups whose gap is "<handle>.name" once that entity has a name.
	named := map[string]bool{}
	for _, e := range out.Entities {
		if e.Name != "" {
			named[e.Handle+".name"] = true
		}
	}
	for _, g := range fuOrder {
		if named[g] {
			continue
		}
		out.Followups = append(out.Followups, fuByGap[g])
	}
	return out
}

// mergeEntity folds a later mention of the same handle into an earlier one: a name
// or a resolved status, once learned, sticks; attrs union.
func mergeEntity(prev, next Entity) Entity {
	if next.Name != "" {
		prev.Name = next.Name
	}
	if next.Resolution == ResolutionResolved {
		prev.Resolution = ResolutionResolved
	}
	if next.Type != "" {
		prev.Type = next.Type
	}
	if len(next.Attrs) > 0 {
		if prev.Attrs == nil {
			prev.Attrs = map[string]string{}
		}
		for _, k := range sortedKeys(next.Attrs) {
			prev.Attrs[k] = next.Attrs[k]
		}
	}
	return prev
}

// KnownEntities flattens the entities recorded across prior turns, so the next
// turn's prompt can tell the model which handles already exist to reuse.
func KnownEntities(turns []Turn) []Entity {
	var results []*Result
	for _, t := range turns {
		if t.Recorded != nil {
			results = append(results, t.Recorded)
		}
	}
	c := Consolidate(results)
	sort.Slice(c.Entities, func(i, j int) bool { return c.Entities[i].Handle < c.Entities[j].Handle })
	return c.Entities
}
