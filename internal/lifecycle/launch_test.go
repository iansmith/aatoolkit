package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForFile polls until the predicate on the file contents is satisfied or
// the timeout elapses — child process output is asynchronous.
func waitForLogContent(t *testing.T, path string, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log %q to contain %q; last content: %q", path, want, last)
	return ""
}

func TestLaunch_PipesStdoutAndStderrToLogFile(t *testing.T) {
	dir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "echoer", Command: "/bin/sh", Args: []string{"-c", "echo stdout-line; echo stderr-line 1>&2"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "stderr-line")
	if !strings.Contains(content, "stdout-line") {
		t.Fatalf("expected stdout captured in log, got: %q", content)
	}
	if !strings.Contains(content, "stderr-line") {
		t.Fatalf("expected stderr captured in log, got: %q", content)
	}
}

func TestLaunch_OwnProcessGroup(t *testing.T) {
	dir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "sleeper", Command: "/bin/sh", Args: []string{"-c", "sleep 2"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	pid := proc.Cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != pid {
		t.Fatalf("expected child to be its own process group leader (pgid==pid); pid=%d pgid=%d", pid, pgid)
	}
}

func TestLaunch_EnvInjectedOverInherited(t *testing.T) {
	dir := t.TempDir()

	if err := os.Setenv("AATOOLKIT_LIFECYCLE_TEST_BASE", "inherited"); err != nil {
		t.Fatalf("Setenv: %v", err)
	}
	defer os.Unsetenv("AATOOLKIT_LIFECYCLE_TEST_BASE")

	env := map[string]string{
		"AATOOLKIT_LIFECYCLE_TEST_BASE": "overridden",
		"AATOOLKIT_LIFECYCLE_TEST_NEW":  "added",
	}
	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "envtest", Command: "/bin/sh", Args: []string{"-c", "echo BASE=$AATOOLKIT_LIFECYCLE_TEST_BASE; echo NEW=$AATOOLKIT_LIFECYCLE_TEST_NEW; echo PATH_SET=$PATH"}, Env: env})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "NEW=added")
	if !strings.Contains(content, "BASE=overridden") {
		t.Fatalf("expected per-server env to override inherited env, got: %q", content)
	}
	if !strings.Contains(content, "PATH_SET=") || strings.Contains(content, "PATH_SET=\n") {
		t.Fatalf("expected inherited PATH to still be present (env overlays, not replaces), got: %q", content)
	}
}

func TestLaunch_WaitInGoroutine_DoesNotBlockLaunch(t *testing.T) {
	dir := t.TempDir()

	start := time.Now()
	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "longsleep", Command: "/bin/sh", Args: []string{"-c", "sleep 5"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("Launch took %v — expected it to return immediately without waiting for the 5s child to exit", elapsed)
	}
}

func TestLaunch_LogFileUnderExpectedDir(t *testing.T) {
	dir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "pathcheck", Command: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	if !strings.HasPrefix(proc.LogPath, dir) {
		t.Fatalf("expected log path under %q, got %q", dir, proc.LogPath)
	}
	if !strings.Contains(proc.LogPath, "pathcheck-") {
		t.Fatalf("expected log filename to contain server name, got %q", proc.LogPath)
	}
}

func TestLaunch_InvalidCommand_ReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := Launch(LaunchSpec{LogDir: dir, Name: "bogus", Command: "/no/such/binary-xyz", Args: nil})
	if err == nil {
		t.Fatalf("expected an error launching a non-existent command, got nil")
	}
}

func TestLaunch_LogDirCreatedIfMissing(t *testing.T) {
	dir := t.TempDir()
	logDir := dir + "/nested/logs"

	proc, err := Launch(LaunchSpec{LogDir: logDir, Name: "freshdir", Command: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	if !strings.HasPrefix(proc.LogPath, logDir) {
		t.Fatalf("expected log under freshly-created dir %q, got %q", logDir, proc.LogPath)
	}
	if _, err := os.Stat(proc.LogPath); err != nil {
		t.Fatalf("expected log file to exist in newly created dir: %v", err)
	}
}

// Each launch gets its own log file named for its launch time, so the
// LogPath handed back (and the `tail -f` hint built from it) only ever names
// this run's output. Launch times are injected: with the wall clock, two
// launches would usually land in the same second and only sometimes straddle
// a boundary, making the assertion timing-dependent.
func TestLaunch_ConsecutiveLaunches_SecondGetsOwnFile(t *testing.T) {
	dir := t.TempDir()

	firstAt := time.Date(2026, 7, 6, 14, 3, 11, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)

	first, err := Launch(LaunchSpec{LogDir: dir, Name: "repeat", Command: "/bin/sh", Args: []string{"-c", "echo first-run"}, Now: firstAt})
	if err != nil {
		t.Fatalf("first Launch error: %v", err)
	}
	waitForLogContent(t, first.LogPath, "first-run")

	second, err := Launch(LaunchSpec{LogDir: dir, Name: "repeat", Command: "/bin/sh", Args: []string{"-c", "echo second-run"}, Now: secondAt})
	if err != nil {
		t.Fatalf("second Launch error: %v", err)
	}

	if second.LogPath == first.LogPath {
		t.Fatalf("expected the second launch to get its own log file, but it reused the first's %q", first.LogPath)
	}

	content := waitForLogContent(t, second.LogPath, "second-run")
	if strings.Contains(content, "first-run") {
		t.Fatalf("expected the second launch's log to hold only its own run, got: %q", content)
	}

	// The first run's log survives, still holding only its own output.
	firstContent, err := os.ReadFile(first.LogPath)
	if err != nil {
		t.Fatalf("reading first launch's log: %v", err)
	}
	if !strings.Contains(string(firstContent), "first-run") {
		t.Fatalf("expected the first run's output to survive, got: %q", firstContent)
	}
}

// TestLaunch_ZeroNowUsesTheWallClock pins the path every production launch
// takes.
//
// Nothing in production sets LaunchSpec.Now; the naming tests all supply it.
// So the wall clock reaches production solely through Launch's zero-value
// fallback, and nothing else asserts it. Removing that fallback as redundant
// -- every visible caller passes a Now -- would name every log for the zero
// time (server-0001-01-01-00-00-00.log), identically for every launch of every
// server forever, while leaving the suite green.
//
// The assertion is on the *shape* of the name, not on a clock reading: any
// plausible year beats year 1, and the test cannot flake on timing.
func TestLaunch_ZeroNowUsesTheWallClock(t *testing.T) {
	dir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: dir, Name: "wallclock", Command: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	base := filepath.Base(proc.LogPath)
	stamp := strings.TrimSuffix(strings.TrimPrefix(base, "wallclock-"), ".log")
	got, err := time.Parse(logTimeLayout, stamp)
	if err != nil {
		t.Fatalf("log name %q does not carry a %s timestamp: %v", base, logTimeLayout, err)
	}
	if got.Year() < 2020 {
		t.Errorf("log named %q (year %d): Launch used the zero value instead of the wall clock, so every launch of every server would share one filename",
			base, got.Year())
	}
}
