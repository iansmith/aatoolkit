package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Verb is a recognized REPL command verb.
type Verb int

const (
	VerbStatus Verb = iota
	VerbUp
	VerbDown
	VerbDead
	VerbBuild
	VerbLogs
	VerbHelp
	VerbKill
	VerbCommand
	VerbView
	VerbBounce
	VerbQuit
	VerbExit
	VerbBye
)

// Command is the parsed result of one REPL input line: a verb plus an
// optional target server name (empty for global verbs and bare status).
type Command struct {
	Verb     Verb
	Target   string
	Modifier string
}

// globalVerbs maps the bare-verb tokens to their Verb value. Per-server
// verbs (a subset: up, down, build) are also recognized as the SECOND
// token of a two-token "<name> <verb>" line — see ParseCommand.
var globalVerbs = map[string]Verb{
	"status": VerbStatus,
	"up":     VerbUp,
	"down":   VerbDown,
	"dead":   VerbDead,
	"build":  VerbBuild,
	"help":   VerbHelp,
	"quit":   VerbQuit,
	"exit":   VerbExit,
	"bye":    VerbBye,
}

// perServerVerbs are the verbs valid in "<name> <verb>" form.
var perServerVerbs = map[string]Verb{
	"up":    VerbUp,
	"down":  VerbDown,
	"build": VerbBuild,
	"view":  VerbView,
}

// ParseCommand parses one REPL input line into a Command. Recognizes:
//   - "" / whitespace-only          -> bare status
//   - a bare global verb            -> that verb, no target
//   - "logs <name>"                 -> VerbLogs with target (name required)
//   - "<name>"                      -> VerbStatus with target (per-server status)
//   - "<name> up|down|build"        -> that verb with target
//
// Anything else — including extra trailing tokens on any of the above
// forms — is a loud parse error naming the offending input.
func ParseCommand(line string) (Command, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return Command{Verb: VerbStatus}, nil
	}

	first := fields[0]

	if first == "logs" {
		if len(fields) != 2 {
			return Command{}, fmt.Errorf("unknown command %q: 'logs' requires exactly one server name", line)
		}
		return Command{Verb: VerbLogs, Target: fields[1]}, nil
	}

	if first == "kill" {
		if len(fields) != 2 {
			return Command{}, fmt.Errorf("unknown command %q: 'kill' requires exactly one PID", line)
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			return Command{}, fmt.Errorf("unknown command %q: PID must be a positive integer", line)
		}
		return Command{Verb: VerbKill, Target: fields[1]}, nil
	}

	if first == "command" {
		if len(fields) != 2 {
			return Command{}, fmt.Errorf("unknown command %q: 'command' requires exactly one server name", line)
		}
		return Command{Verb: VerbCommand, Target: fields[1]}, nil
	}

	if len(fields) == 1 {
		if verb, ok := globalVerbs[first]; ok {
			return Command{Verb: verb}, nil
		}
		// Bare name -> per-server status.
		return Command{Verb: VerbStatus, Target: first}, nil
	}

	if len(fields) == 2 {
		if verb, ok := perServerVerbs[fields[1]]; ok {
			return Command{Verb: verb, Target: first}, nil
		}
		return Command{}, fmt.Errorf("unknown command %q: %q is not a valid verb for %q (want up|down|build|view)", line, fields[1], first)
	}

	if len(fields) == 3 {
		if verb, ok := perServerVerbs[fields[1]]; ok && verb == VerbView {
			if fields[2] != "nowrap" {
				return Command{}, fmt.Errorf("unknown command %q: %q is not a valid modifier for view (want nowrap)", line, fields[2])
			}
			return Command{Verb: VerbView, Target: first, Modifier: "nowrap"}, nil
		}
	}

	return Command{}, fmt.Errorf("unknown command %q: too many tokens", line)
}
