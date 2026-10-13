package smsfake

import (
	"crypto/rand"
	"fmt"
)

// BaseURLEnv renders the environment assignment that points a server's
// outbound Twilio REST client at captureURL. Single-sourced here because every
// sentence that quotes it has to agree on it exactly: twilio-cli's guidance
// and usage text, which the operator copies before launching the server, and
// its timeout error reminding them they didn't.
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
