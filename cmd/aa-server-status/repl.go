package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const prompt = "aa-server-status> "

// Run drives the REPL loop: prints the status table once on entry, then
// repeatedly prompts, reads a line, parses and dispatches it, and prints
// the result — until quit/exit/bye or EOF (Ctrl-D), at which point it
// tears down everything the supervisor owns and returns.
//
// Run never returns a non-nil error for normal REPL operation (bad
// commands and stub "not implemented" verb errors are printed to out, not
// returned) — a non-nil error return is reserved for I/O failures on in.
func Run(in io.Reader, out io.Writer, engine Engine) error {
	printStatus(out, engine.Status())

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, prompt)
		if !scanner.Scan() {
			teardown(out, engine)
			return scanner.Err()
		}

		line := scanner.Text()
		cmd, err := ParseCommand(line)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			continue
		}

		if isExitVerb(cmd.Verb) {
			teardown(out, engine)
			return nil
		}

		dispatch(out, engine, cmd)
	}
}

func isExitVerb(v Verb) bool {
	return v == VerbQuit || v == VerbExit || v == VerbBye
}

// teardown tears down everything the supervisor owns. This is the ONLY
// path that reaches Engine.TeardownAll — it must never be reachable via
// the "down" verb (enabled-only) or "dead" (also reaps foreign strays).
func teardown(out io.Writer, engine Engine) {
	names := engine.TeardownAll()
	fmt.Fprintf(out, "tearing down %d owned server(s): %v\n", len(names), names)
}

// actionVerbs are the verbs that map straight to a same-shaped
// Engine method (name in, error out) — collapsed into one table so
// dispatch doesn't need one near-identical case per verb.
var actionVerbs = map[Verb]struct {
	label       string
	progressive string
	call        func(Engine, string) error
}{
	VerbUp:    {"up", "starting", Engine.Up},
	VerbDown:  {"down", "stopping", Engine.Down},
	VerbDead:  {"dead", "reaping", Engine.Dead},
	VerbBuild: {"build", "building", Engine.Build},
}

func dispatch(out io.Writer, engine Engine, cmd Command) {
	if action, ok := actionVerbs[cmd.Verb]; ok {
		fmt.Fprintf(out, "%s %s...\n", action.progressive, cmd.Target)
		if err := action.call(engine, cmd.Target); err != nil {
			fmt.Fprintf(out, "error: %s %s: %v\n", action.label, cmd.Target, err)
			return
		}
		fmt.Fprintf(out, "%s %s: ok\n", action.label, cmd.Target)
		return
	}

	switch cmd.Verb {
	case VerbStatus:
		if cmd.Target == "" {
			printStatus(out, engine.Status())
			return
		}
		printSingleStatus(out, engine.Status(), cmd.Target)
	case VerbLogs:
		lines, err := engine.Logs(cmd.Target)
		if err != nil {
			fmt.Fprintf(out, "error: logs %s: %v\n", cmd.Target, err)
			return
		}
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
	case VerbKill:
		pid, err := strconv.Atoi(cmd.Target)
		if err != nil || pid <= 0 {
			fmt.Fprintf(out, "error: kill: invalid PID %q\n", cmd.Target)
			return
		}
		if err := engine.Kill(pid); err != nil {
			fmt.Fprintf(out, "error: kill %s: %v\n", cmd.Target, err)
			return
		}
		fmt.Fprintf(out, "killed %s\n", cmd.Target)
	case VerbCommand:
		command, args, err := engine.Command(cmd.Target)
		if err != nil {
			fmt.Fprintf(out, "error: command %s: %v\n", cmd.Target, err)
			return
		}
		if len(args) > 0 {
			fmt.Fprintf(out, "%s %s\n", command, strings.Join(args, " "))
		} else {
			fmt.Fprintln(out, command)
		}
	case VerbView:
		lines, err := engine.View(cmd.Target, cmd.Modifier == "nowrap")
		if err != nil {
			fmt.Fprintf(out, "error: view %s: %v\n", cmd.Target, err)
			return
		}
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
	case VerbHelp:
		printHelp(out)
	default:
		fmt.Fprintf(out, "error: unhandled command %+v\n", cmd)
	}
}

func printStatus(out io.Writer, statuses []ServerStatus) {
	rows := make([][]string, len(statuses))
	for i, s := range statuses {
		rows[i] = formatRow(s)
	}
	printTable(out, rows)
}

func printSingleStatus(out io.Writer, statuses []ServerStatus, name string) {
	for _, s := range statuses {
		if s.Name == name {
			printTable(out, [][]string{formatRow(s)})
			return
		}
	}
	fmt.Fprintf(out, "error: no such server %q\n", name)
}

func printHelp(out io.Writer) {
	fmt.Fprintln(out, "commands:")
	fmt.Fprintln(out, "  status | up | down | dead | build | logs <name> | help | quit | exit | bye")
	fmt.Fprintln(out, "  kill <pid>          send SIGTERM to a process by PID")
	fmt.Fprintln(out, "  command <name>      print the launch command line for a server")
	fmt.Fprintln(out, "  <name>              show one server's status")
	fmt.Fprintln(out, "  <name> view [nowrap]   show last 50 lines of server log")
	fmt.Fprintln(out, "  <name> up|down|build   act on one server")
	fmt.Fprintln(out, "  (bare Enter)        show status")
}
