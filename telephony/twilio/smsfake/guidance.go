package smsfake

import (
	"crypto/rand"
	"fmt"
)

// BaseURLEnv renders the TWILIO_API_BASE_URL=captureURL assignment that
// twilio-cli's guidance tells the operator to launch the server under test
// with. It is a convention between twilio-cli and that server, not one the
// engine keeps: nothing in this module reads the variable, and
// twilio.RESTClient reads no environment at all, so the assignment redirects
// only a server that copies it into its RESTClient's BaseURL itself. A test
// that builds its own client sets that field to the capture server's URL
// instead.
//
// Single-sourced here because every sentence that quotes it has to agree on it
// exactly: twilio-cli's guidance and usage text, which the operator copies
// before launching the server, and its timeout error reminding them they
// didn't.
func BaseURLEnv(captureURL string) string {
	return "TWILIO_API_BASE_URL=" + captureURL
}

// Resolve applies the flag → environment → config-file precedence: it returns
// the first non-empty of flagValue, envValue and configValue, and empty when
// all three are. It only orders values the caller has already read — it reads
// no flag, environment variable or file itself, and what an empty result means
// (an error, a default) is the caller's to decide.
func Resolve(flagValue, envValue, configValue string) string {
	for _, v := range []string{flagValue, envValue, configValue} {
		if v != "" {
			return v
		}
	}
	return ""
}

// NewSID returns a fresh Twilio-shaped SID: prefix followed by 32 lowercase
// hex characters drawn from crypto/rand.
func NewSID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%x", prefix, b)
}
