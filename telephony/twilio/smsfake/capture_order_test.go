package smsfake_test

import (
	"net/url"
	"testing"
	"time"
)

// TestCaptureServer_RecordsBeforeItWakes pins the order Notify's contract rests
// on: a message is recorded before its wake-up is sent, so a waiter it wakes
// always finds it in Captured. A waiter woken first would re-read the old
// count, go back to waiting, and never be woken for that message again.
//
// It holds the recording lock while a POST arrives. No wake-up may come while
// the message cannot yet be recorded; once the lock is released, exactly that
// message is recorded and its wake-up follows. The pass side cannot flake: the
// handler blocks on the lock before it can send. The fail side relies on the
// POST reaching the handler within the hold window, which on loopback it does
// with plenty to spare.
func TestCaptureServer_RecordsBeforeItWakes(t *testing.T) {
	capture := newCapture(t)

	release := capture.HoldRecording()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	posted := make(chan error, 1)
	go func() {
		_, _, err := sendMessage(capture, "ACorder00000000000000000000000", url.Values{"Body": {"recorded first"}})
		posted <- err
	}()

	select {
	case <-capture.Notify():
		t.Fatal("woken while the message could not yet be recorded, want the record before the wake-up")
	case <-time.After(300 * time.Millisecond):
	}

	release()
	released = true

	select {
	case <-capture.Notify():
	case <-time.After(5 * time.Second):
		t.Fatal("no wake-up within 5s of the message being free to record")
	}
	if got := capture.Captured(); len(got) != 1 || got[0].Body != "recorded first" {
		t.Errorf("Captured() = %+v, want the one message whose wake-up arrived", got)
	}
	if err := <-posted; err != nil {
		t.Fatal(err)
	}
}
