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

// resolveWebhookURL loads the merged aa-server-status config at basePath and
// derives the the server server's webhook URL from its host and webhook port.
func resolveWebhookURL(basePath string) (string, error) {
	cfg, err := config.Load(basePath, localConfigPath(basePath))
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
	return fmt.Sprintf("http://%s:%d/webhook", srv.Host, port), nil
}

// webhookTarget resolves the webhook URL to dial: an explicit flag value
// always wins and skips config resolution entirely (even if config is
// missing or broken); otherwise it's derived from the aa-server-status config
// at basePath.
func webhookTarget(explicit, basePath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return resolveWebhookURL(basePath)
}

func main() {
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
