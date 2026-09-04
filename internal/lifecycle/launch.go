package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// Process is a launched child: the running *exec.Cmd plus the log file path
// it's writing to. Reaping happens in a background goroutine (see Launch) —
// callers don't need to call Wait themselves.
type Process struct {
	Cmd     *exec.Cmd
	LogPath string

	// exited is set by the Wait goroutine Launch starts, once the child has
	// been reaped. It exists because exec.Cmd offers no race-free way to ask
	// the question from another goroutine: Cmd.ProcessState is written by
	// Wait and is only safe to read after Wait has returned, so a supervisor
	// polling it would be reading it concurrently with that write.
	//
	// A Process built by any route other than Launch has no Wait goroutine
	// behind it, so this stays false for its whole life and Exited reports
	// "still running". That is the conservative answer — it preserves the
	// behavior callers had before this field existed — but it does mean
	// Exited is only load-bearing for processes this package actually
	// launched.
	exited atomic.Bool
}

// Exited reports whether this child has terminated and been reaped. It is
// nil-safe, so callers holding an optional process handle can ask without
// guarding first.
//
// False does not prove the child is alive: see the exited field's note on
// Processes constructed outside Launch. True does prove it is dead — the flag
// is only ever set after Wait has returned.
func (p *Process) Exited() bool {
	if p == nil {
		return false
	}
	return p.exited.Load()
}

// LaunchSpec describes what to launch and how — a struct rather than a
// growing list of positional params, since every caller already has these
// values on hand from a config.Server.
type LaunchSpec struct {
	LogDir  string
	Name    string
	Command string
	Args    []string
	Env     map[string]string

	// Dir, when non-empty, becomes the child's working directory
	// (cmd.Dir) — a relative venv/entry/binary on that server then
	// resolves against Dir, not against the supervisor's own launch cwd.
	// Empty Dir leaves cmd.Dir unset (inherits the supervisor's cwd),
	// matching today's behavior exactly. A leading "~/" is expanded
	// against the user's home directory, matching the same field's
	// existing expansion convention for source-type build sourcing
	// (see expandTilde in source.go).
	Dir string

	// Now is the launch time, which names this launch's log file
	// (design/aa-server-status.md §9). The zero value means "read the clock
	// here", which is the only reason Launch touches it at all; callers
	// that need a deterministic log name pass it explicitly.
	Now time.Time
}

// Launch starts a child process per spec, per design/aa-server-status.md §6.4:
//
//   - own process group (SysProcAttr{Setpgid: true}) — isolates the child
//     from terminal signals and enables whole-tree group-kill later.
//   - env is injected over the inherited environment: os.Environ() is the
//     base, per-server keys in env win on collision.
//   - stdout and stderr are both piped to the same resolved log file
//     (see openLogForLaunch).
//   - Wait() runs in a goroutine — Launch returns as soon as the process
//     has started; reaping is fire-and-forget. That goroutine records the
//     exit on the returned Process (see Exited), which is the only race-free
//     way for a supervisor to learn a child died without being asked to kill
//     it. Classifying that death is still out of scope for this package.
func Launch(spec LaunchSpec) (*Process, error) {
	dir := ""
	if spec.Dir != "" {
		expanded, err := expandTilde(spec.Dir)
		if err != nil {
			return nil, fmt.Errorf("launching %q: %w", spec.Name, err)
		}
		dir = expanded
	}

	now := spec.Now
	if now.IsZero() {
		now = time.Now()
	}

	logFile, logPath, err := openLogForLaunch(spec.LogDir, spec.Name, now)
	if err != nil {
		return nil, fmt.Errorf("launching %q: %w", spec.Name, err)
	}

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Env = mergeEnv(os.Environ(), spec.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = dir

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting %q: %w", spec.Name, err)
	}

	proc := &Process{Cmd: cmd, LogPath: logPath}
	go func() {
		cmd.Wait()
		proc.exited.Store(true)
		logFile.Close()
	}()

	return proc, nil
}

// launchWithCommand builds the common LaunchSpec shared by every
// LaunchXxx wrapper (only command/args vary by server type) and launches it.
func launchWithCommand(logDir string, s config.Server, command string, args []string) (*Process, error) {
	return Launch(LaunchSpec{LogDir: logDir, Name: s.Name, Command: command, Args: args, Env: s.Env, Dir: s.Dir})
}

// ResolveCommand returns the launch command and args for s, dispatching
// to the per-type *Command function. Used by both the engine's launch()
// and the REPL's "command" verb so the type-switch lives in one place.
func ResolveCommand(s config.Server) (string, []string, error) {
	switch s.Type {
	case config.TypeMLX:
		cmd, args := MLXCommand(s)
		return cmd, args, nil
	case config.TypePython:
		cmd, args := PythonCommand(s)
		return cmd, args, nil
	case config.TypeExec:
		cmd, args := ExecCommand(s)
		return cmd, args, nil
	case config.TypeSource:
		cmd, args := SourceCommand(s)
		return cmd, args, nil
	default:
		return "", nil, fmt.Errorf("unknown server type %q", s.Type)
	}
}

// EnsureHealthProbeBuilt builds s's Health.Source-declared probe program, if
// declared, so it exists (and is fresh) before the caller's first health
// probe — the source-exec form (design/aa-server-status.md §6.1, AATK-41).
// It is a no-op (zero BuildResult, nil error) when Health.Source isn't
// declared, so a Server{} written before this field existed is unaffected.
//
// Building reuses PerformBuild's build-to-temp/hash-compare/replace
// machinery (source.go) via a synthetic TypeSource server value — building
// this probe program is exactly that operation, just for an artifact that is
// not the entry's own launch binary. A nil BuildLifecycle is passed: the
// probe has no running process of its own to stop/start around a replace.
//
// s.Dir is deliberately NOT threaded into the synthetic server: s.Dir sets
// the entry's own launch cwd (a docker-compose Dir, say), an unrelated
// concern from where the probe's Go source lives. health.source carries no
// Dir of its own (its Build string is a complete, self-contained `go build`
// invocation), so reusing s.Dir here would silently point the probe's build
// at the wrong -C directory for any entry that sets one for its own launch.
//
// Callers must invoke this once, during the start sequence, before the
// entry's first health probe — never from inside the probe itself. Building
// on every probe tick is the rejected design (design/aa-server-status.md
// §6.1: 0.13s warm, 4.72s cold against the 2s health_timeout cap); hoisting
// the build out of the probe and into the one-time start step is the whole
// point of this field.
func EnsureHealthProbeBuilt(s config.Server) (BuildResult, error) {
	if !s.Health.Source.Declared() {
		return BuildResult{}, nil
	}
	probe := config.Server{
		Name:   s.Name,
		Type:   config.TypeSource,
		Build:  s.Health.Source.Build,
		Binary: s.Health.Source.Binary,
	}
	result, err := PerformBuild(probe, nil)
	if err != nil {
		return result, fmt.Errorf("building health probe for %q: %w", s.Name, err)
	}
	return result, nil
}

// mergeEnv overlays override onto base ("KEY=VALUE" pairs, os.Environ()
// shape), with override's keys winning on collision.
func mergeEnv(base []string, override map[string]string) []string {
	if len(override) == 0 {
		return base
	}

	merged := make([]string, 0, len(base)+len(override))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := override[key]; ok {
			continue
		}
		merged = append(merged, kv)
	}
	for k, v := range override {
		merged = append(merged, k+"="+v)
	}
	return merged
}
