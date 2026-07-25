package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeStrict_UnknownKeyIsHardError(t *testing.T) {
	data, err := os.ReadFile("testdata/unknown_key_base.toml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	_, err = decodeStrict(data, "unknown_key_base.toml")
	if err == nil {
		t.Fatal("expected strict-decode error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "totally_bogus_field") {
		t.Errorf("expected error to name the offending key, got: %v", err)
	}
}

func TestDecodeStrict_ValidConfigDecodesCleanly(t *testing.T) {
	data, err := os.ReadFile("testdata/valid_base.toml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	cfg, err := decodeStrict(data, "valid_base.toml")
	if err != nil {
		t.Fatalf("expected clean decode, got error: %v", err)
	}
	if len(cfg.Servers) != 4 {
		t.Fatalf("expected 4 servers, got %d", len(cfg.Servers))
	}
}

func TestDecodeStrict_MisspelledKeyRejected(t *testing.T) {
	data := []byte(`
[supervisor]
log_dr = "build/logs"
`)
	_, err := decodeStrict(data, "inline")
	if err == nil {
		t.Fatal("expected error for misspelled 'log_dr', got nil")
	}
}

func TestLoad_MissingBaseFileIsHardError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err == nil {
		t.Fatal("expected error for missing base config file, got nil")
	}
}

func TestLoad_AppliesSupervisorDefaults(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.toml")
	if err := os.WriteFile(basePath, []byte(`
[[server]]
name = "solo"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9000]
command = "run"
health = { port = 9000, path = "/healthz" }
`), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg, err := Load(basePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Supervisor.HealthTimeout.Duration != DefaultHealthTimeout {
		t.Errorf("expected default health_timeout %v, got %v", DefaultHealthTimeout, cfg.Supervisor.HealthTimeout.Duration)
	}
	if cfg.Supervisor.GracePeriod.Duration != DefaultGracePeriod {
		t.Errorf("expected default grace_period %v, got %v", DefaultGracePeriod, cfg.Supervisor.GracePeriod.Duration)
	}
}

func TestLoad_InvalidConfigIsHardError(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.toml")
	// Duplicate names -> structural validation failure.
	if err := os.WriteFile(basePath, []byte(`
[[server]]
name = "dup"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9000]
command = "run"
health = { port = 9000, path = "/healthz" }

[[server]]
name = "dup"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9001]
command = "run"
health = { port = 9001, path = "/healthz" }
`), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	_, err := Load(basePath)
	if err == nil {
		t.Fatal("expected validation error for duplicate server names, got nil")
	}
}

func TestLoad_DurationParsing(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.toml")
	if err := os.WriteFile(basePath, []byte(`
[supervisor]
health_timeout = "3s"

[[server]]
name = "solo"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9000]
command = "run"
health = { port = 9000, path = "/healthz" }
`), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg, err := Load(basePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Supervisor.HealthTimeout.Duration.String() != "3s" {
		t.Errorf("expected health_timeout 3s, got %v", cfg.Supervisor.HealthTimeout.Duration)
	}
}

func TestLoad_InvalidDurationIsHardError(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.toml")
	if err := os.WriteFile(basePath, []byte(`
[supervisor]
health_timeout = "not-a-duration"
`), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	_, err := Load(basePath)
	if err == nil {
		t.Fatal("expected error for invalid duration string, got nil")
	}
}

// --- [server.prompt] (AATK-26) --------------------------------------------

// TestLoad_ParsesServerPrompt pins the sub-table's decoding exactly as
// written, including the partial case: declaring only one branch is legal and
// must decode as "no extra args for the other branch" rather than a
// missing-key error.
func TestLoad_ParsesServerPrompt(t *testing.T) {
	cfg, err := Load("testdata/prompt.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}

	full := cfg.Servers[0]
	if full.Prompt == nil {
		t.Fatalf("expected server %q to carry a prompt spec", full.Name)
	}
	if full.Prompt.Question != "Use plaintext ws for this run?" {
		t.Fatalf("question decoded as %q", full.Prompt.Question)
	}
	if got := strings.Join(full.Prompt.YesArgs, " "); got != "-stream-scheme ws" {
		t.Fatalf("yes_args decoded as %q", got)
	}
	if got := strings.Join(full.Prompt.NoArgs, " "); got != "-stream-scheme wss" {
		t.Fatalf("no_args decoded as %q", got)
	}

	partial := cfg.Servers[1]
	if partial.Prompt == nil {
		t.Fatalf("expected server %q to carry a prompt spec", partial.Name)
	}
	if len(partial.Prompt.NoArgs) != 0 {
		t.Fatalf("an omitted no_args must decode as no extra args, got %v", partial.Prompt.NoArgs)
	}
}

// TestValidate_RejectsPromptWithEmptyQuestion — a prompt with nothing to ask
// would block the launch on a blank line.
func TestValidate_RejectsPromptWithEmptyQuestion(t *testing.T) {
	cfg := Config{Servers: []Server{{
		Name: "svc", Type: TypeExec, Command: "/bin/true", Port: 1234,
		Health: Health{Path: "/healthz"},
		Prompt: &PromptSpec{YesArgs: []string{"-x"}},
	}}}
	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected Validate to reject a prompt with an empty question")
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Fatalf("expected the error to name the offending server, got: %v", err)
	}
}

// TestValidate_RejectsPromptOnTypeThatIgnoresArgs guards the silent no-op:
// MLXCommand and PythonCommand build their args from model/entry and never
// read s.Args, so a prompt on those types would ask the operator a question
// and then discard the answer. A loud config error beats that.
func TestValidate_RejectsPromptOnTypeThatIgnoresArgs(t *testing.T) {
	cfg := Config{Servers: []Server{{
		Name: "brain", Type: TypeMLX, Model: "some/model", Port: 1234,
		Health: Health{Path: "/healthz"},
		Prompt: &PromptSpec{Question: "which?", YesArgs: []string{"-x"}},
	}}}
	err := Validate(cfg)
	if err == nil {
		t.Fatalf("expected Validate to reject a prompt on an mlx server, whose launch args ignore args")
	}
	if !strings.Contains(err.Error(), "brain") {
		t.Fatalf("expected the error to name the offending server, got: %v", err)
	}
}

// promptEnvProbeVar is the key both prompt branches set in the env fixtures. A
// single name, so a test asserting "the yes value" and one asserting "the no
// value" cannot silently be looking at different variables.
const promptEnvProbeVar = "AATOOLKIT_PROMPT_ENV_PROBE"

// TestLoad_ParsesPromptYesEnvAndNoEnv pins AATK-32 observable behavior 1: the
// prompt sub-table accepts yes_env/no_env as TOML tables. Strict decode is what
// makes this a real assertion — an untagged or misnamed field leaves `yes_env`
// in MetaData.Undecoded(), so Load fails outright rather than silently handing
// back an empty map.
func TestLoad_ParsesPromptYesEnvAndNoEnv(t *testing.T) {
	cfg, err := Load("testdata/prompt-env.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(cfg.Servers))
	}

	spec := cfg.Servers[0].Prompt
	if spec == nil {
		t.Fatalf("expected server %q to carry a prompt spec", cfg.Servers[0].Name)
	}
	if len(spec.YesEnv) != 1 {
		t.Fatalf("yes_env decoded as %v, want exactly one entry", spec.YesEnv)
	}
	if got := spec.YesEnv[promptEnvProbeVar]; got != "yes-value" {
		t.Errorf("yes_env[%s] = %q, want %q", promptEnvProbeVar, got, "yes-value")
	}
	if len(spec.NoEnv) != 1 {
		t.Fatalf("no_env decoded as %v, want exactly one entry", spec.NoEnv)
	}
	if got := spec.NoEnv[promptEnvProbeVar]; got != "no-value" {
		t.Errorf("no_env[%s] = %q, want %q", promptEnvProbeVar, got, "no-value")
	}
}

// TestValidate_PromptTypeGateFollowsArgsNotEnv pins observable behavior 4, both
// halves of it.
//
// The existing type gate rejects a prompt on mlx/python because their launch
// paths build args from model/entry and never read s.Args — a discarded answer
// that looks like it worked. That reasoning is specific to *args*: Env reaches
// every type through launchWithCommand, so an env-only prompt on an mlx server
// takes effect and must be allowed. The args rows are in the same table
// deliberately: they are what stops the relaxation from widening into the
// silent no-op the gate exists to prevent.
func TestValidate_PromptTypeGateFollowsArgsNotEnv(t *testing.T) {
	// Minimal per-type valid server (validateType's required fields), so a
	// failure can only come from validatePrompt.
	base := map[ServerType]Server{
		TypeMLX:    {Name: "svc", Type: TypeMLX, Model: "some/model"},
		TypePython: {Name: "svc", Type: TypePython, Venv: ".venv", Entry: "svc serve", Packages: []string{"svc"}},
		TypeExec:   {Name: "svc", Type: TypeExec, Command: "/bin/true"},
		TypeSource: {Name: "svc", Type: TypeSource, Build: "go build ./cmd/svc", Binary: "build/svc"},
	}

	envOnly := &PromptSpec{
		Question: "Use the local endpoint for this run?",
		YesEnv:   map[string]string{promptEnvProbeVar: "yes-value"},
	}
	envOnlyNoBranch := &PromptSpec{
		Question: "Use the local endpoint for this run?",
		NoEnv:    map[string]string{promptEnvProbeVar: "no-value"},
	}
	argsOnly := &PromptSpec{Question: "which?", YesArgs: []string{"-x"}}
	argsOnNoBranch := &PromptSpec{Question: "which?", NoArgs: []string{"-x"}}
	argsAndEnv := &PromptSpec{
		Question: "which?",
		YesArgs:  []string{"-x"},
		YesEnv:   map[string]string{promptEnvProbeVar: "yes-value"},
	}

	cases := []struct {
		typ     ServerType
		spec    *PromptSpec
		wantErr bool
		desc    string
	}{
		{TypeMLX, envOnly, false, "env-only prompt on mlx: env reaches every type, so it takes effect"},
		{TypePython, envOnly, false, "env-only prompt on python"},
		{TypeExec, envOnly, false, "env-only prompt on exec"},
		{TypeSource, envOnly, false, "env-only prompt on source"},
		// Env declared on the no side only is just as legal — a gate keyed on
		// YesEnv alone would reject a valid config.
		{TypeMLX, envOnlyNoBranch, false, "env on the no branch only, on mlx"},
		{TypeMLX, argsOnly, true, "args on mlx: still a discarded answer, still rejected"},
		{TypePython, argsOnly, true, "args on python: still rejected"},
		// Args on the no side are discarded exactly as readily as on the yes
		// side; a gate that inspects only YesArgs would let this through.
		{TypeMLX, argsOnNoBranch, true, "args on the no branch only, on mlx: equally discarded"},
		// The trap: adding an env key must not buy an args-carrying prompt its
		// way onto a type whose launch path ignores args. A gate written as
		// "exempt if any env is declared" accepts this and reintroduces the
		// silent no-op the gate exists to prevent.
		{TypeMLX, argsAndEnv, true, "args AND env on mlx: the args half is still discarded"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			s := base[tc.typ]
			s.Port = 1234
			s.Health = Health{Path: "/healthz"}
			s.Prompt = tc.spec

			err := Validate(Config{Servers: []Server{s}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "not supported for type") {
				t.Errorf("error %q is not the type-gate error — the gate must reject args, not fail for some other reason", err)
			}
		})
	}
}

// TestValidate_RejectsBranchlessPrompt covers the review finding that gating the
// type check on args made a branchless prompt newly legal on mlx and python.
//
// A prompt with a question but no yes_args/no_args/yes_env/no_env asks the
// operator something and then does nothing with the answer — the same silent
// no-op the type check exists to prevent, reached from the other direction. It
// is rejected on every type, including the two where it used to be caught only
// as a side effect of the type gate.
func TestValidate_RejectsBranchlessPrompt(t *testing.T) {
	for _, typ := range []ServerType{TypeMLX, TypePython, TypeExec, TypeSource} {
		t.Run(string(typ), func(t *testing.T) {
			s := Server{
				Name: "svc", Type: typ, Port: 1234,
				Health: Health{Path: "/healthz"},
				Prompt: &PromptSpec{Question: "does this change anything?"},
			}
			switch typ {
			case TypeMLX:
				s.Model = "some/model"
			case TypePython:
				s.Venv, s.Entry, s.Packages = ".venv", "svc serve", []string{"svc"}
			case TypeExec:
				s.Command = "/bin/true"
			case TypeSource:
				s.Build, s.Binary = "go build ./cmd/svc", "build/svc"
			}

			err := Validate(Config{Servers: []Server{s}})
			if err == nil {
				t.Fatalf("Validate accepted a branchless prompt on %s, want a rejection", typ)
			}
			if !strings.Contains(err.Error(), "svc") {
				t.Errorf("error %q does not name the offending server", err)
			}
		})
	}
}

// TestLoad_IgnoresALocalOverlayFileIfPresent pins AATK-33 observable behaviors
// 1 and 2: the overlay is gone, and a leftover file is inert rather than an
// error.
//
// The fixture's overlay would change a value if it were read, which is what
// makes this the test that catches a half-removal — a Load that took one path
// but still derived and merged a sibling would satisfy every other assertion in
// this package while quietly keeping the machinery alive. It must also not
// error: an operator with a stale file from before this change gets the base
// config, not a failure telling them to delete something they had been told to
// create.
func TestLoad_IgnoresALocalOverlayFileIfPresent(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "aa-server-status.toml")
	if err := os.WriteFile(basePath, []byte(`
[[server]]
name = "solo"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9000]
command = "run"
health = { port = 9000, path = "/healthz" }
`), 0o644); err != nil {
		t.Fatalf("writing base: %v", err)
	}
	// Same server name, different host and command: if this file is read at
	// all, the assertions below fail.
	if err := os.WriteFile(filepath.Join(dir, "aa-server-status.local.toml"), []byte(`
[[server]]
name = "solo"
host = "10.0.0.1"
command = "overlay-wins"
`), 0o644); err != nil {
		t.Fatalf("writing overlay: %v", err)
	}

	cfg, err := Load(basePath)
	if err != nil {
		t.Fatalf("Load with a leftover overlay present must not error: %v", err)
	}

	srv, ok := cfg.ServerByName("solo")
	if !ok {
		t.Fatalf("expected the base server %q, got %+v", "solo", cfg.Servers)
	}
	if srv.Host != "127.0.0.1" {
		t.Errorf("host = %q, want the base value %q — the overlay was read", srv.Host, "127.0.0.1")
	}
	if srv.Command != "run" {
		t.Errorf("command = %q, want the base value %q — the overlay was read", srv.Command, "run")
	}
}
