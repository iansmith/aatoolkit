// Package observe is aa-server-status's observation layer: it answers "what is
// actually running" without ever killing or launching anything. See
// design/aa-server-status.md §6.1–§6.2 for the design this package implements.
//
// Two concerns live here:
//
//   - Identity: distinguishing a process aa-server-status itself spawned (an
//     "our-child", for which the supervisor holds an *exec.Cmd handle) from a
//     foreign process (matched only by PID + cmdline).
//   - Listen-set gathering and classification: walking a server's whole
//     process tree (e.g. a uvicorn parent plus its worker children, or an mlx
//     launcher plus its subprocess) to build the *actual* set of listening
//     TCP ports, then comparing it against the server's *declared* set
//     ({port} ∪ listens, treated as exhaustive).
package observe

import (
	"errors"
	"fmt"
	"os/exec"

	gopsnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// Identity names a single OS process for observation purposes: its PID, its
// cmdline (used to match foreign processes), and whether aa-server-status holds
// the *exec.Cmd for it (an "our-child") or merely observed it externally.
type Identity struct {
	PID     int32
	Cmdline []string
	Ours    bool
}

// NewOursIdentity builds the Identity for a process aa-server-status itself
// spawned and holds a live handle for. cmd.Process must be non-nil (i.e. the
// command must already have been started).
func NewOursIdentity(cmd *exec.Cmd) (*Identity, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("observe: NewOursIdentity requires a started *exec.Cmd")
	}
	pid := int32(cmd.Process.Pid)
	cmdline, err := cmdlineForPID(pid)
	if err != nil {
		return nil, fmt.Errorf("observe: cmdline for our-child pid %d: %w", pid, err)
	}
	return &Identity{PID: pid, Cmdline: cmdline, Ours: true}, nil
}

// NewForeignIdentity builds the Identity for an arbitrary PID that
// aa-server-status did not spawn, matched via its cmdline. It returns an error
// if the PID does not exist or its cmdline cannot be read.
func NewForeignIdentity(pid int32) (*Identity, error) {
	cmdline, err := cmdlineForPID(pid)
	if err != nil {
		return nil, fmt.Errorf("observe: cmdline for foreign pid %d: %w", pid, err)
	}
	return &Identity{PID: pid, Cmdline: cmdline, Ours: false}, nil
}

func cmdlineForPID(pid int32) ([]string, error) {
	proc, err := process.NewProcess(pid)
	if err != nil {
		return nil, err
	}
	return proc.CmdlineSlice()
}

// Holder pairs a listening port with the Identity of the process holding it.
type Holder struct {
	Port     int
	Identity Identity
}

// TreeObservation is the result of walking a process tree for its listen-set.
// Degraded lists PIDs discovered in the tree whose ports or cmdline could not
// be read (e.g. the process exited mid-walk, or a gopsutil call failed) —
// Result's declared-vs-actual comparison is computed only from what was
// successfully observed, so a non-empty Degraded means that comparison may be
// incomplete and callers should treat StrayPort/Partial verdicts on this
// observation with reduced confidence.
type TreeObservation struct {
	Holders  map[int]Holder
	Degraded []int32
}

// ListenSet returns the set of TCP ports the process tree rooted at rootPID
// is actually listening on — the root process plus every descendant
// (children, grandchildren, ...), so uvicorn workers or mlx subprocesses
// spawned under the root are included. Every Identity in the result has
// Ours=false; use TreeListenSet when the root (and its whole tree) should be
// marked as "ours".
func ListenSet(rootPID int32) (TreeObservation, error) {
	return treeListenSet(rootPID, false)
}

// TreeListenSet is like ListenSet, but marks every PID discovered under
// rootPID (the root itself plus all descendants) as "ours" in the returned
// Holders. Use this when rootPID is a process aa-server-status spawned, so that
// its whole tree (uvicorn workers, mlx subprocesses, etc.) is correctly
// identified as belonging to that server.
func TreeListenSet(rootPID int32) (TreeObservation, error) {
	return treeListenSet(rootPID, true)
}

// IsRootProcessGone reports whether err — as returned by TreeListenSet or
// ListenSet — reflects the root process being confirmed gone, as opposed to
// a transient failure to determine its state either way (AATK-87 Observable
// behavior 3). The only source of a non-nil error from treeListenSet is
// NewProcessHook's root-resolution call (process.NewProcess in production);
// on darwin its existence check signals the pid with 0 and switches on
// errno — ESRCH confirms gone (wrapped here as
// process.ErrorProcessNotRunning), EPERM confirms alive (no error at all),
// and any other errno falls through to a plain error that is neither. This
// keeps that distinction reachable through the existing %w-wrapped error
// without any change to TreeListenSet's or ListenSet's signature, and keeps
// the gopsutil coupling needed to express it inside this package — the only
// one that already imports gopsutil — rather than adding that import edge
// to a caller.
func IsRootProcessGone(err error) bool {
	return errors.Is(err, process.ErrorProcessNotRunning)
}

// treeListenSet walks the process tree rooted at rootPID, collecting every
// listening TCP port across the root and all descendants. ours marks every
// Holder in the result as belonging to that tree (the TreeListenSet
// behavior) or leaves them unmarked (the plain ListenSet behavior — callers
// classify ownership themselves).
func treeListenSet(rootPID int32, ours bool) (TreeObservation, error) {
	root, err := NewProcessHook(rootPID)
	if err != nil {
		return TreeObservation{}, fmt.Errorf("observe: root pid %d: %w", rootPID, err)
	}

	procs, err := collectTreeProcesses(root)
	if err != nil {
		return TreeObservation{}, err
	}

	obs := TreeObservation{Holders: make(map[int]Holder)}
	var corrob hostCorroboration
	for _, proc := range procs {
		ports, err := listeningPorts(proc)
		if err != nil {
			// A process can exit mid-walk (e.g. a short-lived worker), or a
			// gopsutil call can otherwise fail for it. Recorded as Degraded
			// rather than silently dropped: the exhaustive-set contract
			// Classify relies on only holds for what was actually observed.
			// Without knowing what ports (if any) this PID held, there is
			// nothing more to record for it — skip to the next process.
			obs.Degraded = append(obs.Degraded, proc.Pid)
			continue
		}
		if len(ports) == 0 {
			// AATK-87: a successful-but-empty per-pid read is ambiguous on
			// darwin — ConnectionsPidHook shells out to lsof and can
			// swallow an internal failure, returning (nil, nil) exactly as
			// it would for a process confirmed to hold no ports. Before
			// trusting this as "confirmed to hold no ports", corroborate
			// with a second, real observation — see hostCorroboration's doc
			// comment for exactly what that buys and does not buy.
			if corrob.ambiguousEmptyReadIsDegraded(proc.Pid) {
				obs.Degraded = append(obs.Degraded, proc.Pid)
			}
			continue
		}
		// The process IS listening on ports — that occupancy is real and
		// must still be reflected in Holders even if we can't confirm its
		// identity below, otherwise Classify would wrongly report those
		// ports as not listening. What's uncertain is only the Cmdline
		// (identity), not the port occupancy — so record Degraded for
		// visibility but keep the Holder, with a nil Cmdline signaling
		// "identity unconfirmed" to any caller that checks Result.Degraded.
		cmdline, err := proc.CmdlineSlice()
		if err != nil {
			obs.Degraded = append(obs.Degraded, proc.Pid)
			cmdline = nil
		}
		ident := Identity{PID: proc.Pid, Cmdline: cmdline, Ours: ours}
		for _, port := range ports {
			obs.Holders[port] = Holder{Port: port, Identity: ident}
		}
	}
	return obs, nil
}

// collectTreeProcesses returns root plus every descendant process, discovered
// via a breadth-first walk of Children(). Visited PIDs are tracked to guard
// against pathological cycles (which should not occur in a real process
// tree, but the walk must still terminate). Returning the *process.Process
// handles themselves (rather than just PIDs) lets callers reuse them for
// further gopsutil queries instead of re-resolving each PID from scratch.
func collectTreeProcesses(root *process.Process) ([]*process.Process, error) {
	visited := map[int32]bool{root.Pid: true}
	order := []*process.Process{root}
	queue := []*process.Process{root}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		children, err := current.Children()
		if err != nil {
			// No children (or the process exited) is not fatal to the walk.
			continue
		}
		for _, child := range children {
			if visited[child.Pid] {
				continue
			}
			visited[child.Pid] = true
			order = append(order, child)
			queue = append(queue, child)
		}
	}
	return order, nil
}

// ConnectionsPidHook is the seam listeningPorts calls through to gather a
// single process's TCP connections. It defaults to the real gopsutil call;
// production code must never reassign it. Tests — both in this package and
// in cmd/aa-server-status, which calls TreeListenSet directly — substitute
// it to force "the call succeeded but returned no rows" deterministically
// (AATK-87): on darwin, gopsnet.ConnectionsPid shells out to lsof and can
// swallow an internal failure, returning (nil, nil) exactly as it would for
// a process confirmed to hold no ports. A live subprocess alone cannot
// produce that "succeeded but is wrong" shape on demand — a real listener
// genuinely returns its ports, a real non-listener genuinely returns
// none — so tests that need it substitute this hook instead. A test that
// reassigns it must restore the original via t.Cleanup.
var ConnectionsPidHook = gopsnet.ConnectionsPid

// NewProcessHook is the seam treeListenSet calls through to resolve the root
// PID before walking its tree. It defaults to the real gopsutil call;
// production code must never reassign it. Tests — both in this package and
// in cmd/aa-server-status, which calls TreeListenSet directly — substitute
// it to force a root-process existence-check failure that is deterministic
// and, critically, distinguishable from "confirmed gone" (AATK-87): on
// darwin, process.NewProcess's PidExistsWithContext signals the PID with 0
// and switches on errno — ESRCH confirms gone (wrapped as
// process.ErrorProcessNotRunning by NewProcessWithContext), EPERM confirms
// alive (no error at all), and any other errno falls through to (false,
// err) — a transient observation failure that is neither. Producing that
// third case against a genuinely live, tracked root requires this seam: a
// bogus (<=0, or never-alive) PID also yields a non-ErrorProcessNotRunning
// error from the real unhooked call, but against a process that was never
// alive, which proves nothing about a live tracked child — the exact
// uninformative substitute this ticket's file map rules out. A test that
// reassigns it must restore the original via t.Cleanup.
var NewProcessHook = process.NewProcess

// corroborationResult is holdsListen's verdict for one pid.
type corroborationResult int

const (
	// corroborationEmpty: the corroborating read agrees the pid holds no
	// LISTEN socket — the original empty read is confirmed, not just
	// unrefuted.
	corroborationEmpty corroborationResult = iota
	// corroborationHolds: the corroborating read shows the pid DOES hold a
	// LISTEN socket — the original empty read was wrong.
	corroborationHolds
	// corroborationFailed: the corroborating read itself could not be
	// obtained — neither confirmed nor refuted.
	corroborationFailed
)

// hostCorroboration lazily fetches one host-wide TCP connection scan (real,
// unhooked — see below) and answers "does this pid hold a LISTEN socket"
// against it, reusing the single fetch across every ambiguous per-pid read
// in one treeListenSet walk rather than re-scanning per pid. The zero value
// is ready to use.
//
// DoD item 7 (AATK-87): this is NOT an independent second signal, and this
// package does not claim it is. gopsnet.Connections("tcp") resolves to
// ConnectionsPidWithContext(ctx, "tcp", 0) — the very same function
// ConnectionsPidHook (gopsnet.ConnectionsPid) calls, differing only in the
// pid argument, which downstream becomes lsof's `-p <pid>` flag versus no
// pid filter at all — so both ultimately shell out through gopsutil's
// common.CallLsofWithContext. That shared primitive is exactly why routing
// a corroboration through it without more would NOT satisfy DoD item 7: it
// would be a second draw on the very primitive that just failed, not a
// check on it. What makes this corroboration real is that it does not
// assume the two calls fail independently — it detects their shared
// failure mode instead. common.CallLsofWithContext returns a non-nil error
// only for exec.ErrNotFound (lsof missing) or an "exit status 1" paired
// with empty stdout (a legitimate empty result); for every other failure —
// a transient lsof error, a truncated pipe, anything else — it does not
// return inside the error branch at all. It falls through, parses whatever
// partial output it captured, and returns a nil error. That is the swallow
// both ConnectionsPidHook and this host-wide call share: a failed lsof
// invocation can surface as "succeeded, zero rows," indistinguishable in
// isolation from a genuinely idle scan. holdsListen closes that hole
// directly (see its doc comment): a host-wide `tcp` scan returning zero
// rows on a live machine is never legitimate (measured: 195 rows on an idle
// host), so an empty h.conns is itself treated as the signature of the
// swallow rather than as "confirmed nothing is listening host-wide." The
// two lsof invocations are still correlated under host contention (a
// starved host makes both slow or flaky together), but the corroboration no
// longer needs them to be independent to be informative — it recognizes the
// specific shape their shared failure mode produces and refuses to trust
// it. This is also the only signal darwin exposes here — every gopsutil
// net-enumeration path on darwin converges on the same lsof primitive — so
// writing a native darwin socket enumerator remains out of scope for this
// ticket.
type hostCorroboration struct {
	fetched bool
	conns   []gopsnet.ConnectionStat
	err     error
}

// holdsListen reports, for pid, whether a host-wide TCP scan (fetched once
// and cached for the lifetime of this hostCorroboration) shows it holding a
// LISTEN socket. An empty scan (h.conns has zero rows) is treated the same
// as a fetch error — corroborationFailed — because on any live host a
// genuine `lsof -i tcp` essentially never returns zero rows (measured: 195
// rows on an idle host). A zero-row result is the signature of the
// swallowed lsof failure documented on hostCorroboration, not a legitimate
// host-wide "nothing is listening" finding, so it must not be read as
// confirmation that the ambiguous per-pid read really found nothing.
// Treating it as failed keeps the direction conservative: it leaves the pid
// unconfirmed (Degraded) rather than confidently declaring it empty.
func (h *hostCorroboration) holdsListen(pid int32) corroborationResult {
	if !h.fetched {
		h.conns, h.err = gopsnet.Connections("tcp")
		h.fetched = true
	}
	if h.err != nil {
		return corroborationFailed
	}
	if len(h.conns) == 0 {
		return corroborationFailed
	}
	for _, c := range h.conns {
		if c.Pid == pid && c.Status == "LISTEN" {
			return corroborationHolds
		}
	}
	return corroborationEmpty
}

// ambiguousEmptyReadIsDegraded reports whether a per-pid read that came back
// successful-but-empty for pid should be recorded as Degraded, per a
// corroborating host-wide scan (see holdsListen). Only an agreeing
// corroborationEmpty clears the read as confirmed-empty; the other two
// verdicts both leave the observation unconfirmed:
//
//   - corroborationHolds: the per-pid read was wrong — the corroborating read
//     shows this pid actually holding a LISTEN socket. Recorded as Degraded
//     rather than silently dropped, same reasoning as treeListenSet's
//     per-process read error case. The occupancy is real (this process
//     really does hold something) but which port(s) is unconfirmed, so
//     nothing is added to Holders — a Holder entry claims confident, specific
//     occupancy this observation cannot back up.
//   - corroborationFailed: the corroborating call itself failed. That must
//     not collapse into "found nothing" — doing so would just reproduce
//     AATK-87 one layer downstream, trusting an unconfirmed empty read as
//     confirmed because the second opinion couldn't be reached either. Treat
//     the process as unconfirmed (Degraded), not as confirmed-empty.
//   - corroborationEmpty: the corroborating read agrees — this pid genuinely
//     holds no LISTEN socket. Confirmed-empty, not Degraded.
func (h *hostCorroboration) ambiguousEmptyReadIsDegraded(pid int32) bool {
	return h.holdsListen(pid) != corroborationEmpty
}

// listeningPorts returns the local TCP ports proc is listening on.
func listeningPorts(proc *process.Process) ([]int, error) {
	conns, err := ConnectionsPidHook("tcp", proc.Pid)
	if err != nil {
		return nil, err
	}
	var ports []int
	for _, c := range conns {
		if c.Status == "LISTEN" {
			ports = append(ports, int(c.Laddr.Port))
		}
	}
	return ports, nil
}

// SystemListenSet returns every TCP port currently in LISTEN state anywhere
// on the host, mapped to the PID holding it — a host-wide counterpart to
// ListenSet/TreeListenSet's single-process-tree scope. Used by teardown's
// post-kill verify step (design/aa-server-status.md §6.4): after a kill, the
// question is simply "is this port free," regardless of which process (if
// any) might hold it, so the check is not scoped to any one process tree.
// Like listeningPorts, this only ever counts LISTEN-state sockets — a
// lingering TIME_WAIT entry does not hold LISTEN and is correctly excluded.
func SystemListenSet() (map[int]int32, error) {
	conns, err := gopsnet.Connections("tcp")
	if err != nil {
		return nil, fmt.Errorf("observe: system-wide listen scan: %w", err)
	}
	holders := make(map[int]int32)
	for _, c := range conns {
		if c.Status == "LISTEN" {
			holders[int(c.Laddr.Port)] = c.Pid
		}
	}
	return holders, nil
}

// Classification is the result of comparing a server's actual listen-set
// against its declared set ({port} ∪ listens, treated as exhaustive). See
// design/aa-server-status.md §6.2.
type Classification int

const (
	// CandidateUp: actual == declared. The final "serving" call belongs to
	// the health-probe module — this package only reports the port-level
	// match.
	CandidateUp Classification = iota
	// Partial: actual is a strict subset of declared — some declared ports
	// are not yet listening.
	Partial
	// StrayPort: actual contains a port outside the declared set — a loud
	// anomaly.
	StrayPort
)

func (c Classification) String() string {
	switch c {
	case CandidateUp:
		return "candidate-up"
	case Partial:
		return "partial"
	case StrayPort:
		return "stray-port"
	default:
		return "unknown"
	}
}

// Result is the outcome of classifying one server's observed state against
// its declared port set.
type Result struct {
	Classification Classification
	// Declared is the input declared set, deduplicated.
	Declared []int
	// Actual is the set of ports actually found listening across the
	// server's process tree.
	Actual []int
	// Missing is Declared ports with no actual listener. Can be non-empty
	// even when Classification is StrayPort: a stray port and a missing
	// port are independent anomalies that may occur together, and StrayPort
	// takes priority in Classification as the louder one — check Missing
	// directly rather than assuming it is empty outside of Partial.
	Missing []int
	// Stray is Actual ports outside Declared (non-empty only for StrayPort;
	// unlike Missing, Stray is genuinely tied to that one Classification
	// value, since any non-empty Stray always makes Classification
	// StrayPort).
	Stray []int
	// ForeignHolders maps a declared port that IS actually listening, but
	// whose holder is not "ours", to that holder. This is what feeds the
	// `up` precondition gate and BLOCKED status rendering: a needed port
	// held by a process that isn't the server's own child must be
	// surfaced by PID + cmdline so the user can decide.
	ForeignHolders map[int]Identity
	// Degraded carries forward TreeObservation.Degraded: PIDs in the
	// observed tree whose ports or cmdline could not be read. A non-empty
	// Degraded means the exhaustive-set comparison above may be incomplete
	// — callers should treat the Classification with reduced confidence
	// rather than as a fully-confirmed verdict.
	Degraded []int32
}

// Classify compares declared (the server's {port} ∪ listens, exhaustive) to
// obs (the observed listen-set across the whole process tree, as returned by
// TreeListenSet or ListenSet). Duplicate ports in declared are ignored.
//
//   - actual ⊋ declared → StrayPort (loud anomaly): actual contains a port
//     outside declared. Takes priority over Partial if both a missing
//     declared port and a stray port are present simultaneously, since a
//     stray port is the louder anomaly.
//   - actual ⊊ declared → Partial: every actual port is declared, but at
//     least one declared port has no listener yet.
//   - actual == declared → CandidateUp.
func Classify(declared []int, obs TreeObservation) Result {
	actual := obs.Holders

	declSet := make(map[int]bool, len(declared))
	dedup := make([]int, 0, len(declared))
	for _, p := range declared {
		if !declSet[p] {
			declSet[p] = true
			dedup = append(dedup, p)
		}
	}

	actualPorts := make([]int, 0, len(actual))
	for p := range actual {
		actualPorts = append(actualPorts, p)
	}

	var missing, stray []int
	foreign := make(map[int]Identity)

	for _, p := range dedup {
		holder, ok := actual[p]
		if !ok {
			missing = append(missing, p)
			continue
		}
		if !holder.Identity.Ours {
			foreign[p] = holder.Identity
		}
	}
	for p := range actual {
		if !declSet[p] {
			stray = append(stray, p)
		}
	}

	var class Classification
	switch {
	case len(stray) > 0:
		class = StrayPort
	case len(missing) > 0:
		class = Partial
	default:
		class = CandidateUp
	}

	return Result{
		Classification: class,
		Declared:       dedup,
		Actual:         actualPorts,
		Missing:        missing,
		Stray:          stray,
		ForeignHolders: foreign,
		Degraded:       obs.Degraded,
	}
}
