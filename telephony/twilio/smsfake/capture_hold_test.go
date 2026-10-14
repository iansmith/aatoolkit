package smsfake

import (
	"sync"
	"testing"
	"time"
)

// HoldWindow is how long a test holds a lock — the capture server's recording
// lock or a subscription's — watching for something that must wait for the
// lock and so must not happen while it is held. It is the window in which the
// fail side has to show, not a wait for delivery: the pass side cannot flake,
// however short the window.
const HoldWindow = 300 * time.Millisecond

// holdLock takes l and returns its release. The release also runs when the
// test ends, so a test that fails while holding l still lets it go — before
// newCapture's Close, which waits on any POST blocked on the recording lock,
// as cleanups run last in, first out. Only the first call of release lets go;
// later ones, the cleanup's included, do nothing.
func holdLock(t testing.TB, l sync.Locker) (release func()) {
	l.Lock()
	release = sync.OnceFunc(l.Unlock)
	t.Cleanup(release)
	return release
}

// HoldRecording takes the lock the capture server records a message under and
// returns its release, which holdLock also runs when the test ends. It is for
// the tests, which otherwise cannot stop a POST between its arrival and its
// recording to see which comes first: the record or the wake-up on Notify.
func (c *CaptureServer) HoldRecording(t testing.TB) (release func()) {
	return holdLock(t, &c.mu)
}
