// Package config loads and validates aa-server-status's TOML configuration:
// a committed base file deep-merged with a gitignored local overlay for
// secrets. See design/aa-server-status.md §7 for the design.
package config

import (
	"fmt"
	"strings"
	"time"
)

// Supervisor holds the system-wide settings from the [supervisor] table.
type Supervisor struct {
	LogDir        string   `toml:"log_dir"`
	LockFile      string   `toml:"lock_file"`
	GracePeriod   Duration `toml:"grace_period"`
	ReadyTimeout  Duration `toml:"ready_timeout"`
	PollInterval  Duration `toml:"poll_interval"`
	HealthTimeout Duration `toml:"health_timeout"`

	// BaseDir anchors relative LogDir/LockFile values to something other
	// than the supervisor's own launch cwd. When set and itself relative,
	// BaseDir resolves against the directory containing the --config file
	// (never against process cwd) — see design/aa-server-status.md §7.
	BaseDir string `toml:"base_dir"`
}

// Default supervisor values, applied when the corresponding TOML key is
// absent from both the committed and local files.
const (
	DefaultGracePeriod   = 5 * time.Second
	DefaultReadyTimeout  = 15 * time.Second
	DefaultPollInterval  = 500 * time.Millisecond
	DefaultHealthTimeout = 2 * time.Second
)

// applyDefaults fills in zero-valued duration fields with their defaults.
func (s *Supervisor) applyDefaults() {
	if s.GracePeriod.Duration == 0 {
		s.GracePeriod.Duration = DefaultGracePeriod
	}
	if s.ReadyTimeout.Duration == 0 {
		s.ReadyTimeout.Duration = DefaultReadyTimeout
	}
	if s.PollInterval.Duration == 0 {
		s.PollInterval.Duration = DefaultPollInterval
	}
	if s.HealthTimeout.Duration == 0 {
		s.HealthTimeout.Duration = DefaultHealthTimeout
	}
}

// ServerType is the launch strategy for a [[server]] entry.
type ServerType string

const (
	TypeMLX    ServerType = "mlx"
	TypePython ServerType = "python"
	TypeExec   ServerType = "exec"
	TypeSource ServerType = "source"
)

// Health describes a server's readiness check, in one of the two forms
// design/aa-server-status.md §6.1 defines: the HTTP form (Host/Port/Path), a
// GET whose 2xx answer means serving, or the exec form (Exec), a command whose
// exit 0 means ready. §6.1 carries the rationale for the second — in short,
// nothing that speaks its own wire protocol can supply a path truthfully.
//
// Exactly one form may be declared; Validate rejects both zero and two.
type Health struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
	Path string `toml:"path"`

	// Exec is argv, never a shell line — the supervisor runs the program
	// directly, exactly as lifecycle.ExecCommand does for an exec server. An
	// argument containing a space therefore survives as one argument.
	Exec []string `toml:"exec"`
}

// Declared reports whether the server declares a readiness check at all, and
// IsExec reports which form it is. Between them they are the only place either
// question is answered: every call site asks here rather than inspecting a
// particular form's field, so adding a third form later cannot leave a site
// silently checking the wrong one.
func (h Health) Declared() bool { return h.Path != "" || h.IsExec() }

// IsExec reports whether the check is the command form. See Declared.
func (h Health) IsExec() bool { return len(h.Exec) > 0 }

// UnmarshalTOML implements toml.Unmarshaler, accepting either the existing
// table form (`health = { host = "...", port = ..., path = "..." }`, or
// `health = { exec = ["pg_isready", "-p", "5432"] }`) or a shorthand string
// form (`health = "GET /spend?prefix=SOP"`), where host and port default to the
// server's own (internal/health.ResolveSpec).
//
// Only GET is accepted in the string form — the HTTP probe has no method
// fallback (design/aa-server-status.md §6.1), so the method name is
// documentation, not a configurable verb. There is deliberately no string
// shorthand for the exec form: splitting a command line on spaces would break
// the argv property the Exec field documents.
func (h *Health) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		method, path, ok := strings.Cut(v, " ")
		if !ok || path == "" {
			return fmt.Errorf(`health: string form must be "METHOD /path", got %q`, v)
		}
		if !strings.EqualFold(method, "GET") {
			return fmt.Errorf("health: string form only supports GET, got %q in %q", method, v)
		}
		h.Path = path
		return nil
	case map[string]any:
		if host, ok := v["host"].(string); ok {
			h.Host = host
		}
		if port, ok := v["port"].(int64); ok {
			h.Port = int(port)
		}
		if path, ok := v["path"].(string); ok {
			h.Path = path
		}
		if raw, ok := v["exec"]; ok {
			argv, err := execArgv(raw)
			if err != nil {
				return err
			}
			h.Exec = argv
		}
		return nil
	default:
		return fmt.Errorf("health: expected a table or a \"METHOD /path\" string, got %T", data)
	}
}

// execArgv converts a decoded TOML array into argv, rejecting anything that
// would not survive as a command: a non-array, a non-string element, or an
// empty list. Rejecting these here rather than at launch means a typo is a
// config error at read time (design/aa-server-status.md §6.5) instead of a
// probe that can never succeed.
func execArgv(raw any) ([]string, error) {
	elems, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("health: exec must be an array of strings, got %T", raw)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("health: exec is empty — it must name a command to run")
	}
	argv := make([]string, len(elems))
	for i, e := range elems {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("health: exec[%d] must be a string, got %T (%v)", i, e, e)
		}
		argv[i] = s
	}
	return argv, nil
}

// Warm is an optional request aa-server-status sends once after launching a
// server, before it starts polling the health gate. It exists for servers
// whose health endpoint answers before the server can actually do any work:
// mlx-serve's `/v1/models` lists what is on disk and 200s in milliseconds,
// while the model itself is only loaded on demand — so without a warm-up the
// supervisor reports a 30GB model "up" seconds before it can serve anything,
// and the first real request eats the whole load.
//
// A 2xx is the only thing aa-server-status looks at; the response body is
// discarded. Only once it arrives does the health poll begin.
//
// Unlike Health's probe, the method is a real choice (a warm-up is usually a
// POST carrying a body), so it is honored rather than being documentation.
type Warm struct {
	Host   string
	Port   int
	Method string
	Path   string
	Body   string
}

// UnmarshalTOML implements toml.Unmarshaler for the string form
// `warm = "POST /path <body>"`, extending Health's `"METHOD /path"` shorthand
// with everything after the path taken verbatim as the request body:
//
//	warm = 'POST /v1/chat/completions {"model":"m","messages":[],"max_tokens":1}'
//
// The body is deliberately an opaque string. aa-server-status neither parses nor
// validates it — it is whatever that server wants to be asked, and modelling
// each server's request schema in TOML would buy nothing when the only thing
// that matters is the status code. Host and port default to the server's own
// (internal/health.ResolveWarmSpec).
func (w *Warm) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		method, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
		if !ok || rest == "" {
			return fmt.Errorf(`warm: string form must be "METHOD /path [body]", got %q`, v)
		}
		path, body, _ := strings.Cut(rest, " ")
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf(`warm: path must start with "/", got %q in %q`, path, v)
		}
		w.Method = strings.ToUpper(method)
		w.Path = path
		w.Body = strings.TrimSpace(body)
		return nil
	default:
		return fmt.Errorf("warm: expected a \"METHOD /path [body]\" string, got %T", data)
	}
}

// Server is one [[server]] entry — the schema is a union of all four
// server-type shapes; Validate enforces per-type required fields.
// PromptSpec makes a server's launch arguments and environment an interactive
// choice instead of a config line the operator has to remember to hand-edit
// between runs. When present, bringing the server up asks Question, appends
// YesArgs or NoArgs to whatever args its type already resolves, and merges
// YesEnv or NoEnv into its Env.
//
// Declaring only one branch is legal: the omitted branch simply contributes
// nothing, which is the natural way to express "add this flag, or launch
// normally".
type PromptSpec struct {
	Question string   `toml:"question"`
	YesArgs  []string `toml:"yes_args"`
	NoArgs   []string `toml:"no_args"`

	// YesEnv and NoEnv are merged into the server's Env when their branch is
	// chosen, overriding a static Env entry of the same key and leaving every
	// other key alone.
	//
	// Env exists because the args half only covers choices a server takes on
	// its command line; much of what a child reads comes from its environment,
	// and a question whose answer cannot reach that is a question the operator
	// has to remember to act on twice. Unlike args, Env reaches every server
	// type, so a prompt declaring only env is valid on any of them — see
	// validatePrompt.
	YesEnv map[string]string `toml:"yes_env"`
	NoEnv  map[string]string `toml:"no_env"`
}

type Server struct {
	Name    string     `toml:"name"`
	Type    ServerType `toml:"type"`
	Enabled bool       `toml:"enabled"`
	Host    string     `toml:"host"`

	// Prompt, when set, makes this server ask a y/n question before it
	// launches and append the chosen branch's args. Only server types whose
	// launch path actually reads Args (exec, source) may declare one —
	// Validate rejects the rest rather than letting the answer be silently
	// discarded.
	Prompt *PromptSpec `toml:"prompt"`

	// mlx / python launch port.
	Port int `toml:"port"`

	// source / exec self-listened ports.
	Listens []int `toml:"listens"`

	// mlx
	Model string `toml:"model"`

	// mlx, optional: a Gemma-4-style assistant drafter checkpoint path for
	// speculative decoding (mlx-serve's --drafter DIR|none — see
	// https://github.com/ddalcu/mlx-serve). Empty (the zero value) means
	// "no drafter" — MLXCommand appends no --drafter flag, the launch
	// invocation is unchanged from before this field existed. mlx-serve's
	// model-agnostic PLD speculative decoding runs regardless of this
	// field (it's on by default, unrelated to --drafter).
	Drafter string `toml:"drafter"`

	// python
	Venv     string   `toml:"venv"`
	Entry    string   `toml:"entry"`
	Packages []string `toml:"packages"`

	// source
	Build  string `toml:"build"`
	Binary string `toml:"binary"`

	// optional, any server type: sets the child's working directory
	// (exec.Cmd.Dir) at launch, and anchors a relative Venv/Entry/Binary to
	// itself instead of to aa-server-status's own launch cwd. A leading "~/" is
	// expanded against the user's home directory; otherwise, when relative,
	// Dir resolves against aa-server-status's own launch cwd (it is not
	// config-file-relative — only the supervisor's base_dir is). Unset Dir
	// leaves the child's working directory as today.
	//
	// For source servers, Dir is reused from its pre-existing build-time
	// role (injected as `go -C <dir>`) — the same field now also sets the
	// post-build launch's cmd.Dir. The build's own output is still always
	// rewritten to a temp path and copied into Binary (internal/lifecycle's
	// buildToTemp/replaceBinary), so Binary resolution during the build step
	// itself is unaffected by this launch-time role.
	Dir string `toml:"dir"`

	// exec
	Command string   `toml:"command"`
	Args    []string `toml:"args"`

	Health Health `toml:"health"`

	// Warm is an optional request fired once after launch and before the
	// health gate opens. Empty Path (the zero value) means "no warm-up" --
	// the health poll starts immediately, as it always has.
	Warm Warm `toml:"warm"`

	Env map[string]string `toml:"env"`

	// Per-server overrides of the supervisor defaults. Zero value means
	// "use the supervisor value."
	GracePeriod  Duration `toml:"grace_period"`
	ReadyTimeout Duration `toml:"ready_timeout"`
}

// Config is the fully loaded, merged, and validated configuration.
type Config struct {
	Supervisor Supervisor `toml:"supervisor"`
	Servers    []Server   `toml:"server"`
}
