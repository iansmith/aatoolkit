package smsfake

import (
	"crypto/rand"
	"fmt"
)

// BaseURLEnv renders the environment assignment that points a server's
// outbound Twilio REST client at captureURL. Single-sourced here because two
// places have to agree on it exactly: the guidance the operator copies before
// launching the server, and the timeout error reminding them they didn't.
func BaseURLEnv(captureURL string) string {
	return "TWILIO_API_BASE_URL=" + captureURL
}

// NewSID returns a fresh Twilio-shaped SID: prefix followed by 32 lowercase
// hex characters drawn from crypto/rand.
func NewSID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%x", prefix, b)
}
