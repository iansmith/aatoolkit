package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// TODO(AATK-33): Phase 0 stub. Still derives a sibling ".local.toml" and merges
// it — the half-removal TestLoad_IgnoresALocalOverlayFileIfPresent exists to
// catch. Only the signature has moved so far.
func Load(path string) (Config, error) {
	baseData, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	base, err := decodeStrict(baseData, path)
	if err != nil {
		return Config{}, err
	}

	localPath := strings.TrimSuffix(path, ".toml") + ".local.toml"
	merged := base
	if localData, err := os.ReadFile(localPath); err == nil {
		local, err := decodeStrict(localData, localPath)
		if err != nil {
			return Config{}, err
		}
		merged, err = mergeOverlay(base, local)
		if err != nil {
			return Config{}, err
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("reading %s: %w", localPath, err)
	}

	merged.Supervisor.resolveBaseDir(filepath.Dir(path))
	merged.Supervisor.applyDefaults()

	if err := Validate(merged); err != nil {
		return Config{}, err
	}
	return merged, nil
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
