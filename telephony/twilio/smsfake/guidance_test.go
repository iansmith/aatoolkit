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

// TestResolve_FlagThenEnvThenConfig pins the generic precedence: the first
// non-empty of the flag, environment and config-file values, and empty only
// when all three are. Every source holds a distinct value, so a helper that
// consulted them in any other order returns a value naming the wrong source.
func TestResolve_FlagThenEnvThenConfig(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		flagValue, envValue, cfValue string
		want                         string
	}{
		{"flag wins over env and config", "from-flag", "from-env", "from-config", "from-flag"},
		{"flag wins over config with no env", "from-flag", "", "from-config", "from-flag"},
		{"flag alone", "from-flag", "", "", "from-flag"},
		{"env wins over config when flag absent", "", "from-env", "from-config", "from-env"},
		{"env alone", "", "from-env", "", "from-env"},
		{"config when flag and env absent", "", "", "from-config", "from-config"},
		{"empty when all three absent", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := smsfake.Resolve(tc.flagValue, tc.envValue, tc.cfValue); got != tc.want {
				t.Errorf("Resolve(%q, %q, %q) = %q, want %q", tc.flagValue, tc.envValue, tc.cfValue, got, tc.want)
			}
		})
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
