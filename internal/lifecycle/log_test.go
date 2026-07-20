package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: create a log file for name with given size and mtime.
func writeLogFile(t *testing.T, dir, name string, ts time.Time, size int) string {
	t.Helper()
	path := filepath.Join(dir, name+"-"+ts.Format(logTimeLayout)+".log")
	content := strings.Repeat("x", size)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture log: %v", err)
	}
	// Force the mtime explicitly — creation order alone isn't reliable enough
	// across filesystems with coarse mtime resolution.
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes fixture log: %v", err)
	}
	return path
}

func TestNewestLog_NoExistingLogs(t *testing.T) {
	dir := t.TempDir()

	_, ok, err := NewestLog(dir, "chat-llm")
	if err != nil {
		t.Fatalf("NewestLog returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when no logs exist")
	}
}

func TestNewestLog_PicksMostRecentByMtime(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)

	older := writeLogFile(t, dir, "chat-llm", base, 10)
	newer := writeLogFile(t, dir, "chat-llm", base.Add(time.Hour), 10)

	got, ok, err := NewestLog(dir, "chat-llm")
	if err != nil {
		t.Fatalf("NewestLog error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != newer {
		t.Fatalf("expected newest log %q, got %q (older was %q)", newer, got, older)
	}
}

func TestNewestLog_IgnoresOtherServerNames(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)

	writeLogFile(t, dir, "voice-out", base.Add(time.Hour), 10)
	chatLog := writeLogFile(t, dir, "chat-llm", base, 10)

	got, ok, err := NewestLog(dir, "chat-llm")
	if err != nil {
		t.Fatalf("NewestLog error: %v", err)
	}
	if !ok || got != chatLog {
		t.Fatalf("expected chat-llm's own log %q, got %q (ok=%v)", chatLog, got, ok)
	}
}

func TestNewestLog_DoesNotMatchNameThatIsAPrefixOfAnotherServer(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)

	// "chat" is a "<name>-" prefix of "chat-llm" — the glob "chat-*.log"
	// would also match chat-llm's log files. NewestLog for "chat" must not
	// pick up chat-llm's log.
	writeLogFile(t, dir, "chat-llm", base.Add(time.Hour), 10)

	_, ok, err := NewestLog(dir, "chat")
	if err != nil {
		t.Fatalf("NewestLog error: %v", err)
	}
	if ok {
		t.Fatalf("expected no log for 'chat' (only 'chat-llm' has one), but NewestLog found one")
	}
}

func TestOpenLogForLaunch_NoExistingLog_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 6, 14, 3, 11, 0, time.UTC)

	f, path, err := openLogForLaunch(dir, "chat-llm", now)
	if err != nil {
		t.Fatalf("openLogForLaunch error: %v", err)
	}
	defer f.Close()

	wantSuffix := "chat-llm-2026-07-06-14-03-11.log"
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("expected path ending %q, got %q", wantSuffix, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected new log file to exist: %v", err)
	}
}

// A log's filename names the run it contains, so an earlier run's log is
// never reopened however small it is — the launch gets its own file, and the
// `tail -f` hint built from that path can't point at a previous run's output.
func TestOpenLogForLaunch_ExistingLogFromEarlierRun_StartsOwnFile(t *testing.T) {
	dir := t.TempDir()
	past := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	existing := writeLogFile(t, dir, "chat-llm", past, 100)

	now := time.Date(2026, 7, 6, 14, 3, 11, 0, time.UTC)
	f, path, err := openLogForLaunch(dir, "chat-llm", now)
	if err != nil {
		t.Fatalf("openLogForLaunch error: %v", err)
	}
	defer f.Close()

	if path == existing {
		t.Fatalf("expected this launch to get its own log file, but it reopened the earlier run's log %q", existing)
	}
	wantSuffix := "chat-llm-2026-07-06-14-03-11.log"
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("expected path named for this launch, ending %q, got %q", wantSuffix, path)
	}

	// The earlier run's log survives untouched.
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("reading earlier run's log: %v", err)
	}
	if len(data) != 100 {
		t.Fatalf("expected earlier run's 100-byte log to be untouched, got %d bytes", len(data))
	}
}

// The filename has second granularity, so a fast `down`/`up` cycle can land
// two runs on the same path. The second launch must not truncate the first
// run's output away.
func TestOpenLogForLaunch_SameSecondRelaunch_PreservesEarlierOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 6, 14, 3, 11, 0, time.UTC)

	f1, path1, err := openLogForLaunch(dir, "chat-llm", now)
	if err != nil {
		t.Fatalf("first openLogForLaunch error: %v", err)
	}
	if _, err := f1.WriteString("run one output\n"); err != nil {
		t.Fatalf("write to first log: %v", err)
	}
	f1.Close()

	f2, path2, err := openLogForLaunch(dir, "chat-llm", now)
	if err != nil {
		t.Fatalf("second openLogForLaunch error: %v", err)
	}
	if _, err := f2.WriteString("run two output\n"); err != nil {
		t.Fatalf("write to second log: %v", err)
	}
	f2.Close()

	if path1 != path2 {
		t.Fatalf("same-second launches should resolve to one path, got %q and %q", path1, path2)
	}

	data, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "run one output") {
		t.Fatalf("second launch truncated the first run's output away, got: %q", content)
	}
	if !strings.Contains(content, "run two output") {
		t.Fatalf("expected the second run's output, got: %q", content)
	}
	if !strings.Contains(content, "launched") {
		t.Fatalf("expected a launch banner separating the two runs, got: %q", content)
	}
}
