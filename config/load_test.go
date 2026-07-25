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
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"), filepath.Join(t.TempDir(), "local.toml"))
	if err == nil {
		t.Fatal("expected error for missing base config file, got nil")
	}
}

func TestLoad_MissingLocalOverlayIsFine(t *testing.T) {
	cfg, err := Load("testdata/valid_base.toml", filepath.Join(t.TempDir(), "does-not-exist.local.toml"))
	if err != nil {
		t.Fatalf("expected missing local overlay to be tolerated, got error: %v", err)
	}
	if len(cfg.Servers) != 4 {
		t.Fatalf("expected base servers to load unmodified, got %d", len(cfg.Servers))
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
	cfg, err := Load(basePath, filepath.Join(dir, "missing-local.toml"))
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
	_, err := Load(basePath, filepath.Join(dir, "missing-local.toml"))
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
	cfg, err := Load(basePath, filepath.Join(dir, "missing-local.toml"))
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
	_, err := Load(basePath, filepath.Join(dir, "missing-local.toml"))
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
	cfg, err := Load("testdata/prompt.toml", "testdata/does-not-exist.local.toml")
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
	cfg, err := Load("testdata/prompt-env.toml", "testdata/does-not-exist.local.toml")
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
	argsOnly := &PromptSpec{Question: "which?", YesArgs: []string{"-x"}}

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
		{TypeMLX, argsOnly, true, "args on mlx: still a discarded answer, still rejected"},
		{TypePython, argsOnly, true, "args on python: still rejected"},
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
