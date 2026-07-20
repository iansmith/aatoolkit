package lifecycle

import (
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// TestLaunchExec_ThreadsServerDirToCmdDir closes an adversary-found gap:
// TestLaunch_DirSetsCmdDir only exercised Launch directly. Nothing pinned
// that the four LaunchXxx wrappers actually thread s.Dir through — a
// dropped `s.Dir` argument at any one of those call sites would pass every
// other Phase 0 test.
func TestLaunchExec_ThreadsServerDirToCmdDir(t *testing.T) {
	logDir := t.TempDir()
	workDir := t.TempDir()

	s := config.Server{
		Name:    "caddy",
		Type:    config.TypeExec,
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hi"},
		Dir:     workDir,
	}

	proc, err := LaunchExec(logDir, s)
	if err != nil {
		t.Fatalf("LaunchExec error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	assertCmdDirResolvesTo(t, proc, workDir)
}
