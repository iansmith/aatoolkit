// Package smsfake is the fake-Twilio SMS round trip: it builds and signs the
// inbound-SMS webhook a real Twilio account would post to a server, and it
// stands in for the Twilio Messages REST API so the server's outbound reply
// can be captured and asserted on without a real account.
//
// It is exported rather than internal because the capture server stands in for
// a real provider account, which a consuming project's own integration tests
// need as much as this module's tools do — the same reason
// twilio.ComputeSignature is exported.
package smsfake

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

const (
	// DefaultAccountSID is the placeholder account SID a fake round trip
	// presents as its AccountSid when no real account is involved.
	DefaultAccountSID = "ACtwiliocli0000000000000000000000"
	// DefaultAPIVersion is the Twilio API version a fake round trip presents
	// as its ApiVersion.
	DefaultAPIVersion = "2010-04-01"
)

// InboundForm builds the standard Twilio inbound-SMS webhook field set for
// accountSID, apiVersion, messageSID, from, to, and body, including the
// SmsMessageSid/SmsSid aliases real Twilio inbound-SMS webhooks send alongside
// MessageSid (mirroring the Caller/Called aliases of the voice webhook).
func InboundForm(accountSID, apiVersion, messageSID, from, to, body string) url.Values {
	return url.Values{
		"MessageSid":    {messageSID},
		"SmsMessageSid": {messageSID},
		"SmsSid":        {messageSID},
		"AccountSid":    {accountSID},
		"From":          {from},
		"To":            {to},
		"Body":          {body},
		"ApiVersion":    {apiVersion},
	}
}

// PostWebhook performs the signed Twilio SMS-webhook ceremony: it POSTs form
// (signed over webhookURL, the exact URL posted to) and returns an error if
// the request fails or the server rejects it (e.g. a 403 from a signature
// mismatch caused by a wrong path or auth token).
func PostWebhook(ctx context.Context, webhookURL, authToken string, form url.Values) (status int, body []byte, err error) {
	sig := twilio.ComputeSignature(authToken, webhookURL, form)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("build SMS webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post SMS webhook: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read SMS webhook response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("SMS webhook %s returned status %d: %s", webhookURL, resp.StatusCode, respBody)
	}

	return 0, nil, nil
}
