package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// viewTestCfg builds a minimal config with a single "svc" server and a temp
// log directory. Tests write their own log files under the temp dir.
func viewTestCfg(t *testing.T) (config.Config, string) {
	t.Helper()
	logDir := t.TempDir()
	cfg := config.Config{
		Supervisor: config.Supervisor{LogDir: logDir},
		Servers:    []config.Server{{Name: "svc", Type: config.TypeExec, Enabled: true}},
	}
	return cfg, logDir
}

// writeViewLog writes content to a correctly-named log file for server name
// in logDir. Uses the fixed timestamp "2026-07-07-12-00-00" so NewestLog finds
// it (the filename must match the exact shape "<name>-<19-char-ts>.log").
func writeViewLog(t *testing.T, logDir, name, content string) string {
	t.Helper()
	path := filepath.Join(logDir, name+"-2026-07-07-12-00-00.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeViewLog: %v", err)
	}
	return path
}

// --- edge/boundary ---------------------------------------------------------

// Fewer than 50 lines → returns all lines, no panic.
func TestRealEngine_View_FewerThan50Lines(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	writeViewLog(t, logDir, "svc", "line 0\nline 1\nline 2\n")
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
}

// Exactly 50 lines → returns all 50.
func TestRealEngine_View_Exactly50Lines(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	var sb strings.Builder
	for i := range 50 {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeViewLog(t, logDir, "svc", sb.String())
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
}

// --- error/rejection -------------------------------------------------------

// Unknown server name → error that names the server.
func TestRealEngine_View_UnknownServer(t *testing.T) {
	cfg, _ := viewTestCfg(t)
	_, err := NewEngine(cfg).View("nosuch", false)
	if err == nil {
		t.Fatal("want error for unknown server, got nil")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error should name the server, got: %v", err)
	}
}

// Known server with no log file → error that names the server.
func TestRealEngine_View_NoLogFile(t *testing.T) {
	cfg, _ := viewTestCfg(t)
	_, err := NewEngine(cfg).View("svc", false)
	if err == nil {
		t.Fatal("want error when no log file exists, got nil")
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Errorf("error should name the server, got: %v", err)
	}
}

// --- happy path ------------------------------------------------------------

// More than 50 lines → returns the last 50 in order.
func TestRealEngine_View_ReturnsLast50Lines(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeViewLog(t, logDir, "svc", sb.String())
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	if lines[0] != "line 10" {
		t.Errorf("first line = %q, want %q", lines[0], "line 10")
	}
	if lines[49] != "line 59" {
		t.Errorf("last line = %q, want %q", lines[49], "line 59")
	}
}

// nowrap=false → long lines returned unchanged.
func TestRealEngine_View_NoNowrapPreservesFullLine(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	longLine := strings.Repeat("x", 120)
	writeViewLog(t, logDir, "svc", longLine+"\n")
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 1 || lines[0] != longLine {
		t.Errorf("nowrap=false must not truncate: got len=%d line=%q", len(lines), lines[0])
	}
}

// nowrap=true → lines longer than 80 runes are truncated to exactly 80.
func TestRealEngine_View_NowrapTruncatesAt80Runes(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	longLine := strings.Repeat("x", 120)
	writeViewLog(t, logDir, "svc", longLine+"\n")
	lines, err := NewEngine(cfg).View("svc", true)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if got := len([]rune(lines[0])); got != 80 {
		t.Errorf("nowrap=true: line length = %d, want 80", got)
	}
}

// nowrap=true leaves lines that are already ≤80 runes unchanged.
func TestRealEngine_View_NowrapShortLineUnchanged(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	shortLine := strings.Repeat("y", 40)
	writeViewLog(t, logDir, "svc", shortLine+"\n")
	lines, err := NewEngine(cfg).View("svc", true)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 1 || lines[0] != shortLine {
		t.Errorf("nowrap=true must not shorten ≤80-rune lines: got %q", lines[0])
	}
}

// --- adversary gaps --------------------------------------------------------

// Empty log file (exists, 0 bytes) → no error, returns empty slice.
func TestRealEngine_View_EmptyLogFile(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	writeViewLog(t, logDir, "svc", "")
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("empty log file must not error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("empty log file: want 0 lines, got %d: %v", len(lines), lines)
	}
}

// Log file with no trailing newline — last line must still be returned.
func TestRealEngine_View_NoTrailingNewline(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	writeViewLog(t, logDir, "svc", "first\nsecond")
	lines, err := NewEngine(cfg).View("svc", false)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %v", len(lines), lines)
	}
	if lines[1] != "second" {
		t.Errorf("last line (no trailing newline) = %q, want %q", lines[1], "second")
	}
}

// nowrap=true with a line of exactly 80 runes must NOT truncate (boundary).
func TestRealEngine_View_NowrapExact80Runes(t *testing.T) {
	cfg, logDir := viewTestCfg(t)
	line80 := strings.Repeat("z", 80)
	writeViewLog(t, logDir, "svc", line80+"\n")
	lines, err := NewEngine(cfg).View("svc", true)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(lines) != 1 || lines[0] != line80 {
		t.Errorf("nowrap=true must not truncate a 80-rune line: got len=%d %q", len([]rune(lines[0])), lines[0])
	}
}
