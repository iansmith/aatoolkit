package lifecycle

import (
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// TestResolveCommand_SourceServerWithNoArgsResolvesToNoArgs pins the config
// layer's contribution to a source server's argv: a base entry declaring no
// args yields no args, so whatever default the launched binary applies is the
// one that takes effect.
//
// This file used to hold a pair — this case and a contrasting one where a
// gitignored overlay's args overrode the base entry. AATK-33 deleted the
// overlay, so the contrast case went with it and this test lost the "No
// LocalOverlay" framing in its name; the assertion is unchanged.
func TestResolveCommand_SourceServerWithNoArgsResolvesToNoArgs(t *testing.T) {
	cfg, err := config.Load("testdata/stream_scheme_base.toml")
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
