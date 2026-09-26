package twilio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// AATK-138: the dial window belongs to the backend, not to consumer code.
//
// Two resolvers feed the handshake — instructions and the session ID — so they
// have to run before the dial. They used to run AFTER the dial context was
// created, as arguments to dialRealtime, so whatever time a consumer's lookup
// took was charged against realtimeDialTimeout. A slow enough lookup failed the
// dial with "context deadline exceeded" against a backend that was perfectly
// healthy, and the error said nothing about the resolver.

// dialWindowResolvers are the resolvers whose value goes into the handshake.
// Each case is run separately so that fixing one resolution site and not the
// other stays red. The slow-resolver warning names each as name + " resolver".
var dialWindowResolvers = []struct {
	name string
	with func(fn func(start Frame) string) RealtimeOption
}{
	{"instructions", WithInstructionsFor},
	{"session ID", WithSessionIDFor},
}

// dialBudget is what recordDialBudget saw at dial entry.
type dialBudget struct {
	left    time.Duration
	bounded bool // the dial context carried a deadline at all
}

// recordDialBudget substitutes a dial that notes how much of its deadline is
// left when the dial begins, then dials for real. Restored on cleanup.
func recordDialBudget(t *testing.T) <-chan dialBudget {
	t.Helper()
	seen := make(chan dialBudget, 1)
	orig := dialRealtime
	dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
		deadline, ok := ctx.Deadline()
		seen <- dialBudget{left: time.Until(deadline), bounded: ok}
		return orig(ctx, url, opts...)
	}
	t.Cleanup(func() { dialRealtime = orig })
	return seen
}

// TestDialWindow_SlowResolverDoesNotSpendTheDialBudget pins that the whole
// realtimeDialTimeout is still available when the dial starts, however long a
// handshake resolver took.
//
// It measures the budget left at dial entry rather than sleeping past
// realtimeDialTimeout and watching the dial fail. Those are the same property —
// a resolver can only fail the dial by spending its budget — and the budget is
// visible after a fraction of a second, where the failure takes more than ten
// per case.
//
// The dial must still carry a deadline: a fix that moved the resolver by
// dropping the bound would leave the full budget "available" and be wrong.
//
// slopstop:test regression — guards: "a handshake resolver runs before the dial timeout starts, so its cost is not charged against the backend's handshake budget"
func TestDialWindow_SlowResolverDoesNotSpendTheDialBudget(t *testing.T) {
	const resolverWork = 250 * time.Millisecond

	for _, r := range dialWindowResolvers {
		t.Run(r.name, func(t *testing.T) {
			// Derived from the call's own start frame (the harness sets
			// CallSID to "CA"+t.Name()), so the site must pass the resolver
			// this call's frame, not a zero Frame.
			want := "resolved-CA" + t.Name()
			seen := recordDialBudget(t)
			be := newFakeRealtimeBackend(t)
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
				r.with(func(f Frame) string {
					time.Sleep(resolverWork)
					return "resolved-" + f.CallSID
				}),
			))
			waitBackendReady(t, be, h)

			// Not a blocking receive: the seam sends before it dials, so by
			// now the value is buffered — unless the dial bypassed the seam,
			// which must fail here rather than hang the package.
			var budget dialBudget
			select {
			case budget = <-seen:
			default:
				t.Fatal("the dial did not go through the dialRealtime seam")
			}
			if !budget.bounded {
				t.Fatal("the dial must still be bounded by a deadline")
			}
			if budget.left > realtimeDialTimeout {
				t.Fatalf("dial budget %v exceeds realtimeDialTimeout %v", budget.left, realtimeDialTimeout)
			}
			// Half the resolver's cost as slack: the budget lost to anything
			// else between WithTimeout and the dial is microseconds.
			if min := realtimeDialTimeout - resolverWork/2; budget.left < min {
				t.Fatalf("the %s resolver's %v was charged against the dial: %v of %v left at dial entry",
					r.name, resolverWork, budget.left, realtimeDialTimeout)
			}

			// The resolved value still reaches the handshake: moving the
			// resolution site must not lose it.
			if hs := be.handshake(0); !strings.Contains(hs, want) {
				t.Fatalf("the %s resolver's value must reach the handshake, got %s", r.name, hs)
			}

			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			if err := h.waitDone(5 * time.Second); err != nil {
				t.Fatalf("a stop frame must end the call cleanly, got: %v", err)
			}
		})
	}
}

// TestDialWindow_ResolverSlowerThanTheDialTimeoutStillDials is the ticket's
// literal case: a resolver that takes longer than realtimeDialTimeout, against
// a backend that answers normally, still dials — with the resolver's own value.
//
// The budget test above proves the same property faster; this one also pins
// that the engine WAITS for the value rather than abandoning a slow resolver
// and dialing with "" (the ticket's rejected option B). Any such fallback,
// wherever it is placed, fails here if its bound is below the resolver's
// sleep: the value would be missing from the handshake. No single finite test
// catches every bound.
//
// Both resolvers, as parallel subtests: a fallback could wrap either call
// site alone, and running them together keeps this at one 10 s wait. They
// swap no globals and capture no log, so they are safe to overlap.
//
// slopstop:test regression — guards: "a handshake resolver slower than realtimeDialTimeout neither fails the dial nor is replaced by a fallback value"
func TestDialWindow_ResolverSlowerThanTheDialTimeoutStillDials(t *testing.T) {
	for _, r := range dialWindowResolvers {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			const value = "resolved-after-the-dial-timeout"
			be := newFakeRealtimeBackend(t)
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
				r.with(func(Frame) string {
					time.Sleep(realtimeDialTimeout + 500*time.Millisecond)
					return value
				}),
			))

			h.sendRaw(mediaFrameRaw(h.streamSID, carrierPayloadB64()))
			waitForAppends(t, be, 1, realtimeDialTimeout+5*time.Second)
			if hs := be.handshake(0); !strings.Contains(hs, value) {
				t.Fatalf("the slow %s resolver's own value must reach the handshake, got %s", r.name, hs)
			}

			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			if err := h.waitDone(5 * time.Second); err != nil {
				t.Fatalf("a stop frame must end the call cleanly, got: %v", err)
			}
		})
	}
}

// TestDialWindow_UnresponsiveBackendStillEndsOnTheDialTimeout pins the bound
// the reorder must not disturb: a backend that accepts the connection and then
// never answers the upgrade ends the call with an error once realtimeDialTimeout
// has elapsed — not sooner, and not never.
//
// Slow by necessity (one full realtimeDialTimeout). It is the only test that
// drives the timeout itself; TestHandleStreamRealtime_DialFailureEndsCallAndClosesCarrier
// uses a backend that refuses at once and so never reaches it.
//
// slopstop:test regression — guards: "the handshake is still bounded by realtimeDialTimeout against a backend that never answers"
func TestDialWindow_UnresponsiveBackendStillEndsOnTheDialTimeout(t *testing.T) {
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(silent.Close)
	// Runs before Close (cleanups are LIFO). Close waits for the handler
	// above, and if the dial were ever unbounded nothing else would end it —
	// the test would hang instead of failing.
	t.Cleanup(silent.CloseClientConnections)
	url := "ws" + strings.TrimPrefix(silent.URL, "http")

	start := time.Now()
	h := newRealtimeHarnessWith(t, NewStreamHandler(url))

	err := h.waitDone(realtimeDialTimeout + 5*time.Second)
	if err == nil {
		t.Fatal("a backend that never answers must end the call with a non-nil error")
	}
	// No slack needed: start is taken before the dial context exists, so a
	// correct bound can only end the call at or after realtimeDialTimeout.
	if elapsed := time.Since(start); elapsed < realtimeDialTimeout {
		t.Fatalf("the call ended after %v, before realtimeDialTimeout (%v) could have elapsed: %v",
			elapsed, realtimeDialTimeout, err)
	}
}

// TestDialWindow_WedgedResolverIsLoggedWhileItIsStillRunning pins option D: a
// handshake resolver that has not returned after realtimeSlowResolverWarning is
// logged by name, WHILE it is still running.
//
// The resolver here never returns until the test releases it, which is the
// case the warning exists for: nothing bounds a resolver, the call cannot dial
// until it returns, and before this warning such a call was silent — a
// connected carrier, no backend, and no log line. A warning written only when
// the resolver returns would not appear until the test released it, which is
// after the assertion below.
//
// It names the RIGHT resolver: the other one returns at once, and its name
// must not appear.
//
// slopstop:test regression — guards: "a handshake resolver still running after realtimeSlowResolverWarning is logged by name before it returns"
func TestDialWindow_WedgedResolverIsLoggedWhileItIsStillRunning(t *testing.T) {
	for _, r := range dialWindowResolvers {
		t.Run(r.name, func(t *testing.T) {
			logs := captureLog(t)
			want := r.name + " resolver"
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("log was: %q", logs.String())
				}
			})

			// The resolver cannot return before release is closed, so a log
			// line observed before that is by construction written while it
			// is still running. Released on cleanup too, so a failure below
			// does not leave the call parked in it.
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			be := newFakeRealtimeBackend(t)
			const value = "wedged-then-released"
			h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
				r.with(func(Frame) string {
					<-release
					return value
				}),
			))

			// Pinned relative to the threshold from both sides, with 500 ms of
			// scheduling slack each way. The threshold's own size is pinned by
			// TestDialWindow_PromptResolverIsNotLogged.
			time.Sleep(realtimeSlowResolverWarning - 500*time.Millisecond)
			if strings.Contains(logs.String(), want) {
				t.Fatalf("the %s warning must not fire before realtimeSlowResolverWarning (%v)", want, realtimeSlowResolverWarning)
			}
			waitFor(t, time.Second, func() bool {
				return strings.Contains(logs.String(), want)
			})

			// Released, the call proceeds normally: the warning changes nothing
			// about the call itself.
			unblock()
			waitBackendReady(t, be, h)
			if hs := be.handshake(0); !strings.Contains(hs, value) {
				t.Fatalf("the %s resolver's value must reach the handshake once released, got %s", r.name, hs)
			}
			// Every timer has now fired or been stopped, so this is final.
			for _, other := range dialWindowResolvers {
				if other.name == r.name {
					continue
				}
				if name := other.name + " resolver"; strings.Contains(logs.String(), name) {
					t.Fatalf("only the wedged %s may be reported, but the log names %s", want, name)
				}
			}
			h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
			if err := h.waitDone(5 * time.Second); err != nil {
				t.Fatalf("a released resolver's call must end cleanly on stop, got: %v", err)
			}
		})
	}
}

// TestDialWindow_PromptResolverIsNotLogged is the other half: a resolver that
// returns before the threshold writes no warning, even after the threshold
// has passed — so the timer is stopped, not merely raced.
//
// "Before" is a real lookup's worth, not instant: each resolver takes
// promptWork. That pins the threshold's size from below — a warning tuned
// down to fire on an ordinary cache miss or database round trip would log on
// every call, and fails here.
//
// slopstop:test regression — guards: "a handshake resolver that returns before realtimeSlowResolverWarning, even after a realistic lookup, is never reported as slow"
func TestDialWindow_PromptResolverIsNotLogged(t *testing.T) {
	const promptWork = 750 * time.Millisecond
	logs := captureLog(t)
	be := newFakeRealtimeBackend(t)
	var opts []RealtimeOption
	for _, r := range dialWindowResolvers {
		opts = append(opts, r.with(func(Frame) string {
			time.Sleep(promptWork)
			return "prompt"
		}))
	}
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), opts...))
	waitBackendReady(t, be, h)

	// Both timers were armed before the backend saw the handshake, so the
	// threshold alone, measured from here, is already past both of them.
	time.Sleep(realtimeSlowResolverWarning + 100*time.Millisecond)
	// The same match the wedged test uses, so rewording the warning cannot
	// leave this test searching for text that is no longer written.
	for _, r := range dialWindowResolvers {
		if want := r.name + " resolver"; strings.Contains(logs.String(), want) {
			t.Fatalf("a prompt %s must not be reported as slow, log was: %q", want, logs.String())
		}
	}

	h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
	if err := h.waitDone(5 * time.Second); err != nil {
		t.Fatalf("a stop frame must end the call cleanly, got: %v", err)
	}
}
