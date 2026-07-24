package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/iansmith/aatoolkit/config"
)

// askPrompts resolves every target's [server.prompt] before any of them
// launches, returning the targets with the chosen branch's args appended.
//
// Sequencing is the whole point. Up launches its targets on concurrent
// goroutines, so asking inside the per-target path would put two readers on one
// stdin: interleaved questions, and an answer landing against whichever server
// happened to win the race. Collecting every answer first makes that
// impossible by construction rather than by careful locking.
//
// The chosen args are appended to the target's own Args field rather than
// threaded separately down to the launcher. config.Server travels by value, so
// this mutates only the copy about to be launched, and the args then flow
// through the real LaunchExec/LaunchSource — keeping their type-specific
// pre-work (source's absolute-path resolution, python's preflight) intact,
// which a hand-rolled "resolve and launch generically" path would have
// silently dropped.
func (e *RealEngine) askPrompts(targets []config.Server) ([]config.Server, error) {
	if !slices.ContainsFunc(targets, func(s config.Server) bool { return s.Prompt != nil }) {
		return targets, nil
	}
	// Both streams are checked, not just the input one: a nil io.Writer is
	// not a no-op writer, it panics on the first Fprintf. Refusing loudly
	// here turns a latent nil-deref deep in the launch path into a clear
	// error naming the server that needed asking.
	if e.promptIn == nil || e.promptOut == nil {
		return nil, fmt.Errorf("server %q declares a prompt but this engine has no input/output streams to ask on",
			firstPromptedName(targets))
	}

	resolved := make([]config.Server, len(targets))
	copy(resolved, targets)
	for i, s := range resolved {
		if s.Prompt == nil {
			continue
		}
		yes, err := askYesNo(e.promptIn, e.promptOut, s.Prompt.Question)
		if err != nil {
			return nil, fmt.Errorf("up %s: %w", s.Name, err)
		}
		extra := s.Prompt.NoArgs
		if yes {
			extra = s.Prompt.YesArgs
		}
		resolved[i].Args = append(append([]string{}, s.Args...), extra...)
	}
	return resolved, nil
}

func firstPromptedName(targets []config.Server) string {
	for _, s := range targets {
		if s.Prompt != nil {
			return s.Name
		}
	}
	return ""
}

// askYesNo prints question and reads one line, accepting y/yes/n/no in any
// case. Anything else re-asks rather than erroring or picking a default: the
// operator is choosing how a server launches, and guessing at a typo is worse
// than asking again.
//
// EOF is a hard error, not another loop. A closed stream can never produce an
// answer, so re-asking would spin forever and hang the supervisor.
func askYesNo(in *bufio.Reader, out io.Writer, question string) (bool, error) {
	for {
		fmt.Fprintf(out, "%s [y/n] ", question)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return false, fmt.Errorf("reading answer to %q: %w", question, err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if err != nil {
			// Input ended on an unusable final answer; re-asking cannot help.
			return false, fmt.Errorf("reading answer to %q: input ended on an invalid answer", question)
		}
		fmt.Fprintf(out, "please answer y or n\n")
	}
}
