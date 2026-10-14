package twilio

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// params should be the parsed POST body (r.PostForm); pass nil for GET requests.
// See https://www.twilio.com/docs/usage/webhooks/webhooks-security
func ValidateSignature(authToken, rawURL string, params url.Values, signature string) bool {
	if authToken == "" || signature == "" {
		return false
	}

	expected := ComputeSignature(authToken, rawURL, params)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// ComputeSignature returns the base64-encoded HMAC-SHA1 signature Twilio
// expects for a webhook request: the raw URL followed by each sorted POST
// param key and value, keyed with authToken.
// See https://www.twilio.com/docs/usage/webhooks/webhooks-security
func ComputeSignature(authToken, rawURL string, params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(rawURL))
	for _, k := range keys {
		mac.Write([]byte(k))
		mac.Write([]byte(params.Get(k)))
	}

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// PostSignedForm performs the signed Twilio webhook ceremony: it POSTs form,
// form-encoded and carrying the X-Twilio-Signature ComputeSignature yields for
// authToken over webhookURL (the exact URL posted to), and returns an error if
// the request fails or the server rejects it (e.g. a 403 from a signature
// mismatch caused by a wrong path or auth token). what names the webhook in
// every error text — "post <what>: ...", "<what> <webhookURL> returned status
// ..." — so a caller's failures read as its own. ctx bounds the request:
// http.DefaultClient, which sends it, has no timeout of its own.
//
// Whenever the client hands a response back it also returns that response's
// status code and body: on success, alongside the error when the status was
// not 200, and — with whatever part of the body was read — when reading the
// body failed. When the client gives up on a redirect chain (net/http's
// default policy stops after 10), the last redirect's status comes back with
// the error and a nil body, because the client has already closed it. Every
// other failure returns status 0 and a nil body. That includes a redirect
// whose next hop could not be reached or whose Location would not parse,
// because net/http discards that redirect's response, so a zero status means
// no response was handed back, not that the server never answered.
func PostSignedForm(ctx context.Context, what, webhookURL, authToken string, form url.Values) (status int, body []byte, err error) {
	sig := ComputeSignature(authToken, webhookURL, form)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("build %s request: %w", what, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Do returns a response with its error only when it stopped following
		// redirects, and has closed that response's body already.
		if resp != nil {
			status = resp.StatusCode
		}
		return status, nil, fmt.Errorf("post %s: %w", what, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, respBody, fmt.Errorf("read %s response: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, respBody, fmt.Errorf("%s %s returned status %d: %s", what, webhookURL, resp.StatusCode, respBody)
	}

	return resp.StatusCode, respBody, nil
}
