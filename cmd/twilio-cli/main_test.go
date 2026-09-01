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

// fixtureServerName is the entry name declared by validServerConfig. Named
// rather than repeated: since AATK-98 the lookup name is operator-supplied, so
// every resolution test has to hand one in, and a fixture whose name is spelled
// out at fifteen call sites is a rename waiting to go half-done.
const fixtureServerName = "the server"

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

// buildTwilioCLI builds the entrypoint and returns the binary path together
// with the repo root. Four tests drive the real binary as a subprocess and had
// grown four verbatim copies of this block; the parts that legitimately differ
// — the config fixtures, the run closures, the assertions — all sit below it.
// GOWORK=off is the load-bearing line: without it the build resolves this
// module through the workspace one directory up rather than standalone.
func buildTwilioCLI(t *testing.T, name string) (bin, repoRoot string) {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot = filepath.Join(cwd, "..", "..")

	bin = filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, "./cmd/twilio-cli")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v\n%s", err, out)
	}
	return bin, repoRoot
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

	got, err := webhookTarget("http://explicit.example/webhook", basePath, "", "", "")
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

	got, err := webhookTarget("http://explicit.example/webhook", basePath, "", "", "")
	if err != nil {
		t.Fatalf("webhookTarget with missing config but explicit flag: unexpected error: %v", err)
	}
	if got != "http://explicit.example/webhook" {
		t.Errorf("got %q, want explicit flag value unchanged", got)
	}
}

// TestWebhookTarget_ResolvesFromConfigWhenFlagAbsent covers observable
// behavior 1: with no -webhook flag, the target is derived from the merged
// config's named server host + webhook port.
func TestWebhookTarget_ResolvesFromConfigWhenFlagAbsent(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := webhookTarget("", basePath, "", fixtureServerName, "")
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

	_, err := webhookTarget("", basePath, "", "", "")
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

	_, err := webhookTarget("", basePath, "", "", "")
	if err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
}

// TestWebhookTarget_NoServerProducesClearError is the boundary case
// where config loads fine but has no entry under the looked-up name to resolve.
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

	_, err := webhookTarget("", basePath, "", fixtureServerName, "")
	if err == nil {
		t.Fatalf("expected an error when no %q server is declared", fixtureServerName)
	}
}

// TestWebhookTarget_NoWebhookPortProducesClearError is the boundary case
// where the named server exists but declares fewer than two listens, so it has
// no webhook port to resolve.
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

	_, err := webhookTarget("", basePath, "", fixtureServerName, "")
	if err == nil {
		t.Fatalf("expected an error when the %q server declares no webhook port", fixtureServerName)
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

	bin, repoRoot := buildTwilioCLI(t, "twilio-cli-aatk56")

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

	bin, repoRoot := buildTwilioCLI(t, "twilio-cli-aatk56-default")

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

	got, err := webhookTarget("", "", basePath, fixtureServerName, "")
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://127.0.0.1:9740/webhook" {
		t.Errorf("got %q, want http://127.0.0.1:9740/webhook", got)
	}
}

// TestSMSWebhookTarget_ExplicitFlagWins covers the sms side of the escape
// hatch. The voice route has three tests for this contract; the sms route had
// none, and smsWebhookTarget is a separate three-argument positional forward —
// blanking `explicit` in it left the whole package green. The failure that
// hides there is the cruel one: `twilio-cli sms -webhook <url>` answering with
// an error that tells the operator to pass the flag they just passed.
func TestSMSWebhookTarget_ExplicitFlagWins(t *testing.T) {
	const explicit = "http://explicit.example/sms/inbound"

	t.Run("wins over a resolvable config", func(t *testing.T) {
		got, err := smsWebhookTarget(explicit, writeConfig(t, validServerConfig), "", "", "")
		if err != nil {
			t.Fatalf("smsWebhookTarget: %v", err)
		}
		if got != explicit {
			t.Errorf("got %q, want the explicit flag value unchanged", got)
		}
	})

	t.Run("wins with no config source at all", func(t *testing.T) {
		got, err := smsWebhookTarget(explicit, "", "", "", "")
		if err != nil {
			t.Fatalf("smsWebhookTarget with -webhook and no config source: unexpected error: %v", err)
		}
		if got != explicit {
			t.Errorf("got %q, want the explicit flag value unchanged", got)
		}
	})

	t.Run("wins over a broken config, without reading it", func(t *testing.T) {
		got, err := smsWebhookTarget(explicit, filepath.Join(t.TempDir(), "does-not-exist.toml"), "", "", "")
		if err != nil {
			t.Fatalf("smsWebhookTarget with -webhook and a missing config: unexpected error: %v", err)
		}
		if got != explicit {
			t.Errorf("got %q, want the explicit flag value unchanged", got)
		}
	})
}

// TestSMSWebhookTarget_ResolvesViaEnvConfig mirrors the voice case for the sms
// subcommand, which resolves the same config to a different route.
func TestSMSWebhookTarget_ResolvesViaEnvConfig(t *testing.T) {
	basePath := writeConfig(t, validServerConfig)

	got, err := smsWebhookTarget("", "", basePath, fixtureServerName, "")
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
	got, err := webhookTarget("http://explicit.example/webhook", "", "", "", "")
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

	bin, _ := buildTwilioCLI(t, "twilio-cli-aatk97")

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
		// The server name is held constant across every subtest here: these
		// pin CONFIG-path precedence, and a run that failed for want of a name
		// would report that instead of the resolution under test.
		cmd.Env = append(cmd.Env, serverEnvVar+"="+fixtureServerName)
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
	const otherResolved = "127.0.0.1:4/webhook"

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

	// The short-circuit has to be pinned at the CALL SITE, not just in
	// webhookTarget: main forwards three positional strings, and blanking
	// `explicit` there leaves every function-level test green. What that ships
	// is the cruel failure — the operator passes -webhook and is answered with
	// an error telling them to pass -webhook. The environment is empty, so a
	// run that reaches the explicit URL can only have short-circuited.
	t.Run("-webhook skips config resolution entirely", func(t *testing.T) {
		out := run("", "-webhook", "http://127.0.0.1:1/explicit-webhook")
		if strings.Contains(out, "no config path") {
			t.Errorf("-webhook did not reach resolution: it demanded a config instead of skipping it.\noutput:\n%s", out)
		}
		if !strings.Contains(out, "explicit-webhook") {
			t.Errorf("-webhook was not the target dialed.\noutput:\n%s", out)
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

	bin, _ := buildTwilioCLI(t, "twilio-cli-aatk97-sms")

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
	const otherResolved = "127.0.0.1:4/sms/inbound"

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
		cmd.Env = append(cmd.Env, serverEnvVar+"="+fixtureServerName)
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

	// Precedence needs both sources set to be observable at all, so the three
	// single-source subtests above cannot see the two config arguments
	// transposed at the call site — and a transposition there silently inverts
	// what the README promises for this subcommand.
	t.Run("-config overrides the environment", func(t *testing.T) {
		out := run(otherConfigPath, "-config", configPath)
		if strings.Contains(out, otherResolved) {
			t.Errorf("the environment's config won over -config; precedence is inverted.\noutput:\n%s", out)
		}
		if !strings.Contains(out, resolved) {
			t.Errorf("-config did not win (want %s named).\noutput:\n%s", resolved, out)
		}
	})

	// The sms call site is its own place to get this wrong, for the same reason
	// its config plumbing is: a separate flag set and a separate three-argument
	// forward. TestSMSWebhookTarget_ExplicitFlagWins pins the function; this
	// pins that main actually hands it the flag.
	t.Run("-webhook skips config resolution entirely", func(t *testing.T) {
		out := run("", "-webhook", "http://127.0.0.1:1/explicit-sms")
		if strings.Contains(out, "no config path") {
			t.Errorf("-webhook did not reach resolution: sms demanded a config instead of skipping it.\noutput:\n%s", out)
		}
		if !strings.Contains(out, "explicit-sms") {
			t.Errorf("-webhook was not the target posted to.\noutput:\n%s", out)
		}
	})
}

// twoServerConfig declares two servers so a resolution test can prove the
// looked-up name actually selects between them. A single-entry fixture cannot:
// any name that resolves at all would resolve to the same entry.
const twoServerConfig = `
[[server]]
name = "alpha"
type = "exec"
host = "127.0.0.1"
listens = [9730, 9740]
command = "true"
health = { path = "/healthz", port = 9730 }

[[server]]
name = "beta"
type = "exec"
host = "10.0.0.9"
listens = [8730, 8740]
command = "true"
health = { path = "/healthz", port = 8730 }
`

// TestResolveServerName_FlagWinsOverEnv pins the precedence AATK-98 owns: an
// explicit -server beats the environment, so a one-off run against another
// entry does not require unsetting the operator's exported default.
func TestResolveServerName_FlagWinsOverEnv(t *testing.T) {
	got, err := resolveServerName("from-flag", "from-env")
	if err != nil {
		t.Fatalf("resolveServerName: %v", err)
	}
	if got != "from-flag" {
		t.Errorf("got %q, want the -server flag value to win", got)
	}
}

// TestResolveServerName_EnvUsedWhenFlagAbsent pins the second source. This is
// the mechanism that removes the friction the ticket names: the operator
// exports the name once and every invocation resolves without typing it.
func TestResolveServerName_EnvUsedWhenFlagAbsent(t *testing.T) {
	got, err := resolveServerName("", "from-env")
	if err != nil {
		t.Fatalf("resolveServerName: %v", err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want the environment value", got)
	}
}

// TestResolveServerName_NeitherSetNamesBothSources is the core requirement.
// The old hardcoded name belonged to one particular consuming project and
// stopped matching the moment that project renamed its entry — an invented
// default is exactly what broke, and it failed with an error naming a server
// the operator had never heard of. With no source given the error must instead
// name both ways to supply one.
func TestResolveServerName_NeitherSetNamesBothSources(t *testing.T) {
	got, err := resolveServerName("", "")
	if err == nil {
		t.Fatalf("resolveServerName with no source returned %q and no error; want an actionable error", got)
	}
	for _, want := range []string{"-server", serverEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — the operator cannot act on it", err.Error(), want)
		}
	}
}

// TestWebhookTarget_ServerFlagSelectsEntry proves the resolved name reaches the
// config lookup: against a config declaring two servers, -server picks the one
// it names, host and webhook port and all.
func TestWebhookTarget_ServerFlagSelectsEntry(t *testing.T) {
	basePath := writeConfig(t, twoServerConfig)

	got, err := webhookTarget("", basePath, "", "beta", "")
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://10.0.0.9:8740/webhook" {
		t.Errorf("got %q, want http://10.0.0.9:8740/webhook", got)
	}
}

// TestWebhookTarget_ServerFlagOverridesEnv is the same selection driven through
// both sources at once: the flag must win at the point of the lookup, not just
// inside resolveServerName. A forward that passed the env value along would
// still pass the precedence unit test and fail here.
func TestWebhookTarget_ServerFlagOverridesEnv(t *testing.T) {
	basePath := writeConfig(t, twoServerConfig)

	got, err := webhookTarget("", basePath, "", "alpha", "beta")
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://127.0.0.1:9740/webhook" {
		t.Errorf("got %q, want the -server flag entry http://127.0.0.1:9740/webhook", got)
	}
}

// TestWebhookTarget_ServerNameFromEnv is the end-to-end path the operator
// actually runs: no -server, the name carried by the environment alongside the
// config path.
func TestWebhookTarget_ServerNameFromEnv(t *testing.T) {
	basePath := writeConfig(t, twoServerConfig)

	got, err := webhookTarget("", "", basePath, "", "beta")
	if err != nil {
		t.Fatalf("webhookTarget: %v", err)
	}
	if got != "http://10.0.0.9:8740/webhook" {
		t.Errorf("got %q, want http://10.0.0.9:8740/webhook", got)
	}
}

// TestWebhookTarget_UnknownServerNamesTheValueLookedUp pins the error text on
// the value actually looked up. The failure this ticket started from was an
// error naming a hardcoded server the operator had never configured; an error
// that quotes the name it searched for is the difference between "typo in
// -server" and "no idea what this tool wants".
func TestWebhookTarget_UnknownServerNamesTheValueLookedUp(t *testing.T) {
	basePath := writeConfig(t, twoServerConfig)

	_, err := webhookTarget("", basePath, "", "gamma", "")
	if err == nil {
		t.Fatal("expected an error when the named server is not declared")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("error %q does not name the server it looked up", err.Error())
	}
}

// TestSMSWebhookTarget_ServerNameSelectsEntry pins observable behavior 4: the
// -server default and override apply to the SMS route too, since both go
// through resolveWebhook. smsWebhookTarget is a separate positional forward,
// and this package has already been bitten once by a forward that dropped an
// argument while every voice test stayed green.
func TestSMSWebhookTarget_ServerNameSelectsEntry(t *testing.T) {
	basePath := writeConfig(t, twoServerConfig)

	got, err := smsWebhookTarget("", basePath, "", "beta", "")
	if err != nil {
		t.Fatalf("smsWebhookTarget: %v", err)
	}
	if got != "http://10.0.0.9:8740/sms/inbound" {
		t.Errorf("got %q, want http://10.0.0.9:8740/sms/inbound", got)
	}
}

// TestResolveFrom_EmptyDefaultsWithWarning pins observable behavior 3: a voice
// dial with no positional FROM uses the default source number and produces one
// warning line identifying it as a default, rather than exiting with usage.
func TestResolveFrom_EmptyDefaultsWithWarning(t *testing.T) {
	from, warning := resolveFrom("")
	if from != defaultFrom {
		t.Errorf("got FROM %q, want the default %q", from, defaultFrom)
	}
	if warning == "" {
		t.Fatal("no warning for a defaulted FROM; the operator must be told the number is not theirs")
	}
	for _, want := range []string{"WARNING", defaultFrom} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not contain %q", warning, want)
		}
	}
}

// TestResolveFrom_ExplicitIsVerbatimAndSilent is the other half: an explicit
// FROM is used exactly as given and warns about nothing.
func TestResolveFrom_ExplicitIsVerbatimAndSilent(t *testing.T) {
	from, warning := resolveFrom("+15105551234")
	if from != "+15105551234" {
		t.Errorf("got FROM %q, want the explicit value verbatim", from)
	}
	if warning != "" {
		t.Errorf("got warning %q for an explicit FROM, want none", warning)
	}
}

// TestDefaultFromIsValidE164 guards the default against the same check every
// operator-supplied FROM passes. A default that cannot survive validateE164
// would turn "no FROM" from a usage error into a confusing validation error.
func TestDefaultFromIsValidE164(t *testing.T) {
	if err := validateE164(defaultFrom); err != nil {
		t.Errorf("defaultFrom %q is not valid E.164: %v", defaultFrom, err)
	}
}

// TestTwilioCLIVoiceNoFromWarnsAndProceeds drives the real binary: with no
// positional FROM and no way to resolve a webhook, it must get PAST the FROM
// check — emitting the warning — and fail later on config resolution. The
// paired control below runs the identical invocation with an explicit FROM and
// must emit no warning. Together they pin that the warning tracks the
// defaulting and nothing else.
func TestTwilioCLIVoiceNoFromWarnsAndProceeds(t *testing.T) {
	bin, repoRoot := buildTwilioCLI(t, "twilio-cli-nofrom")

	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "GOWORK=off", "AATOOLKIT_TWILIO_CONFIG=", "AATOOLKIT_SERVER_NAME=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected a failure with no config source; got success\n%s", out)
		}
		return string(out)
	}

	defaulted := run()
	if !strings.Contains(defaulted, "WARNING") || !strings.Contains(defaulted, defaultFrom) {
		t.Errorf("no-FROM run did not warn about the default source number:\n%s", defaulted)
	}

	explicit := run("+15105551234")
	if strings.Contains(explicit, "WARNING") {
		t.Errorf("explicit-FROM run warned about a default it did not use:\n%s", explicit)
	}
}

// TestTwilioCLISMSStillRequiresExplicitFrom pins the deliberate asymmetry: SMS
// mode parses <FROM> <BODY> positionally, so defaulting FROM there would make
// `twilio-cli sms "hello"` ambiguous between the two. One positional argument
// must still be a usage error, not a message sent from a defaulted number.
func TestTwilioCLISMSStillRequiresExplicitFrom(t *testing.T) {
	bin, repoRoot := buildTwilioCLI(t, "twilio-cli-smsfrom")

	cmd := exec.Command(bin, "sms", "hello there")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off", "AATOOLKIT_TWILIO_CONFIG=", "AATOOLKIT_SERVER_NAME=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("sms with a single positional argument succeeded; want a usage error\n%s", out)
	}
	if !strings.Contains(string(out), "<FROM-e164>") {
		t.Errorf("sms usage error does not state the required FROM positional:\n%s", out)
	}
	if strings.Contains(string(out), defaultFrom) {
		t.Errorf("sms mode defaulted the FROM number; it must not:\n%s", out)
	}
}

// TestTwilioCLIServerFlagResolvesEndToEnd drives the real binary for both
// modes. The unit tests above prove resolveServerName's precedence and prove
// webhookTarget honors a name — neither touches main, where the flag and the
// environment value are threaded in as two more positional strings among five.
// A dropped or transposed argument there stops -server working while every
// pure-function test stays green, which is the exact failure this package has
// already been bitten by once.
//
// Ports 1-4 are privileged and bound by nothing, so resolution succeeds and
// the dial fails fast against nobody rather than firing a signed webhook at a
// developer's live server.
func TestTwilioCLIServerFlagResolvesEndToEnd(t *testing.T) {
	bin, _ := buildTwilioCLI(t, "twilio-cli-serverflag")
	elsewhere := t.TempDir()

	configPath := writeConfig(t, `
[[server]]
name = "alpha"
type = "exec"
host = "127.0.0.1"
listens = [1, 2]
command = "true"
health = { path = "/healthz", port = 1 }

[[server]]
name = "beta"
type = "exec"
host = "127.0.0.1"
listens = [3, 4]
command = "true"
health = { path = "/healthz", port = 3 }
`)

	// The environment always names alpha. Every subtest that expects beta can
	// only get there through the flag, so the override is observable rather
	// than inferred.
	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Dir = elsewhere
		cmd.Env = append(os.Environ(),
			"TWILIO_AUTH_TOKEN=aatk98-test-token",
			configEnvVar+"="+configPath,
			serverEnvVar+"=alpha")
		out, _ := cmd.CombinedOutput() // a non-zero exit is expected in every run
		return string(out)
	}

	t.Run("voice: the environment name resolves", func(t *testing.T) {
		if out := run("+15551234567"); !strings.Contains(out, "127.0.0.1:2/webhook") {
			t.Errorf("voice did not resolve alpha from %s (want 127.0.0.1:2/webhook named).\noutput:\n%s", serverEnvVar, out)
		}
	})

	t.Run("voice: -server overrides the environment", func(t *testing.T) {
		out := run("-server", "beta", "+15551234567")
		if strings.Contains(out, "127.0.0.1:2/webhook") {
			t.Errorf("the environment's name won over -server; precedence is inverted.\noutput:\n%s", out)
		}
		if !strings.Contains(out, "127.0.0.1:4/webhook") {
			t.Errorf("-server did not reach resolution (want 127.0.0.1:4/webhook named).\noutput:\n%s", out)
		}
	})

	// Observable behavior 4: the same name drives the SMS route, which is a
	// separate five-argument forward reaching the same resolver.
	t.Run("sms: -server overrides the environment", func(t *testing.T) {
		out := run("sms", "-server", "beta", "+15551234567", "hi")
		if strings.Contains(out, "127.0.0.1:2/sms/inbound") {
			t.Errorf("sms took the environment's name over -server.\noutput:\n%s", out)
		}
		if !strings.Contains(out, "127.0.0.1:4/sms/inbound") {
			t.Errorf("sms -server did not reach resolution (want 127.0.0.1:4/sms/inbound named).\noutput:\n%s", out)
		}
	})

	t.Run("control: with no name anywhere the error names both sources", func(t *testing.T) {
		cmd := exec.Command(bin, "+15551234567")
		cmd.Dir = elsewhere
		cmd.Env = append(os.Environ(),
			"TWILIO_AUTH_TOKEN=aatk98-test-token",
			configEnvVar+"="+configPath,
			serverEnvVar+"=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected a failure with no server name anywhere\n%s", out)
		}
		for _, want := range []string{"-server", serverEnvVar} {
			if !strings.Contains(string(out), want) {
				t.Errorf("error does not name %q — the operator cannot act on it.\noutput:\n%s", want, out)
			}
		}
	})
}
