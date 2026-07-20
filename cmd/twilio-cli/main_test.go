package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validServerConfig = `
[[server]]
name = "the server"
type = "exec"
host = "127.0.0.1"
listens = [9730, 9740]
command = "true"
health = { path = "/healthz", port = 9730 }
`

// writeConfig writes contents to a fresh "aa-server-status.toml" in a temp
// directory and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	basePath := filepath.Join(t.TempDir(), "aa-server-status.toml")
	if err := os.WriteFile(basePath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return basePath
}

// TestWebhookTarget_ExplicitFlagOverridesConfig covers observable behavior 2:
// an explicit -webhook flag wins outright, even when config resolution would
// otherwise succeed with a different value.
func TestWebhookTarget_ExplicitFlagOverridesConfig(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := webhookTarget("http://explicit.example/webhook", basePath)
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://explicit.example/webhook" {
		t.Errorf("got %q, want explicit flag value unchanged", got)
	}
}

// TestWebhookTarget_ExplicitFlagOverridesEvenBrokenConfig is the edge case:
// the flag must win without ever touching config, so a broken/missing
// config file must not surface an error when -webhook was given.
func TestWebhookTarget_ExplicitFlagOverridesEvenBrokenConfig(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "does-not-exist.toml")

	got, err := webhookTarget("http://explicit.example/webhook", basePath)
	if err != nil {
		t.Fatalf("webhookTarget with missing config but explicit flag: unexpected error: %v", err)
	}
	if got != "http://explicit.example/webhook" {
		t.Errorf("got %q, want explicit flag value unchanged", got)
	}
}

// TestWebhookTarget_ResolvesFromConfigWhenFlagAbsent covers observable
// behavior 1: with no -webhook flag, the target is derived from the merged
// config's the server server host + webhook port.
func TestWebhookTarget_ResolvesFromConfigWhenFlagAbsent(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := webhookTarget("", basePath)
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://127.0.0.1:9740/webhook" {
		t.Errorf("got %q, want http://127.0.0.1:9740/webhook", got)
	}
}

// TestWebhookTarget_MissingConfigProducesClearError covers observable
// behavior 3: a missing config file with no -webhook flag must fail with a
// clear, actionable error naming the missing file, not a silent fallback.
func TestWebhookTarget_MissingConfigProducesClearError(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "aa-server-status.toml")

	_, err := webhookTarget("", basePath)
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	if !strings.Contains(err.Error(), basePath) {
		t.Errorf("error %q does not name the missing file %q", err.Error(), basePath)
	}
}

// TestWebhookTarget_MalformedConfigProducesClearError is the error/rejection
// edge case for a parse error in the config file.
func TestWebhookTarget_MalformedConfigProducesClearError(t *testing.T) {
	basePath := writeConfig(t, "not valid = [toml")

	_, err := webhookTarget("", basePath)
	if err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
}

// TestWebhookTarget_NoServerProducesClearError is the boundary case
// where config loads fine but has no server named "the server" to resolve.
func TestWebhookTarget_NoServerProducesClearError(t *testing.T) {
	basePath := writeConfig(t, `
[[server]]
name = "caddy"
type = "exec"
host = "0.0.0.0"
listens = [80, 443]
command = "true"
health = { path = "/healthz", port = 80 }
`)

	_, err := webhookTarget("", basePath)
	if err == nil {
		t.Fatal("expected an error when no the server server is declared")
	}
}

// TestWebhookTarget_NoWebhookPortProducesClearError is the boundary case
// where the the server server exists but declares fewer than two listens, so it
// has no webhook port to resolve.
func TestWebhookTarget_NoWebhookPortProducesClearError(t *testing.T) {
	basePath := writeConfig(t, `
[[server]]
name = "the server"
type = "exec"
host = "127.0.0.1"
listens = [9730]
command = "true"
health = { path = "/healthz", port = 9730 }
`)

	_, err := webhookTarget("", basePath)
	if err == nil {
		t.Fatal("expected an error when the server server declares no webhook port")
	}
}
