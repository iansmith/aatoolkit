package factdb

import (
	"fmt"
	"sort"
	"strings"
)

// Report scores a consolidated extraction against a gold Result. It is deliberately
// forgiving on surface form and strict on the things the probe cares about: did the
// model find the facts (statements), and did it flag the right high-value gaps for
// an immediate follow-up (§6.1)?
type Report struct {
	StmtGold      int // statements in gold
	StmtFound     int // gold statements the model produced (recall numerator)
	StmtExtra     int // model statements with no gold match (over-extraction)
	ImmediateGold int // gold follow-ups marked immediate
	ImmediateHit  int // gold immediate follow-ups the model also marked immediate
	Missed        []string
	Extra         []string
}

// Grade compares a consolidated model result to gold.
func Grade(got, gold *Result) Report {
	var rep Report
	if gold == nil {
		return rep
	}
	goldStmt := map[string]bool{}
	for _, s := range gold.Statements {
		goldStmt[normStmt(s)] = true
	}
	gotStmt := map[string]bool{}
	for _, s := range got.Statements {
		gotStmt[normStmt(s)] = true
	}
	rep.StmtGold = len(goldStmt)
	for k := range goldStmt {
		if gotStmt[k] {
			rep.StmtFound++
		} else {
			rep.Missed = append(rep.Missed, k)
		}
	}
	for k := range gotStmt {
		if !goldStmt[k] {
			rep.StmtExtra++
			rep.Extra = append(rep.Extra, k)
		}
	}

	// Immediate follow-up recall: of the gaps gold says need asking now, how many
	// did the model also mark immediate? Matched on the gap's target, ignoring the
	// exact question wording.
	gotImmediate := map[string]bool{}
	for _, f := range got.Followups {
		if f.Priority == PriorityImmediate {
			gotImmediate[normGap(f.Gap)] = true
		}
	}
	for _, f := range gold.Followups {
		if f.Priority != PriorityImmediate {
			continue
		}
		rep.ImmediateGold++
		if gotImmediate[normGap(f.Gap)] {
			rep.ImmediateHit++
		}
	}
	sort.Strings(rep.Missed)
	sort.Strings(rep.Extra)
	return rep
}

// normStmt is a surface-insensitive key for a fact: subject role + predicate +
// lowercased value/object. Subject/object are compared by role rather than handle,
// since two runs may number handles differently.
func normStmt(s Statement) string {
	obj := s.Object
	if obj == "" {
		obj = strings.ToLower(strings.TrimSpace(s.Value))
	}
	return strings.ToLower(s.Predicate) + "=" + obj
}

func normGap(g string) string {
	// "e1.name" -> "name": compare the attribute being sought, not the handle.
	if i := strings.LastIndex(g, "."); i >= 0 {
		return strings.ToLower(g[i+1:])
	}
	return strings.ToLower(g)
}

// String renders a one-block human summary.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "statements: %d/%d found", r.StmtFound, r.StmtGold)
	if r.StmtExtra > 0 {
		fmt.Fprintf(&b, ", %d extra", r.StmtExtra)
	}
	fmt.Fprintf(&b, "; immediate follow-ups: %d/%d flagged", r.ImmediateHit, r.ImmediateGold)
	for _, m := range r.Missed {
		fmt.Fprintf(&b, "\n  MISSED: %s", m)
	}
	for _, e := range r.Extra {
		fmt.Fprintf(&b, "\n  EXTRA:  %s", e)
	}
	return b.String()
}
