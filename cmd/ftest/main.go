// Command ftest probes how well a local LLM extracts storable facts from everyday
// conversation, against a caller-supplied ontology (predicate vocabulary + entity
// types + system prompt).
//
//	ftest record [-ontology f.json] [-o out.json] [-name NAME]   interactive:
//	                                           converse, extract each turn, and
//	                                           write a reusable fixture
//	ftest run [-ontology f.json] [-cypher] file.json...          replay fixtures
//	                                           through the model and, when a fixture
//	                                           has gold, grade it
//
// record generates the fixtures run consumes: type a user turn, see the facts the
// model pulled out, and on exit the whole conversation (turns + per-turn extraction)
// is saved. Add a hand-written "gold" block to a saved fixture to turn it into a
// scored test. -ontology defaults to a small built-in sample; point it at your own
// ontology JSON to probe a real vocabulary. The model endpoint comes from
// AATOOLKIT_FTEST_URL / AATOOLKIT_FTEST_MODEL.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iansmith/aatoolkit/factdb"
)

//go:embed sample_ontology.json
var sampleOntology []byte

// loadOntology reads the ontology at path, or falls back to the built-in sample
// when path is empty. The sample lets ftest run out of the box; a real probe
// points -ontology at its own vocabulary + system prompt.
func loadOntology(path string) (factdb.Ontology, error) {
	if path != "" {
		return factdb.LoadOntology(path)
	}
	var o factdb.Ontology
	if err := json.Unmarshal(sampleOntology, &o); err != nil {
		return factdb.Ontology{}, fmt.Errorf("built-in sample ontology is corrupt: %w", err)
	}
	return o, nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "record":
		os.Exit(cmdRecord(os.Args[2:]))
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ftest — probe LLM fact extraction against an ontology

usage:
  ftest record [-ontology f.json] [-o out.json] [-name NAME]   converse, write a fixture
  ftest run [-ontology f.json] [-cypher] file.json ...         replay fixtures, grade

-ontology defaults to a small built-in sample; pass your own to probe a real vocabulary.
env: AATOOLKIT_FTEST_URL, AATOOLKIT_FTEST_MODEL select the model endpoint.
`)
}

// cmdRecord runs an interactive session. Each user line is extracted incrementally
// (with all prior turns + known handles as context); prefix a line with "a:" to add
// an assistant turn for realistic context. "/done" or EOF writes the fixture.
func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	out := fs.String("o", "", "output fixture path (default: <name>.json)")
	name := fs.String("name", "conversation", "fixture name")
	ontPath := fs.String("ontology", "", "ontology JSON (default: built-in sample)")
	fs.Parse(args)

	ont, err := loadOntology(*ontPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ontology: %v\n", err)
		return 2
	}
	x := factdb.NewLLMExtractor(ont)
	path := *out
	if path == "" {
		path = *name + ".json"
	}
	fix := &factdb.Fixture{Name: *name, Model: x.Model}

	fmt.Fprintf(os.Stderr, "recording to %s via %s\n", path, x.Model)
	fmt.Fprintln(os.Stderr, "type a user turn; 'a: text' for an assistant turn; '/done' to save & exit.")

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for {
		fmt.Fprint(os.Stderr, "you> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "/done" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "a:"); ok {
			fix.Turns = append(fix.Turns, factdb.Turn{Speaker: "assistant", Text: strings.TrimSpace(rest)})
			continue
		}

		known := factdb.KnownEntities(fix.Turns)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		res, err := x.Extract(ctx, fix.Turns, known, line)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  extract error: %v\n", err)
			continue
		}
		fix.Turns = append(fix.Turns, factdb.Turn{Speaker: "user", Text: line, Recorded: res})
		printResult(res)
	}

	if err := factdb.SaveFixture(path, fix); err != nil {
		fmt.Fprintf(os.Stderr, "save %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nsaved %d turns to %s. Add a \"gold\" block to grade it.\n", len(fix.Turns), path)
	return 0
}

// cmdRun replays each fixture's user turns through the model and prints the
// consolidated facts; with gold present, it grades. -cypher also prints the AGE
// openCypher the consolidated result compiles to.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	showCypher := fs.Bool("cypher", false, "print compiled AGE openCypher")
	ontPath := fs.String("ontology", "", "ontology JSON (default: built-in sample)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "run: need at least one fixture file")
		return 2
	}

	ont, err := loadOntology(*ontPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ontology: %v\n", err)
		return 2
	}
	x := factdb.NewLLMExtractor(ont)
	rc := 0
	for _, path := range fs.Args() {
		fix, err := factdb.LoadFixture(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			rc = 1
			continue
		}
		fmt.Printf("\n=== %s (%s) ===\n", fix.Name, path)

		var results []*factdb.Result
		var prior []factdb.Turn
		for _, t := range fix.Turns {
			if t.Speaker != "user" {
				prior = append(prior, t)
				continue
			}
			known := factdb.KnownEntities(prior)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			res, err := x.Extract(ctx, prior, known, t.Text)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  turn %q: %v\n", t.Text, err)
				rc = 1
				prior = append(prior, t)
				continue
			}
			fmt.Printf("\n> %s\n", t.Text)
			printResult(res)
			results = append(results, res)
			// Replay carries the freshly-extracted facts forward as context.
			ct := t
			ct.Recorded = res
			prior = append(prior, ct)
		}

		consolidated := factdb.Consolidate(results)
		if *showCypher {
			fmt.Printf("\n--- AGE openCypher ---\n%s", factdb.Compile(consolidated))
		}
		if fix.Gold != nil {
			rep := factdb.Grade(consolidated, fix.Gold)
			fmt.Printf("\n--- grade ---\n%s\n", rep.String())
		} else {
			fmt.Printf("\n(no gold block — not graded)\n")
		}
	}
	return rc
}

func printResult(r *factdb.Result) {
	b, _ := json.MarshalIndent(r, "  ", "  ")
	fmt.Printf("  %s\n", b)
}
