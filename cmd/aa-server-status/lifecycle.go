package main

import (
	"fmt"

	"github.com/iansmith/aatoolkit/config"
)

// ServerState is the observed lifecycle state of a server — the STATE
// column's base classification, before the owned-disabled override or the
// stale override (see render.go's formatStateCell) is applied. Values are
// the literal display tokens the status table renders (SOP-19 /
// design/aa-server-status.md §8): StateStray and StateBlocked are uppercase to
// match the ticket's inline-annotation examples, the rest lowercase.
type ServerState string

const (
	StateUp              ServerState = "up"
	StateDown            ServerState = "down"
	StateDisabled        ServerState = "disabled"
	StateStray           ServerState = "STRAY"
	StatePartial         ServerState = "partial"
	StateExtraListener   ServerState = "extra-listener"
	StateForeignConflict ServerState = "foreign-conflict"
	StateBlocked         ServerState = "BLOCKED"
)

// PortStatus is one port in a server's declared/actual port set, as shown in
// the status table's PORTS column: a declared port renders ✓ (actually
// listening) or ✗ (not listening); a port actually listening but outside
// the server's declared set is an "unexpected extra listener" and renders
// its own "+<port> ✗unexpected" annotation instead of the ✓/✗ form.
type PortStatus struct {
	Port       int
	Up         bool
	Unexpected bool
}

// ServerStatus is one row of the status table (SOP-19 / design/aa-server-status.md
// §8). PID, Health, Ports, and the anomaly fields are placeholders until the
// reconciliation engine (a downstream ticket) supplies real observed data —
// StubEngine.Status() below leaves them zero-valued.
type ServerStatus struct {
	Name    string
	Type    config.ServerType
	Enabled bool
	State   ServerState

	// Ports is the per-port declared/actual breakdown for the PORTS column.
	Ports []PortStatus
	// PID is 0 when the server isn't running (nothing observed yet).
	PID int
	// Health is the pre-rendered "path code" HEALTH cell (e.g.
	// "/v1/models 200", matching internal/health.Result.Rendered) — empty
	// means not yet probed.
	Health string

	// OwnedDisabled is true only when Enabled is false AND this server is
	// actually running because we started it ourselves via `<name> up` —
	// renders "up (disabled)" in yellow rather than red STRAY, which is
	// reserved for foreign processes occupying a disabled server's slot.
	OwnedDisabled bool
	// Stale marks the underlying observation as stale (source-staleness
	// probe — placeholder until that ticket lands). When true it overrides
	// every other STATE rendering with a plain yellow "STALE".
	Stale bool
	// AnomalyDetail is the parenthetical detail text for STRAY/BLOCKED
	// states, e.g. "pid 9999, foreign" or "pid 7777 — not ours". Ignored
	// for any other State.
	AnomalyDetail string
}

// Engine is the seam between the REPL's command grammar and the lifecycle
// engine that actually manages child processes. All verbs but Status and
// TeardownAll are stubbed until SOP-11 lands; StubEngine below implements
// them as loud "not implemented" errors rather than silent no-ops.
type Engine interface {
	Status() []ServerStatus
	Up(name string) error
	Down(name string) error
	Dead(name string) error
	Build(name string) error

	// Bounce takes one named server down and immediately back up, by
	// composing Down then Up — never a parallel teardown/launch path, so
	// any behavior added to Up (prompts, staleness rebuilds, health-gate
	// changes) reaches bounce for free.
	Bounce(name string) error
	Logs(name string) ([]string, error)
	Kill(pid int) error
	Command(name string) (string, []string, error)
	View(name string, nowrap bool) ([]string, error)

	// TeardownAll tears down every child this supervisor ever launched,
	// enabled or not, and returns their names. Used only by the REPL's
	// quit/exit/bye/EOF exit path — never by the "down" verb (which only
	// touches enabled children) or "dead" (which also reaps foreign
	// stray processes never launched by this supervisor).
	TeardownAll() []string
}

// notImplementedErr is returned by every stub lifecycle verb. It names the
// tracking ticket so the seam is obvious to whoever wires up the real
// engine.
func notImplementedErr(verb string) error {
	return fmt.Errorf("not implemented: %s lifecycle engine lands in SOP-11", verb)
}

// StubEngine is a placeholder lifecycle engine backed directly by the
// static config. Status() and TeardownAll() are meaningful today; every
// other verb is a stub that fails loudly.
type StubEngine struct {
	servers []config.Server
}

// NewStubEngine builds a StubEngine over the given config's servers.
func NewStubEngine(cfg config.Config) *StubEngine {
	return &StubEngine{servers: cfg.Servers}
}

var _ Engine = (*StubEngine)(nil)

// Status returns one ServerStatus per configured server, including
// disabled ones — the status table shows everything the supervisor knows
// about, not just what's currently running.
func (e *StubEngine) Status() []ServerStatus {
	out := make([]ServerStatus, 0, len(e.servers))
	for _, s := range e.servers {
		out = append(out, ServerStatus{Name: s.Name, Type: s.Type, Enabled: s.Enabled, State: "unknown"})
	}
	return out
}

func (e *StubEngine) Up(name string) error   { return notImplementedErr("up") }
func (e *StubEngine) Down(name string) error { return notImplementedErr("down") }

// Bounce is the interface's compile-time obligation, nothing more — this
// placeholder engine gets no real bounce support (AATK-28 fences that
// explicitly); it fails loudly like every other stub verb above.
func (e *StubEngine) Bounce(name string) error { return notImplementedErr("bounce") }
func (e *StubEngine) Dead(name string) error   { return notImplementedErr("dead") }
func (e *StubEngine) Build(name string) error  { return notImplementedErr("build") }
func (e *StubEngine) Logs(name string) ([]string, error) {
	return nil, notImplementedErr("logs")
}
func (e *StubEngine) Kill(pid int) error { return notImplementedErr("kill") }
func (e *StubEngine) View(name string, nowrap bool) ([]string, error) {
	return nil, notImplementedErr("view")
}
func (e *StubEngine) Command(name string) (string, []string, error) {
	return "", nil, notImplementedErr("command")
}

// TeardownAll reports every server this supervisor owns, enabled or not.
// The stub engine never actually launched anything, so there is nothing to
// kill yet — but the name list is real and reflects everything a live
// engine would need to tear down.
func (e *StubEngine) TeardownAll() []string {
	names := make([]string, 0, len(e.servers))
	for _, s := range e.servers {
		names = append(names, s.Name)
	}
	return names
}
