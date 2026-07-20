package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestPrintTable_StartsOnItsOwnLine: the table is printed at points where the
// cursor may sit mid-line (after a prompt, or a partial write), and a SERVER
// header starting at an arbitrary column misaligns against every row below it.
// Leading with a newline costs one blank line when already at the left edge
// and rescues the layout when not.
func TestPrintTable_StartsOnItsOwnLine(t *testing.T) {
	var out strings.Builder
	out.WriteString("aa-server-status> ") // cursor parked mid-line, as the REPL leaves it

	printTable(&out, [][]string{{"server", "source", "up", "up", "9730 ✓", "123", "/healthz 200"}})

	got := out.String()
	if !strings.HasPrefix(got, "aa-server-status> \n") {
		t.Fatalf("table did not break to a new line before the header; header would start mid-line:\n%q", got)
	}
	// The header must be the first thing on its own line, at column 0.
	lines := strings.Split(got, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "SERVER") {
		t.Fatalf("expected the SERVER header alone at the start of its line, got lines: %q", lines)
	}
}

// --- edge / boundary cases ---

func TestFormatPorts_EmptyRendersPlaceholder(t *testing.T) {
	if got := formatPorts(nil); got != "-" {
		t.Fatalf("expected placeholder for no ports, got %q", got)
	}
}

func TestFormatPorts_UnexpectedExtraListenerRendersExactAnnotation(t *testing.T) {
	got := formatPorts([]PortStatus{{Port: 8081, Unexpected: true}})
	if got != "+8081 ✗unexpected" {
		t.Fatalf("expected ticket's literal extra-listener annotation, got %q", got)
	}
}

func TestFormatPID_ZeroRendersPlaceholder(t *testing.T) {
	if got := formatPID(0); got != "-" {
		t.Fatalf("expected placeholder for PID 0 (not running), got %q", got)
	}
}

func TestFormatHealth_EmptyRendersPlaceholder(t *testing.T) {
	if got := formatHealth(""); got != "-" {
		t.Fatalf("expected placeholder for un-probed health, got %q", got)
	}
}

// --- error / rejection cases (nothing to "reject" here — formatters never
// error; these cover states that must NOT be colored red/green/yellow) ---

func TestFormatStateCell_UnrecognizedStateIsUncoloredPassthrough(t *testing.T) {
	// The stub engine reports "unknown" today (real reconciliation is a
	// downstream ticket) — it must not be misclassified into any of the
	// five defined colors.
	got := formatStateCell(ServerStatus{State: "unknown"})
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("expected an unrecognized state to render uncolored, got %q", got)
	}
	if got != "unknown" {
		t.Fatalf("expected unrecognized state text passed through verbatim, got %q", got)
	}
}

// --- cross-feature interaction ---

func TestFormatStateCell_StaleOverridesEverythingElse(t *testing.T) {
	// Stale must win even over an owned-disabled or anomaly state, since a
	// stale probe means none of those classifications can be trusted.
	got := formatStateCell(ServerStatus{
		State: StateStray, AnomalyDetail: "pid 9999, foreign", Stale: true,
	})
	if !strings.Contains(got, "stale") {
		t.Fatalf("expected STALE to override stray annotation, got %q", got)
	}
	if strings.Contains(got, "STRAY") {
		t.Fatalf("stale must suppress the stray annotation entirely, got %q", got)
	}
	if !strings.Contains(got, ansiYellow) {
		t.Fatalf("expected STALE rendered yellow, got %q", got)
	}
}

func TestFormatStateCell_OwnedDisabledBeatsRedStrayRule(t *testing.T) {
	// A disabled-in-config server that IS running because we started it
	// ourselves via `<name> up` is deliberate: yellow "up (disabled)", never
	// red STRAY (STRAY is reserved for foreign processes).
	got := formatStateCell(ServerStatus{State: StateUp, Enabled: false, OwnedDisabled: true})
	if !strings.Contains(got, "up (disabled)") {
		t.Fatalf("expected literal \"up (disabled)\" text, got %q", got)
	}
	if !strings.Contains(got, ansiYellow) {
		t.Fatalf("expected owned-disabled rendered yellow, got %q", got)
	}
	if strings.Contains(got, ansiRed) {
		t.Fatalf("owned-disabled must never render red, got %q", got)
	}
}

func TestFormatStateCell_ForeignUpWhileDisabledIsNotOwnedDisabled(t *testing.T) {
	// Same raw shape as the owned-disabled case (State up, Enabled false)
	// but NOT owned by us — this is a real anomaly and must NOT get the
	// yellow "up (disabled)" treatment.
	got := formatStateCell(ServerStatus{State: StateUp, Enabled: false, OwnedDisabled: false})
	if strings.Contains(got, "up (disabled)") {
		t.Fatalf("a foreign process must not be mistaken for owned-disabled, got %q", got)
	}
}

// --- happy path ---

func TestFormatStateCell_UpRendersGreenNoParens(t *testing.T) {
	got := formatStateCell(ServerStatus{State: StateUp, Enabled: true})
	if !strings.Contains(got, ansiGreen) {
		t.Fatalf("expected up rendered green, got %q", got)
	}
	if !strings.Contains(got, "up") {
		t.Fatalf("expected literal \"up\" text, got %q", got)
	}
	if strings.Contains(got, "(") {
		t.Fatalf("a plain up server (enabled, not owned-disabled) must have no parenthetical, got %q", got)
	}
}

// --- adversary gap tests (Step 0f) ---

func TestFormatStateCell_AnomalyDetailIgnoredForNonStrayNonBlockedStates(t *testing.T) {
	// AnomalyDetail is only meaningful alongside STRAY/BLOCKED per the
	// ticket's examples — a stray AnomalyDetail value set on some other
	// state (e.g. by a future caller bug) must not leak into the display.
	got := formatStateCell(ServerStatus{State: StatePartial, AnomalyDetail: "pid 1234, foreign"})
	if strings.Contains(got, "pid 1234") {
		t.Fatalf("expected AnomalyDetail to be ignored for partial state, got %q", got)
	}
}

func TestFormatStateCell_OwnedDisabledIgnoredWhenActuallyEnabled(t *testing.T) {
	// OwnedDisabled only means something when the server is declared
	// disabled (Enabled=false). If Enabled=true, State=up is just a normal
	// running server — a stray OwnedDisabled=true flag must not force the
	// "up (disabled)" text onto it.
	got := formatStateCell(ServerStatus{State: StateUp, Enabled: true, OwnedDisabled: true})
	if strings.Contains(got, "disabled") {
		t.Fatalf("OwnedDisabled must be a no-op when the server is actually enabled, got %q", got)
	}
	if !strings.Contains(got, ansiGreen) {
		t.Fatalf("expected plain green up, got %q", got)
	}
}

func TestFormatStateCell_StaleBeatsOwnedDisabledToo(t *testing.T) {
	// Stale is the outermost override — it must win even against the
	// owned-disabled special case, not just against anomaly states.
	got := formatStateCell(ServerStatus{State: StateUp, Enabled: false, OwnedDisabled: true, Stale: true})
	if !strings.Contains(got, "stale") {
		t.Fatalf("expected STALE to override owned-disabled too, got %q", got)
	}
	if strings.Contains(got, "disabled") {
		t.Fatalf("stale must suppress the owned-disabled text entirely, got %q", got)
	}
}

func TestFormatStateCell_DownRendersDim(t *testing.T) {
	got := formatStateCell(ServerStatus{State: StateDown})
	if !strings.Contains(got, ansiDim) {
		t.Fatalf("expected down rendered dim, got %q", got)
	}
}

func TestFormatStateCell_DisabledRendersDim(t *testing.T) {
	got := formatStateCell(ServerStatus{State: StateDisabled})
	if !strings.Contains(got, ansiDim) {
		t.Fatalf("expected disabled rendered dim, got %q", got)
	}
}

func TestFormatStateCell_StrayWithForeignDetailRendersTicketExample(t *testing.T) {
	got := formatStateCell(ServerStatus{State: StateStray, AnomalyDetail: "pid 9999, foreign"})
	if !strings.Contains(got, "STRAY (pid 9999, foreign)") {
		t.Fatalf("expected the ticket's exact STRAY annotation text, got %q", got)
	}
	if !strings.Contains(got, ansiRed) {
		t.Fatalf("expected STRAY rendered red, got %q", got)
	}
}

func TestFormatStateCell_BlockedWithDetailRendersTicketExample(t *testing.T) {
	got := formatStateCell(ServerStatus{State: StateBlocked, AnomalyDetail: "pid 7777 — not ours"})
	if !strings.Contains(got, "BLOCKED (pid 7777 — not ours)") {
		t.Fatalf("expected the ticket's exact BLOCKED annotation text, got %q", got)
	}
	if !strings.Contains(got, ansiRed) {
		t.Fatalf("expected BLOCKED rendered red, got %q", got)
	}
}

func TestFormatStateCell_PartialExtraListenerForeignConflictAllRenderRed(t *testing.T) {
	for _, st := range []ServerState{StatePartial, StateExtraListener, StateForeignConflict} {
		t.Run(string(st), func(t *testing.T) {
			got := formatStateCell(ServerStatus{State: st})
			if !strings.Contains(got, ansiRed) {
				t.Fatalf("expected %s rendered red, got %q", st, got)
			}
		})
	}
}

func TestFormatDesired_ReflectsConfigEnabled(t *testing.T) {
	if got := formatDesired(true); got != "up" {
		t.Fatalf("expected enabled server's DESIRED to be \"up\", got %q", got)
	}
	if got := formatDesired(false); got != "down" {
		t.Fatalf("expected disabled server's DESIRED to be \"down\", got %q", got)
	}
}

func TestFormatPorts_MixOfUpDownAndUnexpected(t *testing.T) {
	got := formatPorts([]PortStatus{
		{Port: 8080, Up: true},
		{Port: 8082, Up: false},
		{Port: 8081, Unexpected: true},
	})
	if !strings.Contains(got, "8080 ✓") {
		t.Fatalf("expected declared up port checkmark, got %q", got)
	}
	if !strings.Contains(got, "8082 ✗") {
		t.Fatalf("expected declared down port cross, got %q", got)
	}
	if !strings.Contains(got, "+8081 ✗unexpected") {
		t.Fatalf("expected unexpected port annotation, got %q", got)
	}
}

func TestFormatPID_NonZeroRendersDecimal(t *testing.T) {
	if got := formatPID(4242); got != strconv.Itoa(4242) {
		t.Fatalf("expected decimal PID, got %q", got)
	}
}

func TestFormatHealth_NonEmptyPassesThroughPathCodeForm(t *testing.T) {
	if got := formatHealth("/v1/models 200"); got != "/v1/models 200" {
		t.Fatalf("expected health string passed through as-is, got %q", got)
	}
}
