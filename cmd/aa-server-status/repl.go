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
//
// in is the caller's ALREADY-BUFFERED stdin, shared with the engine's
// [server.prompt] path rather than wrapped again here. That sharing is the
// whole point (AATK-29): a second buffer over the same stream reads ahead of
// the first, so a prompt answer and the next command end up in whichever
// buffer happened to grab them. One reader, one buffer, no race for input.
func Run(in *bufio.Reader, out io.Writer, engine Engine) error {
	// Before the first prompt, so the table on entry reflects the config as
	// it is on disk right now — not as it was when main loaded it.
	engine.ReloadConfigIfChanged(out)
	printStatus(out, engine.Status())

	for {
		fmt.Fprint(out, prompt)

		// ReadString returns a final line lacking its newline TOGETHER with
		// io.EOF, unlike Scanner which yielded the token first and reported
		// EOF on the next call. Dispatching only on err == nil would silently
		// drop that last command.
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			teardown(out, engine)
			if err == io.EOF {
				return nil
			}
			return err
		}
		lastLine := err != nil

		cmd, parseErr := ParseCommand(strings.TrimRight(line, "\r\n"))
		switch {
		case parseErr != nil:
			fmt.Fprintf(out, "error: %v\n", parseErr)
		case isExitVerb(cmd.Verb):
			teardown(out, engine)
			return nil
		default:
			// Before dispatch, not after: the command about to run is the one
			// that should see the operator's latest edit.
			engine.ReloadConfigIfChanged(out)
			dispatch(out, engine, cmd)
		}

		if lastLine {
			teardown(out, engine)
			if err == io.EOF {
				return nil
			}
			return err
		}
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
	VerbUp:     {"up", "starting", Engine.Up},
	VerbDown:   {"down", "stopping", Engine.Down},
	VerbDead:   {"dead", "reaping", Engine.Dead},
	VerbBuild:  {"build", "building", Engine.Build},
	VerbBounce: {"bounce", "bouncing", Engine.Bounce},
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
		handleStatus(out, engine, cmd)
	case VerbLogs:
		handleLogs(out, engine, cmd)
	case VerbKill:
		handleKill(out, engine, cmd)
	case VerbCommand:
		handleCommand(out, engine, cmd)
	case VerbView:
		handleView(out, engine, cmd)
	case VerbHelp:
		printHelp(out)
	default:
		fmt.Fprintf(out, "error: unhandled command %+v\n", cmd)
	}
}

func handleStatus(out io.Writer, engine Engine, cmd Command) {
	if cmd.Target == "" {
		printStatus(out, engine.Status())
		return
	}
	printSingleStatus(out, engine.Status(), cmd.Target)
}

func handleLogs(out io.Writer, engine Engine, cmd Command) {
	lines, err := engine.Logs(cmd.Target)
	if err != nil {
		fmt.Fprintf(out, "error: logs %s: %v\n", cmd.Target, err)
		return
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
}

func handleKill(out io.Writer, engine Engine, cmd Command) {
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
}

func handleCommand(out io.Writer, engine Engine, cmd Command) {
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
}

func handleView(out io.Writer, engine Engine, cmd Command) {
	lines, err := engine.View(cmd.Target, cmd.Modifier == "nowrap")
	if err != nil {
		fmt.Fprintf(out, "error: view %s: %v\n", cmd.Target, err)
		return
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
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
	fmt.Fprintln(out, "  <name> bounce       take one server down, then straight back up")
	fmt.Fprintln(out, "  (bare Enter)        show status")
}
