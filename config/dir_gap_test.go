package config

import (
	"path/filepath"
	"testing"
)

// TestDecodeStrict_RejectsMisspelledBaseDir and
// TestDecodeStrict_RejectsMisspelledServerDir close an adversary-found gap:
// behavior 5 explicitly names "bas_dir" and "di" as the misspellings that
// must hard-error, but Phase 0 only had the two generic
// TestDecodeStrict_* tests (different key names) — neither proves these
// two specific new fields are covered by strict-decode.
func TestDecodeStrict_RejectsMisspelledBaseDir(t *testing.T) {
	data := []byte(`
[supervisor]
bas_dir = "build"
`)
	_, err := decodeStrict(data, "inline")
	if err == nil {
		t.Fatal("expected error for misspelled 'bas_dir', got nil")
	}
}

func TestDecodeStrict_RejectsMisspelledServerDir(t *testing.T) {
	data := []byte(`
[[server]]
name = "solo"
type = "source"
di = "~/some/project"
`)
	_, err := decodeStrict(data, "inline")
	if err == nil {
		t.Fatal("expected error for misspelled 'di', got nil")
	}
}

// TestLoad_BaseDirSet_AbsoluteLogDirUnaffected closes an adversary-found
// boundary gap: Phase 0 only exercised base_dir resolution for a relative
// log_dir/lock_file. An already-absolute log_dir must pass through
// untouched — joining it against base_dir would silently corrupt it (e.g.
// filepath.Join("/anywhere/build", "/var/log/server") does NOT return
// "/var/log/server").
func TestLoad_BaseDirSet_AbsoluteLogDirUnaffected(t *testing.T) {
	// absLogDir must live outside the (not-yet-created) confDir, so build it
	// from a sibling temp dir before handing the supervisor block to the
	// shared fixture helper.
	absLogDir := filepath.Join(t.TempDir(), "elsewhere", "logs")

	cfg, _ := loadWithSupervisorTOML(t, `
[supervisor]
base_dir = "build"
log_dir = "`+absLogDir+`"
`)

	if cfg.Supervisor.LogDir != absLogDir {
		t.Errorf("got log_dir %q, want unmodified absolute %q", cfg.Supervisor.LogDir, absLogDir)
	}
}
