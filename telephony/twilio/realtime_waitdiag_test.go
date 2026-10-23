package twilio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

const (
	appendWaitPrefix = "backend received 0 of 1 expected appends before deadline"
	conditionPrefix  = "condition never held within the deadline"
)

// TestAppendWaitFailure pins the message builder behind the fake-backend wait:
// the original text stays an exact prefix, and the suffix says which of the
// three situations the wait ended in.
func TestAppendWaitFailure(t *testing.T) {
	sentinel := errors.New("sentinel dial failure")
	cases := []struct {
		name        string
		conns       int
		errs        []error
		wantExact   string
		wantHas     string
		wantMissing string
	}{
		{name: "no error, never connected", conns: 0,
			wantExact: appendWaitPrefix + "; no backend connection was made"},
		{name: "no error, connected", conns: 1,
			wantExact: appendWaitPrefix},
		{name: "error, never connected", conns: 0, errs: []error{sentinel},
			wantHas: "; stream handler returned: sentinel dial failure", wantMissing: "no backend connection"},
		{name: "error, connected", conns: 1, errs: []error{sentinel},
			wantHas: "; stream handler returned: sentinel dial failure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appendWaitFailure(0, 1, c.conns, c.errs)
			if !strings.HasPrefix(got, appendWaitPrefix) {
				t.Fatalf("message %q lost the original prefix %q", got, appendWaitPrefix)
			}
			if c.wantExact != "" && got != c.wantExact {
				t.Fatalf("message = %q, want exactly %q", got, c.wantExact)
			}
			if c.wantHas != "" && !strings.Contains(got, c.wantHas) {
				t.Fatalf("message %q lacks %q", got, c.wantHas)
			}
			if c.wantMissing != "" && strings.Contains(got, c.wantMissing) {
				t.Fatalf("message %q must not contain %q", got, c.wantMissing)
			}
		})
	}
}

// TestAwaitAppends_ReportsInjectedDialError drives a real harness whose
// backend dial fails and checks both waits name the error, without consuming
// the value waitDone reads. The failure is injected through the dialRealtime
// seam, so it cannot itself be hit by the port exhaustion it diagnoses. No
// t.Parallel: dialRealtime is process-global.
func TestAwaitAppends_ReportsInjectedDialError(t *testing.T) {
	sentinel := errors.New("sentinel-dial-failure-7f3a")
	dialErr := fmt.Errorf("dial tcp 127.0.0.1:1: %w: %w", syscall.EADDRNOTAVAIL, sentinel)

	orig := dialRealtime
	dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
		return nil, dialErr
	}
	t.Cleanup(func() { dialRealtime = orig })

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarness(t, be.url())

	waitFor(t, 5*time.Second, func() bool { return len(harnessHandlerErrs(t)) > 0 })

	_, err := awaitAppends(t, be, 1, 50*time.Millisecond)
	if err == nil {
		t.Fatal("awaitAppends succeeded with no appends")
	}
	if !strings.HasPrefix(err.Error(), appendWaitPrefix) {
		t.Fatalf("message %q lost the original prefix", err)
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("message %q does not name the dial error %q", err, sentinel)
	}

	msg := conditionNeverHeld(t)
	if !strings.HasPrefix(msg, conditionPrefix) || !strings.Contains(msg, sentinel.Error()) {
		t.Fatalf("waitFor message %q must start %q and name the dial error %q", msg, conditionPrefix, sentinel)
	}

	if got := h.waitDone(5 * time.Second); !errors.Is(got, sentinel) {
		t.Fatalf("waitDone = %v, want the dial error: the wait must not consume done", got)
	}
}
