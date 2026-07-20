// Package lifecycle implements the common child-launch machinery shared by
// every server type — process-group isolation, per-server environment
// overlay, and log-file management — plus the two launchers whose command
// shapes need no further logic (mlx, exec). See design/aa-server-status.md §4,
// §6.4, §9.
package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logTimeLayout is the Go reference-time layout used in log filenames,
// e.g. "chat-llm-2026-07-06-14-03-11.log" (design/aa-server-status.md §9).
const logTimeLayout = "2006-01-02-15-04-05"

// logTimeLayoutLen is the fixed length of a logTimeLayout-formatted
// timestamp, used to validate a glob match actually belongs to name and
// isn't a different server whose name happens to be a "<name>-" prefix
// (e.g. name="chat" must not match "chat-llm-<ts>.log").
const logTimeLayoutLen = len("2006-01-02-15-04-05")

// NewestLog returns the path of the most-recently-modified log file for
// name under logDir. ok is false if no such file exists.
//
// The glob "<name>-*.log" alone is not precise enough: a server literally
// named "chat" would also match "chat-llm-<ts>.log". Matches are filtered to
// the exact expected shape, "<name>-<logTimeLayout>.log", before comparing
// mtimes.
func NewestLog(logDir, name string) (path string, ok bool, err error) {
	matches, err := filepath.Glob(filepath.Join(logDir, name+"-*.log"))
	if err != nil {
		return "", false, fmt.Errorf("globbing logs for %q: %w", name, err)
	}

	prefix := name + "-"
	var newestPath string
	var newestMod time.Time
	for _, m := range matches {
		base := filepath.Base(m)
		ts := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".log")
		if len(ts) != logTimeLayoutLen {
			// Belongs to a different server whose name is a "<name>-"
			// prefix of this one (e.g. name="chat" vs. "chat-llm-<ts>.log").
			continue
		}

		info, err := os.Stat(m)
		if err != nil {
			// Racing with deletion or similar — skip rather than fail the
			// whole resolution.
			continue
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newestPath = m
		}
	}
	if newestPath == "" {
		return "", false, nil
	}
	return newestPath, true, nil
}

// openLogForLaunch opens the log file for this launch of name, per
// design/aa-server-status.md §9: every launch starts its own file, named
// "<name>-<now formatted as logTimeLayout>.log", so a log's filename always
// names the single run it contains.
//
// Two launches within the same second resolve to the same path (the filename
// has second granularity, and a down/up cycle can land inside one second).
// The file is therefore opened for append and never truncated, so the earlier
// run's output survives; a launch banner marks where the later run begins.
//
// The directory is created if it does not already exist.
func openLogForLaunch(logDir, name string, now time.Time) (*os.File, string, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("creating log dir %q: %w", logDir, err)
	}

	path := filepath.Join(logDir, name+"-"+now.Format(logTimeLayout)+".log")

	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("stat log %q: %w", path, err)
	}
	sameSecondRelaunch := err == nil && info.Size() > 0

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("creating log %q: %w", path, err)
	}
	if sameSecondRelaunch {
		if _, err := fmt.Fprintf(f, "=== launched %s at %s ===\n", name, now.Format(time.RFC3339)); err != nil {
			f.Close()
			return nil, "", fmt.Errorf("writing launch banner to %q: %w", path, err)
		}
	}
	return f, path, nil
}
