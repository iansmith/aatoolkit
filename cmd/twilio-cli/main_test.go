package main

import (
	"os"
	"os/exec"
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

// TestE164Validation pins AATK-16 observable behavior 1: the CLI validates the
// caller's FROM number locally (^\+[1-9]\d{1,14}$) before any network call.
func TestE164Validation(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
		desc    string
	}{
		{"+15555550100", false, "valid +1 US number (11 digits)"},
		{"5103844134", true, "invalid: missing + prefix"},
		{"+0123", true, "invalid: leading 0 after +"},
		{"+1", true, "invalid: too short (only 2 chars)"},
		{"+123456789012345", false, "valid: 15 digits total"},
		{"+1234567890123456", true, "invalid: 16 digits (too long)"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			err := validateE164(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateE164(%q) = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
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

// TestTwilioCLIRelativeAudioPathFromOtherCwd exercises the entrypoint as a
// subprocess, from a working directory that is not the project root, passing a
// relative path argument (AATK-56).
//
// It runs a PAIR of subprocesses with the identical relative argument
// `-audio ./how_are_you.ulaw`. The paired control is the point: the "no file-open
// error" assertion alone is vacuously satisfied by a build that never opens the
// file at all, so only the red control proves the relative path is genuinely
// resolved against the process working directory rather than the repo root.
func TestTwilioCLIRelativeAudioPathFromOtherCwd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(cwd, "..", "..")

	// Build the entrypoint.
	bin := filepath.Join(t.TempDir(), "twilio-cli-aatk56")
	build := exec.Command("go", "build", "-o", bin, "./cmd/twilio-cli")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v\n%s", err, out)
	}

	// Stage the fixture in a directory that is NOT the repo root.
	fixture := filepath.Join(repoRoot, "telephony", "testdata", "how_are_you.ulaw")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	audioDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(audioDir, "how_are_you.ulaw"), data, 0o644); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}

	// Both runs pass the SAME relative argument; only the cwd differs.
	run := func(dir string) string {
		cmd := exec.Command(bin,
			"-audio", "./how_are_you.ulaw",
			"-webhook", "http://127.0.0.1:1/voice",
			"+15551234567",
		)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TWILIO_AUTH_TOKEN=aatk56-test-token")
		out, _ := cmd.CombinedOutput() // a non-zero exit is expected in both runs
		return string(out)
	}

	const missingMarker = "no such file"

	t.Run("fixture present in cwd: resolves, fails later at the webhook", func(t *testing.T) {
		out := run(audioDir)
		if strings.Contains(out, missingMarker) {
			t.Errorf("relative -audio path was not resolved against the process working directory.\ncwd: %s\noutput:\n%s", audioDir, out)
		}
	})

	t.Run("control: fixture absent from repo root, must fail at file open", func(t *testing.T) {
		out := run(repoRoot)
		if !strings.Contains(out, missingMarker) {
			t.Errorf("control run did not fail at file open — the relative path is not being resolved against the process working directory (or validation never ran).\ncwd: %s\noutput:\n%s", repoRoot, out)
		}
		if !strings.Contains(out, "how_are_you.ulaw") {
			t.Errorf("control run's error does not name the path as given.\noutput:\n%s", out)
		}
	})
}

// TestTwilioCLIWithoutAudioFlagKeepsMicDefault guards the ticket's behavior 4:
// with no -audio, the mic path stays selected. The whole feature is a
// reassignment of the streamMic package var (dial.go), so a wiring bug that
// installed the file source unconditionally — with an empty path — would break
// every existing mic invocation. dial_test.go's withFakeMic overrides that var,
// so no in-process test can catch a wrong default; only a subprocess can.
func TestTwilioCLIWithoutAudioFlagKeepsMicDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(cwd, "..", "..")

	bin := filepath.Join(t.TempDir(), "twilio-cli-aatk56-default")
	build := exec.Command("go", "build", "-o", bin, "./cmd/twilio-cli")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v\n%s", err, out)
	}

	cmd := exec.Command(bin,
		"-webhook", "http://127.0.0.1:1/voice",
		"+15551234567",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "TWILIO_AUTH_TOKEN=aatk56-test-token")
	out, _ := cmd.CombinedOutput() // non-zero exit expected: the webhook is unreachable
	got := string(out)

	if strings.Contains(got, "no such file") {
		t.Errorf("a run with no -audio attempted audio validation — the mic path must stay the default.\noutput:\n%s", got)
	}
	if strings.Contains(got, "panic:") {
		t.Errorf("a run with no -audio panicked.\noutput:\n%s", got)
	}
}
