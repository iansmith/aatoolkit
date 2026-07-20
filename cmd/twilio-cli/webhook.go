package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// defaultFrom is the caller-id twilio-cli presents on every webhook POST.
// Placeholder for the eventual real-Twilio swap (PRD D9) — no -from flag.
const defaultFrom = "+15105551234"

const (
	defaultTo         = "+15105559999"
	defaultAccountSid = "ACtwiliocli0000000000000000000000"
	defaultAPIVersion = "2010-04-01"
	defaultCallStatus = "ringing"
	defaultDirection  = "inbound"
)

// webhookForm builds the standard Twilio voice-webhook field set for callSid.
func webhookForm(callSid string) url.Values {
	return url.Values{
		"CallSid":    {callSid},
		"AccountSid": {defaultAccountSid},
		"From":       {defaultFrom},
		"To":         {defaultTo},
		"CallStatus": {defaultCallStatus},
		"Direction":  {defaultDirection},
		"ApiVersion": {defaultAPIVersion},
	}
}

// fetchStreamURL performs the signed Twilio webhook ceremony: it POSTs the
// standard voice-webhook field set (signed over webhookURL, the exact URL
// posted to) and returns the Media Streams URL extracted from the TwiML
// response.
func fetchStreamURL(ctx context.Context, webhookURL, authToken, callSid string) (string, error) {
	form := webhookForm(callSid)
	sig := twilio.ComputeSignature(authToken, webhookURL, form)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read webhook response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webhook %s returned status %d: %s", webhookURL, resp.StatusCode, body)
	}

	return parseStreamURL(body)
}

// twiMLStream unmarshals the <Connect><Stream url="..."/></Connect> element
// out of a TwiML response body.
type twiMLStream struct {
	XMLName xml.Name `xml:"Response"`
	Connect struct {
		Stream struct {
			URL string `xml:"url,attr"`
		} `xml:"Stream"`
	} `xml:"Connect"`
}

// parseStreamURL extracts the Media Streams URL from a TwiML
// <Response><Connect><Stream url="..."/></Connect></Response> body, returning
// a clear error if the response is not that shape.
func parseStreamURL(twiml []byte) (string, error) {
	var tw twiMLStream
	if err := xml.Unmarshal(twiml, &tw); err != nil {
		return "", fmt.Errorf("parse TwiML: %w (body: %s)", err, twiml)
	}
	if tw.Connect.Stream.URL == "" {
		return "", fmt.Errorf("TwiML response did not contain <Connect><Stream url=...>: %s", twiml)
	}
	return tw.Connect.Stream.URL, nil
}
