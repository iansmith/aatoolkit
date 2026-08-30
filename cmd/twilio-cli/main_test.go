package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// writeConfig writes contents to a fresh config file in a temp directory and
// returns its absolute path. The filename is arbitrary on purpose: twilio-cli
// no longer knows any config filename, so nothing here may depend on one.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	basePath := filepath.Join(t.TempDir(), "fleet-config.toml")
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

	got, err := webhookTarget("http://explicit.example/webhook", basePath, "")
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

	got, err := webhookTarget("http://explicit.example/webhook", basePath, "")
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

	got, err := webhookTarget("", basePath, "")
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
	basePath := filepath.Join(t.TempDir(), "fleet-config.toml")

	_, err := webhookTarget("", basePath, "")
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

	_, err := webhookTarget("", basePath, "")
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

	_, err := webhookTarget("", basePath, "")
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

	_, err := webhookTarget("", basePath, "")
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

// --- AATK-97: config-path resolution -----------------------------------------
//
// twilio-cli used to bake a bare relative config filename into the binary and
// read it from the process working directory. That name belonged to one
// consuming project, and it stopped existing when that project reorganized its
// config; the no--webhook invocation then died with `open <name>: no such file`
// and offered the operator no way to point the CLI anywhere else. A bare
// relative path is also wrong on its own terms: the documented workflow runs
// twilio-cli from this checkout while the config lives in the consumer's, so a
// cwd-relative name can never resolve across that boundary.
//
// The contract these tests pin: the config path comes from the operator — the
// -config flag first, then AATOOLKIT_TWILIO_CONFIG — and when neither is given
// the error names both, rather than a filename the engine invented.

// TestResolveConfigPath_FlagWinsOverEnv pins the precedence: an explicit
// -config beats the environment, so a one-off run can override the operator's
// exported default without unsetting it.
func TestResolveConfigPath_FlagWinsOverEnv(t *testing.T) {
	got, err := resolveConfigPath("/from/flag.toml", "/from/env.toml")
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if got != "/from/flag.toml" {
		t.Errorf("got %q, want the -config flag value to win", got)
	}
}

// TestResolveConfigPath_EnvUsedWhenFlagAbsent pins the second source: with no
// -config, the environment supplies the path. This is the mechanism that makes
// resolution work across the repo boundary — the operator exports one absolute
// path and every invocation resolves from any working directory.
func TestResolveConfigPath_EnvUsedWhenFlagAbsent(t *testing.T) {
	got, err := resolveConfigPath("", "/from/env.toml")
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if got != "/from/env.toml" {
		t.Errorf("got %q, want the environment value", got)
	}
}

// TestResolveConfigPath_NeitherSetNamesBothSources is the ticket's core
// requirement. With no source given there is nothing to fall back to: inventing
// a filename is what broke, and it produced an error the operator could not act
// on. The error must instead name both ways to supply the path.
func TestResolveConfigPath_NeitherSetNamesBothSources(t *testing.T) {
	got, err := resolveConfigPath("", "")
	if err == nil {
		t.Fatalf("resolveConfigPath with no source returned %q and no error; want an actionable error", got)
	}
	for _, want := range []string{"-config", configEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — the operator cannot act on it", err.Error(), want)
		}
	}
}

// TestWebhookTarget_ResolvesViaEnvConfig is the end-to-end path for a voice
// call: no -webhook, no -config, the environment carries an absolute config
// path, and the target is derived from it.
func TestWebhookTarget_ResolvesViaEnvConfig(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := webhookTarget("", "", basePath)
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://127.0.0.1:9740/webhook" {
		t.Errorf("got %q, want http://127.0.0.1:9740/webhook", got)
	}
}

// TestSMSWebhookTarget_ResolvesViaEnvConfig mirrors the voice case for the sms
// subcommand, which resolves the same config to a different route.
func TestSMSWebhookTarget_ResolvesViaEnvConfig(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := smsWebhookTarget("", "", basePath)
	if err != nil {
		t.Fatalf("smsWebhookTarget: %v", err)
	}
	if got != "http://127.0.0.1:9740/sms/inbound" {
		t.Errorf("got %q, want http://127.0.0.1:9740/sms/inbound", got)
	}
}

// TestWebhookTarget_ExplicitFlagWinsWithNoConfigSourceAtAll guards the
// documented escape hatch: -webhook skips config resolution entirely, so it
// must still work when no config path is available from any source. Without
// this, making a missing path an error would break the one invocation that was
// unblocking people.
func TestWebhookTarget_ExplicitFlagWinsWithNoConfigSourceAtAll(t *testing.T) {
	got, err := webhookTarget("http://explicit.example/webhook", "", "")
	if err != nil {
		t.Fatalf("webhookTarget with -webhook and no config source: unexpected error: %v", err)
	}
	if got != "http://explicit.example/webhook" {
		t.Errorf("got %q, want the explicit flag value unchanged", got)
	}
}

// TestTwilioCLIResolvesConfigFromEnvOutsideRepoRoot exercises the whole
// entrypoint the way the operator does, from a working directory that holds no
// config of its own.
//
// The paired control is what makes it meaningful. Both runs are identical
// except for AATOOLKIT_TWILIO_CONFIG: the control (unset) must fail at config
// resolution naming both sources, and the treatment (set to an absolute path)
// must get past resolution entirely and die later at the unreachable webhook.
// The "gets past resolution" assertion alone would be satisfied by a build that
// resolved nothing at all, so only the control proves the environment value is
// what carried it.
func TestTwilioCLIResolvesConfigFromEnvOutsideRepoRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(cwd, "..", "..")

	bin := filepath.Join(t.TempDir(), "twilio-cli-aatk97")
	build := exec.Command("go", "build", "-o", bin, "./cmd/twilio-cli")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v\n%s", err, out)
	}

	// A directory that is neither the repo root nor anywhere a config lives.
	elsewhere := t.TempDir()

	// NOT validServerConfig: this test actually launches the binary, which
	// dials whatever the config resolves to. validServerConfig names 9730/9740
	// — the ports a developer's own server binds — so it would fire an
	// unsolicited signed webhook at a live server, and against one that
	// answered 200 the subprocess would open a media stream and hold the
	// microphone until the go test timeout. Ports 1 and 2 are privileged and
	// bound by nothing, so resolution still succeeds and the dial still fails
	// fast, against nobody.
	configPath := writeConfig(t, `
[[server]]
name = "the server"
type = "exec"
host = "127.0.0.1"
listens = [1, 2]
command = "true"
health = { path = "/healthz", port = 1 }
`)

	// env is always set explicitly — including to empty — so a value exported in
	// the developer's own shell cannot leak in and make a subtest pass for the
	// wrong reason.
	run := func(env string, args ...string) string {
		cmd := exec.Command(bin, append(args, "+15551234567")...)
		cmd.Dir = elsewhere
		cmd.Env = append(os.Environ(), "TWILIO_AUTH_TOKEN=aatk97-test-token")
		cmd.Env = append(cmd.Env, configEnvVar+"="+env)
		out, _ := cmd.CombinedOutput() // a non-zero exit is expected in every run
		return string(out)
	}

	// resolved is the target the config above produces: host, its second listen
	// (the webhook port), and the voice route.
	const resolved = "127.0.0.1:2/webhook"

	// A second, distinguishable config for the precedence subtest: same shape,
	// different webhook port, so which file won is visible in the output.
	otherConfigPath := writeConfig(t, `
[[server]]
name = "the server"
type = "exec"
host = "127.0.0.1"
listens = [1, 3]
command = "true"
health = { path = "/healthz", port = 1 }
`)
	const otherResolved = "127.0.0.1:3/webhook"

	t.Run("config path in the environment resolves from any cwd", func(t *testing.T) {
		out := run(configPath)
		if strings.Contains(out, "no such file") {
			t.Errorf("resolution did not use %s=%s; it still looked for a file of its own.\ncwd: %s\noutput:\n%s",
				configEnvVar, configPath, elsewhere, out)
		}
		// Port 2 is the config's second listen — its webhook port. Reaching a
		// connection failure against that exact target proves resolution read
		// the file rather than merely skipping the missing-file error.
		if !strings.Contains(out, resolved) {
			t.Errorf("run did not get as far as dialing the resolved target (want %s named).\noutput:\n%s", resolved, out)
		}
	})

	// -config must be pinned end-to-end, not just as a pure function. The
	// plumbing is four positional string parameters into resolveWebhook, so a
	// dropped or transposed argument would silently stop the flag working while
	// TestResolveConfigPath_FlagWinsOverEnv — which never touches main — stayed
	// green. The environment is empty here, so only the flag can carry this.
	t.Run("-config resolves with nothing in the environment", func(t *testing.T) {
		out := run("", "-config", configPath)
		if strings.Contains(out, "no config path") {
			t.Errorf("-config was not consulted: resolution reported no config source at all.\noutput:\n%s", out)
		}
		if !strings.Contains(out, resolved) {
			t.Errorf("-config did not reach resolution (want %s named).\noutput:\n%s", resolved, out)
		}
	})

	// The documented precedence — "-config overrides the environment" — is only
	// observable when both are set, so neither single-source subtest above can
	// see the two arguments transposed at the call site. This one can: the two
	// configs resolve to different ports.
	t.Run("-config overrides the environment", func(t *testing.T) {
		out := run(otherConfigPath, "-config", configPath)
		if strings.Contains(out, otherResolved) {
			t.Errorf("the environment's config won over -config; precedence is inverted.\noutput:\n%s", out)
		}
		if !strings.Contains(out, resolved) {
			t.Errorf("-config did not win (want %s named).\noutput:\n%s", resolved, out)
		}
	})

	t.Run("control: with no config source the error names both", func(t *testing.T) {
		out := run("")
		for _, want := range []string{"-config", configEnvVar} {
			if !strings.Contains(out, want) {
				t.Errorf("control run's error does not name %q, so the operator is not told how to supply a config.\noutput:\n%s", want, out)
			}
		}
	})
}

// TestTwilioCLISMSResolvesConfig pins the sms subcommand's config plumbing.
//
// The sms entrypoint parses its own flag set and makes its own resolution call,
// so the voice test above proves nothing about it: severing both sources from
// smsWebhookTarget leaves every other test in this package green. The two
// entrypoints are the two places this can be got wrong, so both are pinned.
func TestTwilioCLISMSResolvesConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(cwd, "..", "..")

	bin := filepath.Join(t.TempDir(), "twilio-cli-aatk97-sms")
	build := exec.Command("go", "build", "-o", bin, "./cmd/twilio-cli")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v\n%s", err, out)
	}

	elsewhere := t.TempDir()
	// Ports 1 and 2 are privileged and bound by nothing, so resolution succeeds
	// and the POST fails fast against nobody.
	configPath := writeConfig(t, `
[[server]]
name = "the server"
type = "exec"
host = "127.0.0.1"
listens = [1, 2]
command = "true"
health = { path = "/healthz", port = 1 }
`)
	const resolved = "127.0.0.1:2/sms/inbound"

	// The sms entrypoint validates and binds the capture port after resolving
	// the config, so a real free port is needed to reach the assertion. Take
	// one from the kernel and release it immediately.
	capturePort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve a capture port: %v", err)
		}
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}()

	run := func(env string, args ...string) string {
		full := append([]string{"sms", "-capture-port", strconv.Itoa(capturePort)}, args...)
		cmd := exec.Command(bin, append(full, "+15551234567", "hi")...)
		cmd.Dir = elsewhere
		cmd.Env = append(os.Environ(), "TWILIO_AUTH_TOKEN=aatk97-test-token")
		cmd.Env = append(cmd.Env, configEnvVar+"="+env)
		out, _ := cmd.CombinedOutput() // a non-zero exit is expected in every run
		return string(out)
	}

	t.Run("resolves from the environment", func(t *testing.T) {
		if out := run(configPath); !strings.Contains(out, resolved) {
			t.Errorf("sms did not resolve from %s (want %s named).\noutput:\n%s", configEnvVar, resolved, out)
		}
	})

	t.Run("resolves from -config with nothing in the environment", func(t *testing.T) {
		if out := run("", "-config", configPath); !strings.Contains(out, resolved) {
			t.Errorf("sms did not resolve from -config (want %s named).\noutput:\n%s", resolved, out)
		}
	})

	t.Run("control: with no config source the error names both", func(t *testing.T) {
		out := run("")
		for _, want := range []string{"-config", configEnvVar} {
			if !strings.Contains(out, want) {
				t.Errorf("sms error does not name %q.\noutput:\n%s", want, out)
			}
		}
	})
}
