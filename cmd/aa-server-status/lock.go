package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock is an acquired exclusive flock on a lock file. Release drops the
// flock and closes the file.
type Lock struct {
	path string
	f    *os.File
}

// AcquireLock takes an exclusive, non-blocking flock on path, creating any
// missing parent directories first. On success it writes the current
// process's PID into the file (truncating any stale contents) so a second
// launch can name the running holder.
//
// If the lock is already held, AcquireLock fails loudly, naming the
// holder's PID as read from the file (or "unknown pid" if the file's
// contents can't be parsed as a PID).
func AcquireLock(path string) (*Lock, error) {
	if path == "" {
		return nil, fmt.Errorf("aa-server-status: lock path is empty")
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("aa-server-status: creating lock directory %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("aa-server-status: opening lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readHolderPID(f)
		f.Close()
		return nil, fmt.Errorf("aa-server-status already running (pid %s), refusing second instance: %s", holder, path)
	}

	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("aa-server-status: truncating lock file %s: %w", path, err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("aa-server-status: writing pid to lock file %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("aa-server-status: syncing lock file %s: %w", path, err)
	}

	return &Lock{path: path, f: f}, nil
}

// readHolderPID reads the existing lock file's contents and returns a
// display string for the holder's PID, or "unknown pid" if the contents
// can't be parsed.
func readHolderPID(f *os.File) string {
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return "unknown pid"
	}
	trimmed := strings.TrimSpace(string(data))
	if _, err := strconv.Atoi(trimmed); err != nil || trimmed == "" {
		return "unknown pid"
	}
	return trimmed
}

// Release drops the flock and closes the underlying file. The lock file
// itself is left in place (not removed) — AcquireLock reclaims it on the
// next launch regardless of its contents, so deleting it here would only
// add a race against a concurrent AcquireLock with no benefit.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("aa-server-status: unlocking %s: %w", l.path, err)
	}
	return l.f.Close()
}
