package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("Up(\"\") error: %v", err)
	}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateUp {
		t.Fatalf("expected server up, got %+v", statuses)
	}
}

// TestRealEngine_Up_BuildsHealthProbeFromSourceBeforeReachingHealthy is
// AATK-41's DoD #1: a fleet entry whose readiness check is a source-built
// probe program (health.source, the source-exec form) reaches healthy on
// Up() from a tree with no build artifacts — the probe binary does not exist
// anywhere until Up() itself builds it, exactly as the ticket's own datastore
// example describes (launch = "docker compose up", not something this fleet
// compiles; the probe is).
//
// The launch command here is the tdlistener fixture (a stand-in for "docker
// compose up" — an exec-type launch requiring no build of its own), while
// the health probe is the internal/lifecycle buildable_v1 fixture, declared
// via health.source rather than assumed to already exist.
func TestRealEngine_Up_BuildsHealthProbeFromSourceBeforeReachingHealthy(t *testing.T) {
	port := freeTestPort(t)
	bin := tdlistenerBinary(t)
	probeBin := filepath.Join(t.TempDir(), "db-probe")

	if _, err := os.Stat(probeBin); !os.IsNotExist(err) {
		t.Fatalf("precondition: probe binary must not exist yet, stat err: %v", err)
	}

	s := config.Server{
		Name:    "datastore",
		Type:    config.TypeExec,
		Enabled: true,
		Host:    "127.0.0.1",
		Listens: []int{port},
		Command: bin,
		Args:    []string{"-port", strconv.Itoa(port)},
		Health: config.Health{
			Exec: []string{probeBin},
			Source: config.SourceExec{
				Build:  "go build -o " + probeBin + " ../../internal/lifecycle/testdata/buildable_v1",
				Binary: probeBin,
			},
		},
	}

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("Up(\"\") error (health probe should be built from source, not require a pre-existing binary): %v", err)
	}

	if _, err := os.Stat(probeBin); err != nil {
		t.Fatalf("expected Up() to have built the health-probe binary, stat err: %v", err)
	}

	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].State != StateUp {
		t.Fatalf("expected server up (health probe built + probe passing), got %+v", statuses)
	}
}

// --- edge / boundary -----------------------------------------------------

func TestRealEngine_Up_AlreadyUpIsIdempotentSkip(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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
	eng := NewEngine(cfg, nil, nil)
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

// --- bounce (AATK-28) -----------------------------------------------------

// TestRealEngine_Bounce_UpServer_RestartsWithNewPID pins the core contract:
// bouncing a healthy server is a real teardown + relaunch, not a no-op. A
// changed PID is the only proof of that which cannot be faked by an
// implementation that merely re-runs the health check; the new process must
// also clear the same readiness gate a normal Up requires.
func TestRealEngine_Bounce_UpServer_RestartsWithNewPID(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}
	before := eng.Status()[0]
	if before.PID == 0 {
		t.Fatalf("expected a live PID before bounce, got %+v", before)
	}

	if err := eng.Bounce("svc"); err != nil {
		t.Fatalf("Bounce(\"svc\") error: %v", err)
	}

	after := eng.Status()[0]
	if after.State != StateUp {
		t.Fatalf("expected server up after bounce, got state %q (%+v)", after.State, after)
	}
	if after.PID == 0 {
		t.Fatalf("expected a non-zero PID after bounce, got %+v", after)
	}
	if after.PID == before.PID {
		t.Fatalf("expected bounce to relaunch with a NEW pid (proof of a real down+up, not a no-op), got %d both times", before.PID)
	}
}

// TestRealEngine_Bounce_DownServer_IsSynonymForUp pins Observable behavior 2:
// bounce on an already-down server is not an error case needing its own
// branch in Bounce — it falls out of composing Down (a no-op when nothing is
// running) with Up. The end state must be indistinguishable from a plain Up
// from the same starting point.
func TestRealEngine_Bounce_DownServer_IsSynonymForUp(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	// Never brought up — Down's half of the bounce has nothing to tear down.
	if err := eng.Bounce("svc"); err != nil {
		t.Fatalf("Bounce(\"svc\") on a down server must not error (it is a synonym for up), got: %v", err)
	}

	status := eng.Status()[0]
	if status.State != StateUp {
		t.Fatalf("expected server up after bouncing a down server, got state %q (%+v)", status.State, status)
	}
	if status.PID == 0 {
		t.Fatalf("expected a non-zero PID after bouncing a down server, got %+v", status)
	}
}

// TestRealEngine_Bounce_DownFailureDoesNotAttemptUp pins Observable behavior 4's
// fail-fast half: a teardown that cannot be verified clean must abort the
// bounce, never launch on top of it.
//
// The teardown failure is injected exactly the way teardown.go's own
// failure-path tests do it (TestTeardown_SurvivorAfterKill_IsLoudError):
// a declared port held by an INDEPENDENT process outside the torn-down
// group keeps portsFree false forever, so verify returns a loud
// "still listening" error no matter what the kill achieved.
//
// Proving Up was never *attempted* needs more than "no new process": Up
// would fail here too (checkPortConflict refuses a foreign-held port), so
// the absence of a process is consistent with either ordering. Up's failure
// path always writes to os.Stdout (printTailHint), so silent stdout is the
// unambiguous discriminator.
func TestRealEngine_Bounce_DownFailureDoesNotAttemptUp(t *testing.T) {
	portA := freeTestPort(t)
	portB := freeTestPort(t)

	s := tdlistenerServer(t, "svc", portA, true)
	// Both ports are declared, so teardown must verify both are free.
	s.Listens = []int{portA, portB}

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{s}}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	// portA's holder is what observedRunningPID finds and hands to teardown.
	// It ignores SIGTERM so teardown is driven through to SIGKILL and on to
	// the post-kill verification — the same lever the reference test pulls.
	spawnForeignListenerIgnoringTerm(t, portA)
	// portB's holder is independent of that group and survives the kill,
	// so the post-kill verification can never come back clean.
	spawnForeignListener(t, portB)

	var bounceErr error
	stdout := captureStdout(t, func() {
		bounceErr = eng.Bounce("svc")
	})

	if bounceErr == nil {
		t.Fatalf("expected Bounce to report the teardown failure rather than swallowing it")
	}
	if !strings.Contains(bounceErr.Error(), "still listening") {
		t.Fatalf("expected the surfaced error to be the teardown verification failure, got: %v", bounceErr)
	}
	if stdout != "" {
		t.Fatalf("expected Up to never be attempted after a failed Down — Up's failure path always prints to stdout, but got: %q", stdout)
	}
	if pid := eng.Status()[0].PID; pid != 0 {
		t.Fatalf("expected no server of ours running after a failed bounce, got pid %d", pid)
	}
}

// TestRealEngine_Bounce_EmptyNameIsRefused guards the engine seam behind the
// grammar's: Down("") and Up("") both mean "the whole fleet", so a Bounce that
// merely forwarded an empty name would silently cycle every enabled server —
// exactly the out-of-scope whole-fleet bounce, reachable by accident from any
// non-REPL caller.
func TestRealEngine_Bounce_EmptyNameIsRefused(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	err := eng.Bounce("")
	if err == nil {
		t.Fatalf("expected Bounce(\"\") to be refused rather than bouncing the whole fleet")
	}
	if !strings.Contains(err.Error(), "server name is required") {
		t.Fatalf("expected the refusal to name the missing argument, got: %v", err)
	}
	if pid := eng.Status()[0].PID; pid != 0 {
		t.Fatalf("expected Bounce(\"\") to touch nothing, but a server is running (pid %d)", pid)
	}
}

// --- config hot-reload (AATK-27) ------------------------------------------

// TestRealEngine_ReplaceConfig_RacesInFlightUp is the race the ticket is
// actually written against. Up fans its targets out across goroutines
// (engine.go's per-target WaitGroup), and each of those goroutines reads the
// engine's config on the way to launching — log dir, ready timeout, poll
// interval, grace period. A reload landing from the dispatch loop while that
// fan-out is in flight writes the same field those goroutines are reading.
//
// Under -race this fails loudly if config access is unsynchronized; it also
// pins that the swap leaves a coherent final state rather than a torn one.
func TestRealEngine_ReplaceConfig_RacesInFlightUp(t *testing.T) {
	portA := freeTestPort(t)
	portB := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			tdlistenerServer(t, "svc-a", portA, true),
			tdlistenerServer(t, "svc-b", portB, true),
		},
	}
	eng := NewEngine(cfg, nil, nil)
	t.Cleanup(func() { eng.TeardownAll() })

	// A replacement config that differs in a field the launch path reads, so
	// a racing reader would observe a genuinely different value rather than
	// an identical copy.
	replacement := cfg
	replacement.Supervisor.ReadyTimeout = config.Duration{Duration: 7 * time.Second}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Hammer the swap while Up's own goroutines read the config.
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				eng.ReplaceConfig(replacement)
			} else {
				eng.ReplaceConfig(cfg)
			}
		}
	}()

	upErr := eng.Up("")
	wg.Wait()

	if upErr != nil {
		t.Fatalf("Up(\"\") raced with ReplaceConfig: %v", upErr)
	}
	if got := len(eng.Status()); got != 2 {
		t.Fatalf("expected a coherent config after the racing swaps (2 servers), got %d", got)
	}
}

// --- [server.prompt] (AATK-26) --------------------------------------------

// promptedServer is a tdlistener exec fixture carrying a [server.prompt], so
// the launched process's own argv is the evidence of which branch was taken:
// tdlistener rejects unknown flags, so an arg that reached it is an arg that
// was actually on the command line.
func promptedServer(t *testing.T, name string, port int, spec *config.PromptSpec) config.Server {
	t.Helper()
	s := tdlistenerServer(t, name, port, true)
	s.Prompt = spec
	return s
}

// schemeSpec branches on an argument tdlistener understands, so the answer is
// observable in the child's behavior rather than only in our own bookkeeping.
func schemeSpec() *config.PromptSpec {
	return &config.PromptSpec{
		Question: "Use plaintext ws for this run?",
		YesArgs:  []string{"-ignore-term"},
		NoArgs:   []string{"-child-port", "0"},
	}
}

// launchedArgs reports the argv of the process the engine actually started.
// This is the only honest oracle for "the chosen branch's args were appended":
// Command() reports the args resolved from the STORED config, and the prompt
// answer exists only at launch time, so Command() can never reflect it.
func launchedArgs(t *testing.T, eng *RealEngine, name string) []string {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	proc, ok := eng.procs[name]
	if !ok {
		t.Fatalf("no launched process recorded for %q", name)
	}
	return proc.Cmd.Args
}

func TestRealEngine_Up_PromptedServer_YesAnswerAppendsYesArgs(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, schemeSpec())},
	}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}
	if !strings.Contains(promptOut.String(), "Use plaintext ws for this run?") {
		t.Fatalf("expected the question to be printed, got: %q", promptOut.String())
	}

	got := strings.Join(launchedArgs(t, eng, "svc"), " ")
	if !strings.Contains(got, "-ignore-term") {
		t.Fatalf("expected the yes branch's args appended to the resolved launch args, got %q", got)
	}
	if strings.Contains(got, "-child-port") {
		t.Fatalf("a yes answer must not also take the no branch, got %q", got)
	}
}

func TestRealEngine_Up_PromptedServer_NoAnswerAppendsNoArgs(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, schemeSpec())},
	}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("n\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}
	got := strings.Join(launchedArgs(t, eng, "svc"), " ")
	if !strings.Contains(got, "-child-port 0") {
		t.Fatalf("expected the no branch's args appended, got %q", got)
	}
	if strings.Contains(got, "-ignore-term") {
		t.Fatalf("a no answer must not take the yes branch, got %q", got)
	}
}

// TestRealEngine_Up_UnpromptedServerUnchanged is the regression guard: a
// server with no [server.prompt] must be byte-identical to today — nothing
// printed, nothing appended.
func TestRealEngine_Up_UnpromptedServerUnchanged(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{tdlistenerServer(t, "svc", port, true)},
	}
	before := cfg.Servers[0].Args

	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}
	if promptOut.String() != "" {
		t.Fatalf("an unprompted server must print nothing to the prompt stream, got %q", promptOut.String())
	}
	_, args, err := eng.Command("svc")
	if err != nil {
		t.Fatalf("Command(\"svc\"): %v", err)
	}
	if strings.Join(args, " ") != strings.Join(before, " ") {
		t.Fatalf("an unprompted server's args must be unchanged: was %v, now %v", before, args)
	}
}

// TestRealEngine_Up_RepromptsOnInvalidAnswer pins that a typo is recoverable
// rather than fatal or silently defaulted.
func TestRealEngine_Up_RepromptsOnInvalidAnswer(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, schemeSpec())},
	}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("maybe\ny\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error after a reprompt: %v", err)
	}
	if n := strings.Count(promptOut.String(), "Use plaintext ws for this run?"); n != 2 {
		t.Fatalf("expected the question asked exactly twice (initial + one reprompt), got %d in %q", n, promptOut.String())
	}
	if got := strings.Join(launchedArgs(t, eng, "svc"), " "); !strings.Contains(got, "-ignore-term") {
		t.Fatalf("expected the eventual y answer to take the yes branch, got %q", got)
	}
}

// TestRealEngine_Up_BulkUp_PromptsSequentialBeforeAnyLaunch is behavior 4, and
// the reason the prompt pre-pass exists at all. Asking inside upOne would put
// concurrent reads on one stdin from the WaitGroup fan-out.
//
// Asserting "both eventually launched" would not catch that. The discriminator
// is ordering: every question must appear on the prompt stream before the
// FIRST process starts, so the check is against launch time, not launch count.
func TestRealEngine_Up_BulkUp_PromptsSequentialBeforeAnyLaunch(t *testing.T) {
	portA, portB := freeTestPort(t), freeTestPort(t)
	specA := &config.PromptSpec{Question: "QUESTION-A?", YesArgs: []string{"-serve-health"}}
	specB := &config.PromptSpec{Question: "QUESTION-B?", YesArgs: []string{"-serve-health"}}
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			promptedServer(t, "svc-a", portA, specA),
			promptedServer(t, "svc-b", portB, specB),
		},
	}

	// watchedWriter records the prompt text as it is written, and snapshots
	// what had been written at the moment the first child process appeared.
	out := &watchedWriter{}
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\ny\n")), out)
	t.Cleanup(func() { eng.TeardownAll() })
	out.firstLaunchSeen = func() bool {
		for _, s := range eng.Status() {
			if s.PID != 0 {
				return true
			}
		}
		return false
	}

	if err := eng.Up(""); err != nil {
		t.Fatalf("bulk Up(\"\") error: %v", err)
	}

	atFirstLaunch := out.snapshotAtFirstLaunch()
	if !strings.Contains(atFirstLaunch, "QUESTION-A?") || !strings.Contains(atFirstLaunch, "QUESTION-B?") {
		t.Fatalf("both questions must be asked before ANY server launches; prompt output at first launch was: %q", atFirstLaunch)
	}
}

// watchedWriter is the prompt-output sink for the sequencing test. Every write
// checks whether a child process has appeared yet, and the first time one has,
// it freezes a copy of everything written so far.
type watchedWriter struct {
	mu              sync.Mutex
	buf             strings.Builder
	snapshot        string
	snapped         bool
	firstLaunchSeen func() bool
}

func (w *watchedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if !w.snapped && w.firstLaunchSeen != nil && w.firstLaunchSeen() {
		w.snapshot = w.buf.String()
		w.snapped = true
	}
	return n, err
}

func (w *watchedWriter) snapshotAtFirstLaunch() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.snapped {
		return w.snapshot
	}
	// No launch was ever observed mid-write, which means every write
	// completed before the first process started — the property under test.
	return w.buf.String()
}

// launchedEnv reports the environment of the process the engine actually
// started. The env sibling of launchedArgs, and the same oracle argument
// applies: the prompt answer exists only at launch time, so nothing derived
// from the stored config can reflect it.
func launchedEnv(t *testing.T, eng *RealEngine, name string) []string {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	proc, ok := eng.procs[name]
	if !ok {
		t.Fatalf("no launched process recorded for %q", name)
	}
	return proc.Cmd.Env
}

// promptEnvProbeVar is the variable the env-branching specs below set. One
// name, so "the yes value" and "the no value" cannot drift onto different keys.
const promptEnvProbeVar = "AATOOLKIT_PROMPT_ENV_PROBE"

// envSpec branches on an environment variable rather than an argument — the env
// counterpart of schemeSpec.
func envSpec() *config.PromptSpec {
	return &config.PromptSpec{
		Question: "Use the local endpoint for this run?",
		YesEnv:   map[string]string{promptEnvProbeVar: "yes-value"},
		NoEnv:    map[string]string{promptEnvProbeVar: "no-value"},
	}
}

// TestRealEngine_Up_PromptedServer_YesAnswerAppliesYesEnv pins AATK-32
// observable behavior 2 on the yes branch, end to end: the answer has to reach
// the child's environment, not merely the resolved config. Asserting on
// Cmd.Env covers the whole chain — askPrompts -> Server.Env -> LaunchSpec.Env
// -> mergeEnv -> cmd.Env — so no hop can quietly drop the value.
func TestRealEngine_Up_PromptedServer_YesAnswerAppliesYesEnv(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, envSpec())},
	}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}

	env := launchedEnv(t, eng, "svc")
	want := promptEnvProbeVar + "=yes-value"
	if !slices.Contains(env, want) {
		t.Fatalf("expected %q in the launched child's env, got %v", want, promptEnvEntries(env))
	}
	if notWant := promptEnvProbeVar + "=no-value"; slices.Contains(env, notWant) {
		t.Fatalf("a yes answer must not also take the no branch, but found %q", notWant)
	}
}

// TestRealEngine_Up_PromptedServer_NoAnswerAppliesNoEnv is the same for the no
// branch — the branch not taken contributes nothing.
func TestRealEngine_Up_PromptedServer_NoAnswerAppliesNoEnv(t *testing.T) {
	port := freeTestPort(t)
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, envSpec())},
	}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("n\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}

	env := launchedEnv(t, eng, "svc")
	want := promptEnvProbeVar + "=no-value"
	if !slices.Contains(env, want) {
		t.Fatalf("expected %q in the launched child's env, got %v", want, promptEnvEntries(env))
	}
	if notWant := promptEnvProbeVar + "=yes-value"; slices.Contains(env, notWant) {
		t.Fatalf("a no answer must not also take the yes branch, but found %q", notWant)
	}
}

// TestRealEngine_Up_PromptedServer_BranchEnvOverridesStaticEnv is the
// interaction case: the same key declared both statically and by the chosen
// branch. The branch wins, and it wins by *replacing* — exactly one entry for
// the key reaches the child, because a duplicate would leave which value the
// child reads up to os/exec's last-wins behavior rather than to this config.
func TestRealEngine_Up_PromptedServer_BranchEnvOverridesStaticEnv(t *testing.T) {
	port := freeTestPort(t)
	srv := promptedServer(t, "svc", port, &config.PromptSpec{
		Question: "Use the local endpoint for this run?",
		YesEnv:   map[string]string{promptEnvProbeVar: "chosen"},
	})
	srv.Env = map[string]string{promptEnvProbeVar: "static"}

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{srv}}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}

	got := promptEnvEntries(launchedEnv(t, eng, "svc"))
	if len(got) != 1 {
		t.Fatalf("child env has %d entries for %s (%v), want exactly 1 — a duplicate leaves the winner to os/exec", len(got), promptEnvProbeVar, got)
	}
	if want := promptEnvProbeVar + "=chosen"; got[0] != want {
		t.Errorf("child env has %q, want %q — the chosen branch must beat the static env", got[0], want)
	}
}

// promptEnvEntries filters an environment down to the probe variable, so a
// failure message shows the relevant entries instead of the whole environment.
func promptEnvEntries(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, promptEnvProbeVar+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// TestRealEngine_Up_PromptedServer_BranchEnvKeepsUnrelatedStaticEnv is the
// merge-versus-replace test, and the reason the rest of the env suite is not
// sufficient on its own.
//
// Every other env test uses a single key, with the static env either nil or
// holding only that same key — which makes "merge the branch into Env" and
// "replace Env with the branch" observationally identical. An implementation
// that assigns the branch map wholesale passes all of them while silently
// dropping every static key the branch does not restate: tap settings, model
// names, PYTHONPATH, credentials. So this server declares a static key the
// branch never mentions, and a branch carrying a second key of its own.
func TestRealEngine_Up_PromptedServer_BranchEnvKeepsUnrelatedStaticEnv(t *testing.T) {
	const keepVar, secondVar = "AATOOLKIT_PROMPT_ENV_KEEP", "AATOOLKIT_PROMPT_ENV_SECOND"

	port := freeTestPort(t)
	srv := promptedServer(t, "svc", port, &config.PromptSpec{
		Question: "Use the local endpoint for this run?",
		YesEnv: map[string]string{
			promptEnvProbeVar: "chosen",
			secondVar:         "two",
		},
	})
	srv.Env = map[string]string{
		promptEnvProbeVar: "static",  // overridden by the branch
		keepVar:           "keep-me", // untouched by the branch — must survive
	}

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{srv}}
	var promptOut strings.Builder
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\") error: %v", err)
	}

	env := launchedEnv(t, eng, "svc")
	for _, want := range []string{
		promptEnvProbeVar + "=chosen",
		secondVar + "=two",
		keepVar + "=keep-me",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q from the launched child's env — the branch must MERGE into the static env, not replace it", want)
		}
	}
}

// TestRealEngine_Up_BulkUp_EachServerGetsItsOwnAnswersEnv pins the answers to
// their own servers across a bulk Up, and rules out a whole class of wrong
// implementation the single-server tests cannot see.
//
// Two things are being caught. First, mispairing: the branch resolved once
// outside the loop, or written to resolved[0], gives both servers the same
// value. The existing sequencing test cannot detect that — its two specs carry
// identical YesArgs and both are answered yes. Second, and worse, an
// implementation that "injects" the branch with os.Setenv on the supervisor
// itself would satisfy every other test here, because mergeEnv bases on
// os.Environ() — while leaking the chosen value into every server launched
// afterwards and persisting for the life of the REPL. The unprompted sibling is
// what makes that leak visible.
func TestRealEngine_Up_BulkUp_EachServerGetsItsOwnAnswersEnv(t *testing.T) {
	portA, portB, portC := freeTestPort(t), freeTestPort(t), freeTestPort(t)
	specA := &config.PromptSpec{
		Question: "QUESTION-A?",
		YesEnv:   map[string]string{promptEnvProbeVar: "a-yes"},
		NoEnv:    map[string]string{promptEnvProbeVar: "a-no"},
	}
	specB := &config.PromptSpec{
		Question: "QUESTION-B?",
		YesEnv:   map[string]string{promptEnvProbeVar: "b-yes"},
		NoEnv:    map[string]string{promptEnvProbeVar: "b-no"},
	}
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers: []config.Server{
			promptedServer(t, "svc-a", portA, specA),
			promptedServer(t, "svc-b", portB, specB),
			tdlistenerServer(t, "svc-c", portC, true), // no prompt at all
		},
	}

	var promptOut strings.Builder
	// Answers are consumed in declaration order: yes for svc-a, no for svc-b.
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\nn\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up(""); err != nil {
		t.Fatalf("bulk Up(\"\") error: %v", err)
	}

	if got := promptEnvEntries(launchedEnv(t, eng, "svc-a")); len(got) != 1 || got[0] != promptEnvProbeVar+"=a-yes" {
		t.Errorf("svc-a env = %v, want exactly [%s=a-yes] — its own yes answer", got, promptEnvProbeVar)
	}
	if got := promptEnvEntries(launchedEnv(t, eng, "svc-b")); len(got) != 1 || got[0] != promptEnvProbeVar+"=b-no" {
		t.Errorf("svc-b env = %v, want exactly [%s=b-no] — its own no answer", got, promptEnvProbeVar)
	}
	if got := promptEnvEntries(launchedEnv(t, eng, "svc-c")); len(got) != 0 {
		t.Errorf("svc-c env = %v, want none — an unprompted server must not inherit another server's answer", got)
	}
}

// promptedSourceServer is the source-type counterpart of promptedServer: a
// genuinely buildable, health-checkable server that also declares a prompt, so
// the build verb's relaunch path can be exercised against a real rebuild rather
// than a stub.
func promptedSourceServer(t *testing.T, name string, port int, binPath string, spec *config.PromptSpec) config.Server {
	t.Helper()
	s := tdlistenerSourceServer(t, name, port, binPath)
	s.Prompt = spec
	return s
}

// TestRealEngine_Build_PromptedServerReAsksAndAppliesTheNewAnswer pins AATK-34's
// contract: the build verb re-asks the prompt and relaunches on the freshly
// chosen branch.
//
// The server comes up on the yes branch, then is rebuilt with "n" queued. Both
// halves matter. The question must be asked again — Build resolved its target
// from the STORED config, where no answer exists, so without re-asking the
// rebuilt child carries neither branch. And the answer that takes effect must be
// the new one: a fix that cached the first answer would satisfy "the branch
// survived a rebuild" while quietly reintroducing exactly the remembered state
// the prompt design exists to avoid.
//
// The binary is corrupted between the up and the build on purpose. PerformBuild
// returns early when nothing is stale, so a build against the freshly-built
// binary would never reach the stop/start path at all and this test would assert
// nothing about a relaunch. Making it genuinely stale is what puts the relaunch
// on the path being tested.
func TestRealEngine_Build_PromptedServerReAsksAndAppliesTheNewAnswer(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-prompted")
	srv := promptedSourceServer(t, "svc", port, binPath, envSpec())

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{srv}}
	var promptOut strings.Builder
	// "y" for the initial up, then "n" for the rebuild.
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\nn\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\"): %v", err)
	}
	if got := promptEnvEntries(launchedEnv(t, eng, "svc")); len(got) != 1 || got[0] != promptEnvProbeVar+"=yes-value" {
		t.Fatalf("after up, child env = %v, want exactly [%s=yes-value]", got, promptEnvProbeVar)
	}
	asksAfterUp := strings.Count(promptOut.String(), "Use the local endpoint for this run?")

	// Diverge the on-disk binary from what a fresh build produces, so the
	// staleness probe fires and the rebuild actually stops and relaunches.
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staling the binary: %v", err)
	}

	if err := eng.Build("svc"); err != nil {
		t.Fatalf("Build(\"svc\"): %v", err)
	}

	if asks := strings.Count(promptOut.String(), "Use the local endpoint for this run?"); asks <= asksAfterUp {
		t.Errorf("the question was asked %d time(s) total, same as after up — build must re-ask, since the stored config carries no answer", asks)
	}
	got := promptEnvEntries(launchedEnv(t, eng, "svc"))
	if len(got) != 1 || got[0] != promptEnvProbeVar+"=no-value" {
		t.Errorf("after build, child env = %v, want exactly [%s=no-value] — the rebuilt server must run on the freshly chosen branch", got, promptEnvProbeVar)
	}
}

// TestRealEngine_Build_PromptedServerRefusesWhenTheAnswerCannotBeRead is the
// error path: when the rebuild needs an answer and cannot get one, it must refuse
// and name the server rather than relaunch on whatever the binary's own defaults
// are — which is exactly today's silent behavior.
//
// Input is exhausted deliberately: one answer is queued, the up consumes it, and
// the build's question then hits EOF. That is the honest way to reach this path.
// An engine constructed with nil streams cannot be used, because with no process
// of ours running there is no lifecycle at all — PerformBuild replaces the binary
// and returns without ever calling Start, so nothing would ask and nothing would
// fail.
//
// The server is left down with the binary replaced. That is the correct outcome
// of a refused relaunch, and better than the alternative of guessing a branch.
func TestRealEngine_Build_PromptedServerRefusesWhenTheAnswerCannotBeRead(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-prompted")
	srv := promptedSourceServer(t, "svc", port, binPath, envSpec())

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{srv}}
	var promptOut strings.Builder
	// Exactly one answer: the up takes it, the build finds EOF.
	eng := NewEngine(cfg, bufio.NewReader(strings.NewReader("y\n")), &promptOut)
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\"): %v", err)
	}
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staling the binary: %v", err)
	}

	err := eng.Build("svc")
	if err == nil {
		t.Fatal("Build succeeded with no answer available, want a refusal rather than a relaunch on defaults")
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Errorf("error %q does not name the offending server", err)
	}
}

// TestRealEngine_Build_UnpromptedServerAsksNothing is a guard, not a red test:
// it passes before AATK-34 and after. It is here because the fix put an
// askPrompts call on the rebuild path, and askPrompts errors when it has no
// streams to ask on — so the way to get this wrong is to ask unconditionally and
// break every rebuild of every unprompted server, including from an engine
// constructed with nil streams as most tests do.
func TestRealEngine_Build_UnpromptedServerAsksNothing(t *testing.T) {
	port := freeTestPort(t)
	binPath := filepath.Join(t.TempDir(), "tdlistener-unprompted")
	srv := tdlistenerSourceServer(t, "svc", port, binPath)

	cfg := config.Config{Supervisor: testSupervisor(t), Servers: []config.Server{srv}}
	eng := NewEngine(cfg, nil, nil) // no streams at all
	t.Cleanup(func() { eng.TeardownAll() })

	if err := eng.Up("svc"); err != nil {
		t.Fatalf("Up(\"svc\"): %v", err)
	}
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staling the binary: %v", err)
	}

	if err := eng.Build("svc"); err != nil {
		t.Fatalf("Build on an unprompted server must not need streams to ask on: %v", err)
	}
	if got := eng.Status(); len(got) != 1 || got[0].State != StateUp {
		t.Errorf("expected the rebuilt server back up, got %+v", got)
	}
}
