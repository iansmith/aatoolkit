package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
//     has started; reaping is fire-and-forget. A child that later dies
//     simply shows as down at the next observation (observation is out of
//     scope for this package).
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

	go func() {
		cmd.Wait()
		logFile.Close()
	}()

	return &Process{Cmd: cmd, LogPath: logPath}, nil
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
