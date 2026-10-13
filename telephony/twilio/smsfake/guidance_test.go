package smsfake_test

import (
	"regexp"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
)

// TestBaseURLEnv_IsExactlyTheAssignment pins BaseURLEnv to the bare
// environment assignment and nothing around it. Equality, not containment:
// the sentences that quote it (twilio-cli's guidance line and its timeout
// error) are asserted by containment elsewhere, and only hold if this is
// exactly the line the operator copies.
func TestBaseURLEnv_IsExactlyTheAssignment(t *testing.T) {
	const captureURL = "http://127.0.0.1:9750"

	if got, want := smsfake.BaseURLEnv(captureURL), "TWILIO_API_BASE_URL="+captureURL; got != want {
		t.Errorf("BaseURLEnv(%q) = %q, want %q", captureURL, got, want)
	}
}

// TestNewSID_PrefixThen32LowercaseHex pins the SID shape for each prefix the
// module mints — "SM" for messages, "CA" for calls, "MZ" for media streams —
// and that two mints are distinct.
func TestNewSID_PrefixThen32LowercaseHex(t *testing.T) {
	for _, prefix := range []string{"SM", "CA", "MZ"} {
		t.Run(prefix, func(t *testing.T) {
			shape := regexp.MustCompile(`^` + prefix + `[0-9a-f]{32}$`)

			first, second := smsfake.NewSID(prefix), smsfake.NewSID(prefix)
			for _, sid := range []string{first, second} {
				if !shape.MatchString(sid) {
					t.Errorf("NewSID(%q) = %q, want %q followed by 32 lowercase hex characters", prefix, sid, prefix)
				}
			}
			if first == second {
				t.Errorf("two NewSID(%q) calls both returned %q, want distinct SIDs", prefix, first)
			}
		})
	}
}
