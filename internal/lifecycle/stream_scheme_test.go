package lifecycle

import (
	"slices"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// TestResolveCommand_LocalOverlayArgsOverrideSourceServer proves a local
// overlay's args on a source server entry (e.g. server's -stream-scheme
// override) survives the base/local merge and reaches ResolveCommand's
// output — the actual argv a launched server process receives, per
// SOP-107's observable behavior 2.
func TestResolveCommand_LocalOverlayArgsOverrideSourceServer(t *testing.T) {
	cfg, err := config.Load("testdata/stream_scheme_base.toml", "testdata/stream_scheme_local.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server, ok := cfg.ServerByName("server")
	if !ok {
		t.Fatalf("expected 'server' server in merged config")
	}

	cmd, args, err := ResolveCommand(server)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "build/server" {
		t.Errorf("expected command %q, got %q", "build/server", cmd)
	}
	if !slices.Equal(args, []string{"-stream-scheme", "ws"}) {
		t.Errorf("expected local overlay args to reach ResolveCommand, got %v", args)
	}
}

// TestResolveCommand_NoLocalOverlayKeepsProductionDefaultArgs proves that
// without a local overlay present (committed-only case, e.g. CI or a fresh
// checkout), server's args stay empty at the config layer — main.go's own
// "wss" default applies unmodified, per SOP-107's observable behavior 3.
func TestResolveCommand_NoLocalOverlayKeepsProductionDefaultArgs(t *testing.T) {
	cfg, err := config.Load("testdata/stream_scheme_base.toml", "testdata/does_not_exist.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server, ok := cfg.ServerByName("server")
	if !ok {
		t.Fatalf("expected 'server' server in merged config")
	}

	_, args, err := ResolveCommand(server)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("expected no args without a local overlay, got %v", args)
	}
}
