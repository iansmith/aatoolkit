package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/health"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// KillSignal records which signal (if any) actually ended a teardown: the
// group either honored SIGTERM within the grace period, or needed a
// follow-up SIGKILL, or was already gone before any signal was needed.
type KillSignal string

const (
	// KillSignalNone means the group was already gone (no live process to
	// signal at all) — teardown is a no-op verify.
	KillSignalNone KillSignal = "none"
	// KillSignalTerm means SIGTERM alone was sufficient: the group exited
	// (and every declared port went free) within the grace period.
	KillSignalTerm KillSignal = "term"
	// KillSignalKill means SIGTERM was not enough — something in the group
	// survived the grace period, or a declared port was still listening —
	// so SIGKILL was sent to the group as well.
	KillSignalKill KillSignal = "kill"
)

// Target names the process group teardown operates on: a PID (the group
// leader, since every launched child is its own process-group leader per
// Launch's Setpgid) plus the declared ports and optional health spec used to
// verify the kill actually landed. Ports is the server's exhaustive
// {port} ∪ listens set (design/aa-server-status.md §6.2) — every one of them must
// be free (not merely TIME_WAIT — see observe.listeningPorts, which only
// counts LISTEN) for teardown to report success.
type Target struct {
	// Name identifies the target in errors and TeardownResult (a server
	// name for the down/dead/TeardownAll paths, or a synthetic label like
	// "foreign pid 1234" for a stray with no configured name).
	Name string
	// PID is the process-group leader to signal. Negated internally to
	// address the whole group (syscall.Kill(-PID, sig)).
	PID int32
	// Ports is the exhaustive declared port set to verify free after kill.
	Ports []int
	// Health is an optional post-kill probe target — when non-nil, the
	// verify step also confirms the health endpoint is no longer answering
	// (a listener some other way still alive would otherwise slip through
	// if it isn't itself one of Ports, though in practice health.Port is
	// always a member of Ports per config validation).
	Health *health.Spec
}

// Result is the outcome of tearing down a single Target.
type Result struct {
	Name   string
	Signal KillSignal
	// VerifiedClean is true only when every declared port was confirmed
	// free and (if a health spec was given) the health probe no longer
	// responds. Teardown never returns a nil error with VerifiedClean
	// false — verification failure is always surfaced as an error (see
	// "never report a kill you didn't achieve").
	VerifiedClean bool
}

// groupSignal sends sig to pid's whole process group (negative PID). There
// is no mock-injection seam here by design — the unit tests exercise this
// function directly against real spawned processes, never a fake.
func groupSignal(pid int32, sig syscall.Signal) error {
	err := syscall.Kill(-int(pid), sig)
	if err == syscall.ESRCH {
		// No such process (group already gone) is not a failure — the
		// caller's verify step confirms cleanliness independently.
		return nil
	}
	return err
}

// groupAlive reports whether any process in pid's group still exists, by
// probing with signal 0 (no-op signal used purely for existence checks).
func groupAlive(pid int32) bool {
	err := syscall.Kill(-int(pid), syscall.Signal(0))
	return err == nil
}

// Teardown tears down one server's process group: SIGTERM → wait the
// resolved grace period → if the group survives or any declared port still
// listens, SIGKILL the group → re-probe and verify every declared port is
// free and (if health is non-nil) the health probe is dead. Per
// design/aa-server-status.md §6.4, a surviving listener after SIGKILL is a loud
// error — Teardown never returns success alongside an unverified kill.
//
// grace is resolved by the caller (ResolveGracePeriod) from the per-server
// override falling back to the supervisor default — Teardown itself takes
// the already-resolved duration and never hard-codes one.
func Teardown(ctx context.Context, target Target, grace time.Duration) (Result, error) {
	if !groupAlive(target.PID) {
		return verify(ctx, target, KillSignalNone)
	}

	if err := groupSignal(target.PID, syscall.SIGTERM); err != nil {
		return Result{Name: target.Name}, fmt.Errorf(
			"teardown %q: SIGTERM process group %d: %w", target.Name, target.PID, err)
	}

	deadline := time.Now().Add(grace)
	signal := KillSignalTerm
	for time.Now().Before(deadline) {
		if !groupAlive(target.PID) && portsFree(target.Ports) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if groupAlive(target.PID) || !portsFree(target.Ports) {
		signal = KillSignalKill
		if err := groupSignal(target.PID, syscall.SIGKILL); err != nil {
			return Result{Name: target.Name}, fmt.Errorf(
				"teardown %q: SIGKILL process group %d: %w", target.Name, target.PID, err)
		}
		// SIGKILL is not interceptible, but the OS still needs a moment to
		// actually reap the process and release its listening sockets.
		waitUntilDead(target.PID, target.Ports, 2*time.Second)
	}

	return verify(ctx, target, signal)
}

// TeardownForeign tears down a process aa-server-status never launched —
// matched only by PID, exactly the "foreign stray kill" path used by the
// `dead` verb (design/aa-server-status.md §6.4: "Foreign strays killed by `dead`
// are group-killed by PID via gopsutil"). It shares the same
// TERM→grace→KILL→verify mechanism as Teardown; the only difference is that
// the caller supplies a bare PID discovered via observation instead of a
// Process handle this package itself launched.
func TeardownForeign(ctx context.Context, name string, pid int32, ports []int, grace time.Duration) (Result, error) {
	return Teardown(ctx, Target{Name: name, PID: pid, Ports: ports}, grace)
}

// waitUntilDead polls until pid's group is gone and every port is free, or
// timeout elapses. Used only after SIGKILL, where the outcome is expected
// almost immediately — this just absorbs the small scheduling delay between
// the kernel delivering SIGKILL and the socket actually being released.
func waitUntilDead(pid int32, ports []int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !groupAlive(pid) && portsFree(ports) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// portsFree reports whether none of ports currently have a LISTEN-state
// holder anywhere on the host. It deliberately does not scope the check to
// any one process tree: after a kill, the only question is "is this port
// free for the next launch," and observe's underlying gopsutil query only
// ever counts LISTEN-state sockets — a lingering TIME_WAIT entry for a
// closed connection does not hold LISTEN and is correctly treated as free,
// matching design/aa-server-status.md §6.4's note. A single host-wide scan
// covers every port in one gopsutil call rather than one scan per port.
func portsFree(ports []int) bool {
	holders := listeningPortSet(ports)
	return len(holders) == 0
}

// listeningPortSet returns the subset of ports currently holding a
// LISTEN-state socket anywhere on the host, via one host-wide scan
// (observe.SystemListenSet) regardless of how many ports are checked. On a
// scan failure, every port is conservatively reported as still listening —
// "can't confirm free" errs on the side of the loud error rather than
// silently reporting success.
func listeningPortSet(ports []int) []int {
	holders, err := observe.SystemListenSet()
	if err != nil {
		return ports
	}
	var stillListening []int
	for _, p := range ports {
		if _, ok := holders[p]; ok {
			stillListening = append(stillListening, p)
		}
	}
	return stillListening
}

// verify re-probes target after a (possible) kill and reports the final
// Result. It never returns a nil error alongside an unverified kill: a
// surviving listener is a loud error, per "never report a kill you didn't
// achieve."
func verify(ctx context.Context, target Target, signal KillSignal) (Result, error) {
	if stillListening := listeningPortSet(target.Ports); len(stillListening) > 0 {
		return Result{Name: target.Name, Signal: signal}, fmt.Errorf(
			"teardown %q: port(s) %v still listening after SIGKILL — kill not achieved", target.Name, stillListening)
	}

	if target.Health != nil {
		// health.Probe already applies this timeout internally via its own
		// context.WithTimeout(ctx, timeout) — no need to additionally wrap
		// ctx here first.
		result := health.Probe(ctx, *target.Health, 2*time.Second)
		if result.Healthy {
			return Result{Name: target.Name, Signal: signal}, fmt.Errorf(
				"teardown %q: health probe %s still answering 2xx after SIGKILL — kill not achieved",
				target.Name, target.Health.URL())
		}
	}

	return Result{Name: target.Name, Signal: signal, VerifiedClean: true}, nil
}

// portListening reports whether any process on the host currently holds
// port p in LISTEN state, using the same observation primitives as the
// up-precondition gate (internal/observe), so "free" here means exactly
// what it means everywhere else in aa-server-status: no LISTEN holder,
// regardless of which process (ours, a fresh relaunch, or a stray) holds
// it. Teardown's own target process, if it still exists at all, would
// necessarily show up here too — there is no separate "is it still ours"
// check because after a kill attempt, any listener at all on a declared
// port is the anomaly. A single-port convenience wrapper around
// listeningPortSet, used by tests and by callers that only care about one
// port at a time.
func portListening(port int) bool {
	return len(listeningPortSet([]int{port})) > 0
}

// ResolveGracePeriod returns s's own grace-period override when set, else
// falls back to supervisor's (already-defaulted) value. This is the only
// place the per-server-override-falls-back-to-supervisor-default rule
// (design/aa-server-status.md §7.1, config.Server.GracePeriod) is implemented —
// Teardown itself always takes an already-resolved time.Duration and never
// hard-codes or re-derives a default.
func ResolveGracePeriod(s config.Server, supervisor config.Supervisor) time.Duration {
	if s.GracePeriod.Duration != 0 {
		return s.GracePeriod.Duration
	}
	return supervisor.GracePeriod.Duration
}

// DeclaredPorts returns s's exhaustive declared port set ({port} ∪ listens,
// design/aa-server-status.md §6.2), deduplicated, for use as a Target's Ports.
func DeclaredPorts(s config.Server) []int {
	var ports []int
	seen := make(map[int]bool)
	if s.Port != 0 {
		ports = append(ports, s.Port)
		seen[s.Port] = true
	}
	for _, p := range s.Listens {
		if !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	return ports
}

// TeardownAll tears down every entry in servers in reverse configured
// order — design/aa-server-status.md §6.4's "reverse config order" applies only
// to this multi-server path, never to Teardown itself. pids maps server
// name to the process-group leader PID to signal; a server with no entry in
// pids is skipped (nothing to tear down — e.g. it was never launched this
// session). Every server is attempted even if an earlier one errors — per
// §6.5, multi-server commands attempt all targets and report a loud
// aggregate rather than stopping at the first failure.
func TeardownAll(ctx context.Context, servers []config.Server, supervisor config.Supervisor, pids map[string]int32) ([]Result, error) {
	results := make([]Result, 0, len(servers))
	var errs []error

	for i := len(servers) - 1; i >= 0; i-- {
		s := servers[i]
		pid, ok := pids[s.Name]
		if !ok {
			continue
		}

		var healthSpec *health.Spec
		if s.Health.Path != "" {
			spec := health.ResolveSpec(s.Health, s.Host, s.Port)
			healthSpec = &spec
		}

		target := Target{
			Name:   s.Name,
			PID:    pid,
			Ports:  DeclaredPorts(s),
			Health: healthSpec,
		}
		grace := ResolveGracePeriod(s, supervisor)

		result, err := Teardown(ctx, target, grace)
		results = append(results, result)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return results, fmt.Errorf("teardown: %d server(s) failed to verify clean: %w", len(errs), errors.Join(errs...))
	}
	return results, nil
}
