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
	defaultBasePath = "aa-server-status.toml"
	serverName      = "the server"
)

// localConfigPath derives the local overlay path from the base config path
// by convention: a ".toml" suffix is swapped for ".local.toml"; otherwise
// ".local.toml" is appended. Mirrors cmd/aa-server-status/main.go.
func localConfigPath(basePath string) string {
	if trimmed, ok := strings.CutSuffix(basePath, ".toml"); ok {
		return trimmed + ".local.toml"
	}
	return basePath + ".local.toml"
}

// resolveTarget loads the merged aa-server-status config at basePath and
// derives the server's URL for pathSuffix (e.g. "/webhook" or "/sms/inbound")
// from its host and webhook port. Shared by webhookTarget and
// smsWebhookTarget, which differ only in which route they need.
func resolveTarget(basePath, pathSuffix string) (string, error) {
	cfg, err := config.Load(basePath)
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

// webhookTarget resolves the voice webhook URL to dial: an explicit flag
// value always wins and skips config resolution entirely (even if config is
// missing or broken); otherwise it's derived from the aa-server-status config
// at basePath.
func webhookTarget(explicit, basePath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return resolveTarget(basePath, "/webhook")
}

// smsWebhookTarget resolves the inbound-SMS webhook URL to post to (the
// /sms/inbound route, not /webhook): an explicit flag value always wins,
// skipping config resolution entirely; otherwise it's derived from the
// aa-server-status config at basePath. Mirrors webhookTarget.
func smsWebhookTarget(explicit, basePath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return resolveTarget(basePath, "/sms/inbound")
}

// defaultCapturePort is the port the SMS capture server binds unless
// -capture-port overrides it. Fixed rather than ephemeral so the operator can
// launch the server with TWILIO_API_BASE_URL pointed at it *before* the CLI
// runs; free against every port aa-server-status.toml declares, and adjacent
// to the server's own 9730/9740 pair.
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
	webhookURL := fs.String("webhook", "", "the server server SMS webhook URL (default: resolved from aa-server-status.toml)")
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

	target, err := smsWebhookTarget(*webhookURL, defaultBasePath)
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

	fmt.Printf("captured reply: To=%s Body=%q\n", msg.To, msg.Body)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sms" {
		runSMSMode(os.Args[2:])
		return
	}

	webhookURL := flag.String("webhook", "", "the server server webhook URL (default: resolved from aa-server-status.toml)")
	noEchoMarks := flag.Bool("no-echo-marks", false, "suppress mark-echo (for testing the server's AwaitingMarkEcho timeout)")
	toNumber := flag.String("to", defaultTo, "dialed (listening) number, E.164")
	flag.Parse()

	// The caller's E.164 number is a required positional arg, validated locally
	// before any network call so a typo fails fast with a clear message rather
	// than a 403 from the server's own signature/E.164 check.
	from := flag.Arg(0)
	if from == "" {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <FROM-e164>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}
	if err := validateE164(from); err != nil {
		log.Fatalf("twilio-cli: FROM: %v", err)
	}
	if err := validateE164(*toNumber); err != nil {
		log.Fatalf("twilio-cli: -to: %v", err)
	}

	target, err := webhookTarget(*webhookURL, defaultBasePath)
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
	if err := dial(ctx, callSid, streamURL, dialOpts...); err != nil {
		log.Fatalf("twilio-cli: %v", err)
	}
}
