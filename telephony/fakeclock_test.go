package telephony_test

import (
	"sync"
	"testing"
	"time"
)

// fakeClock supplies the passage of time to a Session's timers (see
// telephony.WithClock) so a test fires them itself instead of sleeping past
// their deadlines.
//
// Timers are keyed by the duration they were armed for, which is how a test
// picks one out: a session has several armed at once (idle, utterance,
// markEcho) and firing the wrong one ends the call instead of advancing the
// turn. The durations are distinct by construction, so the duration names the
// timer unambiguously.
type fakeClock struct {
	mu    sync.Mutex
	armed map[time.Duration][]chan time.Time
	// armedCh announces each arming so waitArmed can block on a condition
	// rather than poll.
	armedCh chan time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		armed:   make(map[time.Duration][]chan time.Time),
		armedCh: make(chan time.Duration, 64),
	}
}

// after is the func handed to telephony.WithClock. It never fires on its own:
// only fire() releases a timer.
func (f *fakeClock) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	ch := make(chan time.Time, 1)
	f.armed[d] = append(f.armed[d], ch)
	f.mu.Unlock()

	select {
	case f.armedCh <- d:
	default: // announcement is best-effort; a full buffer means nobody is waiting
	}
	return ch
}

// waitArmed blocks until a timer for exactly d has been armed, and reports
// whether one was. Arming happens on the session's own goroutine, so a test
// cannot fire a timer the instant it sends the input that arms it.
//
// This waits on an event, not on a duration: the timeout is a deadlock
// backstop that a passing test never reaches, not a deadline the assertion
// depends on.
func (f *fakeClock) waitArmed(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f.isArmed(d) {
			return
		}
		select {
		case <-f.armedCh:
		case <-deadline:
			t.Fatalf("no timer armed for %s within the backstop; armed: %v", d, f.armedDurations())
		}
	}
}

func (f *fakeClock) isArmed(d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.armed[d]) > 0
}

func (f *fakeClock) armedDurations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ds []time.Duration
	for d, chans := range f.armed {
		if len(chans) > 0 {
			ds = append(ds, d)
		}
	}
	return ds
}

// fire releases every timer armed for exactly d, as though that much time had
// passed. It reports how many fired, so a test can assert it actually fired
// the timer it meant to rather than silently firing none.
func (f *fakeClock) fire(t *testing.T, d time.Duration) int {
	t.Helper()
	f.waitArmed(t, d)

	f.mu.Lock()
	chans := f.armed[d]
	delete(f.armed, d)
	f.mu.Unlock()

	// waitArmed has already fataled if nothing was armed for d, so chans is
	// non-empty here by construction.
	for _, ch := range chans {
		ch <- time.Time{}
	}
	return len(chans)
}
