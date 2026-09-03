package telephony

import (
	"testing"
	"time"
)

// TestMuLawDuration pins the exact-arithmetic conversion from a mu-law byte
// count to playout time. The values are the contract: they are what
// time.Duration(n) * time.Second / SampleRateHz produces, sub-millisecond
// remainder included.
//
// n=100 and n=12345 are the guards. A reintroduced truncate-to-whole-
// milliseconds form (n*1000/SampleRateHz, then * time.Millisecond) agrees with
// this function only when n is a multiple of 8, so it would return 12ms and
// 1.543s for those two and fail here. Frame-sized payloads (160, 256) are
// multiples of 8 and agree either way, which is why the divergence went
// unnoticed -- they are here so the table shows both halves.
func TestMuLawDuration(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want time.Duration
	}{
		{"not a multiple of 8", 100, 12500 * time.Microsecond},
		{"one 20ms frame", 160, 20 * time.Millisecond},
		{"256-byte frame", 256, 32 * time.Millisecond},
		{"1000 bytes", 1000, 125 * time.Millisecond},
		{"long odd run", 12345, 1543125 * time.Microsecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MuLawDuration(tc.n); got != tc.want {
				t.Errorf("MuLawDuration(%d) = %s, want %s", tc.n, got, tc.want)
			}
		})
	}
}
