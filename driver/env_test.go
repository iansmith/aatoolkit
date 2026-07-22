package driver

import (
	"os"
	"testing"
)

func TestEnvFloatOr(t *testing.T) {
	const key = "AATOOLKIT_TEST_FLOAT_XYZ"
	cases := []struct {
		name string
		set  bool
		val  string
		def  float64
		want float64
	}{
		{"unset returns default", false, "", 2.0, 2.0},
		{"valid float parsed", true, "3.5", 2.0, 3.5},
		{"integer-ish parsed", true, "8", 2.0, 8.0},
		{"empty string returns default", true, "", 2.0, 2.0},
		{"invalid returns default", true, "abc", 2.0, 2.0},
		// adversary gaps
		{"zero parses (not treated as unset)", true, "0", 2.0, 0.0},
		{"negative parses", true, "-1.5", 2.0, -1.5},
		{"whitespace-padded returns default", true, " 3.5 ", 2.0, 2.0},
	}
	for _, c := range cases {
		os.Unsetenv(key)
		if c.set {
			os.Setenv(key, c.val)
		}
		if got := EnvFloatOr(key, c.def); got != c.want {
			t.Errorf("%s: EnvFloatOr = %v, want %v", c.name, got, c.want)
		}
	}
	os.Unsetenv(key)
}
