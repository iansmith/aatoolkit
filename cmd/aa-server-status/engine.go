// RealEngine is the reconciliation engine wiring launchers
// (internal/lifecycle), teardown (internal/lifecycle), observation
// (internal/observe), and health (internal/health) into the fleet verbs
// (design/aa-server-status.md §2, §3, §6.3, §6.5). It replaces StubEngine as
// aa-server-status's live Engine implementation.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/health"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
	"github.com/iansmith/aatoolkit/internal/observe"
)

// RealEngine holds the live process registry (servers this supervisor
// itself launched, this session) alongside the static config, and is the
// seam through which every fleet verb (up/down/dead/build/status) actually
// touches child processes.
type RealEngine struct {
	cfg config.Config

	mu    sync.Mutex
	procs map[string]*lifecycle.Process // server name -> our live child, if any
}

// NewEngine builds a RealEngine over cfg. No processes are launched by
// construction — the registry starts empty, matching a freshly started
// supervisor that hasn't reconciled anything yet.
func NewEngine(cfg config.Config) *RealEngine {
	return &RealEngine{cfg: cfg, procs: make(map[string]*lifecycle.Process)}
}

var _ Engine = (*RealEngine)(nil)

// serverByName returns the configured server named name, or ok=false if no
// such server exists.
func (e *RealEngine) serverByName(name string) (config.Server, bool) {
	for _, s := range e.cfg.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return config.Server{}, false
}

// livePID returns the PID and true if we currently hold a live child
// process for name (registered by a prior Up), else 0, false. Must be
// called with e.mu held.
func (e *RealEngine) livePIDLocked(name string) (int32, bool) {
	p, ok := e.procs[name]
	if !ok || p.Cmd == nil || p.Cmd.Process == nil {
		return 0, false
	}
	return int32(p.Cmd.Process.Pid), true
}

// ============================================================================
// Status
// ============================================================================

// Status returns one ServerStatus per configured server, populated from real
// observed state (internal/observe + internal/health) rather than
// config-derived placeholders.
func (e *RealEngine) Status() []ServerStatus {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]ServerStatus, 0, len(e.cfg.Servers))
	for _, s := range e.cfg.Servers {
		out = append(out, e.statusForLocked(s))
	}
	return out
}

// statusForLocked computes one server's ServerStatus. Must be called with
// e.mu held.
func (e *RealEngine) statusForLocked(s config.Server) ServerStatus {
	declared := lifecycle.DeclaredPorts(s)
	pid, isOurs := e.livePIDLocked(s.Name)

	status := ServerStatus{
		Name:    s.Name,
		Type:    s.Type,
		Enabled: s.Enabled,
		State:   StateDown,
	}

	var obs observe.TreeObservation
	if isOurs {
		treeObs, err := observe.TreeListenSet(pid)
		if err == nil {
			obs = treeObs
		}
	} else {
		obs = hostObservationFor(declared)
	}

	class := observe.Classify(declared, obs)

	status.Ports = renderPorts(declared, class)

	switch {
	case len(class.Actual) == 0:
		// Nothing listening at all for this server's declared ports.
		status.State = StateDown
	case isOurs:
		status.PID = int(pid)
		status.State = classifyOwned(s, class)
		if status.State == StateUp && !s.Enabled {
			// We started this disabled server ourselves via imperative
			// `<name> up` — render.go's formatStateCell renders this as
			// yellow "up (disabled)", not red STRAY (STRAY is reserved for
			// a foreign process occupying a disabled server's slot).
			status.OwnedDisabled = true
		}
	case len(class.ForeignHolders) > 0:
		// Ports are up, but we don't hold this process — either a stray
		// (disabled server someone/something started) or a foreign
		// conflict on an enabled server's port.
		holder := firstForeignHolder(class.ForeignHolders)
		status.AnomalyDetail = fmt.Sprintf("pid %d, foreign", holder.PID)
		status.State = StateStray
	case class.Classification == observe.StrayPort:
		status.State = StateExtraListener
	default:
		status.State = StatePartial
	}

	if status.State == StateUp || status.State == StateStray {
		if s.Health.Path != "" {
			spec := health.ResolveSpec(s.Health, s.Host, s.Port)
			result := health.Probe(context.Background(), spec, resolveHealthTimeout(e.cfg.Supervisor))
			status.Health = result.Rendered
		}
	}

	if s.Type == config.TypeSource {
		if staleResult, err := lifecycle.ProbeStaleness(s); err == nil {
			defer staleResult.Cleanup()
			status.Stale = staleResult.Stale
		}
	}

	return status
}

// classifyOwned determines the STATE for a server we hold a live child for
// (class already reflects the declared-vs-actual port comparison). A
// disabled-but-owned server (started via imperative `<name> up`) still
// renders StateUp here — render.go's formatStateCell applies the yellow
// "up (disabled)" text via the OwnedDisabled flag; StateStray is reserved
// for foreign processes occupying a disabled server's slot.
func classifyOwned(s config.Server, class observe.Result) ServerState {
	switch class.Classification {
	case observe.StrayPort:
		return StateExtraListener
	case observe.Partial:
		return StatePartial
	default:
		return StateUp
	}
}

// hostObservationFor builds a TreeObservation-shaped view of the declared
// ports using a host-wide scan, for servers we don't hold a live child for
// (never launched this session, or a stray). Every holder found this way is
// necessarily "not ours" (Ours=false) since we only reach this path when
// e.procs has no entry for the server.
func hostObservationFor(declared []int) observe.TreeObservation {
	holders, err := observe.SystemListenSet()
	if err != nil {
		return observe.TreeObservation{Holders: map[int]observe.Holder{}}
	}
	out := observe.TreeObservation{Holders: make(map[int]observe.Holder)}
	for _, port := range declared {
		pid, ok := holders[port]
		if !ok {
			continue
		}
		ident := observe.Identity{PID: pid, Ours: false}
		if fullIdent, err := observe.NewForeignIdentity(pid); err == nil {
			ident = *fullIdent
		}
		out.Holders[port] = observe.Holder{Port: port, Identity: ident}
	}
	return out
}

func firstForeignHolder(m map[int]observe.Identity) observe.Identity {
	ports := make([]int, 0, len(m))
	for p := range m {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return m[ports[0]]
}

func renderPorts(declared []int, class observe.Result) []PortStatus {
	actualSet := make(map[int]bool, len(class.Actual))
	for _, p := range class.Actual {
		actualSet[p] = true
	}
	out := make([]PortStatus, 0, len(declared)+len(class.Stray))
	for _, p := range declared {
		out = append(out, PortStatus{Port: p, Up: actualSet[p]})
	}
	for _, p := range class.Stray {
		out = append(out, PortStatus{Port: p, Unexpected: true})
	}
	return out
}

func resolveHealthTimeout(sup config.Supervisor) time.Duration {
	if sup.HealthTimeout.Duration != 0 {
		return sup.HealthTimeout.Duration
	}
	return config.DefaultHealthTimeout
}

func resolveReadyTimeout(s config.Server, sup config.Supervisor) time.Duration {
	if s.ReadyTimeout.Duration != 0 {
		return s.ReadyTimeout.Duration
	}
	if sup.ReadyTimeout.Duration != 0 {
		return sup.ReadyTimeout.Duration
	}
	return config.DefaultReadyTimeout
}

func resolvePollInterval(sup config.Supervisor) time.Duration {
	if sup.PollInterval.Duration != 0 {
		return sup.PollInterval.Duration
	}
	return config.DefaultPollInterval
}

// ============================================================================
// verbOutcome / aggregate reporting (§6.5)
// ============================================================================

// verbOutcome is one server's result within a multi-server up/down/dead
// command — the unit the loud aggregate (§6.5) is built from.
type verbOutcome struct {
	Name    string
	Err     error
	LogPath string
	// Warn marks Err as a non-fatal advisory (e.g. "stray, ignoring it")
	// rather than a failure: formatAggregate still prints it in the loud
	// aggregate, but it does not count toward the failed total or make the
	// overall command return a non-nil error on its own.
	Warn bool
}

// formatOutcomeLine renders one server's outcome as the §6.5 "loud aggregate"
// line: a checkmark with its log path on success, an X with reason and log
// path on failure, or a warning marker for a non-fatal advisory.
func formatOutcomeLine(o verbOutcome) string {
	switch {
	case o.Err == nil:
		if o.LogPath != "" {
			return fmt.Sprintf("%s ✓ (%s)", o.Name, o.LogPath)
		}
		return fmt.Sprintf("%s ✓", o.Name)
	case o.Warn:
		return fmt.Sprintf("%s ⚠ (%s)", o.Name, o.Err.Error())
	default:
		detail := o.Err.Error()
		if o.LogPath != "" && !strings.Contains(detail, o.LogPath) {
			detail = fmt.Sprintf("%s, see %s", detail, o.LogPath)
		}
		return fmt.Sprintf("%s ✗ (%s)", o.Name, detail)
	}
}

// printOutcomes prints every outcome's §6.5 line to w — including
// successes, so a server's log path is visible the moment it starts, not
// only when a start fails (SOP-108). Callers must only invoke this when the
// same lines won't also reach the console via a returned aggregate error
// (see Up) — otherwise every line prints twice.
func printOutcomes(w io.Writer, outcomes []verbOutcome) {
	for _, o := range outcomes {
		fmt.Fprintln(w, formatOutcomeLine(o))
	}
}

// formatTailHint renders a copy-pasteable `tail -f <log> <log>...` command
// covering every outcome that produced a log file this launch (successes
// and failures alike — a failed server's log is exactly what you want to
// tail). Empty when nothing was freshly launched (e.g. every target was
// already up).
func formatTailHint(outcomes []verbOutcome) string {
	var paths []string
	for _, o := range outcomes {
		if o.LogPath != "" {
			paths = append(paths, shellQuote(o.LogPath))
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return "tail -f " + strings.Join(paths, " ")
}

// shellQuote makes path safe to paste into a shell. log_dir is configurable,
// so a path can contain a space -- unquoted, `tail -f /My Logs/server.log`
// is two filenames and an error, from output that claims to be
// copy-pasteable. Single quotes disable every shell expansion; an embedded
// single quote is closed, escaped, and reopened.
func shellQuote(path string) string {
	if !strings.ContainsAny(path, " \t\n'\"\\$`*?[]{}()|&;<>#~!") {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// printTailHint prints formatTailHint's command to w, if any.
func printTailHint(w io.Writer, outcomes []verbOutcome) {
	if hint := formatTailHint(outcomes); hint != "" {
		fmt.Fprintln(w, hint)
	}
}

// formatAggregate renders outcomes as the §6.5 "loud aggregate" error: a
// per-server checkmark/X line with reason and log path on failure. Returns
// nil if every outcome succeeded (nothing to report — the caller returns nil
// for a fully clean multi/single-server command).
func formatAggregate(verb string, outcomes []verbOutcome) error {
	var failed int
	var lines []string
	for _, o := range outcomes {
		if o.Err != nil && !o.Warn {
			failed++
		}
		lines = append(lines, formatOutcomeLine(o))
	}
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("%s: %d of %d server(s) failed:\n%s", verb, failed, len(outcomes), strings.Join(lines, "\n"))
}

// ============================================================================
// up
// ============================================================================

// Up reconciles toward the healthy desired state (design/aa-server-status.md §3):
//   - name == "" -> every enabled+down server, plus rebuild+relaunch of any
//     stale owned source server (staleness is the only reason up restarts
//     something it owns).
//   - name != "" -> imperative: that one server, regardless of its enabled
//     flag.
//
// Before launching, every target's declared ports are precondition-gated
// (§6.3): a port held by a process that is not our own live child for that
// same server is a hard refusal naming the holder — never adopted. Targets
// are launched in parallel; each is polled for health readiness. Failures
// abort only that server; every target is attempted and a loud aggregate is
// returned when any fail (§6.5).
func (e *RealEngine) Up(name string) error {
	targets, err := e.resolveUpTargets(name)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	outcomes := make([]verbOutcome, len(targets))
	for i, s := range targets {
		wg.Add(1)
		go func(i int, s config.Server) {
			defer wg.Done()
			outcomes[i] = e.upOne(s)
		}(i, s)
	}
	wg.Wait()

	// Print outcome lines (including successes, so a log path is visible
	// the moment a server starts) exactly once: on the success path here,
	// or — on any failure — inside the aggregate error dispatch prints,
	// never both. Printing unconditionally here would duplicate every line
	// with dispatch's own error print (SOP-108).
	if len(outcomes) == 1 {
		if outcomes[0].Err != nil {
			// The hint prints here too. A single server failing to start is
			// the commonest way to reach this function, and the case where
			// its log matters most -- returning the error alone would
			// withhold the path in exactly the situation the hint is for,
			// while a failing sibling in a multi-server up still got one.
			printTailHint(os.Stdout, outcomes)
			return outcomes[0].Err
		}
		printOutcomes(os.Stdout, outcomes)
		printTailHint(os.Stdout, outcomes)
		return nil
	}

	if err := formatAggregate("up", outcomes); err != nil {
		// The aggregate error carries the per-server lines; the tail hint
		// still prints here so the launched servers' logs stay one paste
		// away even when a sibling failed.
		printTailHint(os.Stdout, outcomes)
		return err
	}
	printOutcomes(os.Stdout, outcomes)
	printTailHint(os.Stdout, outcomes)
	return nil
}

// resolveUpTargets computes the target set for Up(name): the imperative
// single-server form, or (for name=="") every enabled+down server plus every
// enabled owned source server currently stale.
func (e *RealEngine) resolveUpTargets(name string) ([]config.Server, error) {
	if name != "" {
		s, ok := e.serverByName(name)
		if !ok {
			return nil, fmt.Errorf("up %s: no such server", name)
		}
		return []config.Server{s}, nil
	}

	var targets []config.Server
	for _, s := range e.cfg.Servers {
		if !s.Enabled {
			continue
		}
		targets = append(targets, s)
	}
	return targets, nil
}

// upOne reconciles a single server up: precondition gate, then either a
// stale-rebuild relaunch (owned source server) or a fresh launch, then a
// health-readiness poll.
func (e *RealEngine) upOne(s config.Server) verbOutcome {
	e.mu.Lock()
	pid, isOurs := e.livePIDLocked(s.Name)
	e.mu.Unlock()

	if isOurs {
		// Already our own live child for this server. Check staleness (the
		// only reason up touches something it already owns) before
		// declaring an idempotent skip.
		if s.Type == config.TypeSource {
			if outcome, handled := e.rebuildIfStaleOwned(s); handled {
				return outcome
			}
		}
		return verbOutcome{Name: s.Name}
	}

	declared := lifecycle.DeclaredPorts(s)
	if conflict := e.checkPortConflict(s, declared, pid, isOurs); conflict != nil {
		return verbOutcome{Name: s.Name, Err: conflict}
	}

	// Nothing of ours is running yet for this server — this is a cold
	// launch. A source server can still be stale here (the common case:
	// the very first `up` in a new aa-server-status session), and this path
	// previously skipped the staleness check entirely, launching whatever
	// binary happened to be on disk. Check and rebuild-in-place before the
	// first launch, same as an already-owned stale server, just without a
	// stop step (nothing to stop).
	if s.Type == config.TypeSource {
		if outcome, handled := e.rebuildIfStaleCold(s); handled {
			return outcome
		}
	}

	proc, err := e.launch(s)
	if err != nil {
		return verbOutcome{Name: s.Name, Err: fmt.Errorf("launching %s: %w", s.Name, err)}
	}

	e.mu.Lock()
	e.procs[s.Name] = proc
	e.mu.Unlock()

	if err := e.pollReady(s, proc); err != nil {
		return verbOutcome{Name: s.Name, Err: err, LogPath: proc.LogPath}
	}
	return verbOutcome{Name: s.Name, LogPath: proc.LogPath}
}

// checkPortConflict implements the §6.3 precondition gate for one server: if
// any of its declared ports is currently held by a process that is not our
// own live child for this same server, it's a hard refusal naming the
// holder. A completely free port, or a port we already hold via a live
// child for this exact server, is not a conflict.
func (e *RealEngine) checkPortConflict(s config.Server, declared []int, ownPID int32, haveOwnPID bool) error {
	holders, err := observe.SystemListenSet()
	if err != nil {
		// Can't confirm the ports are free — err on the side of refusing
		// rather than silently launching over an unconfirmed holder.
		return fmt.Errorf("up %s: checking port availability: %w", s.Name, err)
	}

	var ourPorts map[int]bool
	if haveOwnPID {
		if obs, err := observe.TreeListenSet(ownPID); err == nil {
			ourPorts = make(map[int]bool, len(obs.Holders))
			for p := range obs.Holders {
				ourPorts[p] = true
			}
		}
	}

	for _, port := range declared {
		holderPID, listening := holders[port]
		if !listening {
			continue
		}
		if ourPorts[port] {
			continue
		}
		ident, identErr := observe.NewForeignIdentity(holderPID)
		if identErr != nil {
			return fmt.Errorf("up %s: port %d is held by pid %d (a process not started by this supervisor) — refusing to launch", s.Name, port, holderPID)
		}
		return fmt.Errorf("up %s: port %d is held by pid %d (%s) — a process not started by this supervisor for this server; refusing to launch",
			s.Name, port, ident.PID, strings.Join(ident.Cmdline, " "))
	}
	return nil
}

// rebuildIfStaleOwned probes an owned source server for staleness and, if
// stale, rebuilds+relaunches it via lifecycle.PerformBuild's stop->replace
// ->start sequencing. handled is false when the server isn't stale (caller
// should fall through to the idempotent already-up skip).
func (e *RealEngine) rebuildIfStaleOwned(s config.Server) (outcome verbOutcome, handled bool) {
	probe, err := lifecycle.ProbeStaleness(s)
	if err != nil {
		return verbOutcome{Name: s.Name, Err: fmt.Errorf("probing staleness for %s: %w", s.Name, err)}, true
	}
	defer probe.Cleanup()
	if !probe.Stale {
		return verbOutcome{}, false
	}

	var logPath string
	lc := &lifecycle.BuildLifecycle{
		Stop: func() error {
			return e.teardownOne(s)
		},
		Start: func() error {
			proc, err := e.launch(s)
			if err != nil {
				return err
			}
			logPath = proc.LogPath
			e.mu.Lock()
			e.procs[s.Name] = proc
			e.mu.Unlock()
			return e.pollReady(s, proc)
		},
	}

	if _, err := lifecycle.PerformBuild(s, lc); err != nil {
		return verbOutcome{Name: s.Name, Err: fmt.Errorf("rebuilding stale %s: %w", s.Name, err), LogPath: logPath}, true
	}
	return verbOutcome{Name: s.Name, LogPath: logPath}, true
}

// rebuildIfStaleCold is rebuildIfStaleOwned's counterpart for a server
// nothing of ours is running yet (upOne's cold-launch path): same
// probe-and-replace-if-stale, but with no Stop callback — there is nothing
// to tear down before replacing the on-disk binary. handled is false when
// the server isn't stale, in which case the caller proceeds with its
// normal (unchanged) launch.
func (e *RealEngine) rebuildIfStaleCold(s config.Server) (outcome verbOutcome, handled bool) {
	probe, err := lifecycle.ProbeStaleness(s)
	if err != nil {
		return verbOutcome{Name: s.Name, Err: fmt.Errorf("probing staleness for %s: %w", s.Name, err)}, true
	}
	defer probe.Cleanup()
	if !probe.Stale {
		return verbOutcome{}, false
	}

	var logPath string
	lc := &lifecycle.BuildLifecycle{
		Start: func() error {
			proc, err := e.launch(s)
			if err != nil {
				return err
			}
			logPath = proc.LogPath
			e.mu.Lock()
			e.procs[s.Name] = proc
			e.mu.Unlock()
			return e.pollReady(s, proc)
		},
	}

	if _, err := lifecycle.PerformBuild(s, lc); err != nil {
		return verbOutcome{Name: s.Name, Err: fmt.Errorf("rebuilding stale %s: %w", s.Name, err), LogPath: logPath}, true
	}
	return verbOutcome{Name: s.Name, LogPath: logPath}, true
}

// launch dispatches to the per-type launcher (internal/lifecycle), using the
// supervisor's configured log directory.
func (e *RealEngine) launch(s config.Server) (*lifecycle.Process, error) {
	logDir := e.cfg.Supervisor.LogDir
	switch s.Type {
	case config.TypeMLX:
		return lifecycle.LaunchMLX(logDir, s)
	case config.TypePython:
		return lifecycle.LaunchPython(logDir, s)
	case config.TypeExec:
		return lifecycle.LaunchExec(logDir, s)
	case config.TypeSource:
		return lifecycle.LaunchSource(logDir, s)
	default:
		return nil, fmt.Errorf("server %q: unknown type %q", s.Name, s.Type)
	}
}

// warmUp sends s's configured warm-up request, if it declares one, and does
// not return until it answers 2xx (or the ready-timeout elapses).
//
// This runs before the health gate, not after: a server that needs a warm-up
// is one whose health endpoint answers before it can do any work, so polling
// health first would report ready and let the first real caller pay the cost
// the warm-up exists to absorb. A server with no warm key skips straight to
// health, exactly as before.
func (e *RealEngine) warmUp(s config.Server, proc *lifecycle.Process, budget time.Duration) error {
	if s.Warm.Path == "" {
		return nil
	}
	_, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(s.Warm, s.Host, s.Port),
		PollInterval: resolvePollInterval(e.cfg.Supervisor),
		Timeout:      budget,
		ServerName:   s.Name,
		LogPath:      proc.LogPath,
	})
	return err
}

// pollReady warms s up (if it declares a warm-up), then polls its health
// endpoint, until it is ready or its resolved ready-timeout elapses.
//
// ready_timeout bounds the two together, not each. It is the answer to "how
// long may this server take to become ready", and a server does not become
// ready twice: giving each stage the full budget would let a server declaring
// 180s take 360s before `up` reports anything, which is neither what the knob
// says nor what an operator watching a REPL would assume.
func (e *RealEngine) pollReady(s config.Server, proc *lifecycle.Process) error {
	deadline := time.Now().Add(resolveReadyTimeout(s, e.cfg.Supervisor))

	if err := e.warmUp(s, proc, time.Until(deadline)); err != nil {
		return err
	}
	if s.Health.Path == "" {
		return nil
	}

	// Whatever the warm-up did not spend. A warm-up that used the whole
	// budget leaves a non-positive remainder; PollReady always attempts one
	// probe regardless, so the server still gets a chance to answer rather
	// than failing on arithmetic alone.
	spec := health.ResolveSpec(s.Health, s.Host, s.Port)
	cfg := health.PollConfig{
		Spec:         spec,
		ProbeTimeout: resolveHealthTimeout(e.cfg.Supervisor),
		PollInterval: resolvePollInterval(e.cfg.Supervisor),
		ReadyTimeout: time.Until(deadline),
		ServerName:   s.Name,
		LogPath:      proc.LogPath,
	}
	_, err := health.PollReady(context.Background(), cfg)
	return err
}

// ============================================================================
// down / dead
// ============================================================================

// Down tears down enabled+running servers (design/aa-server-status.md §3):
//   - name == "" -> every enabled server we hold a live child for.
//   - name != "" -> imperative: that one server, regardless of its enabled
//     flag.
//
// Strays (running but disabled, and not the imperative target) are warned
// about via the aggregate, never touched.
func (e *RealEngine) Down(name string) error {
	return e.downOrDead(name, false)
}

// Dead is Down plus killing strays too (design/aa-server-status.md §3).
func (e *RealEngine) Dead(name string) error {
	return e.downOrDead(name, true)
}

func (e *RealEngine) Bounce(name string) error {
	return fmt.Errorf("not implemented: bounce lands in AATK-28")
}

func (e *RealEngine) downOrDead(name string, killStrays bool) error {
	if name != "" {
		if _, ok := e.serverByName(name); !ok {
			return fmt.Errorf("down %s: no such server", name)
		}
		outcome := e.downOne(name)
		return outcome.Err
	}

	var outcomes []verbOutcome
	for _, s := range e.cfg.Servers {
		e.mu.Lock()
		_, isOurs := e.livePIDLocked(s.Name)
		e.mu.Unlock()

		if s.Enabled && isOurs {
			outcomes = append(outcomes, e.downOne(s.Name))
			continue
		}

		if !s.Enabled {
			strayPID, strayIsOurs, isRunning := e.observedRunningPID(s)
			if !isRunning {
				continue
			}
			if killStrays {
				outcomes = append(outcomes, e.killForeignOrOwned(s, strayPID, strayIsOurs))
			} else {
				outcomes = append(outcomes, verbOutcome{
					Name: s.Name,
					Err:  fmt.Errorf("%s is up but not enabled, so ignoring it", s.Name),
					Warn: true,
				})
			}
		}
	}

	if len(outcomes) == 0 {
		return nil
	}
	verb := "down"
	if killStrays {
		verb = "dead"
	}
	return formatAggregate(verb, outcomes)
}

// observedRunningPID reports whether s currently appears to be running (any
// declared port listening), and whose PID that is — our own registered
// child, if we have one, else whatever the host-wide scan finds.
func (e *RealEngine) observedRunningPID(s config.Server) (pid int32, isOurs bool, running bool) {
	e.mu.Lock()
	ownPID, haveOwn := e.livePIDLocked(s.Name)
	e.mu.Unlock()
	if haveOwn {
		return ownPID, true, true
	}

	declared := lifecycle.DeclaredPorts(s)
	holders, err := observe.SystemListenSet()
	if err != nil {
		return 0, false, false
	}
	for _, port := range declared {
		if holderPID, ok := holders[port]; ok {
			return holderPID, false, true
		}
	}
	return 0, false, false
}

// downOne tears down a single server we hold (or believe we hold) a live
// child for, via lifecycle.Teardown, and removes it from the registry on
// success.
func (e *RealEngine) downOne(name string) verbOutcome {
	s, ok := e.serverByName(name)
	if !ok {
		return verbOutcome{Name: name, Err: fmt.Errorf("no such server")}
	}

	e.mu.Lock()
	_, isOurs := e.livePIDLocked(name)
	e.mu.Unlock()
	if !isOurs {
		// Imperative down on a server we don't hold — nothing registered
		// to tear down via our handle; fall back to a foreign-style kill
		// by observed PID if it's actually running (e.g. imperative
		// `<name> down` on a disabled-but-running stray we started in a
		// prior session, or discovered on the host).
		observedPID, observedIsOurs, running := e.observedRunningPID(s)
		if !running {
			return verbOutcome{Name: name}
		}
		return e.killForeignOrOwned(s, observedPID, observedIsOurs)
	}

	if err := e.teardownOne(s); err != nil {
		return verbOutcome{Name: name, Err: err}
	}

	e.mu.Lock()
	delete(e.procs, name)
	e.mu.Unlock()
	return verbOutcome{Name: name}
}

// teardownOne runs lifecycle.Teardown against our registered live child for
// s, using the server's resolved grace period and declared ports/health.
func (e *RealEngine) teardownOne(s config.Server) error {
	e.mu.Lock()
	pid, isOurs := e.livePIDLocked(s.Name)
	e.mu.Unlock()
	if !isOurs {
		return nil
	}
	return e.teardownPID(s, pid)
}

func (e *RealEngine) teardownPID(s config.Server, pid int32) error {
	var healthSpec *health.Spec
	if s.Health.Path != "" {
		spec := health.ResolveSpec(s.Health, s.Host, s.Port)
		healthSpec = &spec
	}
	target := lifecycle.Target{
		Name:   s.Name,
		PID:    pid,
		Ports:  lifecycle.DeclaredPorts(s),
		Health: healthSpec,
	}
	grace := lifecycle.ResolveGracePeriod(s, e.cfg.Supervisor)
	_, err := lifecycle.Teardown(context.Background(), target, grace)
	return err
}

// killForeignOrOwned tears down a stray by PID: if it's a PID we happen to
// have registered (owned-disabled server), goes through the normal
// registered teardown path; otherwise it's a genuinely foreign process,
// torn down via TeardownForeign (PID-matched only, per §6.4).
func (e *RealEngine) killForeignOrOwned(s config.Server, pid int32, isOurs bool) verbOutcome {
	var err error
	if isOurs {
		err = e.teardownPID(s, pid)
		e.mu.Lock()
		delete(e.procs, s.Name)
		e.mu.Unlock()
	} else {
		grace := lifecycle.ResolveGracePeriod(s, e.cfg.Supervisor)
		_, err = lifecycle.TeardownForeign(context.Background(), s.Name, pid, lifecycle.DeclaredPorts(s), grace)
	}
	return verbOutcome{Name: s.Name, Err: err}
}

// ============================================================================
// build
// ============================================================================

// Build rebuilds a source server's on-disk binary if stale, mirroring its
// prior lifecycle (was running -> stop -> replace -> start; was down ->
// replace, stay down). Non-source servers are a loud error, matching
// design/aa-server-status.md §2's build verb contract.
func (e *RealEngine) Build(name string) error {
	s, ok := e.serverByName(name)
	if !ok {
		return fmt.Errorf("build %s: no such server", name)
	}
	if s.Type != config.TypeSource {
		return fmt.Errorf("build %s: build verb only applies to source servers (got type %q)", name, s.Type)
	}

	e.mu.Lock()
	pid, isOurs := e.livePIDLocked(name)
	e.mu.Unlock()

	var lc *lifecycle.BuildLifecycle
	if isOurs {
		lc = &lifecycle.BuildLifecycle{
			Stop: func() error {
				err := e.teardownPID(s, pid)
				e.mu.Lock()
				delete(e.procs, name)
				e.mu.Unlock()
				return err
			},
			Start: func() error {
				proc, err := e.launch(s)
				if err != nil {
					return err
				}
				e.mu.Lock()
				e.procs[s.Name] = proc
				e.mu.Unlock()
				return e.pollReady(s, proc)
			},
		}
	}

	_, err := lifecycle.PerformBuild(s, lc)
	return err
}

// ============================================================================
// kill / command / logs
// ============================================================================

// Kill sends SIGTERM to the process with the given PID using Go's
// os.FindProcess + Signal — no shell spawn.
func (e *RealEngine) Kill(pid int) error {
	// os.FindProcess on Unix always succeeds; the real check is Signal.
	proc, _ := os.FindProcess(pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}

// Command returns the launch command and args for the named server.
func (e *RealEngine) Command(name string) (string, []string, error) {
	s, ok := e.serverByName(name)
	if !ok {
		return "", nil, fmt.Errorf("command %s: no such server", name)
	}
	cmd, args, err := lifecycle.ResolveCommand(s)
	if err != nil {
		return "", nil, fmt.Errorf("command %s: %w", name, err)
	}
	return cmd, args, nil
}

// Logs returns the not-implemented error — full log retrieval (reading back
// build/logs/<name>-<ts>.log content) is out of this ticket's scope; the
// `logs <name>` verb resolving a path is covered by internal/lifecycle's
// NewestLog, wired by a future ticket if/when the REPL needs it beyond what
// build/logs already gives operators via the filesystem directly.
func (e *RealEngine) Logs(name string) ([]string, error) {
	return nil, notImplementedErr("logs")
}

// viewTailLines is how many trailing log lines `view` returns; viewWrapWidth is
// the rune width each line is truncated to under nowrap.
const (
	viewTailLines = 50
	viewWrapWidth = 80
)

// View returns the last viewTailLines lines of the named server's newest log.
// When nowrap is true, each line is truncated to viewWrapWidth runes.
func (e *RealEngine) View(name string, nowrap bool) ([]string, error) {
	if _, ok := e.serverByName(name); !ok {
		return nil, fmt.Errorf("view %s: unknown server", name)
	}
	logPath, ok, err := lifecycle.NewestLog(e.cfg.Supervisor.LogDir, name)
	if err != nil {
		return nil, fmt.Errorf("view %s: %w", name, err)
	}
	if !ok {
		return nil, fmt.Errorf("view %s: no log found", name)
	}
	lines, err := readLastLines(logPath, viewTailLines, nowrap)
	if err != nil {
		return nil, fmt.Errorf("view %s: %w", name, err)
	}
	return lines, nil
}

// readLastLines reads the last n non-empty-trailing lines from path. If nowrap
// is true, lines longer than viewWrapWidth runes are truncated to exactly that.
// It reads only the tail of the file, not the whole thing (see tailLines).
func readLastLines(path string, n int, nowrap bool) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return tailLines(f, info.Size(), n, nowrap)
}

// tailChunkSize is how many bytes tailLines reads per backward step.
const tailChunkSize = 8192

// tailLines returns the last n non-empty-trailing lines of the size-byte
// content readable via ra, reading backward from the end in tailChunkSize
// chunks so it never buffers more than the tail it needs. When nowrap is true,
// each returned line is truncated to viewWrapWidth runes.
func tailLines(ra io.ReaderAt, size int64, n int, nowrap bool) ([]string, error) {
	if size == 0 {
		return []string{}, nil
	}
	// Read backward from the end, keeping each chunk (end-first) so the tail is
	// assembled exactly once below — prepending per chunk would recopy the whole
	// accumulated buffer every step (quadratic on a long, newline-sparse tail).
	// A rune split across a chunk boundary is only decoded after the bytes are
	// rejoined in file order.
	var chunks [][]byte
	total := 0
	// newlines counts '\n' seen so far; trailing counts the run of '\n' at the
	// very end of the file (which may span more than one chunk). Their
	// difference is the number of line separators in the non-trailing content —
	// once it reaches n, the last n lines are all complete and the partial head
	// line can be dropped. Counting each chunk once keeps this linear.
	newlines, trailing := 0, 0
	inTrailing := true
	for frontier := size; frontier > 0; {
		readLen := min(frontier, int64(tailChunkSize))
		start := frontier - readLen
		chunk := make([]byte, readLen)
		nRead, err := ra.ReadAt(chunk, start)
		if err != nil && err != io.EOF {
			return nil, err
		}
		chunk = chunk[:nRead]
		chunks = append(chunks, chunk)
		total += len(chunk)
		newlines += bytes.Count(chunk, []byte{'\n'})
		if inTrailing {
			stripped := len(chunk) - len(bytes.TrimRight(chunk, "\n"))
			trailing += stripped
			if stripped < len(chunk) {
				inTrailing = false // a non-newline byte ends the trailing run
			}
		}
		frontier = start
		if newlines-trailing >= n {
			break
		}
	}
	// Reassemble in file order (chunks were collected end-first).
	buf := make([]byte, 0, total)
	for i := len(chunks) - 1; i >= 0; i-- {
		buf = append(buf, chunks[i]...)
	}
	return sliceTail(buf, n, nowrap), nil
}

// sliceTail turns the tail buffer read by tailLines into its final lines: split
// on newlines, drop trailing empty lines (log files always end with \n), keep
// the last n, and truncate each to viewWrapWidth runes when nowrap is set.
func sliceTail(buf []byte, n int, nowrap bool) []string {
	lines := strings.Split(string(buf), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if nowrap {
		for i, l := range lines {
			// Byte length bounds rune count, so a short line can't exceed the
			// width — skip the []rune allocation for the common case.
			if len(l) <= viewWrapWidth {
				continue
			}
			if r := []rune(l); len(r) > viewWrapWidth {
				lines[i] = string(r[:viewWrapWidth])
			}
		}
	}
	return lines
}

// ============================================================================
// TeardownAll
// ============================================================================

// TeardownAll tears down every child this supervisor ever launched this
// session (enabled or not), via lifecycle.TeardownAll, and returns their
// names — used only by the REPL's quit/exit/bye/EOF path (see repl.go's
// teardown wrapper). Any teardown failures are printed to stderr rather than
// silently swallowed, since the Engine interface's TeardownAll returns only
// []string (no error) and the REPL caller does not itself check for one.
func (e *RealEngine) TeardownAll() []string {
	e.mu.Lock()
	pids := make(map[string]int32, len(e.procs))
	names := make([]string, 0, len(e.procs))
	for name, proc := range e.procs {
		if proc.Cmd != nil && proc.Cmd.Process != nil {
			pids[name] = int32(proc.Cmd.Process.Pid)
			names = append(names, name)
		}
	}
	e.mu.Unlock()

	_, err := lifecycle.TeardownAll(context.Background(), e.cfg.Servers, e.cfg.Supervisor, pids)

	e.mu.Lock()
	for name := range pids {
		delete(e.procs, name)
	}
	e.mu.Unlock()

	if err != nil {
		fmt.Fprintf(os.Stderr, "aa-server-status: teardown: %v\n", err)
	}

	sort.Strings(names)
	return names
}
