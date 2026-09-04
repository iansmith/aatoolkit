package lifecycle

import (
	"testing"
	"time"
)

// AATK-106 review round 2, F7: Exited's contract is pinned in the package
// that owns it, not only through its supervisor caller. Both halves matter —
// the false answer is the one a Process built outside Launch always gives,
// and callers are entitled to rely on it never becoming a spurious true.
func TestProcess_Exited(t *testing.T) {
	t.Run("nil Process reports not exited", func(t *testing.T) {
		var p *Process
		if p.Exited() {
			t.Fatal("a nil Process must report not-exited, not panic or report true")
		}
	})

	t.Run("a Process built outside Launch never reports exited", func(t *testing.T) {
		// No Wait goroutine stands behind this one, so nothing can ever set
		// the flag. Conservative by design: it preserves the answer callers
		// had before the field existed.
		p := &Process{LogPath: "hand-built.log"}
		if p.Exited() {
			t.Fatal("a hand-built Process must report not-exited")
		}
	})

	t.Run("a launched child reports exited once reaped", func(t *testing.T) {
		proc, err := Launch(LaunchSpec{
			LogDir:  t.TempDir(),
			Name:    "exits-at-once",
			Command: "/bin/sh",
			Args:    []string{"-c", "exit 0"},
		})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if proc.Exited() {
			t.Fatal("Exited must not be set before the child has been reaped")
		}

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if proc.Exited() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("Launch's Wait goroutine never recorded the child's exit")
	})

	t.Run("a live child does not report exited", func(t *testing.T) {
		proc, err := Launch(LaunchSpec{
			LogDir:  t.TempDir(),
			Name:    "long-lived",
			Command: "/bin/sh",
			Args:    []string{"-c", "sleep 30"},
		})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		t.Cleanup(func() { _ = proc.Cmd.Process.Kill() })

		// Long enough that a flag set unconditionally by the Wait goroutine
		// — rather than after Wait actually returns — would show up here.
		time.Sleep(200 * time.Millisecond)
		if proc.Exited() {
			t.Fatal("a running child must not report exited")
		}
	})
}
