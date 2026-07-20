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
