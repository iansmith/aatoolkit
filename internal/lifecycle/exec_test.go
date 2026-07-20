package lifecycle

import (
	"reflect"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

func TestExecArgs_VerbatimCommandAndArgs(t *testing.T) {
	s := config.Server{
		Name:    "caddy",
		Type:    config.TypeExec,
		Command: "caddy",
		Args:    []string{"run", "--config", "Caddyfile"},
		Host:    "0.0.0.0", // must be ignored — exec launcher does not auto-append host/port
		Port:    0,
	}

	command, args := ExecCommand(s)

	if command != "caddy" {
		t.Fatalf("expected command 'caddy', got %q", command)
	}
	want := []string{"run", "--config", "Caddyfile"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected verbatim args %v, got %v", want, args)
	}
}

func TestExecArgs_NoAutoAppendedHostPortFlags(t *testing.T) {
	s := config.Server{
		Name:    "caddy",
		Type:    config.TypeExec,
		Command: "caddy",
		Args:    []string{"run"},
		Host:    "127.0.0.1",
		Port:    8080,
	}

	_, args := ExecCommand(s)

	for _, a := range args {
		if a == "--host" || a == "--port" || a == "127.0.0.1" || a == "8080" {
			t.Fatalf("exec launcher must not auto-append host/port flags, got args %v", args)
		}
	}
}

func TestExecArgs_EmptyArgsAllowed(t *testing.T) {
	s := config.Server{
		Name:    "minimal",
		Type:    config.TypeExec,
		Command: "true",
	}

	command, args := ExecCommand(s)

	if command != "true" {
		t.Fatalf("expected command 'true', got %q", command)
	}
	if len(args) != 0 {
		t.Fatalf("expected no args, got %v", args)
	}
}
