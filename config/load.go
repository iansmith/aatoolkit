package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// decodeStrict parses TOML bytes into a Config, hard-erroring on any
// unknown/misspelled key (BurntSushi/toml's MetaData.Undecoded()).
func decodeStrict(data []byte, path string) (Config, error) {
	var cfg Config
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("%s: parse error: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("%s: unknown key(s): %v", path, keys)
	}
	return cfg, nil
}

// Load reads the config at path, applies supervisor defaults, and validates the
// result. Any failure is a hard error — callers should treat a non-nil error as
// fatal (config errors abort the whole program).
//
// One file, one path. There was once a second, gitignored ".local.toml" deep-
// merged on top of this one; AATK-33 removed it. It had been silently dropping
// any [server.prompt] declared only in the overlay since prompts were
// introduced, and nobody noticed — which was the clearest evidence available
// that nothing depended on it. Per-machine and secret values come from the
// environment the supervisor inherits and passes to its children, not from a
// second config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg, err := decodeStrict(data, path)
	if err != nil {
		return Config{}, err
	}

	cfg.Supervisor.resolveBaseDir(filepath.Dir(path))
	cfg.Supervisor.applyDefaults()

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// resolveBaseDir anchors LogDir/LockFile to BaseDir, per
// design/aa-server-status.md §7. A no-op when BaseDir is unset (today's
// behavior: LogDir/LockFile resolve against the supervisor's own launch
// cwd, which is the caller's job, not Load's). BaseDir itself, when
// relative, resolves against configDir — the directory containing the
// --config file — never against process cwd. Already-absolute LogDir/
// LockFile values pass through untouched.
func (s *Supervisor) resolveBaseDir(configDir string) {
	if s.BaseDir == "" {
		return
	}
	if !filepath.IsAbs(s.BaseDir) {
		s.BaseDir = filepath.Join(configDir, s.BaseDir)
	}
	anchor := func(v string) string {
		if v == "" || filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(s.BaseDir, v)
	}
	s.LogDir = anchor(s.LogDir)
	s.LockFile = anchor(s.LockFile)
}
