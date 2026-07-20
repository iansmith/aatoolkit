// Column formatting for the status table (SOP-19 / design/aa-server-status.md
// §8). Colors use raw stdlib ANSI escapes only — no color dependency.
package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxNameLen = 16

var statusColumns = []string{"SERVER", "TYPE", "DESIRED", "STATE", "PORTS", "PID", "HEALTH"}

func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}

func padRight(s string, width int) string {
	pad := width - visibleLen(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func formatRow(s ServerStatus) []string {
	name := s.Name
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return []string{
		name,
		string(s.Type),
		formatDesired(s.Enabled),
		formatStateCell(s),
		formatPorts(s.Ports),
		formatPID(s.PID),
		formatHealth(s.Health),
	}
}

func printTable(out io.Writer, rows [][]string) {
	// Lead with a newline: the caller may have left the cursor mid-line (a
	// prompt, a partial write), and a header column that starts at an
	// arbitrary offset misaligns against every row beneath it.
	fmt.Fprintln(out)

	widths := make([]int, len(statusColumns))
	for i, h := range statusColumns {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if vl := visibleLen(cell); vl > widths[i] {
				widths[i] = vl
			}
		}
	}
	for i, h := range statusColumns {
		if i > 0 {
			fmt.Fprint(out, "  ")
		}
		if i < len(statusColumns)-1 {
			fmt.Fprint(out, padRight(h, widths[i]))
		} else {
			fmt.Fprint(out, h)
		}
	}
	fmt.Fprintln(out)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(out, "  ")
			}
			if i < len(row)-1 {
				fmt.Fprint(out, padRight(cell, widths[i]))
			} else {
				fmt.Fprint(out, cell)
			}
		}
		fmt.Fprintln(out)
	}
}

const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[32m" // up
	ansiDim    = "\x1b[2m"  // down / disabled
	ansiYellow = "\x1b[33m" // stale, owned-disabled
	ansiRed    = "\x1b[31m" // stray / partial / extra-listener / foreign-conflict / blocked
)

func colorize(code, text string) string {
	return code + text + ansiReset
}

// redStates are the STATE keywords the ticket calls out as anomalies —
// always red, regardless of AnomalyDetail.
var redStates = map[ServerState]bool{
	StateStray:           true,
	StatePartial:         true,
	StateExtraListener:   true,
	StateForeignConflict: true,
	StateBlocked:         true, // inferred from anomaly pattern, not in ticket's explicit color list
}

// colorForState returns the ANSI code for a plain (non-overridden) state's
// color, or "" if State isn't one of the five colored classifications (e.g.
// the stub engine's "unknown" placeholder, which is passed through
// uncolored rather than guessed at).
func colorForState(state ServerState) string {
	switch {
	case state == StateUp:
		return ansiGreen
	case state == StateDown || state == StateDisabled:
		return ansiDim
	case redStates[state]:
		return ansiRed
	default:
		return ""
	}
}

// formatStateCell renders the STATE column's full display text (including
// any anomaly parenthetical) wrapped in the color the ticket specifies.
//
// Precedence, outermost first:
//  1. Stale — always "STALE" yellow, regardless of every other field: a
//     stale observation can't be trusted enough to show anything else.
//  2. Owned-disabled — only when State is up AND the server is declared
//     disabled AND we're the ones who started it (`<name> up`): yellow
//     "up (disabled)", never red STRAY (STRAY is reserved for foreign
//     processes in the same up-while-disabled situation).
//  3. Per-state color table (colorForState) — up green; down/disabled dim;
//     the anomaly states red, with AnomalyDetail appended in parens for
//     STRAY/BLOCKED; unrecognized states pass through uncolored.
func formatStateCell(s ServerStatus) string {
	if s.Stale {
		return colorize(ansiYellow, "stale")
	}
	if s.State == StateUp && !s.Enabled && s.OwnedDisabled {
		return colorize(ansiYellow, "up (disabled)")
	}

	text := string(s.State)
	if s.AnomalyDetail != "" && (s.State == StateStray || s.State == StateBlocked) {
		text = fmt.Sprintf("%s (%s)", text, s.AnomalyDetail)
	}

	if color := colorForState(s.State); color != "" {
		return colorize(color, text)
	}
	return text
}

// formatPorts renders the PORTS column: each declared port as "<port> ✓" or
// "<port> ✗" depending on whether it's actually listening, with any
// unexpected extra listener appended as "+<port> ✗unexpected".
func formatPorts(ports []PortStatus) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Unexpected {
			parts = append(parts, fmt.Sprintf("+%d ✗unexpected", p.Port))
			continue
		}
		symbol := "✗"
		if p.Up {
			symbol = "✓"
		}
		parts = append(parts, fmt.Sprintf("%d %s", p.Port, symbol))
	}
	return strings.Join(parts, " ")
}

// formatDesired renders the DESIRED column: what the config declares this
// server should be — "up" for enabled, "down" for disabled.
func formatDesired(enabled bool) string {
	if enabled {
		return "up"
	}
	return "down"
}

// formatPID renders the PID column. 0 (the stub's placeholder for "not yet
// observed") renders as "-" rather than a misleading literal "0".
func formatPID(pid int) string {
	if pid == 0 {
		return "-"
	}
	return strconv.Itoa(pid)
}

// formatHealth renders the HEALTH column. Empty means "not probed yet" (the
// stub engine's placeholder); once wired, this is expected to already be in
// the "path code" form (internal/health.Result.Rendered produces exactly
// that, e.g. "/v1/models 200").
func formatHealth(health string) string {
	if health == "" {
		return "-"
	}
	return health
}
