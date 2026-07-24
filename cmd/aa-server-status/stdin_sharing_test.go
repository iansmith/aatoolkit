package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// oneStdin builds the single buffered reader main.go is supposed to hand to
// both Run and the engine. Sharing ONE reader is the property under test: two
// readers over the same stream is exactly the bug (AATK-29), and a test that
// injected separate streams would pass against the broken code — which is how
// AATK-26's suite missed this in the first place.
func oneStdin(script string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(script))
}

// TestRun_PromptAnswerAndNextCommandShareOneStdin drives a prompt answer and a
// following command through a single stream, the way a real terminal does.
//
// The discriminator is that BOTH halves land in the right consumer: the prompt
// must take "y" (proved by the launched process's argv carrying the yes
// branch), and "status" must still reach dispatch afterwards (proved by a
// second status table in the output). Asserting only the first would pass even
// while the REPL silently ate the answer.
func TestRun_PromptAnswerAndNextCommandShareOneStdin(t *testing.T) {
	port := freeTestPort(t)
	spec := &config.PromptSpec{
		Question: "PROMPT-Q?",
		YesArgs:  []string{"-ignore-term"},
		NoArgs:   []string{"-child-port", "0"},
	}
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, spec)},
	}

	in := oneStdin("svc up\ny\nstatus\nquit\n")
	out := &launchSnapshotWriter{}
	eng := NewEngine(cfg, in, out)
	out.snap = func() []string { return launchedArgsOrNil(eng, "svc") }
	t.Cleanup(func() { eng.TeardownAll() })

	if err := Run(in, out, eng); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "PROMPT-Q?") {
		t.Fatalf("expected the question to be asked, got:\n%s", got)
	}
	if strings.Contains(got, `no such server "y"`) {
		t.Fatalf("the prompt answer leaked into the REPL and was dispatched as a command:\n%s", got)
	}

	args := strings.Join(out.argsAtLaunch, " ")
	if !strings.Contains(args, "-ignore-term") {
		t.Fatalf("expected the prompt to have consumed \"y\" and taken the yes branch, got args %q", args)
	}

	// The status table header appears once on entry; the "status" command must
	// produce a second. One occurrence means "status" was swallowed.
	if n := strings.Count(got, "SERVER"); n < 2 {
		t.Fatalf("expected the 'status' command after the answer to still dispatch (>=2 status tables), got %d in:\n%s", n, got)
	}
}

// TestRun_PastedInputDoesNotMisrouteAnswer is the same script delivered as one
// write, which is what a terminal paste looks like to the process. It is the
// realistic form of this bug: an operator pasting a short sequence of commands.
func TestRun_PastedInputDoesNotMisrouteAnswer(t *testing.T) {
	port := freeTestPort(t)
	spec := &config.PromptSpec{
		Question: "PASTE-Q?",
		YesArgs:  []string{"-ignore-term"},
	}
	cfg := config.Config{
		Supervisor: testSupervisor(t),
		Servers:    []config.Server{promptedServer(t, "svc", port, spec)},
	}

	in := oneStdin("svc up\ny\nquit\n")
	out := &launchSnapshotWriter{}
	eng := NewEngine(cfg, in, out)
	out.snap = func() []string { return launchedArgsOrNil(eng, "svc") }
	t.Cleanup(func() { eng.TeardownAll() })

	if err := Run(in, out, eng); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "reading answer") {
		t.Fatalf("the prompt hit EOF because the REPL had already buffered the answer:\n%s", got)
	}
	if args := strings.Join(out.argsAtLaunch, " "); !strings.Contains(args, "-ignore-term") {
		t.Fatalf("expected the pasted \"y\" to reach the prompt, got args %q", args)
	}
	// "quit" must have been dispatched as a command, which is the only path
	// that reaches teardown with a message; EOF teardown means it was eaten.
	if !strings.Contains(got, "tearing down") {
		t.Fatalf("expected the pasted 'quit' to dispatch and tear down, got:\n%s", got)
	}
}

// launchSnapshotWriter is the REPL's output sink, which also freezes the
// launched process's argv the first time one exists.
//
// The snapshot has to happen mid-run: this script ends with "quit", so by the
// time Run returns, TeardownAll has already cleared the process registry and
// the argv is unrecoverable. Reading it afterwards is what the first version
// of this test got wrong.
type launchSnapshotWriter struct {
	buf          strings.Builder
	snap         func() []string
	argsAtLaunch []string
}

func (w *launchSnapshotWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if w.argsAtLaunch == nil && w.snap != nil {
		w.argsAtLaunch = w.snap()
	}
	return n, err
}

func (w *launchSnapshotWriter) String() string { return w.buf.String() }

// launchedArgsOrNil is launchedArgs without the t.Fatalf — the snapshot writer
// polls speculatively and a not-yet-launched server is the expected case, not
// a failure.
func launchedArgsOrNil(eng *RealEngine, name string) []string {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if proc, ok := eng.procs[name]; ok {
		return proc.Cmd.Args
	}
	return nil
}
