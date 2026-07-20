package twilio_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"sort"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// computeSig is the reference Twilio HMAC-SHA1 algorithm used to produce
// known-good values for the acceptance tests below.
func computeSig(authToken, rawURL string, params url.Values) string {
	h := hmac.New(sha1.New, []byte(authToken))
	h.Write([]byte(rawURL))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(params[k][0]))
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Edge: empty signature is always rejected.
func TestValidateSignature_EmptySignatureRejected(t *testing.T) {
	if twilio.ValidateSignature("authtoken", "https://example.com/webhook", nil, "") {
		t.Fatal("empty signature must be rejected")
	}
}

// Edge: URL with query params contributes to the HMAC (not stripped).
func TestValidateSignature_URLWithQueryParamsValidates(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook?foo=bar"
	sig := computeSig(token, rawURL, nil)
	if !twilio.ValidateSignature(token, rawURL, nil, sig) {
		t.Fatal("correct signature over URL with query params must validate")
	}
}

// Rejection: garbled signature bytes are rejected.
func TestValidateSignature_WrongSignatureRejected(t *testing.T) {
	if twilio.ValidateSignature("authtoken", "https://example.com/webhook", nil, "bm90YXJlYWxzaWc=") {
		t.Fatal("wrong signature must be rejected")
	}
}

// Rejection: correct signature against a different auth token is rejected.
func TestValidateSignature_WrongTokenRejected(t *testing.T) {
	sig := computeSig("correcttoken", "https://example.com/webhook", nil)
	if twilio.ValidateSignature("wrongtoken", "https://example.com/webhook", nil, sig) {
		t.Fatal("signature valid for a different token must be rejected")
	}
}

// Happy: correct HMAC-SHA1 with no POST params validates.
func TestValidateSignature_CorrectSignatureAccepted(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	sig := computeSig(token, rawURL, nil)
	if !twilio.ValidateSignature(token, rawURL, nil, sig) {
		t.Fatal("correct HMAC-SHA1 signature must validate")
	}
}

// Happy: correct HMAC-SHA1 with sorted POST params validates.
func TestValidateSignature_CorrectSignatureWithParamsAccepted(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	params := url.Values{"CallSid": {"CA123"}, "From": {"+15005550006"}}
	sig := computeSig(token, rawURL, params)
	if !twilio.ValidateSignature(token, rawURL, params, sig) {
		t.Fatal("correct HMAC-SHA1 with POST params must validate")
	}
}

// Happy: ComputeSignature's output is accepted by ValidateSignature, and a
// signature computed with a different token is rejected.
func TestComputeSignature_AcceptedByValidate(t *testing.T) {
	const token = "authtoken"
	const rawURL = "https://example.com/webhook"
	params := url.Values{"CallSid": {"CA123"}, "From": {"+15005550006"}}

	sig := twilio.ComputeSignature(token, rawURL, params)
	if !twilio.ValidateSignature(token, rawURL, params, sig) {
		t.Fatal("ComputeSignature output must be accepted by ValidateSignature")
	}

	wrongSig := twilio.ComputeSignature("wrongtoken", rawURL, params)
	if twilio.ValidateSignature(token, rawURL, params, wrongSig) {
		t.Fatal("signature computed with a different token must be rejected")
	}
}
