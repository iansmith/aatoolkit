package twilio

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultBaseURL is Twilio's production REST API host, used when RESTClient.BaseURL
// is empty. Tests (and SOP-188's CLI fake mode) override BaseURL to point at a local
// capture server instead.
const defaultBaseURL = "https://api.twilio.com"

// RESTClient sends outbound SMS via the Twilio Messages REST API. Credentials are
// supplied by the caller — the client never reads TWILIO_* environment variables
// itself (charter R8; the consumer is responsible for sourcing its own credentials).
type RESTClient struct {
	AccountSID string
	KeySID     string
	KeySecret  string

	// BaseURL defaults to defaultBaseURL when empty. Overriding it is the test/fake
	// seam (SOP-188 points it at a local capture server).
	BaseURL string

	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
}

// SendSMS POSTs a Messages.json request to send an SMS from `from` to `to` with the
// given body. Auth is HTTP Basic with the API key pair (KeySID:KeySecret), per D11's
// "prefer the API-key pair". A non-2xx response yields a non-nil error carrying the
// status code and response body; credentials never appear in that error text.
func (c *RESTClient) SendSMS(ctx context.Context, from, to, body string) error {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", base, c.AccountSID)

	form := url.Values{
		"From": {from},
		"To":   {to},
		"Body": {body},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.KeySID, c.KeySecret)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: send request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio: send failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
