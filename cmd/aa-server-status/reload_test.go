package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// writeConfig writes a minimal valid aa-server-status config declaring one
// exec server named name, and stamps its mtime to modTime. The mtime is set
// explicitly rather than relying on the write clock: reload detection is
// mtime-based, so a test that depended on two writes landing in different
// filesystem timestamps would be betting on filesystem resolution.
func writeConfig(t *testing.T, path, name string, modTime time.Time) {
	t.Helper()
	body := `[supervisor]
log_dir = "` + filepath.Dir(path) + `/logs"

[[server]]
name = "` + name + `"
type = "exec"
enabled = false
command = "/bin/true"
port = 19999

[server.health]
path = "/healthz"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writeConfig %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// baseTime is a fixed instant the reload tests advance by hand. Nothing in
// the reload path reads the wall clock, so the tests don't have to either.
var baseTime = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

// TestRun_ReloadsConfigWhenFileMtimeChanges pins Observable behaviors 2 and 4:
// a config edit between two prompts is picked up, and the NEXT dispatched
// command sees it.
func TestRun_ReloadsConfigWhenFileMtimeChanges(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "aa-server-status.toml")
	writeConfig(t, cfgPath, "before", baseTime)

	eng := newTestEngineWatching(t, cfgPath)

	// First line is a bare Enter (status); between it and the second, the
	// file is rewritten with a different server name and a later mtime.
	in := &scriptedReader{lines: []string{"", "", "quit"}, before: map[int]func(){
		1: func() { writeConfig(t, cfgPath, "after", baseTime.Add(time.Minute)) },
	}}
	var out strings.Builder
	if err := Run(bufio.NewReader(in), &out, eng); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names := serverNames(eng)
	if len(names) != 1 || names[0] != "after" {
		t.Fatalf("expected the reloaded config's server %q to be live, got %v", "after", names)
	}
	if !strings.Contains(out.String(), "after") {
		t.Fatalf("expected the post-reload status table to show the new server, got:\n%s", out.String())
	}
}

// TestRun_FailedReloadKeepsPreviousConfig pins Observable behavior 3: a config
// that no longer parses must not take the REPL down with it, and must not
// discard the last-known-good config.
func TestRun_FailedReloadKeepsPreviousConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "aa-server-status.toml")
	writeConfig(t, cfgPath, "good", baseTime)

	eng := newTestEngineWatching(t, cfgPath)

	in := &scriptedReader{lines: []string{"", "", "quit"}, before: map[int]func(){
		1: func() {
			if err := os.WriteFile(cfgPath, []byte("this is not [ valid toml"), 0o644); err != nil {
				t.Fatalf("corrupting config: %v", err)
			}
			if err := os.Chtimes(cfgPath, baseTime.Add(time.Minute), baseTime.Add(time.Minute)); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
		},
	}}
	var out strings.Builder
	if err := Run(bufio.NewReader(in), &out, eng); err != nil {
		t.Fatalf("Run must not return an error when a reload fails, got: %v", err)
	}

	names := serverNames(eng)
	if len(names) != 1 || names[0] != "good" {
		t.Fatalf("expected the last-known-good config to survive a failed reload, got %v", names)
	}
	if !strings.Contains(out.String(), cfgPath) {
		t.Fatalf("expected the reload error to name the offending file, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), prompt) {
		t.Fatalf("expected the REPL to keep prompting after a failed reload, got:\n%s", out.String())
	}
}

// TestRun_NoReloadWhenMtimeUnchanged pins Observable behavior 5: with no edit
// between commands, the config is never re-parsed. Without this, the feature
// silently becomes "re-read and re-validate the whole config on every
// keystroke", which is what the mtime check exists to avoid.
func TestRun_NoReloadWhenMtimeUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "aa-server-status.toml")
	writeConfig(t, cfgPath, "svc", baseTime)

	eng := newTestEngineWatching(t, cfgPath)

	in := &scriptedReader{lines: []string{"", "", "help", "", "quit"}}
	var out strings.Builder
	if err := Run(bufio.NewReader(in), &out, eng); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := eng.reloadCount(); n != 0 {
		t.Fatalf("expected zero reloads across %d commands with no file edit, got %d", 5, n)
	}
}

// --- helpers -------------------------------------------------------------

// scriptedReader feeds Run one line per Read call, optionally running a hook
// immediately before a given line is handed over. One line per Read is
// load-bearing: bufio.Scanner only calls Read when it needs more data, so
// returning a single complete line guarantees the hook for line N runs after
// line N-1 has already been dispatched. Handing the scanner every line at
// once would let it buffer the whole script before any hook fired.
type scriptedReader struct {
	lines  []string
	before map[int]func()
	i      int
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.lines) {
		return 0, io.EOF
	}
	if hook, ok := r.before[r.i]; ok {
		hook()
	}
	line := r.lines[r.i] + "\n"
	r.i++
	n := copy(p, line)
	if n < len(line) {
		return n, fmt.Errorf("scriptedReader: buffer too small for line %d", r.i-1)
	}
	return n, nil
}

// newTestEngineWatching loads cfgPath, builds a RealEngine over it, and points
// the engine's reload check at that same path (plus its conventional .local
// overlay, which these tests never create).
func newTestEngineWatching(t *testing.T, cfgPath string) *RealEngine {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("initial config.Load(%s): %v", cfgPath, err)
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })
	eng.WatchConfig(cfgPath)
	return eng
}

// serverNames reports the server names in the engine's currently live config,
// which is what "the next command observes the new config" actually means.
func serverNames(eng *RealEngine) []string {
	statuses := eng.Status()
	names := make([]string, 0, len(statuses))
	for _, s := range statuses {
		names = append(names, s.Name)
	}
	return names
}

// TestNewConfigReloader_WatchesTheSinglePath is the reloader's half of AATK-33's
// arity guard, and it does assert something real: that a change to the one
// watched file is still detected after the second stamp was removed. A removal
// that dropped the overlay stamp and the detection with it would pass a
// compile-only check and fail this one.
func TestNewConfigReloader_WatchesTheSinglePath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "aa-server-status.toml")
	if err := os.WriteFile(cfgPath, []byte("[supervisor]\nlog_dir = \"build/logs\"\n"), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	r := newConfigReloader(cfgPath)
	if r.changed() {
		t.Error("changed() reported a change immediately after construction — the stamp was not taken")
	}

	// A distinct mtime, not just distinct content: detection is mtime+size
	// based, and rewriting identical bytes within the clock's resolution is
	// documented as invisible.
	if err := os.WriteFile(cfgPath, []byte("[supervisor]\nlog_dir = \"build/other-logs\"\n"), 0o644); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if !r.changed() {
		t.Error("changed() missed a modification to the single watched path")
	}
	if r.changed() {
		t.Error("changed() reported the same modification twice — the stamp was not updated on detection")
	}
}
