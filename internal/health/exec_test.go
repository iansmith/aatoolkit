package health

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// Every exec probe here runs a real process — /bin/sh with a one-liner — not a
// stubbed runner. The probe itself never invokes a shell; sh is simply the
// program these tests choose to run, the way a real config would name
// pg_isready.
func shSpec(script string) Spec {
	return Spec{Exec: []string{"/bin/sh", "-c", script}}
}

// execProbeTimeout is the bound every test below hands to Probe. Short, because
// nothing here is waiting on real work.
const execProbeTimeout = 200 * time.Millisecond

// execProbeSlack absorbs process spawn and scheduler jitter. It is not part of
// the contract — only the multiplier below is.
const execProbeSlack = 2 * time.Second

// execProbeBound is the worst case Probe may take: it kills the process when
// the timeout expires, then allows the same budget again for the output pipes
// to drain (a grandchild can still be holding them open). Two stages, each
// bounded by the configured timeout — so the whole call is bounded by twice it.
const execProbeBound = 2*execProbeTimeout + execProbeSlack

// --- ResolveSpec ---

func TestResolveSpec_CarriesExecArgvThrough(t *testing.T) {
	argv := []string{"pg_isready", "-p", "5432"}
	spec := ResolveSpec(config.Health{Exec: argv}, "127.0.0.1", 9000)
	if !slices.Equal(spec.Exec, argv) {
		t.Errorf("Exec = %q, want %q", spec.Exec, argv)
	}
}

// --- Probe: exit status is the whole signal ---

func TestProbe_ExecExitZeroIsReady(t *testing.T) {
	result := Probe(context.Background(), shSpec("exit 0"), execProbeTimeout)
	if !result.Healthy {
		t.Errorf("Healthy = false, want true for a probe exiting 0 (err: %v, output: %q)", result.Err, result.Output)
	}
	if result.Err != nil {
		t.Errorf("Err = %v, want nil on a successful probe", result.Err)
	}
}

func TestProbe_ExecNonZeroExitIsNotReady(t *testing.T) {
	result := Probe(context.Background(), shSpec("exit 3"), execProbeTimeout)
	if result.Healthy {
		t.Error("Healthy = true, want false for a probe exiting 3")
	}
	if result.Err == nil {
		t.Error("Err = nil, want the non-zero exit surfaced as an error")
	}
}

// --- Probe: bounded ---

// A probe that never returns must not become the supervisor's problem.
func TestProbe_ExecHangingCommandIsBoundedAndNotReady(t *testing.T) {
	start := time.Now()
	result := Probe(context.Background(), shSpec("sleep 5"), execProbeTimeout)
	elapsed := time.Since(start)

	if result.Healthy {
		t.Error("Healthy = true, want false for a probe that overran its timeout")
	}
	if elapsed > execProbeBound {
		t.Errorf("Probe took %v, want <= %v — a hanging probe must be bounded", elapsed, execProbeBound)
	}
}

// The sharper version of the same failure, one level down: the probe process
// itself exits promptly, but leaves a background child holding the inherited
// output pipes. Waiting on those pipes would block for the grandchild's whole
// lifetime even though the process being probed is long gone.
func TestProbe_ExecIsBoundedWhenAGrandchildHoldsTheOutputPipes(t *testing.T) {
	start := time.Now()
	result := Probe(context.Background(), shSpec("sleep 5 & exit 1"), execProbeTimeout)
	elapsed := time.Since(start)

	if result.Healthy {
		t.Error("Healthy = true, want false for a probe exiting 1")
	}
	if elapsed > execProbeBound {
		t.Errorf("Probe took %v, want <= %v — a lingering grandchild must not extend the probe", elapsed, execProbeBound)
	}
}

// --- Probe: a failing probe says why ---

// A probe that fails silently is as useless as no probe, and either stream may
// carry the reason — a probe program is as likely to explain itself on stdout
// as on stderr.
func TestProbe_ExecFailureOutputIsRetained(t *testing.T) {
	for _, tc := range []struct{ name, redirect string }{
		{"stderr", " >&2"},
		{"stdout", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Probe(context.Background(), shSpec("echo could-not-connect"+tc.redirect+"; exit 1"), execProbeTimeout)
			if result.Healthy {
				t.Fatal("Healthy = true, want false")
			}
			if !strings.Contains(result.Output, "could-not-connect") {
				t.Errorf("Output = %q, want the probe's %s retained", result.Output, tc.name)
			}
		})
	}
}

// The timeout bounds how long a probe may run, not how much it may write, so a
// chatty probe would otherwise buffer without limit into supervisor memory on
// every poll tick.
func TestProbe_ExecOutputIsCapped(t *testing.T) {
	// yes(1) writes until killed — the unbounded case, in one process.
	result := Probe(context.Background(), shSpec("yes aaaaaaaa"), execProbeTimeout)
	if len(result.Output) > maxProbeOutput+len(" […truncated]") {
		t.Errorf("Output is %d bytes, want capped at %d", len(result.Output), maxProbeOutput)
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Error("a capped probe output must say it was truncated, or the reader trusts a partial diagnosis")
	}
}

func TestProbe_ExecMissingProgramIsNotReady(t *testing.T) {
	result := Probe(context.Background(), Spec{Exec: []string{"aatoolkit-no-such-probe-program"}}, execProbeTimeout)
	if result.Healthy {
		t.Error("Healthy = true, want false when the probe program does not exist")
	}
	if result.Err == nil {
		t.Error("Err = nil, want the exec failure surfaced")
	}
}

// The status table renders Result.Rendered verbatim, so the exec form needs one
// that is meaningful rather than an empty HTTP path.
func TestProbe_ExecRenderedFormForStatusTable(t *testing.T) {
	ready := Probe(context.Background(), shSpec("exit 0"), execProbeTimeout)
	if ready.Rendered == "" {
		t.Error("Rendered = empty for a successful exec probe")
	}
	failed := Probe(context.Background(), shSpec("exit 3"), execProbeTimeout)
	if !strings.Contains(failed.Rendered, "3") {
		t.Errorf("Rendered = %q, want the exit status visible in the status table", failed.Rendered)
	}
}

// --- Spec.Describe: error text that is not a URL ---

func TestSpec_DescribeDistinguishesTheTwoForms(t *testing.T) {
	http := Spec{Host: "127.0.0.1", Port: 9000, Path: "/healthz"}
	if got := http.Describe(); !strings.Contains(got, http.URL()) {
		t.Errorf("Describe() = %q, want it to carry the URL %q for an HTTP probe", got, http.URL())
	}
	exec := Spec{Exec: []string{"pg_isready", "-p", "5432"}}
	if got := exec.Describe(); !strings.Contains(got, "pg_isready") {
		t.Errorf("Describe() = %q, want it to name the probe command", got)
	}
}

// --- PollReady ---

func TestPollReady_ExecSucceedsOnExitZero(t *testing.T) {
	_, err := PollReady(context.Background(), PollConfig{
		Spec:         shSpec("exit 0"),
		ProbeTimeout: execProbeTimeout,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: execProbeBound,
		ServerName:   "postgres",
		LogPath:      "/var/log/postgres.log",
	})
	if err != nil {
		t.Fatalf("PollReady error = %v, want nil for a probe exiting 0", err)
	}
}

// The server's own log file cannot hold a probe program's output — the probe is
// a separate process. So the timeout error has to carry it, or the diagnosis is
// lost exactly when it is needed.
func TestPollReady_ExecTimeoutErrorCarriesProbeOutput(t *testing.T) {
	_, err := PollReady(context.Background(), PollConfig{
		Spec:         shSpec("echo initdb-still-running >&2; exit 1"),
		ProbeTimeout: execProbeTimeout,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: 100 * time.Millisecond,
		ServerName:   "postgres",
		LogPath:      "/var/log/postgres.log",
	})
	if err == nil {
		t.Fatal("PollReady error = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("error %q must name the server", err.Error())
	}
	if !strings.Contains(err.Error(), "initdb-still-running") {
		t.Errorf("error %q must carry the last probe's output", err.Error())
	}
}
