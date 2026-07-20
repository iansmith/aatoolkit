package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// loadWithSupervisorTOML writes supervisorBlock plus a fixed minimal server
// stanza to a config file under a fresh temp dir, loads it, and returns the
// result plus the config file's own directory (for asserting paths resolved
// relative to it, not to process cwd).
func loadWithSupervisorTOML(t *testing.T, supervisorBlock string) (cfg Config, confDir string) {
	t.Helper()
	root := t.TempDir()
	confDir = filepath.Join(root, "sub")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	basePath := filepath.Join(confDir, "foo.toml")
	src := supervisorBlock + `
[[server]]
name = "solo"
type = "exec"
enabled = true
host = "127.0.0.1"
listens = [9000]
command = "run"
health = { port = 9000, path = "/healthz" }
`
	if err := os.WriteFile(basePath, []byte(src), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := Load(basePath, filepath.Join(confDir, "missing-local.toml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return cfg, confDir
}

func TestServer_DirField_RoundTrips(t *testing.T) {
	var v struct {
		Server Server `toml:"server"`
	}
	src := `
[server]
name = "solo"
type = "source"
dir = "~/some/project"
`
	if _, err := toml.Decode(src, &v); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if v.Server.Dir != "~/some/project" {
		t.Errorf("got Dir %q, want %q", v.Server.Dir, "~/some/project")
	}
}

func TestSupervisor_BaseDirField_RoundTrips(t *testing.T) {
	var v struct {
		Supervisor Supervisor `toml:"supervisor"`
	}
	src := `
[supervisor]
base_dir = "build"
`
	if _, err := toml.Decode(src, &v); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if v.Supervisor.BaseDir != "build" {
		t.Errorf("got BaseDir %q, want %q", v.Supervisor.BaseDir, "build")
	}
}

// TestLoad_BaseDirResolvesRelativeToConfigFile pins behavior 3: base_dir,
// when relative, resolves against the directory containing the --config
// file (never against process cwd), and log_dir/lock_file resolve against
// that resolved base_dir.
func TestLoad_BaseDirResolvesRelativeToConfigFile(t *testing.T) {
	cfg, confDir := loadWithSupervisorTOML(t, `
[supervisor]
base_dir = "build"
log_dir = "logs"
lock_file = "server.lock"
`)

	wantLogDir := filepath.Join(confDir, "build", "logs")
	if cfg.Supervisor.LogDir != wantLogDir {
		t.Errorf("got log_dir %q, want %q (config-file-relative, not cwd-relative)", cfg.Supervisor.LogDir, wantLogDir)
	}
	wantLockFile := filepath.Join(confDir, "build", "server.lock")
	if cfg.Supervisor.LockFile != wantLockFile {
		t.Errorf("got lock_file %q, want %q (config-file-relative, not cwd-relative)", cfg.Supervisor.LockFile, wantLockFile)
	}
}

// TestLoad_BaseDirUnset_MatchesCurrentBehavior pins behavior 4: with
// base_dir unset, log_dir/lock_file resolve exactly as they do today
// (untouched — resolution against the supervisor's own launch cwd is the
// caller's job, not Load's).
func TestLoad_BaseDirUnset_MatchesCurrentBehavior(t *testing.T) {
	cfg, _ := loadWithSupervisorTOML(t, `
[supervisor]
log_dir = "logs"
lock_file = "server.lock"
`)

	if cfg.Supervisor.LogDir != "logs" {
		t.Errorf("got log_dir %q, want unmodified %q (base_dir unset -> no resolution)", cfg.Supervisor.LogDir, "logs")
	}
	if cfg.Supervisor.LockFile != "server.lock" {
		t.Errorf("got lock_file %q, want unmodified %q (base_dir unset -> no resolution)", cfg.Supervisor.LockFile, "server.lock")
	}
}
