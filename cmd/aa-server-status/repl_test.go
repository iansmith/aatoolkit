package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// fakeEngine records calls made against it, for asserting dispatch behavior
// (in particular the exit-rule distinction between TeardownAll and Down).
type fakeEngine struct {
	statuses           []ServerStatus
	teardownCalls      int
	teardownReturn     []string
	downCalls          []string
	upCalls            []string
	deadCalls          []string
	buildCalls         []string
	logsCalls          []string
	killCalls          []int
	killErr            error
	commandCalls       []string
	commandResult      struct {
		cmd  string
		args []string
	}
	commandErr         error
	viewCalls          []struct{ name string; nowrap bool }
	viewResult         []string
	viewErr            error
	failNotImplemented bool
}

func (f *fakeEngine) Status() []ServerStatus { return f.statuses }

func (f *fakeEngine) Up(name string) error {
	f.upCalls = append(f.upCalls, name)
	if f.failNotImplemented {
		return fmt.Errorf("not implemented: lifecycle engine lands in SOP-11")
	}
	return nil
}

func (f *fakeEngine) Down(name string) error {
	f.downCalls = append(f.downCalls, name)
	if f.failNotImplemented {
		return fmt.Errorf("not implemented: lifecycle engine lands in SOP-11")
	}
	return nil
}

func (f *fakeEngine) Dead(name string) error {
	f.deadCalls = append(f.deadCalls, name)
	return fmt.Errorf("not implemented: lifecycle engine lands in SOP-11")
}

func (f *fakeEngine) Build(name string) error {
	f.buildCalls = append(f.buildCalls, name)
	return fmt.Errorf("not implemented: lifecycle engine lands in SOP-11")
}

func (f *fakeEngine) Logs(name string) ([]string, error) {
	f.logsCalls = append(f.logsCalls, name)
	return nil, fmt.Errorf("not implemented: lifecycle engine lands in SOP-11")
}

func (f *fakeEngine) Kill(pid int) error {
	f.killCalls = append(f.killCalls, pid)
	return f.killErr
}

func (f *fakeEngine) Command(name string) (string, []string, error) {
	f.commandCalls = append(f.commandCalls, name)
	if f.commandErr != nil {
		return "", nil, f.commandErr
	}
	return f.commandResult.cmd, f.commandResult.args, nil
}

func (f *fakeEngine) View(name string, nowrap bool) ([]string, error) {
	f.viewCalls = append(f.viewCalls, struct{ name string; nowrap bool }{name, nowrap})
	if f.viewErr != nil {
		return nil, f.viewErr
	}
	return f.viewResult, nil
}

func (f *fakeEngine) TeardownAll() []string {
	f.teardownCalls++
	return f.teardownReturn
}

var _ Engine = (*fakeEngine)(nil)

// --- edge / boundary cases ---

func TestRun_EmptyInputPrintsStatusOnLaunchAndOnBareEnter(t *testing.T) {
	// Use "help" (not another status trigger) as the second line so the
	// count below can only be satisfied by two DISTINCT status prints: one
	// at launch and one for the bare-Enter line. A no-op Run that merely
	// echoes the engine's status once per loop iteration would fail this,
	// since "help" must not itself print the status table. Count the
	// status-table header line rather than the server name, since the
	// prompt string itself also contains "server" as a substring.
	eng := &fakeEngine{statuses: []ServerStatus{{Name: "server", Enabled: true, State: "unknown"}}}
	in := strings.NewReader("\nhelp\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if got := strings.Count(out.String(), "SERVER"); got != 2 {
		t.Fatalf("expected status table header printed exactly twice (launch + bare enter, not for help), got %d times in:\n%s", got, out.String())
	}
}

func TestPrintStatus_EmitsAllSevenColumnsInOrder(t *testing.T) {
	var out strings.Builder
	printStatus(&out, []ServerStatus{{
		Name:    "server",
		Type:    "mlx",
		Enabled: true,
		State:   StateUp,
		Ports:   []PortStatus{{Port: 8080, Up: true}},
		PID:     4242,
		Health:  "/v1/models 200",
	}})
	got := out.String()

	for _, want := range []string{"SERVER", "TYPE", "DESIRED", "STATE", "PORTS", "PID", "HEALTH"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected header to contain %q, got:\n%s", want, got)
		}
	}
	for _, want := range []string{"server", "mlx", "up", "4242", "/v1/models 200", "8080 ✓"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, got)
		}
	}
}

func TestPrintSingleStatus_UsesSameSevenColumnHeader(t *testing.T) {
	statuses := []ServerStatus{{Name: "server", Type: "mlx", Enabled: true, State: StateUp}}
	var out strings.Builder
	printSingleStatus(&out, statuses, "server")
	for _, want := range []string{"SERVER", "TYPE", "DESIRED", "STATE", "PORTS", "PID", "HEALTH"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected single-row variant to contain column %q, got:\n%s", want, out.String())
		}
	}
}

// Adversary gap test (Step 0f): a per-row anomaly must not bleed into a
// neighboring row when the whole table is printed together.
func TestPrintStatus_MultipleRowsDoNotBleedAnomalyState(t *testing.T) {
	var out strings.Builder
	printStatus(&out, []ServerStatus{
		{Name: "clean", State: StateUp, Enabled: true},
		{Name: "haunted", State: StateStray, AnomalyDetail: "pid 9999, foreign"},
	})
	got := out.String()

	cleanLine, hauntedLine := "", ""
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "clean") {
			cleanLine = line
		}
		if strings.HasPrefix(line, "haunted") {
			hauntedLine = line
		}
	}
	if strings.Contains(cleanLine, "STRAY") {
		t.Fatalf("clean row must not inherit the neighboring row's STRAY annotation, got %q", cleanLine)
	}
	if !strings.Contains(hauntedLine, "STRAY (pid 9999, foreign)") {
		t.Fatalf("haunted row must carry its own STRAY annotation, got %q", hauntedLine)
	}
}

func TestRun_EOFTearsDownAndExitsCleanly(t *testing.T) {
	eng := &fakeEngine{teardownReturn: []string{"server"}}
	in := strings.NewReader("") // immediate EOF, no lines at all
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run on EOF: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once on EOF, got %d", eng.teardownCalls)
	}
}

func TestRun_EOFAfterCommandsWithNoTrailingNewlineStillTearsDownOnce(t *testing.T) {
	eng := &fakeEngine{}
	// No trailing newline before EOF — a classic bufio.Scanner boundary:
	// the last line must still be processed exactly once, and EOF must
	// still drive exactly one teardown.
	in := strings.NewReader("myserver up")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.upCalls) != 1 || eng.upCalls[0] != "myserver" {
		t.Fatalf("expected the final line without a trailing newline to still dispatch Up(\"myserver\") exactly once, got %v", eng.upCalls)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected EOF to trigger TeardownAll exactly once, got %d", eng.teardownCalls)
	}
}

func TestRun_QuitWithZeroOwnedServersStillCallsTeardownAll(t *testing.T) {
	eng := &fakeEngine{teardownReturn: []string{}}
	in := strings.NewReader("quit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("TeardownAll must be called even when nothing is owned (not skipped as a no-op optimization), got %d calls", eng.teardownCalls)
	}
}

// --- error / rejection cases ---

func TestRun_UnknownCommandPrintsErrorAndContinuesLoop(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("myserver up now\nquit\n") // 3 tokens: a syntax error, not just an unknown name
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "error") && !strings.Contains(strings.ToLower(out.String()), "unknown") {
		t.Fatalf("expected a loud error message for unknown command, got:\n%s", out.String())
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected the loop to continue after the bad command and still process 'quit' -> teardown once, got %d teardown calls", eng.teardownCalls)
	}
}

// A single unrecognized token parses fine at the grammar layer (bare-name
// status form) but must still be a loud error at dispatch time, since no
// such server exists in the engine's status list.
func TestRun_UnknownServerNameIsLoudError(t *testing.T) {
	eng := &fakeEngine{statuses: []ServerStatus{{Name: "server", Enabled: true, State: "unknown"}}}
	in := strings.NewReader("frobnicate\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "error") && !strings.Contains(strings.ToLower(out.String()), "no such server") {
		t.Fatalf("expected a loud error naming the unknown server, got:\n%s", out.String())
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected the loop to continue after the bad name and still process 'quit' -> teardown once, got %d teardown calls", eng.teardownCalls)
	}
}

func TestRun_DownVerbDoesNotTriggerTeardownAll(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("down\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once (from quit, not from down), got %d", eng.teardownCalls)
	}
	if len(eng.downCalls) != 1 {
		t.Fatalf("expected the bare 'down' verb to route through Down(), got downCalls=%v", eng.downCalls)
	}
}

func TestRun_DownVerbFailureDoesNotTriggerTeardownAll(t *testing.T) {
	// down/up/dead/build are stubs that return a real "not implemented"
	// error in production. A failing Down() must NOT fall through to
	// TeardownAll as an accidental "recovery path" — down and exit are
	// distinct, non-overlapping code paths regardless of Down's outcome.
	eng := &fakeEngine{failNotImplemented: true}
	in := strings.NewReader("down\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once (from quit only, even though down failed), got %d", eng.teardownCalls)
	}
	if len(eng.downCalls) != 1 {
		t.Fatalf("expected 'down' to still route through Down() despite its error, got downCalls=%v", eng.downCalls)
	}
}

func TestRun_DeadVerbDispatchesAndDoesNotTeardown(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("dead\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.deadCalls) != 1 {
		t.Fatalf("expected 'dead' verb to dispatch to Dead(), got deadCalls=%v", eng.deadCalls)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected TeardownAll called exactly once (from quit, not from dead), got %d", eng.teardownCalls)
	}
}

func TestRun_HelpVerbPrintsUsageWithoutEngineCalls(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("help\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected 'help' to print some usage text")
	}
	if len(eng.upCalls) != 0 || len(eng.downCalls) != 0 || len(eng.deadCalls) != 0 ||
		len(eng.buildCalls) != 0 || len(eng.logsCalls) != 0 {
		t.Fatalf("expected 'help' to make no engine calls, got up=%v down=%v dead=%v build=%v logs=%v",
			eng.upCalls, eng.downCalls, eng.deadCalls, eng.buildCalls, eng.logsCalls)
	}
}

// --- cross-feature interaction ---

func TestRun_QuitExitByeAllTriggerExactlyOneTeardown(t *testing.T) {
	for _, word := range []string{"quit", "exit", "bye"} {
		t.Run(word, func(t *testing.T) {
			eng := &fakeEngine{}
			in := strings.NewReader(word + "\n")
			var out strings.Builder

			if err := Run(in, &out, eng); err != nil {
				t.Fatalf("Run(%q): unexpected error: %v", word, err)
			}
			if eng.teardownCalls != 1 {
				t.Fatalf("%q should trigger TeardownAll exactly once, got %d", word, eng.teardownCalls)
			}
		})
	}
}

func TestRun_PerServerVerbDispatchesToEngine(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("myserver up\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.upCalls) != 1 || eng.upCalls[0] != "myserver" {
		t.Fatalf("expected Up(\"myserver\") called once, got %v", eng.upCalls)
	}
}

// --- happy path ---

func TestRun_QuitExitsLoop(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("quit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected teardown once on quit, got %d", eng.teardownCalls)
	}
}

func testConfigForCompile() config.Config { return config.Config{} } // keeps config import honest pre-impl

// --- kill verb dispatch ---

func TestRun_KillDispatchesToEngine(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("kill 9999\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.killCalls) != 1 || eng.killCalls[0] != 9999 {
		t.Fatalf("expected Kill(9999) called once, got %v", eng.killCalls)
	}
}

func TestRun_KillErrorIsPrintedAndLoopContinues(t *testing.T) {
	eng := &fakeEngine{killErr: fmt.Errorf("no such process")}
	in := strings.NewReader("kill 9999\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "no such process") {
		t.Fatalf("expected kill error to appear in output, got:\n%s", out.String())
	}
	if eng.teardownCalls != 1 {
		t.Fatalf("expected loop to continue after kill error and process quit, got %d teardown calls", eng.teardownCalls)
	}
}

// --- command verb dispatch ---

func TestRun_CommandDispatchesToEngine(t *testing.T) {
	eng := &fakeEngine{
		commandResult: struct {
			cmd  string
			args []string
		}{cmd: "mlx-serve", args: []string{"serve", "model", "--host", "127.0.0.1", "--port", "1235"}},
	}
	in := strings.NewReader("command chat-llm\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.commandCalls) != 1 || eng.commandCalls[0] != "chat-llm" {
		t.Fatalf("expected Command(\"chat-llm\") called once, got %v", eng.commandCalls)
	}
	if !strings.Contains(out.String(), "mlx-serve") {
		t.Fatalf("expected command output to contain 'mlx-serve', got:\n%s", out.String())
	}
}

func TestRun_CommandErrorIsPrintedAndLoopContinues(t *testing.T) {
	eng := &fakeEngine{commandErr: fmt.Errorf("no such server")}
	in := strings.NewReader("command nonexistent\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "no such server") {
		t.Fatalf("expected command error to appear in output, got:\n%s", out.String())
	}
}

// --- help text ---

func TestRun_HelpMentionsKillAndCommand(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("help\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "kill") {
		t.Fatalf("expected help text to mention 'kill', got:\n%s", output)
	}
	if !strings.Contains(output, "command") {
		t.Fatalf("expected help text to mention 'command', got:\n%s", output)
	}
}

// --- view verb dispatch ---

func TestRun_ViewDispatchesToEngine(t *testing.T) {
	eng := &fakeEngine{viewResult: []string{"line1", "line2", "line3"}}
	in := strings.NewReader("chat-llm view\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.viewCalls) != 1 || eng.viewCalls[0].name != "chat-llm" || eng.viewCalls[0].nowrap != false {
		t.Fatalf("expected View(\"chat-llm\", false) called once, got %v", eng.viewCalls)
	}
	output := out.String()
	for _, want := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}
}

func TestRun_ViewNowrapDispatchesToEngine(t *testing.T) {
	eng := &fakeEngine{viewResult: []string{"truncated"}}
	in := strings.NewReader("server view nowrap\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if len(eng.viewCalls) != 1 || eng.viewCalls[0].name != "server" || eng.viewCalls[0].nowrap != true {
		t.Fatalf("expected View(\"server\", true) called once, got %v", eng.viewCalls)
	}
}

func TestRun_ViewErrorIsPrintedAndLoopContinues(t *testing.T) {
	eng := &fakeEngine{viewErr: fmt.Errorf("no log file")}
	in := strings.NewReader("chat-llm view\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "error") || !strings.Contains(output, "no log file") {
		t.Fatalf("expected view error printed, got:\n%s", output)
	}
	if !strings.Contains(output, "tearing down") {
		t.Fatalf("expected quit to still work after view error, got:\n%s", output)
	}
}

func TestRun_HelpMentionsView(t *testing.T) {
	eng := &fakeEngine{}
	in := strings.NewReader("help\nquit\n")
	var out strings.Builder

	if err := Run(in, &out, eng); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "view") {
		t.Fatalf("expected help text to mention 'view', got:\n%s", out.String())
	}
}
