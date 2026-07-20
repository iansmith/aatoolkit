package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeOverlay_UnknownLocalServerNameIsHardError(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "chat-llm", Type: TypeMLX, Model: "m", Port: 1235, Health: Health{Path: "/v1/models"}},
	}}
	local := Config{Servers: []Server{
		{Name: "does-not-exist", Host: "0.0.0.0"},
	}}
	_, err := mergeOverlay(base, local)
	if err == nil {
		t.Fatal("expected error for local server name not present in base, got nil")
	}
}

func TestMergeOverlay_ScalarLocalWins(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "chat-llm", Type: TypeMLX, Model: "m", Host: "127.0.0.1", Port: 1235, Health: Health{Path: "/v1/models"}},
	}}
	local := Config{Servers: []Server{
		{Name: "chat-llm", Host: "0.0.0.0"},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Servers[0].Host != "0.0.0.0" {
		t.Errorf("expected local host to win, got %q", merged.Servers[0].Host)
	}
	// Fields not touched by local retain base values.
	if merged.Servers[0].Port != 1235 {
		t.Errorf("expected base port to survive untouched, got %d", merged.Servers[0].Port)
	}
	if merged.Servers[0].Model != "m" {
		t.Errorf("expected base model to survive untouched, got %q", merged.Servers[0].Model)
	}
}

func TestMergeOverlay_EnvMapsMergePerKey(t *testing.T) {
	base := Config{Servers: []Server{
		{
			Name: "server", Type: TypeSource, Build: "go build", Binary: "b",
			Listens: []int{9730}, Health: Health{Port: 9730, Path: "/healthz"},
			Env: map[string]string{"A": "base-a", "B": "base-b"},
		},
	}}
	local := Config{Servers: []Server{
		{Name: "server", Env: map[string]string{"B": "local-b", "C": "local-c"}},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := merged.Servers[0].Env
	if env["A"] != "base-a" {
		t.Errorf("expected base-only key A to survive, got %q", env["A"])
	}
	if env["B"] != "local-b" {
		t.Errorf("expected local B to win over base B, got %q", env["B"])
	}
	if env["C"] != "local-c" {
		t.Errorf("expected local-only key C to be added, got %q", env["C"])
	}
}

func TestMergeOverlay_ServersNotInLocalAreUntouched(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "a", Host: "127.0.0.1"},
		{Name: "b", Host: "127.0.0.1"},
	}}
	local := Config{Servers: []Server{
		{Name: "a", Host: "0.0.0.0"},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Servers[1].Host != "127.0.0.1" {
		t.Errorf("expected server 'b' untouched by overlay, got %q", merged.Servers[1].Host)
	}
}

func TestMergeOverlay_SupervisorScalarLocalWins(t *testing.T) {
	base := Config{Supervisor: Supervisor{LogDir: "build/logs"}}
	local := Config{Supervisor: Supervisor{LogDir: "/tmp/other-logs"}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Supervisor.LogDir != "/tmp/other-logs" {
		t.Errorf("expected local log_dir to win, got %q", merged.Supervisor.LogDir)
	}
}

func TestMergeOverlay_SupervisorBaseDirLocalWins(t *testing.T) {
	base := Config{Supervisor: Supervisor{BaseDir: "build"}}
	local := Config{Supervisor: Supervisor{BaseDir: "/opt/other-base"}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Supervisor.BaseDir != "/opt/other-base" {
		t.Errorf("expected local base_dir to win, got %q", merged.Supervisor.BaseDir)
	}
}

func TestMergeOverlay_ServerDirLocalWins(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "server", Type: TypeSource, Build: "go build", Binary: "b", Dir: "~/checkout-a"},
	}}
	local := Config{Servers: []Server{
		{Name: "server", Dir: "~/checkout-b"},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Servers[0].Dir != "~/checkout-b" {
		t.Errorf("expected local dir to win, got %q", merged.Servers[0].Dir)
	}
}

func TestMergeOverlay_EmptyLocalOverlayIsNoop(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "a", Host: "127.0.0.1", Env: map[string]string{"X": "1"}},
	}}
	merged, err := mergeOverlay(base, Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Servers[0].Host != "127.0.0.1" {
		t.Errorf("expected base untouched by empty overlay, got %q", merged.Servers[0].Host)
	}
	if merged.Servers[0].Env["X"] != "1" {
		t.Errorf("expected base env untouched, got %v", merged.Servers[0].Env)
	}
}

// TestLoad_FullOverlayFixtures exercises the merge through the public Load
// entrypoint using the checked-in valid_base.toml / valid_local.toml
// fixtures, cross-checking the design's worked example end-to-end.
func TestLoad_FullOverlayFixtures(t *testing.T) {
	cfg, err := Load("testdata/valid_base.toml", "testdata/valid_local.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var server, chatLLM *Server
	for i := range cfg.Servers {
		switch cfg.Servers[i].Name {
		case "server":
			server = &cfg.Servers[i]
		case "chat-llm":
			chatLLM = &cfg.Servers[i]
		}
	}
	if server == nil || chatLLM == nil {
		t.Fatalf("expected 'server' and 'chat-llm' servers in merged config")
	}

	if server.Env["TWILIO_AUTH_TOKEN"] != "shh" {
		t.Errorf("expected local secret env merged in, got %v", server.Env)
	}
	if server.Env["AATOOLKIT_STT_MAX_SEC"] != "180" {
		t.Errorf("expected base env key preserved alongside local secret, got %v", server.Env)
	}
	if chatLLM.Host != "0.0.0.0" {
		t.Errorf("expected local host override on chat-llm, got %q", chatLLM.Host)
	}
}

func TestLoad_UnknownLocalServerNameHardErrorsThroughLoad(t *testing.T) {
	_, err := Load("testdata/valid_base.toml", "testdata/unknown_local_server.toml")
	if err == nil {
		t.Fatal("expected hard error for unknown local server name via Load, got nil")
	}
}

func TestMergeOverlay_LocalEnabledDoesNotOverrideBase(t *testing.T) {
	// enabled is intentionally excluded from overlay merge (see merge.go
	// comment) since bool decoding can't distinguish "false" from "unset."
	base := Config{Servers: []Server{
		{Name: "a", Enabled: true, Host: "127.0.0.1"},
	}}
	local := Config{Servers: []Server{
		{Name: "a", Enabled: false, Host: "0.0.0.0"},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !merged.Servers[0].Enabled {
		t.Errorf("expected base 'enabled=true' to survive overlay merge, got false")
	}
}

func TestMergeOverlay_HealthStructLocalWinsWhenSet(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "a", Health: Health{Path: "/healthz", Port: 9000}},
	}}
	local := Config{Servers: []Server{
		{Name: "a", Health: Health{Path: "/custom-health", Port: 9001}},
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Servers[0].Health.Path != "/custom-health" {
		t.Errorf("expected local health override to win, got %+v", merged.Servers[0].Health)
	}
}

// A local override that sets only one health field must not wipe the base's
// other fields — health merges field by field, like every other scalar.
func TestMergeOverlay_HealthPartialOverrideKeepsBaseFields(t *testing.T) {
	base := Config{Servers: []Server{
		{Name: "a", Health: Health{Host: "127.0.0.1", Port: 9000, Path: "/healthz"}},
	}}
	local := Config{Servers: []Server{
		{Name: "a", Health: Health{Path: "/custom"}}, // only path set
	}}
	merged, err := mergeOverlay(base, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := merged.Servers[0].Health
	want := Health{Host: "127.0.0.1", Port: 9000, Path: "/custom"}
	if got != want {
		t.Errorf("partial health override lost base fields: got %+v, want %+v", got, want)
	}
}

func TestLoad_LocalOverlayReadErrorSurfaces(t *testing.T) {
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
	// A directory in place of the local file triggers a read error that is
	// NOT os.IsNotExist, so Load must surface it (not silently treat it as
	// "no overlay present").
	localDirPath := filepath.Join(dir, "local-is-a-dir.toml")
	if err := os.Mkdir(localDirPath, 0o755); err != nil {
		t.Fatalf("setting up fixture dir: %v", err)
	}
	_, err := Load(basePath, localDirPath)
	if err == nil {
		t.Fatal("expected error when local overlay path is unreadable for reasons other than not-exist, got nil")
	}
}
