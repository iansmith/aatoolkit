package lifecycle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

func TestMLXArgs_BuildsExpectedCommand(t *testing.T) {
	s := config.Server{
		Name:  "chat-llm",
		Type:  config.TypeMLX,
		Host:  "127.0.0.1",
		Port:  1235,
		Model: "qwen2.5-14b",
	}

	command, args := MLXCommand(s)

	if command != "mlx-serve" {
		t.Fatalf("expected command 'mlx-serve', got %q", command)
	}
	want := []string{"serve", "qwen2.5-14b", "--host", "127.0.0.1", "--port", "1235"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestMLXArgs_AppendsDrafterWhenSet(t *testing.T) {
	// Names deliberately won't match any real cached directory on any
	// machine, so this test deterministically exercises the pass-through
	// branch (no matching local cache -> use the name as given) regardless
	// of what happens to be downloaded on the machine running the test.
	s := config.Server{
		Name:    "chat-llm",
		Type:    config.TypeMLX,
		Host:    "127.0.0.1",
		Port:    1234,
		Model:   "test-org/not-a-real-cached-model-xyz",
		Drafter: "test-org/not-a-real-cached-drafter-xyz",
	}

	_, args := MLXCommand(s)

	want := []string{
		"serve", "test-org/not-a-real-cached-model-xyz",
		"--host", "127.0.0.1",
		"--port", "1234",
		"--drafter", "test-org/not-a-real-cached-drafter-xyz",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestResolveMLXModelPath_ExpandsWhenCachedDirExists(t *testing.T) {
	root := t.TempDir()
	cached := filepath.Join(root, "mlx-community", "gemma-4-31B-it-assistant-bf16")
	if err := os.MkdirAll(cached, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := resolveMLXModelPath(root, "mlx-community/gemma-4-31B-it-assistant-bf16")
	if got != cached {
		t.Fatalf("expected resolved path %q, got %q", cached, got)
	}
}

func TestResolveMLXModelPath_PassesThroughWhenNotCached(t *testing.T) {
	root := t.TempDir()

	got := resolveMLXModelPath(root, "mlx-community/never-pulled-model")
	if got != "mlx-community/never-pulled-model" {
		t.Fatalf("expected unchanged name, got %q", got)
	}
}

func TestResolveMLXModelPath_PassesThroughAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "already", "a", "full", "path")

	got := resolveMLXModelPath(root, abs)
	if got != abs {
		t.Fatalf("expected absolute path unchanged, got %q", got)
	}
}

// TestMLXArgs_AppendsServerArgs pins AATK-91: an `args = [...]` on a
// `type = "mlx"` server reaches the mlx-serve process instead of being
// silently discarded. The order is the contract, not an accident — the
// built-in flags come first and the operator's args come last, so a flag
// repeated in args wins under mlx-serve's last-one-wins parsing. That is the
// deliberate choice AATK-91 asks to be documented: the operator can override a
// built-in, which is the point of exposing args at all.
func TestMLXArgs_AppendsServerArgs(t *testing.T) {
	s := config.Server{
		Name:  "chat-llm",
		Type:  config.TypeMLX,
		Host:  "127.0.0.1",
		Port:  1235,
		Model: "qwen2.5-14b",
		Args:  []string{"--prefix-cache-mem", "8GB", "--kv-quant", "8"},
	}

	_, args := MLXCommand(s)

	want := []string{
		"serve", "qwen2.5-14b", "--host", "127.0.0.1", "--port", "1235",
		"--prefix-cache-mem", "8GB", "--kv-quant", "8",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

// TestMLXArgs_AppendsServerArgsAfterDrafter pins the order against the one
// other flag MLXCommand appends for itself: --drafter is a built-in, so it
// still precedes the operator's args.
func TestMLXArgs_AppendsServerArgsAfterDrafter(t *testing.T) {
	// Names deliberately match no real cached directory, so the pass-through
	// branch is exercised deterministically regardless of what is downloaded
	// on the machine running the test.
	s := config.Server{
		Name:    "chat-llm",
		Type:    config.TypeMLX,
		Host:    "127.0.0.1",
		Port:    1234,
		Model:   "test-org/not-a-real-cached-model-xyz",
		Drafter: "test-org/not-a-real-cached-drafter-xyz",
		Args:    []string{"--prefix-cache-disk", "64GB"},
	}

	_, args := MLXCommand(s)

	want := []string{
		"serve", "test-org/not-a-real-cached-model-xyz",
		"--host", "127.0.0.1", "--port", "1234",
		"--drafter", "test-org/not-a-real-cached-drafter-xyz",
		"--prefix-cache-disk", "64GB",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestMLXArgs_NoAutoFlagsBeyondHostPort(t *testing.T) {
	s := config.Server{
		Name:  "code-llm",
		Type:  config.TypeMLX,
		Host:  "0.0.0.0",
		Port:  1234,
		Model: "deepseek-coder",
	}

	_, args := MLXCommand(s)

	for _, a := range args {
		if a == "--verbose" || a == "--debug" {
			t.Fatalf("unexpected auto-appended flag %q in args %v", a, args)
		}
	}
	if len(args) != 6 {
		t.Fatalf("expected exactly 6 args (serve, model, --host, host, --port, port), got %d: %v", len(args), args)
	}
}
