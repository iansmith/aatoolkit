package main

import (
	"context"
	"fmt"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
)

// defaultCaptureWait bounds how long runSMS waits for the outbound reply. It
// must exceed the responder's own 30s delivery deadline, because a turn that
// hits that deadline still emits a failure-report SMS — a shorter wait would
// report "no reply" for a turn still legitimately in flight.
const defaultCaptureWait = 35 * time.Second

// smsOptions configures optional runSMS behavior.
type smsOptions struct {
	captureWait time.Duration
}

// smsOption configures smsOptions.
type smsOption func(*smsOptions)

// withCaptureWait overrides defaultCaptureWait, so a test can exercise the
// wait-expiry path without waiting out the production bound.
func withCaptureWait(d time.Duration) smsOption {
	return func(o *smsOptions) { o.captureWait = d }
}

// runSMS performs the SMS fake-mode round trip: it posts a signed inbound-SMS
// webhook to webhookURL, then waits up to defaultCaptureWait for the outbound
// REST reply to land on capture and returns the first one that does.
//
// The wait is required, not defensive. The server answers the webhook from a
// buffered queue and runs the turn on a detached goroutine, so the outbound
// Messages.json POST lands after smsfake.PostWebhook has already returned. It
// is signalled by the capture server rather than polled, so the reply is
// returned the moment it arrives. The caller is responsible for having launched
// the target server with TWILIO_API_BASE_URL pointed at capture.URL.
func runSMS(ctx context.Context, webhookURL, authToken, from, to, body string, capture *smsfake.CaptureServer, opts ...smsOption) (smsfake.Message, error) {
	cfg := smsOptions{captureWait: defaultCaptureWait}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Anything already recorded belongs to an earlier call, so this run waits
	// for the count to grow past it rather than for "any message at all". That
	// is what makes a second runSMS against the same capture server return its
	// own reply instead of re-reporting the first one.
	before := len(capture.Captured())

	messageSid := smsfake.NewSID("SM")
	form := smsfake.InboundForm(smsfake.DefaultAccountSID, smsfake.DefaultAPIVersion, messageSid, from, to, body)
	if _, _, err := smsfake.PostWebhook(ctx, webhookURL, authToken, form); err != nil {
		return smsfake.Message{}, err
	}

	timeout := time.NewTimer(cfg.captureWait)
	defer timeout.Stop()

	for {
		select {
		case <-capture.Notify():
			if msgs := capture.Captured(); len(msgs) > before {
				return msgs[before], nil
			}
		case <-timeout.C:
			return smsfake.Message{}, fmt.Errorf("no reply reached the capture server within %s — check the server was launched with %s", cfg.captureWait, smsfake.BaseURLEnv(capture.URL))
		case <-ctx.Done():
			return smsfake.Message{}, fmt.Errorf("waiting for the reply: %w", ctx.Err())
		}
	}
}
