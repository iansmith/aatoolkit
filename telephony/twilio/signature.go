package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"sort"
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
