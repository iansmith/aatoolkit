// RealEngine is the reconciliation engine wiring launchers
// (internal/lifecycle), teardown (internal/lifecycle), observation
// (internal/observe), and health (internal/health) into the fleet verbs
// (design/aa-server-status.md §2, §3, §6.3, §6.5). It replaces StubEngine as
// aa-server-status's live Engine implementation.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// cfg is replaced wholesale by the config hot-reload (AATK-27), never
	// mutated in place. It is an atomic.Pointer rather than a mu-guarded
	// field because Up fans its targets out across goroutines that each
	// read config on the way to launching: guarding it with mu would put a
	// lock acquisition on those hot paths and raise a lock-ordering
	// question with the procs registry mu already protects.
	cfg atomic.Pointer[config.Config]

	mu    sync.Mutex
	procs map[string]*lifecycle.Process // server name -> our live child, if any

	// reloader is nil unless WatchConfig was called; a nil reloader means
	// this engine never re-reads its config, which is the right default for
	// every construction path that isn't main's.
	reloader *configReloader

	// promptIn/promptOut carry the streams a [server.prompt] is asked on.
	// promptIn is the caller's reader, stored as-is and never re-wrapped:
	// wrapping it here would create a second buffer over the same stream,
	// racing the REPL loop's own reads for input (AATK-29). When stdin is
	// the source, main hands the same *bufio.Reader to both.
	// Both are nil for engines constructed without them, which is safe as
	// long as no target declares a prompt — askPrompts errors loudly if one
	// does.
	promptIn  *bufio.Reader
	promptOut io.Writer
}

// NewEngine builds a RealEngine over cfg. No processes are launched by
// construction — the registry starts empty, matching a freshly started
// supervisor that hasn't reconciled anything yet.
func NewEngine(cfg config.Config, promptIn *bufio.Reader, promptOut io.Writer) *RealEngine {
	e := &RealEngine{procs: make(map[string]*lifecycle.Process), promptIn: promptIn, promptOut: promptOut}
	e.cfg.Store(&cfg)
	return e
}

// config returns a snapshot of the engine's live configuration. Callers that
// read more than one field must capture the snapshot once rather than calling
// this per field: a reload landing mid-operation would otherwise let a single
// operation observe two different configs.
//
// Assumes construction went through NewEngine, which always stores a config —
// a zero-value RealEngine would nil-deref here (as it would already panic on
// its nil procs map).
//
// The returned value shares slice backing arrays with the stored config, which
// is safe precisely because a stored config is never mutated in place — the
// reload path swaps the whole pointer.
func (e *RealEngine) config() config.Config { return *e.cfg.Load() }

var _ Engine = (*RealEngine)(nil)

// serverByName returns the configured server named name, or ok=false if no
// such server exists.
func (e *RealEngine) serverByName(name string) (config.Server, bool) {
	for _, s := range e.config().Servers {
		if s.Name == name {
			return s, true
		}
	}
	return config.Server{}, false
}

// pidOf returns p's OS pid and true when p is a live child we can observe,
// else 0, false. One definition of what "we hold a live process" means, so a
// change to lifecycle.Process's nil-safety contract lands in one place.
func pidOf(p *lifecycle.Process) (int32, bool) {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return 0, false
	}
	return int32(p.Cmd.Process.Pid), true
}

// livePID returns the PID and true if we currently hold a live child
// process for name (registered by a prior Up), else 0, false. Must be
// called with e.mu held.
func (e *RealEngine) livePIDLocked(name string) (int32, bool) {
	p, ok := e.procs[name]
	if !ok {
		return 0, false
	}
	return pidOf(p)
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

	servers := e.config().Servers
	out := make([]ServerStatus, 0, len(servers))
	for _, s := range servers {
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
	var unconfirmedRoot bool
	if isOurs {
		obs, unconfirmedRoot = observeOwnedTree(pid)
	} else {
		obs = hostObservationFor(declared)
	}

	class := observe.Classify(declared, obs)

	status.Ports = renderPorts(declared, class)

	unconfirmedObservation := isUnconfirmedObservation(isOurs, class, unconfirmedRoot)

	switch {
	case len(class.Actual) == 0 && !unconfirmedObservation:
		// Nothing listening at all for this server's declared ports, and
		// nothing about that read was left unconfirmed.
		status.State = StateDown
	case isOurs:
		status.PID = int(pid)
		status.State = classifyOwned(s, class)
		switch {
		case unconfirmedObservation:
			// AATK-87 F3: this branch was reached only because
			// unconfirmedObservation suppressed the StateDown case above —
			// class.Actual is empty and nothing about this cycle's read was
			// confirmed. classifyOwned still renders StatePartial here
			// (every declared port shows not-listening), which is a
			// confident-looking verdict built from a read that could not be
			// confirmed. Say so, rather than let it render as a plain,
			// unqualified partial.
			status.AnomalyDetail = "observation unconfirmed this cycle — reported ports may not reflect reality"
		case len(class.Degraded) > 0:
			// AATK-87 F2: class.Actual is non-empty here (unconfirmedObservation
			// requires it to be empty), so the declared ports this server
			// actually cares about are genuinely confirmed listening — the
			// State above is trustworthy. But some OTHER member of this
			// same process tree came back with an ambiguous read that
			// corroboration could not resolve either way (class.Degraded).
			// That member could be holding an undeclared (stray) port we
			// simply didn't see this cycle — internal/observe's forced-empty
			// read erases a real stray exactly as it would erase a real
			// declared listener, so its absence from class.Stray is not
			// confirmation there isn't one. Flag it instead of rendering a
			// confident, anomaly-free state.
			//
			// The wording stays generic because class.Degraded has three
			// sources, and only two of them are listen-set failures: a
			// member's CmdlineSlice() can fail while its ports were read
			// fine and kept in Holders (see TreeObservation.Degraded), and
			// for that source claiming the listen-set is unconfirmed — or
			// that a stray cannot be ruled out — would be false in both
			// halves and send an operator hunting a listener the walk had
			// already excluded.
			status.AnomalyDetail = "a tree member could not be fully observed — treat this reading as incomplete"
		}
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
		if s.Health.Declared() {
			spec := health.ResolveSpec(s.Health, s.Host, s.Port)
			result := health.Probe(context.Background(), spec, resolveHealthTimeout(e.config().Supervisor))
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

// observeOwnedTree resolves the listen-set observation for a live, owned
// child (isOurs == true in statusForLocked), consulting TreeListenSet's own
// error return to distinguish a confirmed-gone root from a merely
// unconfirmed one. The returned bool — unconfirmedRoot — is set when
// TreeListenSet's error return could not confirm the root is gone (AATK-87
// Observable behavior 3): the returned observation stays zero-valued exactly
// as the discarded-error code used to leave it, but unlike a confirmed-gone
// root, that zero value must not be read as "down" by the caller.
func observeOwnedTree(pid int32) (obs observe.TreeObservation, unconfirmedRoot bool) {
	treeObs, err := observe.TreeListenSet(pid)
	switch {
	case err == nil:
		obs = treeObs
	case observe.IsRootProcessGone(err):
		// Confirmed gone: obs stays zero-valued, and the
		// len(class.Actual) == 0 arm below renders State: down, exactly
		// the pre-AATK-87 behavior for this case (Observable behavior 3,
		// first bullet).
	default:
		// Neither confirmed alive nor confirmed gone — a transient
		// existence-check failure (e.g. an errno that is neither ESRCH
		// nor EPERM). obs stays zero-valued, but this is an unconfirmed
		// observation, not a confirmed-down one.
		unconfirmedRoot = true
	}
	return obs, unconfirmedRoot
}

// isUnconfirmedObservation reports whether we hold a live child (isOurs) but
// could not confirm its listen-set this cycle — either the root's own
// existence-check failed transiently (unconfirmedRoot) or a per-process read
// came back ambiguous and corroboration could not rule out real occupancy
// (internal/observe's Degraded, carried into class.Degraded; see
// treeListenSet's hostCorroboration).
//
// The len(class.Actual) == 0 term is load-bearing. statusForLocked consults
// this predicate at two places, not one:
//
//   - `case len(class.Actual) == 0 && !unconfirmedObservation:`, where the
//     term is indeed redundant because the case already establishes it; and
//   - the inner `case unconfirmedObservation:` under `case isOurs:`, where it
//     is not. That inner switch is also reached with class.Actual NON-empty,
//     and the term is the only thing separating F3 (nothing about this cycle
//     was confirmed) from F2 (the declared ports are confirmed, some other
//     tree member is not) — two branches that render different AnomalyDetail
//     text.
//
// Dropping the term therefore makes the F2 scenario report F3's message:
// removing it and re-running that shape rendered "observation unconfirmed
// this cycle …" in place of the tree-member text. The suite does not catch
// that, because TestStatusForLocked_UnrelatedTreeMemberDegraded_KeepsConfirmedStateFlagsUncertainty
// asserts only that AnomalyDetail is non-empty and never inspects its text.
// That is a coverage gap, not an equivalence — a mutant surviving here means
// the assertion is too weak, not that the term is inert.
func isUnconfirmedObservation(isOurs bool, class observe.Result, unconfirmedRoot bool) bool {
	return isOurs && len(class.Actual) == 0 && (unconfirmedRoot || len(class.Degraded) > 0)
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
	return m[sortedKeys(m)[0]]
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

	// Every [server.prompt] is asked and answered here, before the fan-out
	// below starts a single goroutine. See askPrompts for why this cannot
	// live inside upOne.
	targets, err = e.askPrompts(targets)
	if err != nil {
		return err
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
	for _, s := range e.config().Servers {
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

	if err := e.buildProbeAndPollReady(s, proc); err != nil {
		return verbOutcome{Name: s.Name, Err: err, LogPath: proc.LogPath}
	}
	return verbOutcome{Name: s.Name, LogPath: proc.LogPath}
}

// buildProbeAndPollReady runs the two steps that must happen, in order,
// after a server's process has started and been registered, and before it
// can be reported up: build s's declared health-probe source (AATK-41), if
// any — a no-op when Health.Source isn't declared — then poll health.
//
// This must run once here, not inside the probe itself: probeExec has no
// build step to run against, by design (design/aa-server-status.md §6.1).
// The three launch paths that reach this point (upOne's cold-launch body,
// and rebuildIfStaleOwned/rebuildIfStaleCold's Start callbacks) all funnel
// through this one function rather than repeating the sequence, so a future
// change to the ordering can't land in two of the three and miss the third.
func (e *RealEngine) buildProbeAndPollReady(s config.Server, proc *lifecycle.Process) error {
	if _, err := lifecycle.EnsureHealthProbeBuilt(s); err != nil {
		return err
	}
	return e.pollReady(s, proc)
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
			return e.buildProbeAndPollReady(s, proc)
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
			return e.buildProbeAndPollReady(s, proc)
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
	logDir := e.config().Supervisor.LogDir
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
		PollInterval: resolvePollInterval(e.config().Supervisor),
		Timeout:      budget,
		ServerName:   s.Name,
		LogPath:      proc.LogPath,
	})
	return err
}

// pollReady warms s up (if it declares a warm-up), polls its health endpoint
// (if it declares one), and finally verifies its exact listen-set, until it
// is ready or its resolved ready-timeout elapses.
//
// ready_timeout bounds all three together, not each. It is the answer to "how
// long may this server take to become ready", and a server does not become
// ready twice: giving each stage the full budget would let a server declaring
// 180s take 540s before `up` reports anything, which is neither what the knob
// says nor what an operator watching a REPL would assume.
//
// The port verdict always wins when it has one to render — not only on the
// path where warm-up and health both succeed. If warm-up or health fails
// first, that looks identical from the port check's point of view to health
// never having been declared at all: either way, ports have to be observed to
// know whether the declared set ever appeared. A warm-up or health failure
// that turns out to be caused by a declared port never binding — the server
// never started listening, so nothing it needed to warm up or answer health
// checks on was ever there — should be reported as the missing port, not as
// "did not become healthy", which sends an operator looking at the wrong
// layer. Both early-return sites below route through pollPortsVerdict, which
// renders the same pollPortsReady call the unconditional tail path always
// made and lets it override the stage error; the stage error survives only
// when the ports are fine or there is no live child to observe.
func (e *RealEngine) pollReady(s config.Server, proc *lifecycle.Process) error {
	sup := e.config().Supervisor
	deadline := time.Now().Add(resolveReadyTimeout(s, sup))

	if err := e.warmUp(s, proc, time.Until(deadline)); err != nil {
		return e.pollPortsVerdict(s, proc, deadline, err)
	}

	if s.Health.Declared() {
		// Whatever the warm-up did not spend. A warm-up that used the whole
		// budget leaves a non-positive remainder; PollReady always attempts one
		// probe regardless, so the server still gets a chance to answer rather
		// than failing on arithmetic alone.
		spec := health.ResolveSpec(s.Health, s.Host, s.Port)
		cfg := health.PollConfig{
			Spec:         spec,
			ProbeTimeout: resolveHealthTimeout(sup),
			PollInterval: resolvePollInterval(sup),
			ReadyTimeout: time.Until(deadline),
			ServerName:   s.Name,
			LogPath:      proc.LogPath,
		}
		if _, err := health.PollReady(context.Background(), cfg); err != nil {
			return e.pollPortsVerdict(s, proc, deadline, err)
		}
	}

	// Deliberately last, and deliberately outside the Health.Declared() branch
	// above — see pollPortsReady's own comment for both reasons.
	return e.pollPortsVerdict(s, proc, deadline, nil)
}

// pollPortsVerdict is the single decision point pollReady's three exits — the
// warm-up early return, the health early return, and the unconditional tail
// — all route through, so the port verdict is rendered the same way and can
// win the same way regardless of which stage failed or how much of the
// shared budget it spent getting there.
//
// stageErr is the fallback: the error of whichever stage failed first
// (warm-up or health), or nil on the path where both succeeded (or were
// never declared). It is what pollPortsVerdict returns when pollPortsReady
// has nothing to say — either because the exact-listen-set check itself
// passes, so the failure genuinely was the earlier stage's and not a missing
// port, or because there is no live PID to check against at all.
//
// No live child means there is no tree to observe — the same judgement
// livePIDLocked makes about the same state, and not a new error on top of
// stageErr. The three real launch paths cannot reach it: launch() returns an
// error if the child never started and upOne returns on that before
// pollReady runs, so a pid exists by construction here. It is reachable only
// from a unit test that fabricates a Process to exercise the warm-up and
// health stages without a subprocess (warm_order_test.go). Deciding it here
// keeps pollPortsReady itself unconditional: given a pid, it verifies.
func (e *RealEngine) pollPortsVerdict(s config.Server, proc *lifecycle.Process, deadline time.Time, stageErr error) error {
	pid, ok := pidOf(proc)
	if !ok {
		return stageErr
	}
	if err := e.pollPortsReady(s, pid, time.Until(deadline)); err != nil {
		return err
	}
	return stageErr
}

// pollPortsReady enforces the exact-listen-set contract
// (design/aa-server-status.md §6.2) against a server we have just launched:
// its observed listen-set, unioned across its whole process tree, must equal
// its declared {port} ∪ listens. A declared port that never appears leaves it
// `partial`; a port bound outside the declared set is a stray, the loud
// anomaly. Either one fails `up` for this server on exactly the same terms as
// a failed health check — the error is reported and the child is left running
// and registered, so its log survives for inspection and `down` can still
// tear it down.
//
// Why this runs LAST, after warm-up and health, rather than first:
// ports appear asynchronously after exec, so this stage has to poll — and
// warm-up and health already absorb that wait, because both are HTTP requests
// against a declared port and neither can answer until the tree is listening.
// Running last therefore costs nothing in the common case: by the time health
// has returned 2xx the ports are necessarily bound, the first probe here
// succeeds, and no polling happens at all. Running it first would serialize a
// wait the later stages were going to perform anyway, adding its full duration
// to every successful `up`.
//
// Why it runs even when no health check is declared: that is the case it is
// worth most for. Such a server previously got no readiness verification of
// any kind — `up` reported success on exec not failing — so an early return on
// Health.Declared() would skip the check for precisely the servers with
// nothing else watching them.
//
// It runs, too, when warm-up or health has already failed — the only caller
// of this function is pollPortsVerdict, invoked from both of pollReady's
// early-return sites as well as its unconditional tail. A warm-up
// or health failure caused by a declared port never binding looks, from
// here, exactly like the "no health declared" case above: either way this is
// the only stage that can tell the difference between "the server is merely
// slow" and "the server never bound what it declared". Skipping the check on
// an earlier stage's error would leave exactly that distinction unreported.
func (e *RealEngine) pollPortsReady(s config.Server, pid int32, budget time.Duration) error {
	declared := lifecycle.DeclaredPorts(s)
	if len(declared) == 0 {
		return nil
	}

	deadline := time.Now().Add(budget)
	interval := resolvePollInterval(e.config().Supervisor)

	var (
		class  observe.Result
		obs    observe.TreeObservation
		obsErr error
	)
	for {
		// One probe always runs, whatever the budget arithmetic says, so a
		// server is never failed on a non-positive remainder alone. PollReady
		// makes the same guarantee, for the same reason.
		probeStart := time.Now()
		obs, obsErr = observe.TreeListenSet(pid)
		if obsErr == nil {
			class = observe.Classify(declared, obs)
			if class.Classification == observe.CandidateUp && len(class.ForeignHolders) == 0 {
				return nil
			}
		}
		probeCost := time.Since(probeStart)

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Never sleep less than the probe itself took, so this loop cannot
		// exceed a 50% duty cycle however the interval is configured.
		// resolvePollInterval is tuned for health.PollReady's local HTTP
		// round-trip; a TreeListenSet probe is far more expensive — on darwin
		// gopsutil has no connections-by-pid syscall and shells out to `lsof`
		// once per process in the tree, measured at ~100ms for a single-process
		// tree and scaling with it. AATK-87 roughly doubles that for the shape
		// this loop actually sees: while a server is still binding, every tree
		// member's per-pid read comes back empty, and internal/observe then
		// fires one host-wide `lsof -i tcp` corroboration per walk — measured
		// at ~197ms per probe against a live non-listening single process,
		// versus ~101ms with the corroboration stubbed out. The loop stays
		// inside its wall-clock budget either way; the cost is paid in fewer
		// probes per ready_timeout, so bind detection lags by up to one extra
		// probe interval. At the interval health uses, a server that
		// is slow to bind would otherwise fork subprocesses back-to-back for
		// the whole ready_timeout. Backing off by the observed cost adapts to
		// the real tree size instead of guessing at a second constant.
		time.Sleep(min(remaining, max(interval, probeCost)))
	}

	if obsErr != nil {
		return fmt.Errorf("up %s: observing listening ports: %w", s.Name, obsErr)
	}
	return listenSetError(s, class, obs)
}

// listenSetError renders a failed exact-listen-set check as one actionable
// error naming every anomaly found: declared ports that never appeared, ports
// the tree bound without declaring — each with the PID holding it, which is
// what makes a stray actionable rather than merely alarming — and declared
// ports held by a process this supervisor did not start.
func listenSetError(s config.Server, class observe.Result, obs observe.TreeObservation) error {
	var parts []string

	if len(class.Missing) > 0 {
		listed := listPorts(class.Missing, nil)
		parts = append(parts, fmt.Sprintf("declared %s never started listening", listed))
	}

	if len(class.Stray) > 0 {
		listed := listPorts(class.Stray, func(p int) (int32, bool) {
			h, ok := obs.Holders[p]
			if !ok || h.Identity.PID == 0 {
				return 0, false
			}
			return h.Identity.PID, true
		})
		parts = append(parts, fmt.Sprintf("this server's process tree is listening on undeclared %s", listed))
	}

	if len(class.ForeignHolders) > 0 {
		listed := listPorts(sortedKeys(class.ForeignHolders), func(p int) (int32, bool) {
			return class.ForeignHolders[p].PID, true
		})
		parts = append(parts, fmt.Sprintf("declared %s held by a process this supervisor did not start", listed))
	}

	if len(class.Degraded) > 0 {
		parts = append(parts, fmt.Sprintf("%d process(es) in the tree could not be read, so this comparison may be incomplete", len(class.Degraded)))
	}

	if len(parts) == 0 {
		return fmt.Errorf("up %s: could not confirm the declared listen-set within the ready timeout", s.Name)
	}
	return fmt.Errorf("up %s: %s (design §6.2: {port} ∪ listens is exhaustive)", s.Name, strings.Join(parts, "; "))
}

// listPorts renders ports for a prose message, sorted for determinism and
// pluralized to match: "port 8080", "ports 8080, 8081". When pidFor resolves a
// PID for a port, it is cited as "8080 (pid 4711)" — naming the holder is what
// makes an anomaly actionable rather than merely alarming. A nil pidFor names
// no PIDs, for the cases where there is no holder to name.
func listPorts(ports []int, pidFor func(port int) (int32, bool)) string {
	sorted := append([]int(nil), ports...)
	sort.Ints(sorted)

	named := make([]string, len(sorted))
	for i, p := range sorted {
		named[i] = fmt.Sprintf("%d", p)
		if pidFor == nil {
			continue
		}
		if pid, ok := pidFor(p); ok {
			named[i] = fmt.Sprintf("%d (pid %d)", p, pid)
		}
	}

	if len(named) == 1 {
		return "port " + named[0]
	}
	return "ports " + strings.Join(named, ", ")
}

// sortedKeys returns m's port keys in ascending order, so a message built from
// a map is deterministic.
func sortedKeys(m map[int]observe.Identity) []int {
	ports := make([]int, 0, len(m))
	for p := range m {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
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

// Bounce takes one named server down and immediately back up. It is a
// literal composition of the public Down and Up — deliberately not a
// parallel teardown/launch path — so everything Up already does (port
// preconditions, stale-source rebuilds, readiness polling) and everything
// it grows later reaches bounce for free, with no maintenance here.
//
// A Down failure aborts the bounce and Up is never attempted: never launch
// on top of an incomplete teardown. Bouncing an already-down server needs
// no special case — Down is a no-op there, leaving a plain Up.
//
// A named target is required. Every other bare verb acts on the whole
// fleet, but cycling every enabled server is a materially different risk
// (long warm-ups, externally-facing ports) than any of them, so it is
// refused here rather than silently inherited from Down/Up's own
// empty-name fleet semantics.
func (e *RealEngine) Bounce(name string) error {
	if name == "" {
		return fmt.Errorf("bounce: a server name is required (whole-fleet bounce is not supported)")
	}
	if err := e.Down(name); err != nil {
		return err
	}
	return e.Up(name)
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
	for _, s := range e.config().Servers {
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
	target := lifecycle.Target{
		Name:   s.Name,
		PID:    pid,
		Ports:  lifecycle.DeclaredPorts(s),
		Health: health.SpecFor(s),
	}
	grace := lifecycle.ResolveGracePeriod(s, e.config().Supervisor)
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
		grace := lifecycle.ResolveGracePeriod(s, e.config().Supervisor)
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
				// Re-ask the server's [server.prompt], if it has one, and
				// relaunch on the freshly chosen branch. s came from the
				// stored config, where no answer exists — launching it
				// directly is what silently put a rebuilt server on the
				// other branch (AATK-34).
				//
				// Resolved here rather than at the top of Build because
				// PerformBuild returns early when nothing is stale: asking
				// up front would question the operator on every `build`,
				// including the common case where it then does nothing at
				// all. The cost is that the question arrives after the
				// teardown; the operator asked for a rebuild, so activity
				// at that point is expected.
				//
				// A prompt that cannot be answered fails the relaunch
				// rather than falling back to a default. The binary has
				// already been replaced by then and the server is left
				// down, which is the honest outcome of a refused relaunch
				// — better than guessing which branch was wanted.
				resolved, err := e.askPrompts([]config.Server{s})
				if err != nil {
					return err
				}
				launchable := resolved[0]

				proc, err := e.launch(launchable)
				if err != nil {
					return err
				}
				e.mu.Lock()
				e.procs[launchable.Name] = proc
				e.mu.Unlock()
				return e.pollReady(launchable, proc)
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
	logPath, ok, err := lifecycle.NewestLog(e.config().Supervisor.LogDir, name)
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
		if pid, ok := pidOf(proc); ok {
			pids[name] = pid
			names = append(names, name)
		}
	}
	e.mu.Unlock()

	cfg := e.config()
	_, err := lifecycle.TeardownAll(context.Background(), cfg.Servers, cfg.Supervisor, pids)

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
