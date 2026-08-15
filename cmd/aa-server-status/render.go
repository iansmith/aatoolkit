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

// quietDisabled reports whether s is a disabled server with nothing observed
// on it — the only case the status table suppresses (AATK-94). A fleet that
// has switched to speech-to-speech leaves its old servers disabled
// permanently, and their rows are noise in every render from then on.
//
// The test is observed quiet, not Enabled alone, and that distinction is the
// whole design. A disabled server whose port is quietly in use is exactly what
// the old always-render behaviour was protecting against, and it already
// renders as red STRAY; hiding it would trade one silent failure for another.
// So every clause below is a way of *not* being quiet, and each has its own
// test in repl_test.go:
//
//   - a declared port is listening, or something is listening outside the
//     declared set (the STRAY and extra-listener cases);
//   - a PID is still tracked, so something is running even with no port up;
//   - the state is anything but the ordinary down/disabled — an anomaly is
//     never suppressed, whatever produced it;
//   - the observation is stale, where "we could not look" must not be read as
//     "we looked and it was quiet".
//
// Callers apply this to the whole-table render only. printSingleStatus asks
// for one row by name, which is an explicit request for that row — putting
// this check inside formatRow instead would silently break it.
func quietDisabled(s ServerStatus) bool {
	if s.Enabled || s.Stale || s.PID != 0 {
		return false
	}
	if s.State != StateDown && s.State != StateDisabled {
		return false
	}
	for _, p := range s.Ports {
		if p.Up || p.Unexpected {
			return false
		}
	}
	return true
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

// anomalyDetailStates are the STATE values for which a non-empty
// AnomalyDetail is actually rendered in the parenthetical. STRAY/BLOCKED are
// the ticket's original two; StatePartial, StateUp and StateExtraListener
// were added for AATK-87 F2/F3, which are exactly the three states
// statusForLocked's owned-server branch can pair an AnomalyDetail with:
// nothing could be confirmed at all (classifyOwned renders StatePartial), a
// declared port is confirmed while another tree member's read was ambiguous
// (StateUp), or that same ambiguity coexists with a genuine stray port
// (StateExtraListener). A state not in this list still ignores
// AnomalyDetail even if one happens to be set.
var anomalyDetailStates = map[ServerState]bool{
	StateStray:         true,
	StateBlocked:       true,
	StatePartial:       true,
	StateUp:            true,
	StateExtraListener: true,
}

// withAnomalyDetail appends s.AnomalyDetail to text as a parenthetical when
// the detail is non-empty and s.State is one the allow-list renders it for.
// Both of formatStateCell's return paths that can carry a detail go through
// here, so the allow-list is consulted in exactly one place — the
// owned-disabled path used to return before reaching it, silently dropping
// the AATK-87 F2 warning for a server started imperatively via `<name> up`.
func withAnomalyDetail(s ServerStatus, text string) string {
	if s.AnomalyDetail != "" && anomalyDetailStates[s.State] {
		return fmt.Sprintf("%s (%s)", text, s.AnomalyDetail)
	}
	return text
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
//     processes in the same up-while-disabled situation). It still carries
//     any AnomalyDetail: this path is reachable with the AATK-87 F2 detail
//     set (classifyOwned returns StateUp, then statusForLocked sets
//     OwnedDisabled on the very next line), and swallowing the warning
//     there would report an unconfirmed observation as a confident verdict
//     for exactly the imperatively-started servers.
//  3. Per-state color table (colorForState) — up green; down/disabled dim;
//     the anomaly states red, with AnomalyDetail appended in parens for
//     STRAY/BLOCKED and (AATK-87 F2/F3) for an owned server's
//     up/partial/extra-listener state when this cycle's observation
//     couldn't be fully confirmed.
func formatStateCell(s ServerStatus) string {
	if s.Stale {
		return colorize(ansiYellow, "stale")
	}
	if s.State == StateUp && !s.Enabled && s.OwnedDisabled {
		return colorize(ansiYellow, withAnomalyDetail(s, "up (disabled)"))
	}

	text := withAnomalyDetail(s, string(s.State))

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
// stub engine's placeholder); anything else is passed through verbatim,
// already rendered by internal/health.Result.Rendered — "/v1/models 200" for
// an HTTP check, "db-probe exit 0" for an exec one. Nothing here parses it,
// so a new check form needs no change on this side.
func formatHealth(health string) string {
	if health == "" {
		return "-"
	}
	return health
}
