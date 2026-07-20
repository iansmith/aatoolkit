package factdb

import (
	"sort"
	"strconv"
	"strings"
)

// Compile turns an extraction Result into deterministic openCypher for Apache AGE.
//
// The output is the inner Cypher (the body you'd wrap in
// `SELECT * FROM cypher('graph', $$ ... $$)`), one statement per line, so it is
// easy to eyeball and to golden-test. Design choices, all from
// docs/fact-database-research.md §7:
//
//   - MERGE (never CREATE) on a stable key `k`, so replaying a conversation is
//     idempotent and never duplicates a person node.
//   - Facts are reified as Statement nodes carrying provenance/confidence/temporal
//     properties, wired subject-[:SUBJECT_OF]->stmt-[:ABOUT]->object.
//   - Deterministic ordering (entities by handle, statements by key) and escaped
//     string literals, so the output is reproducible and diff-friendly.
//
// Follow-ups are emitted as trailing comments, not graph writes: they are the caller's
// question queue, not facts about the world.
func Compile(r *Result) string {
	if r == nil {
		return ""
	}
	var b strings.Builder

	// Entities, ordered by handle.
	ents := append([]Entity(nil), r.Entities...)
	sort.Slice(ents, func(i, j int) bool { return ents[i].Handle < ents[j].Handle })
	b.WriteString("// entities\n")
	for _, e := range ents {
		b.WriteString("MERGE (")
		b.WriteString(e.Handle)
		b.WriteString(":")
		b.WriteString(e.Type)
		b.WriteString(" {k:")
		b.WriteString(q(entityKey(e)))
		b.WriteString("})")

		// SET clause: mention, resolution, name (if any), then sorted attrs.
		sets := []string{
			e.Handle + ".mention=" + q(e.Mention),
			e.Handle + ".resolution=" + q(e.Resolution),
		}
		if e.Name != "" {
			sets = append(sets, e.Handle+".name="+q(e.Name))
		}
		for _, k := range sortedKeys(e.Attrs) {
			sets = append(sets, e.Handle+"."+k+"="+q(e.Attrs[k]))
		}
		b.WriteString(" SET ")
		b.WriteString(strings.Join(sets, ", "))
		b.WriteString(";\n")
	}

	// Statements, ordered by their key.
	stmts := append([]Statement(nil), r.Statements...)
	sort.Slice(stmts, func(i, j int) bool { return statementKey(stmts[i]) < statementKey(stmts[j]) })
	b.WriteString("// statements\n")
	for _, s := range stmts {
		sk := statementKey(s)
		svar := "s_" + s.Subject + "_" + sanitize(s.Predicate)
		b.WriteString("MERGE (")
		b.WriteString(s.Subject)
		b.WriteString(")-[:SUBJECT_OF]->(")
		b.WriteString(svar)
		b.WriteString(":Statement {k:")
		b.WriteString(q(sk))
		b.WriteString("})")

		sets := []string{svar + ".predicate=" + q(s.Predicate)}
		if s.Value != "" {
			sets = append(sets, svar+".value="+q(s.Value))
		}
		if s.ValidFrom != "" {
			sets = append(sets, svar+".valid_from="+q(s.ValidFrom))
		}
		if s.ValidUntil != "" {
			sets = append(sets, svar+".valid_until="+q(s.ValidUntil))
		}
		if s.Recurrence != "" {
			sets = append(sets, svar+".recurrence="+q(s.Recurrence))
		}
		if s.Confidence != 0 {
			sets = append(sets, svar+".confidence="+strconv.FormatFloat(s.Confidence, 'g', -1, 64))
		}
		if s.SourceSpan != "" {
			sets = append(sets, svar+".source_span="+q(s.SourceSpan))
		}
		b.WriteString(" SET ")
		b.WriteString(strings.Join(sets, ", "))
		b.WriteString(";\n")

		if s.Object != "" {
			b.WriteString("MERGE (")
			b.WriteString(svar)
			b.WriteString(")-[:ABOUT]->(")
			b.WriteString(s.Object)
			b.WriteString(");\n")
		}
	}

	// Follow-ups: comments only — the caller's question queue, not world facts.
	fus := append([]Followup(nil), r.Followups...)
	sort.Slice(fus, func(i, j int) bool { return fus[i].Gap < fus[j].Gap })
	b.WriteString("// followups (not written to graph; shown for review)\n")
	for _, f := range fus {
		b.WriteString("// [")
		b.WriteString(f.Priority)
		b.WriteString("] ")
		b.WriteString(f.Gap)
		b.WriteString(" — ")
		b.WriteString(f.Question)
		b.WriteString("\n")
	}

	return b.String()
}

// entityKey is the stable MERGE key: the canonical name when resolved, else a
// type+mention key so a placeholder stays a single node across a conversation.
func entityKey(e Entity) string {
	if e.Resolution == ResolutionResolved && e.Name != "" {
		return e.Name
	}
	return e.Type + ":" + e.Mention
}

// statementKey uniquely and stably identifies a reified fact so identical facts
// MERGE onto one node.
func statementKey(s Statement) string {
	return s.Subject + "|" + s.Predicate + "|" + s.Value + "|" + s.Object
}

// q renders a Cypher single-quoted string literal, escaping backslashes and quotes.
func q(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// sanitize makes a predicate safe as part of a Cypher variable name.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
