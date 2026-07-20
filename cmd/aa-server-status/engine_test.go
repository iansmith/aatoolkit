package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// tdlistenerServer builds a config.Server of type "exec" that launches the
// tdlistener test fixture with the given port and health flag — RealEngine's
// tests exercise real launch/teardown/observe/health plumbing against this
// fixture instead of mocking any of internal/lifecycle, internal/observe, or
// internal/health.
func tdlistenerServer(t *testing.T, name string, port int, enabled bool) config.Server {
	t.Helper()
	bin := tdlistenerBinary(t)
	return config.Server{
		Name:    name,
		Type:    config.TypeExec,
		Enabled: enabled,
		Host:    "127.0.0.1",
		Listens: []int{port},
		Command: bin,
		Args:    []string{"-port", strconv.Itoa(port), "-serve-health"},
		Health:  config.Health{Path: "/healthz", Port: port},
	}
}

func testSupervisor(t *testing.T) config.Supervisor {
	t.Helper()
	return config.Supervisor{
		LogDir:       t.TempDir(),
		GracePeriod:  config.Duration{Duration: 2 * time.Second},
		ReadyTimeout: config.Duration{Duration: 5 * time.Second},
		PollInterval: config.Duration{Duration: 50 * time.Millisecond},
	}
}

// --- happy path ---------------------------------------------------------

func TestRealEngine_Up_LaunchesEnabledDownServer(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("Up(\"\") error: %v", err)
	}

	statuses := eng.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %+v", statuses)
	}
	if statuses[0].State != StateUp {
		t.Fatalf("expected server up after Up(), got state %q (%+v)", statuses[0].State, statuses[0])
	}
	if statuses[0].PID == 0 {
		t.Fatalf("expected a non-zero PID after Up(), got %+v", statuses[0])
	}
}

// tdlistenerSourceBuild is the go build command that produces the
// tdlistener fixture as a "source"-type server's artifact, relative to
// this package's directory (matches tdlistenerBinary's own relative path).
const tdlistenerSourcePkg = "../../internal/lifecycle/testdata/tdlistener"

// tdlistenerSourceServer builds a config.Server of type "source" whose
// build command compiles the tdlistener fixture to binPath — used to
// exercise the real staleness-probe/rebuild path (ProbeStaleness,
// PerformBuild) against a genuinely runnable, health-checkable binary,
// rather than the internal/lifecycle package's own non-serving
// buildable_v1/v2 fixtures.
func tdlistenerSourceServer(t *testing.T, name string, port int, binPath string) config.Server {
	t.Helper()
	return config.Server{
		Name:    name,
		Type:    config.TypeSource,
		Enabled: true,
		Host:    "127.0.0.1",
		Listens: []int{port},
		Build:   "go build -o " + binPath + " " + tdlistenerSourcePkg,
		Binary:  binPath,
		Args:    []string{"-port", strconv.Itoa(port), "-serve-health"},
		Health:  config.Health{Path: "/healthz", Port: port},
	}
}

// TestRealEngine_Up_ColdLaunch_RebuildsStaleSourceServerBeforeLaunching
// guards against the bug this test was written to catch: upOne's cold-launch
// path (nothing of ours tracked yet — the common case of the very first `up`
// in a new aa-server-status session) previously launched whatever binary
// happened to be on disk without ever checking staleness at all — the
// staleness probe only ran on a server already tracked as ours. A stale
// binary from days earlier would launch untouched. Seeding a bogus,
// non-executable "binary" simulates that: without the fix, Up() fails
// trying to exec it; with the fix, staleness is caught and the binary is
// rebuilt before the first launch.
func TestRealEngine_Up_ColdLaunch_RebuildsStaleSourceServerBeforeLaunching(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-source")

	if err := os.WriteFile(binPath, []byte("not a real binary"), 0o755); err != nil {
		t.Fatalf("seeding stale binary: %v", err)
	}

	s := tdlistenerSourceServer(t, "svc", port, binPath)
	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	var upErr error
	stdout := captureStdout(t, func() {
		upErr = eng.Up("")
	})
	if upErr != nil {
		t.Fatalf("Up(\"\") error (cold launch should rebuild the stale binary first, not exec it as-is): %v", upErr)
	}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateUp {
		t.Fatalf("expected server up after rebuild+launch, got %+v", statuses)
	}

	// The rebuild-then-launch path must surface the log path exactly like a
	// plain launch does — a real gap found alongside this fix: the rebuild
	// helpers launched the process but never threaded proc.LogPath back
	// into the returned outcome, so a successful rebuild+launch printed
	// "svc ✓" with no path at all.
	if !strings.Contains(stdout, ".log") {
		t.Fatalf("expected console output to include the log path after a rebuild+launch, got: %q", stdout)
	}
}

// TestRealEngine_Up_ColdLaunch_NotStale_LaunchesNormally is the sibling
// case: a source server that is NOT stale on a cold launch must behave
// exactly as before the fix — no rebuild attempted, just a normal launch.
func TestRealEngine_Up_ColdLaunch_NotStale_LaunchesNormally(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-source")

	// Pre-build the binary with the exact same flags PerformBuild's own
	// staleness probe would use (-buildvcs=false), so the on-disk binary
	// hashes identically to a fresh build and is correctly seen as fresh.
	cmd := exec.Command("go", "build", "-o", binPath, "-buildvcs=false", tdlistenerSourcePkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding fresh binary: %v\n%s", err, out)
	}

	s := tdlistenerSourceServer(t, "svc", port, binPath)
	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("Up(\"\") error: %v", err)
	}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateUp {
		t.Fatalf("expected server up, got %+v", statuses)
	}
}

// --- edge / boundary -----------------------------------------------------

func TestRealEngine_Up_AlreadyUpIsIdempotentSkip(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("first Up() error: %v", err)
	}
	firstStatus := eng.Status()[0]

	// Calling Up again while our own child is already healthy must not
	// error and must not relaunch (same PID).
	if err := eng.Up(""); err != nil {
		t.Fatalf("second Up() (idempotent) error: %v", err)
	}
	secondStatus := eng.Status()[0]
	if secondStatus.PID != firstStatus.PID {
		t.Fatalf("expected idempotent Up() to leave the same PID running, got %d then %d", firstStatus.PID, secondStatus.PID)
	}
}

// --- error / rejection ----------------------------------------------------

func TestRealEngine_Up_PortConflictRefusesAndNamesHolder(t *testing.T) {
	port := freeTestPort(t)
	// A foreign process squatting on the port our server declares — spawned
	// directly, NOT through the engine, so it is not a registered live
	// child for "svc".
	foreign := spawnForeignListener(t, port)
	defer foreign.forceKill()

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Up("")
	if err == nil {
		t.Fatal("expected Up() to refuse when a foreign process holds a declared port")
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Itoa(int(foreign.pid))) {
		t.Fatalf("expected refusal to name the foreign holder's PID %d, got: %v", foreign.pid, err)
	}
	if !strings.Contains(strings.ToLower(msg), "svc") {
		t.Fatalf("expected refusal to name the blocked server 'svc', got: %v", err)
	}

	// Nothing should have been launched for svc.
	statuses := eng.Status()
	if statuses[0].State == StateUp {
		t.Fatalf("server must not be considered up when its port precondition failed, got %+v", statuses[0])
	}
}

func TestRealEngine_Up_OwnLiveChildOnPort_IsNotAConflict(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("first Up() error: %v", err)
	}
	// Second Up() must see the SAME port held by our own live child for
	// "svc" and treat it as an already-up skip, not a foreign-conflict
	// refusal.
	if err := eng.Up(""); err != nil {
		t.Fatalf("expected no port-conflict refusal against our own live child, got: %v", err)
	}
}

// --- down: enabled+running only, strays warned not touched ---------------

func TestRealEngine_Down_StopsEnabledRunning_WarnsButDoesNotTouchStrays(t *testing.T) {
	enabledPort := freeTestPort(t)
	strayPort := freeTestPort(t)

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "enabled-svc", enabledPort, true),
			tdlistenerServer(t, "stray-svc", strayPort, false),
		},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	// Bring both up via imperative up (disabled server started for test
	// setup, mirroring "how you'd start code-llm for testing" per the
	// design doc).
	if err := eng.Up("enabled-svc"); err != nil {
		t.Fatalf("Up(enabled-svc) error: %v", err)
	}
	if err := eng.Up("stray-svc"); err != nil {
		t.Fatalf("Up(stray-svc) error: %v", err)
	}

	if err := eng.Down(""); err != nil {
		t.Fatalf("Down(\"\") error: %v", err)
	}

	statuses := eng.Status()
	var enabledState, strayState ServerState
	for _, s := range statuses {
		switch s.Name {
		case "enabled-svc":
			enabledState = s.State
		case "stray-svc":
			strayState = s.State
		}
	}
	if enabledState != StateDown {
		t.Fatalf("expected enabled-svc down after Down(), got %q", enabledState)
	}
	// The stray (disabled but running) must still be running — down never
	// touches it.
	if !isRunningState(strayState) {
		t.Fatalf("expected stray-svc to remain running (untouched) after Down(), got %q", strayState)
	}
}

func isRunningState(s ServerState) bool {
	return s == StateUp || s == StateStray || s == StatePartial
}

// --- dead: down + kill strays too -----------------------------------------

func TestRealEngine_Dead_KillsEnabledAndStrays(t *testing.T) {
	enabledPort := freeTestPort(t)
	strayPort := freeTestPort(t)

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "enabled-svc", enabledPort, true),
			tdlistenerServer(t, "stray-svc", strayPort, false),
		},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("enabled-svc"); err != nil {
		t.Fatalf("Up(enabled-svc) error: %v", err)
	}
	if err := eng.Up("stray-svc"); err != nil {
		t.Fatalf("Up(stray-svc) error: %v", err)
	}

	if err := eng.Dead(""); err != nil {
		t.Fatalf("Dead(\"\") error: %v", err)
	}

	for _, s := range eng.Status() {
		if s.State == StateUp || s.State == StateStray {
			t.Fatalf("expected every server (including strays) down after Dead(), got %+v", s)
		}
	}
}

// --- imperative <name> up ignores enabled flag ----------------------------

func TestRealEngine_ImperativeUp_StartsDisabledServer(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "disabled-svc", port, false)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	// A plain reconcile-up must NOT start a disabled server.
	if err := eng.Up(""); err != nil {
		t.Fatalf("Up(\"\") error: %v", err)
	}
	if eng.Status()[0].State == StateUp {
		t.Fatalf("reconcile-up must not start a disabled server")
	}

	// The imperative form must.
	if err := eng.Up("disabled-svc"); err != nil {
		t.Fatalf("Up(disabled-svc) error: %v", err)
	}
	status := eng.Status()[0]
	if status.State != StateUp {
		t.Fatalf("expected imperative Up(name) to start a disabled server, got %+v", status)
	}
	// A disabled server we started ourselves renders as owned-disabled
	// (render.go's yellow "up (disabled)"), never as a red STRAY — STRAY is
	// reserved for a foreign process occupying a disabled server's slot.
	if !status.OwnedDisabled {
		t.Fatalf("expected OwnedDisabled=true for a disabled server started via imperative up, got %+v", status)
	}
}

func TestRealEngine_ImperativeDown_StopsDisabledServer(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "disabled-svc", port, false)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("disabled-svc"); err != nil {
		t.Fatalf("Up(disabled-svc) error: %v", err)
	}
	if err := eng.Down("disabled-svc"); err != nil {
		t.Fatalf("Down(disabled-svc) error: %v", err)
	}
	if eng.Status()[0].State == StateUp {
		t.Fatalf("expected imperative Down(name) to stop a disabled-but-running server")
	}
}

// --- multi-server aggregate reporting -------------------------------------

func TestRealEngine_Up_MultiServer_OneFails_OthersStillAttemptedAndAggregateNamesEach(t *testing.T) {
	goodPort := freeTestPort(t)
	badPort := freeTestPort(t)
	// Occupy badPort with a foreign process so its server's Up fails the
	// precondition gate, while goodPort's server should still launch fine.
	foreign := spawnForeignListener(t, badPort)
	defer foreign.forceKill()

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "good-svc", goodPort, true),
			tdlistenerServer(t, "bad-svc", badPort, true),
		},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Up("")
	if err == nil {
		t.Fatal("expected an aggregate error since bad-svc's port is blocked")
	}
	msg := err.Error()
	if !strings.Contains(msg, "good-svc") {
		t.Fatalf("expected the multi-server aggregate to name the succeeding server too, got: %v", err)
	}
	if !strings.Contains(msg, "bad-svc") {
		t.Fatalf("expected the multi-server aggregate to name the failing server, got: %v", err)
	}

	var goodState ServerState
	for _, s := range eng.Status() {
		if s.Name == "good-svc" {
			goodState = s.State
		}
	}
	if goodState != StateUp {
		t.Fatalf("expected good-svc to still be launched despite bad-svc's failure, got %q", goodState)
	}
	if !strings.Contains(msg, ".log") {
		t.Fatalf("expected the aggregate's succeeding member to also carry its log path (SOP-108), got: %v", err)
	}
}

// --- SOP-108: log path printed to console on a successful start -----------

// A fully successful single-server up must print that server's log path to
// the console at that moment — today Up() bypasses formatAggregate entirely
// when there's exactly one target and nothing is printed on success.
func TestRealEngine_Up_SingleServer_PrintsLogPathOnSuccess(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	var upErr error
	stdout := captureStdout(t, func() {
		upErr = eng.Up("")
	})
	if upErr != nil {
		t.Fatalf("Up(\"\") error: %v", upErr)
	}

	if !strings.Contains(stdout, "svc") {
		t.Fatalf("expected console output to name the started server, got: %q", stdout)
	}
	if !strings.Contains(stdout, ".log") {
		t.Fatalf("expected console output to include the server's log path on a successful start, got: %q", stdout)
	}
}

// The imperative single-server form (`voice-in up` at the REPL -> Up(name))
// must print the tail hint too — that is the form actually used to start one
// server at a time, so it is the one most likely to want the log command.
func TestRun_ImperativeUp_SingleServer_PrintsTailHint(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	cmd, err := ParseCommand("svc up")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	// Up() writes its own lines straight to os.Stdout while dispatch writes
	// to `out`; capture the combined real-usage stream (see the duplication
	// test below) so the hint is observed where a user would see it.
	stdout := captureStdout(t, func() {
		dispatch(os.Stdout, eng, cmd)
	})

	if !strings.Contains(stdout, "tail -f ") {
		t.Fatalf("expected a `tail -f` hint after an imperative single-server up, got: %q", stdout)
	}
}

// A fully successful multi-server up must print every started server's log
// path, not just the one named in a failure.
func TestRealEngine_Up_MultiServer_AllSucceed_PrintsEachLogPath(t *testing.T) {
	portA := freeTestPort(t)
	portB := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "svc-a", portA, true),
			tdlistenerServer(t, "svc-b", portB, true),
		},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	var upErr error
	stdout := captureStdout(t, func() {
		upErr = eng.Up("")
	})
	if upErr != nil {
		t.Fatalf("Up(\"\") error: %v", upErr)
	}

	for _, name := range []string{"svc-a", "svc-b"} {
		if !strings.Contains(stdout, name) {
			t.Fatalf("expected console output to name %q, got: %q", name, stdout)
		}
	}
	// 2 outcome lines + the tail hint repeating both paths = 4.
	if strings.Count(stdout, ".log") != 4 {
		t.Fatalf("expected console output to include each server's log path in its outcome line and in the tail hint, got: %q", stdout)
	}
	if !strings.Contains(stdout, "tail -f ") {
		t.Fatalf("expected console output to include a `tail -f` hint covering the started servers' logs, got: %q", stdout)
	}
}

// --- tail hint ------------------------------------------------------------

// formatTailHint renders one copy-pasteable tail command over every outcome
// that produced a log file, and nothing when no launch produced one.
func TestFormatTailHint(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []verbOutcome
		want     string
	}{
		{
			name: "successes and failures with logs, skips logless",
			outcomes: []verbOutcome{
				{Name: "server", LogPath: "build/logs/server-1.log"},
				{Name: "already-up"}, // idempotent skip: no fresh log
				{Name: "voice-in", Err: os.ErrDeadlineExceeded, LogPath: "build/logs/voice-in-1.log"},
			},
			want: "tail -f build/logs/server-1.log build/logs/voice-in-1.log",
		},
		{
			name:     "nothing launched",
			outcomes: []verbOutcome{{Name: "already-up"}},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTailHint(tc.outcomes); got != tc.want {
				t.Errorf("formatTailHint: got %q, want %q", got, tc.want)
			}
		})
	}
}

// A multi-server up with a partial failure, driven through the real REPL
// dispatch path (not just Up()'s return value), must print each server's
// outcome line exactly once. Up() writes its outcome lines straight to
// os.Stdout as a side effect (§6.5, SOP-108), and dispatch separately
// prints whatever error Up() returns — if Up() always prints AND returns an
// error whose text repeats the same lines, every line would appear twice
// once dispatch's own print lands on the same stream (PR #69 review
// finding).
func TestRun_Up_MultiServer_PartialFailure_DispatchPrintsEachLineOnce(t *testing.T) {
	goodPort := freeTestPort(t)
	badPort := freeTestPort(t)

	foreign := spawnForeignListener(t, badPort)
	defer foreign.forceKill()

	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "good-svc", goodPort, true),
			tdlistenerServer(t, "bad-svc", badPort, true),
		},
	}
	eng := NewEngine(cfg)
	t.Cleanup(func() { eng.TeardownAll() })

	cmd, err := ParseCommand("up")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}

	// dispatch's own console writes and Up()'s internal side-effect print
	// both target os.Stdout in real usage (main.go wires Run(os.Stdin,
	// os.Stdout, engine)) — capture that combined stream, not dispatch's
	// `out` alone, or the duplication this test pins would go unobserved.
	stdout := captureStdout(t, func() {
		dispatch(os.Stdout, eng, cmd)
	})

	// Count outcome *lines* (not raw substring occurrences of the name —
	// a server's own §6.5 line legitimately repeats its name once as the
	// line's subject and again inside its embedded log path filename).
	lines := strings.Split(stdout, "\n")
	for _, prefix := range []string{"good-svc ", "bad-svc "} {
		var got int
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				got++
			}
		}
		if got != 1 {
			t.Fatalf("expected exactly one outcome line starting with %q, got %d: %q", prefix, got, stdout)
		}
	}
}

// TestFormatTailHint_QuotesPathsNeedingIt: the hint is advertised as
// copy-pasteable, so a log_dir containing a space (it is configurable) must
// not render a command the shell reads as several filenames.
func TestFormatTailHint_QuotesPathsNeedingIt(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"ordinary path is left bare", "build/logs/server-1.log", "tail -f build/logs/server-1.log"},
		{"space is quoted", "/My Logs/server-1.log", "tail -f '/My Logs/server-1.log'"},
		{"single quote is escaped", "/ian's/server-1.log", `tail -f '/ian'\''s/server-1.log'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTailHint([]verbOutcome{{Name: "server", LogPath: tc.path}})
			if got != tc.want {
				t.Errorf("formatTailHint(%q):\n got: %s\nwant: %s", tc.path, got, tc.want)
			}
		})
	}
}

// TestUp_SingleServerFailure_StillPrintsTailHint: a lone server failing to
// start is the commonest way into Up, and the case where its log matters
// most. A failing sibling in a multi-server up already got a hint; this one
// must too.
func TestUp_SingleServerFailure_StillPrintsTailHint(t *testing.T) {
	hint := formatTailHint([]verbOutcome{
		{Name: "svc", Err: os.ErrDeadlineExceeded, LogPath: "build/logs/svc-1.log"},
	})
	if !strings.Contains(hint, "build/logs/svc-1.log") {
		t.Fatalf("a failed server's log must be in the hint, got: %q", hint)
	}
}
