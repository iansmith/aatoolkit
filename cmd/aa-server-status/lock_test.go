package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// --- edge / boundary cases ---

func TestAcquireLock_CreatesMissingParentDirs(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "build", "run", "aa-server-status.lock")

	if _, err := os.Stat(filepath.Dir(lockPath)); !os.IsNotExist(err) {
		t.Fatalf("precondition: parent dir should not exist yet, stat err=%v", err)
	}

	lk, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock: unexpected error: %v", err)
	}
	defer lk.Release()

	if _, err := os.Stat(filepath.Dir(lockPath)); err != nil {
		t.Fatalf("expected parent dirs to be created, stat err=%v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file to exist, stat err=%v", err)
	}
}

func TestAcquireLock_EmptyPath(t *testing.T) {
	_, err := AcquireLock("")
	if err == nil {
		t.Fatal("AcquireLock(\"\") should fail loudly, got nil error")
	}
}

func TestAcquireLock_WritesOwnPID(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	lk, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock: unexpected error: %v", err)
	}
	defer lk.Release()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("lock file contents not a valid PID: %q, err=%v", data, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("lock file PID = %d, want %d (own pid)", pid, os.Getpid())
	}
}

// --- error / rejection cases ---

func TestAcquireLock_SecondAcquireFailsNamingHolderPID(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	first, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("first AcquireLock: unexpected error: %v", err)
	}
	defer first.Release()

	_, err = AcquireLock(lockPath)
	if err == nil {
		t.Fatal("second AcquireLock should fail while first holds the lock")
	}
	want := strconv.Itoa(os.Getpid())
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name holder PID %s", err.Error(), want)
	}
}

func TestAcquireLock_UnreadableLockFileStillFailsLoudly(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	// Pre-create a lock file with garbage (unparseable) contents, held by a
	// real flock so the second acquire is forced down the conflict path.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("setup: opening lock file: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("setup: flock: %v", err)
	}
	if _, err := f.WriteString("not-a-pid"); err != nil {
		t.Fatalf("setup: writing garbage: %v", err)
	}

	_, err = AcquireLock(lockPath)
	if err == nil {
		t.Fatal("AcquireLock should fail loudly even when the holder's PID is unparseable")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown") && !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected a loud error mentioning the running/unknown holder, got: %v", err)
	}
}

func TestAcquireLock_StaleLockFileWithDeadPIDIsReclaimed(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	// Simulate a supervisor that died without cleanup: the lock file
	// exists and has a PID written in it, but nothing holds the flock
	// (the OS releases flocks automatically when the holding process
	// exits/dies, even on unclean termination). A fresh AcquireLock must
	// succeed and reclaim the file rather than treating stale bytes as a
	// live conflict.
	if err := os.WriteFile(lockPath, []byte("999999999"), 0o644); err != nil {
		t.Fatalf("setup: writing stale lock file: %v", err)
	}

	lk, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock on a stale (unflocked) lock file should succeed, got: %v", err)
	}
	defer lk.Release()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("lock file contents not a valid PID after reclaim: %q, err=%v", data, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("lock file PID = %d, want %d (own pid, reclaimed)", pid, os.Getpid())
	}
}

// --- cross-feature interaction ---

func TestAcquireLock_ReleaseAllowsReacquire(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	first, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("first AcquireLock: unexpected error: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: unexpected error: %v", err)
	}

	second, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock after release: unexpected error: %v", err)
	}
	defer second.Release()
}

// --- happy path ---

func TestAcquireLock_Basic(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "aa-server-status.lock")

	lk, err := AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("AcquireLock: unexpected error: %v", err)
	}
	if lk == nil {
		t.Fatal("AcquireLock returned nil lock with nil error")
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: unexpected error: %v", err)
	}
}
