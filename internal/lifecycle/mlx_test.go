package lifecycle

import (
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

func TestMLXArgs_AppendsDraftModelWhenSet(t *testing.T) {
	s := config.Server{
		Name:       "chat-llm",
		Type:       config.TypeMLX,
		Host:       "127.0.0.1",
		Port:       1234,
		Model:      "mlx-community/gemma-4-31b-it-8bit",
		DraftModel: "mlx-community/gemma-4-2b-it-4bit",
	}

	_, args := MLXCommand(s)

	want := []string{
		"serve", "mlx-community/gemma-4-31b-it-8bit",
		"--host", "127.0.0.1",
		"--port", "1234",
		"--draft-model", "mlx-community/gemma-4-2b-it-4bit",
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
