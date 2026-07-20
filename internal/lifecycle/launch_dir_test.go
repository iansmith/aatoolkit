package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

// assertCmdDirResolvesTo resolves symlinks on both sides before comparing
// (t.TempDir() can return a path through a symlink, e.g. macOS's
// /var -> /private/var) and fails t if proc's cmd.Dir doesn't match want.
func assertCmdDirResolvesTo(t *testing.T, proc *Process, want string) {
	t.Helper()
	wantDir, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("EvalSymlinks(want) — want=%q: %v", want, err)
	}
	gotDir, err := filepath.EvalSymlinks(proc.Cmd.Dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(cmd.Dir) — cmd.Dir=%q: %v", proc.Cmd.Dir, err)
	}
	if gotDir != wantDir {
		t.Fatalf("expected cmd.Dir %q, got %q", wantDir, gotDir)
	}
}

// TestLaunch_DirSetsCmdDir pins behavior 1: a non-empty dir becomes the
// child's actual working directory (exec.Cmd.Dir).
func TestLaunch_DirSetsCmdDir(t *testing.T) {
	logDir := t.TempDir()
	workDir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: logDir, Name: "dirtest", Command: "/bin/sh", Args: []string{"-c", "echo hi"}, Dir: workDir})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	assertCmdDirResolvesTo(t, proc, workDir)
}

// TestLaunch_DirUnsetLeavesCmdDirEmpty pins behavior 2: an empty dir leaves
// cmd.Dir unset, so the child inherits the supervisor's own cwd exactly as
// it does today — no regression for every existing server.
func TestLaunch_DirUnsetLeavesCmdDirEmpty(t *testing.T) {
	logDir := t.TempDir()

	proc, err := Launch(LaunchSpec{LogDir: logDir, Name: "nodirtest", Command: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	if proc.Cmd.Dir != "" {
		t.Fatalf("expected cmd.Dir empty when dir unset, got %q", proc.Cmd.Dir)
	}
}

// TestLaunch_DirExpandsTilde closes a reuse gap found by /simplify: dir was
// being handed to cmd.Dir verbatim, without the same "~/" expansion
// source.go already applies to this same field for build-time -C sourcing
// (expandTilde) — and the ticket's own round-trip test uses "~/some/project"
// as a valid Dir value, so a bare "~/..." must actually resolve.
func TestLaunch_DirExpandsTilde(t *testing.T) {
	logDir := t.TempDir()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	proc, err := Launch(LaunchSpec{LogDir: logDir, Name: "tildetest", Command: "/bin/sh", Args: []string{"-c", "echo hi"}, Dir: "~"})
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	assertCmdDirResolvesTo(t, proc, home)
}
