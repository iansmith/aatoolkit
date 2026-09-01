package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/iansmith/aatoolkit/config"
)

// e164Pattern matches an E.164 number: a leading '+', a non-zero first digit,
// then 1–14 more digits (2–15 total). Mirrors the server's own check so an
// invalid caller number is rejected at the CLI before any network call.
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// validateE164 returns an error if s is not a well-formed E.164 number.
func validateE164(s string) error {
	if !e164Pattern.MatchString(s) {
		return fmt.Errorf("invalid E.164 number %q (want +<country><subscriber>, e.g. +15105551234)", s)
	}
	return nil
}

const (
	// configEnvVar names the environment variable that points twilio-cli at
	// the config to resolve webhook targets from.
	configEnvVar = "AATOOLKIT_TWILIO_CONFIG"

	// serverEnvVar names the environment variable that supplies the name of
	// the config server entry to resolve webhook targets from.
	serverEnvVar = "AATOOLKIT_SERVER_NAME"
)

// resolveServerName returns the name of the config server entry to look up:
// the -server flag wins, then the serverEnvVar environment value.
//
// There is deliberately no built-in default, for the same reason
// resolveConfigPath has none. The previous one was a fixed name belonging to a
// particular consuming project's fleet config — so it named an entry this
// engine has no business knowing about, stopped resolving the moment that
// project renamed it, and failed with an error quoting a server the operator
// had never configured. An operator-supplied name resolves against any
// consumer's config; an error naming both ways to supply one is more use than
// a name the engine invented.
func resolveServerName(flagValue, envValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envValue != "" {
		return envValue, nil
	}
	return "", fmt.Errorf("no server name: pass -server <name>, set %s, or skip config resolution entirely with -webhook <url>", serverEnvVar)
}

// resolveFrom returns the caller's E.164 for a voice dial together with the
// warning line to log when the positional FROM was omitted — empty when one
// was given, so the caller logs exactly one warning and only for a default it
// actually substituted.
//
// Returning the line rather than logging it is what makes the pairing
// testable: main ends in log.Fatalf/os.Exit and cannot be driven from a test,
// so a warning emitted in there could drift from the number actually dialed
// with nothing to catch it. One input, both artifacts out.
//
// Voice only. SMS mode parses <FROM> <BODY> positionally, where defaulting the
// first would make `twilio-cli sms "hello"` ambiguous between the two.
func resolveFrom(positional string) (from, warning string) {
	if positional != "" {
		return positional, ""
	}
	return defaultFrom, fmt.Sprintf("twilio-cli: WARNING: no FROM given, defaulting source number to %s", defaultFrom)
}

// resolveConfigPath returns the config file to load for webhook resolution:
// the -config flag wins, then the configEnvVar environment value.
//
// There is deliberately no built-in default. The previous one was a bare
// relative filename belonging to a particular consuming project, resolved
// against the process working directory — which meant it named a file this
// engine has no business knowing about, stopped resolving the moment that
// project reorganized, and could never have resolved at all from the checkout
// the documented workflow runs twilio-cli in. An operator-supplied absolute
// path resolves from any directory; an error naming both ways to supply one is
// more use than a filename the engine invented.
func resolveConfigPath(flagValue, envValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envValue != "" {
		return envValue, nil
	}
	return "", fmt.Errorf("no config path: pass -config <file>, set %s, or skip config resolution entirely with -webhook <url>", configEnvVar)
}

// resolveWebhook resolves the URL to send to for pathSuffix (e.g. "/webhook"
// or "/sms/inbound"): an explicit flag value always wins and skips config
// resolution entirely (even when no config is available anywhere), otherwise
// the config named by resolveConfigPath is loaded and the host and webhook
// port of the entry named by resolveServerName are read from it.
//
// One function rather than a voice copy and an SMS copy: the two routes differ
// only in pathSuffix, and every other line — the short-circuit, the two
// resolutions, the three error cases — is identical. webhookTarget and
// smsWebhookTarget name the two routes for their callers, which is also what
// makes the server name apply to both routes by construction rather than by a
// second lookup path someone has to remember to keep in step.
func resolveWebhook(explicit, configFlag, configEnv, serverFlag, serverEnv, pathSuffix string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	basePath, err := resolveConfigPath(configFlag, configEnv)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(basePath)
	if err != nil {
		return "", err
	}
	// After the load, not before: a config that is missing or malformed is the
	// more fundamental problem and its error names a file the operator can go
	// look at. Resolving the name first would answer "pass -server" to someone
	// whose real problem is a path typo.
	serverName, err := resolveServerName(serverFlag, serverEnv)
	if err != nil {
		return "", err
	}
	srv, ok := cfg.ServerByName(serverName)
	if !ok {
		return "", fmt.Errorf("no %q server declared in %s", serverName, basePath)
	}
	port, ok := srv.WebhookPort()
	if !ok {
		return "", fmt.Errorf("%q server in %s declares no webhook port (needs two listens)", serverName, basePath)
	}
	return fmt.Sprintf("http://%s:%d%s", srv.Host, port, pathSuffix), nil
}

// webhookTarget resolves the voice webhook URL to dial.
func webhookTarget(explicit, configFlag, configEnv, serverFlag, serverEnv string) (string, error) {
	return resolveWebhook(explicit, configFlag, configEnv, serverFlag, serverEnv, "/webhook")
}

// smsWebhookTarget resolves the inbound-SMS webhook URL to post to — the
// /sms/inbound route, not /webhook.
func smsWebhookTarget(explicit, configFlag, configEnv, serverFlag, serverEnv string) (string, error) {
	return resolveWebhook(explicit, configFlag, configEnv, serverFlag, serverEnv, "/sms/inbound")
}

// defaultCapturePort is the port the SMS capture server binds unless
// -capture-port overrides it. Fixed rather than ephemeral so the operator can
// launch the server with TWILIO_API_BASE_URL pointed at it *before* the CLI
// runs; free against every port the fleet config declares, and adjacent to the
// server's own 9730/9740 pair.
const defaultCapturePort = 9750

// smsCaptureGuidance returns the operator-facing line printed before the round
// trip. It states the environment assignment verbatim, because that is the only
// thing the operator can act on: the server reads TWILIO_API_BASE_URL once at
// startup, so naming an internal Go field told them to do something impossible.
func smsCaptureGuidance(port int) string {
	return fmt.Sprintf("capture server listening on port %d — launch the server with %s",
		port, captureBaseURLEnv(fmt.Sprintf("http://127.0.0.1:%d", port)))
}

// startCapture binds the reply-capture server on port and returns it together
// with the guidance line naming that same port.
//
// The pairing is the point. When runSMSMode called newSMSCaptureServer and
// smsCaptureGuidance separately, nothing stopped them being given different
// ports — and nothing could catch it, because runSMSMode ends in
// log.Fatalf/os.Exit and cannot be driven from a test at all. The two would then
// disagree in the worst possible way: the operator follows a correct-looking
// instruction naming one port while the CLI listens on another, and gets a
// timeout with no hint why. One port in, both artifacts out, and a test can
// assert they agree.
func startCapture(port int) (*smsCaptureServer, string, error) {
	capture, err := newSMSCaptureServer(port)
	if err != nil {
		return nil, "", err
	}
	return capture, smsCaptureGuidance(port), nil
}

// runSMSMode implements the `twilio-cli sms <FROM-e164> <BODY>` subcommand:
// it posts a signed inbound-SMS webhook to the server, starts a local capture
// server for the outbound REST reply, and prints the captured To/Body.
//
// The capture server must be launched *second* but bound *first* in the
// operator's mind: the server reads TWILIO_API_BASE_URL once at startup, so it
// has to be started already pointed at this fixed port. Hence the two-step in
// the usage text.
func runSMSMode(args []string) {
	fs := flag.NewFlagSet("sms", flag.ExitOnError)
	webhookURL := fs.String("webhook", "", "the server SMS webhook URL; skips config resolution entirely (default: resolved from -config / $"+configEnvVar+")")
	configPath := fs.String("config", "", "path to the fleet config to resolve the webhook target from; overrides $"+configEnvVar)
	serverFlag := fs.String("server", "", "name of the config server entry to resolve the webhook target from; overrides $"+serverEnvVar)
	toNumber := fs.String("to", defaultTo, "the Twilio number the SMS was sent to, E.164")
	capturePort := fs.Int("capture-port", defaultCapturePort, "port the local reply-capture server binds; must match the TWILIO_API_BASE_URL the server was launched with")
	fs.Parse(args)

	from := fs.Arg(0)
	var body string
	if fs.NArg() > 1 {
		body = strings.Join(fs.Args()[1:], " ")
	}
	if from == "" || body == "" {
		fmt.Fprintf(os.Stderr, "usage: %s sms [flags] <FROM-e164> <BODY>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Two steps, in this order:\n"+
			"  1. launch the server with %s\n"+
			"  2. run this command; it prints the reply the server sends back\n\n"+
			"The server reads that variable once at startup, so step 1 cannot be skipped\n"+
			"or reordered. Requires TWILIO_AUTH_TOKEN in the environment.\n\n",
			captureBaseURLEnv(fmt.Sprintf("http://127.0.0.1:%d", defaultCapturePort)))
		fs.PrintDefaults()
		os.Exit(1)
	}
	if err := validateE164(from); err != nil {
		log.Fatalf("twilio-cli: sms: FROM: %v", err)
	}
	if err := validateE164(*toNumber); err != nil {
		log.Fatalf("twilio-cli: sms: -to: %v", err)
	}

	target, err := smsWebhookTarget(*webhookURL, *configPath, os.Getenv(configEnvVar), *serverFlag, os.Getenv(serverEnvVar))
	if err != nil {
		log.Fatalf("twilio-cli: sms: %v", err)
	}

	authToken := os.Getenv("TWILIO_AUTH_TOKEN")

	capture, guidance, err := startCapture(*capturePort)
	if err != nil {
		log.Fatalf("twilio-cli: sms: %v", err)
	}
	defer capture.Close()
	fmt.Println(guidance)

	msg, err := runSMS(context.Background(), target, authToken, from, *toNumber, body, capture)
	if err != nil {
		log.Fatalf("twilio-cli: sms: %v", err)
	}

	fmt.Printf("captured reply — From=%s To=%s\n\n%s\n", msg.From, msg.To, msg.Body)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sms" {
		runSMSMode(os.Args[2:])
		return
	}

	webhookURL := flag.String("webhook", "", "the server webhook URL; skips config resolution entirely (default: resolved from -config / $"+configEnvVar+")")
	configPath := flag.String("config", "", "path to the fleet config to resolve the webhook target from; overrides $"+configEnvVar)
	serverFlag := flag.String("server", "", "name of the config server entry to resolve the webhook target from; overrides $"+serverEnvVar)
	noEchoMarks := flag.Bool("no-echo-marks", false, "suppress mark-echo (for testing the server's AwaitingMarkEcho timeout)")
	toNumber := flag.String("to", defaultTo, "dialed (listening) number, E.164")
	audioPath := flag.String("audio", "", "stream this raw μ-law file instead of capturing the mic (any platform)")
	recordPath := flag.String("record", "", "record inbound server audio to this raw μ-law file, with per-arrival timing in <file>.jsonl")
	recordSentPath := flag.String("record-sent", "", "record the outbound caller audio (mic or -audio) to this raw μ-law file, replayable with -audio")
	flag.Parse()

	// The caller's E.164 number is optional in voice mode: a local validation
	// call almost always wants the same throwaway source, and refusing to dial
	// without it bought nothing. When omitted, resolveFrom substitutes the
	// default and hands back the warning that says so — the operator must never
	// be left thinking a number they chose is on the wire.
	//
	// Whatever it resolves to is still validated locally before any network
	// call, so a typo fails fast with a clear message rather than a 403 from
	// the server's own signature/E.164 check.
	from, warning := resolveFrom(flag.Arg(0))
	if warning != "" {
		log.Println(warning)
	}
	if err := validateE164(from); err != nil {
		log.Fatalf("twilio-cli: FROM: %v", err)
	}
	if err := validateE164(*toNumber); err != nil {
		log.Fatalf("twilio-cli: -to: %v", err)
	}

	// -audio is resolved here — after the local E.164 checks, before webhook
	// resolution and any network call — so a bad path fails fast and on its own
	// terms, rather than behind a config-resolution or connection error that
	// says nothing about the file.
	if err := installAudioFrameSource(*audioPath); err != nil {
		log.Fatalf("twilio-cli: %v", err)
	}

	target, err := webhookTarget(*webhookURL, *configPath, os.Getenv(configEnvVar), *serverFlag, os.Getenv(serverEnvVar))
	if err != nil {
		log.Fatalf("twilio-cli: %v", err)
	}

	authToken := os.Getenv("TWILIO_AUTH_TOKEN")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	callSid := newSID("CA")

	streamURL, err := fetchStreamURL(ctx, target, authToken, callSid, from, *toNumber)
	if err != nil {
		log.Fatalf("twilio-cli: %v", err)
	}

	var dialOpts []dialOption
	if *noEchoMarks {
		dialOpts = append(dialOpts, withNoEchoMarks())
	}
	if *recordPath != "" {
		dialOpts = append(dialOpts, withRecording(*recordPath))
	}
	if *recordSentPath != "" {
		dialOpts = append(dialOpts, withSentRecording(*recordSentPath))
	}
	if err := dial(ctx, callSid, streamURL, dialOpts...); err != nil {
		log.Fatalf("twilio-cli: %v", err)
	}
}
