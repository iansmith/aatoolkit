package twilio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
// backend dial fails and checks that awaitAppends and conditionNeverHeld name
// the error, without consuming the value waitDone reads. That the failing
// waits print these messages is pinned by TestWaits_FailWithDialError. The failure is injected through the dialRealtime
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

// waitChildEnv selects which wait the re-executed child fails in.
const waitChildEnv = "AATK_WAITDIAG_CHILD"

// TestWaits_FailWithDialError runs waitForAppends and waitFor to a timeout in
// a re-executed child test and checks the child's failure output carries the
// original sentence and the injected dial error. A child is needed because
// both waits end in t.Fatal, which a test cannot observe on its own t.
func TestWaits_FailWithDialError(t *testing.T) {
	sentinel := errors.New("sentinel-dial-failure-c41e")
	if mode := os.Getenv(waitChildEnv); mode != "" {
		dialErr := fmt.Errorf("dial tcp 127.0.0.1:1: %w: %w", syscall.EADDRNOTAVAIL, sentinel)
		orig := dialRealtime
		dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
			return nil, dialErr
		}
		t.Cleanup(func() { dialRealtime = orig })

		be := newFakeRealtimeBackend(t)
		newRealtimeHarness(t, be.url())
		waitFor(t, 5*time.Second, func() bool { return len(harnessHandlerErrs(t)) > 0 })

		switch mode {
		case "appends":
			waitForAppends(t, be, 1, 50*time.Millisecond)
		case "cond":
			waitFor(t, 50*time.Millisecond, func() bool { return false })
		}
		t.Fatalf("child mode %q: the wait returned instead of failing", mode)
	}

	for _, c := range []struct{ mode, prefix string }{
		{"appends", appendWaitPrefix},
		{"cond", conditionPrefix},
	} {
		t.Run(c.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWaits_FailWithDialError$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), waitChildEnv+"="+c.mode)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child must fail with a non-zero exit, got err=%v\n%s", err, out)
			}
			if strings.Contains(string(out), "the wait returned instead of failing") {
				t.Fatalf("child's wait did not fail:\n%s", out)
			}
			// The handler's own dial log also names the sentinel, so the
			// error must sit on the wait's failure line itself.
			want := c.prefix + "; stream handler returned: "
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, want) && strings.Contains(line, sentinel.Error()) {
					return
				}
			}
			t.Fatalf("no child output line holds %q and the dial error %q:\n%s", want, sentinel, out)
		})
	}
}
