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

const (
	// defaultTo is the -to flag's default dialed number.
	defaultTo         = "+15105559999"
	defaultAccountSid = "ACtwiliocli0000000000000000000000"
	defaultAPIVersion = "2010-04-01"
	defaultCallStatus = "ringing"
	defaultDirection  = "inbound"
	// defaultCallerName is the CallerName alias placeholder. On a real call
	// this is caller-id-lookup gated and often absent; a fixed placeholder is
	// enough for a faithful stand-in.
	defaultCallerName = "Anonymous"
)

// webhookForm builds the standard Twilio voice-webhook field set for callSid,
// from, and to. Twilio sends the caller-id aliases Caller (= From) and Called
// (= To) alongside From/To, plus CallerName; twilio-cli sends them too so the
// signed form matches what the real webhook carries.
func webhookForm(callSid, from, to string) url.Values {
	return url.Values{
		"CallSid":    {callSid},
		"AccountSid": {defaultAccountSid},
		"From":       {from},
		"To":         {to},
		"CallStatus": {defaultCallStatus},
		"Direction":  {defaultDirection},
		"ApiVersion": {defaultAPIVersion},
		// AATK-16 RED: caller-id aliases (Caller/Called/CallerName) not yet added.
	}
}

// fetchStreamURL performs the signed Twilio webhook ceremony: it POSTs the
// standard voice-webhook field set (signed over webhookURL, the exact URL
// posted to) and returns the Media Streams URL extracted from the TwiML
// response.
func fetchStreamURL(ctx context.Context, webhookURL, authToken, callSid, from, to string) (string, error) {
	form := webhookForm(callSid, from, to)
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
