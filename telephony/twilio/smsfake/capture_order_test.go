package smsfake_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
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
	release := capture.HoldRecording(t)

	posted := make(chan error, 1)
	go func() {
		_, _, err := sendMessage(capture, "ACorder00000000000000000000000", url.Values{"Body": {"recorded first"}})
		posted <- err
	}()

	select {
	case <-capture.Notify():
		t.Fatal("woken while the message could not yet be recorded, want the record before the wake-up")
	case <-time.After(smsfake.HoldWindow):
	}

	release()

	select {
	case <-capture.Notify():
	case <-time.After(smsfake.SubscribeWait):
		t.Fatalf("no wake-up within %s of the message being free to record", smsfake.SubscribeWait)
	}
	if got := requireOneRecorded(t, capture)[0].Body; got != "recorded first" {
		t.Errorf("recorded Body = %q, want %q, the one message whose wake-up arrived", got, "recorded first")
	}
	if err := <-posted; err != nil {
		t.Fatal(err)
	}
}

// TestCapture_SubscriptionLeavesTheSetBeforeItEnds pins the order unsubscribe
// and Close end a subscription in: they take it out of the server's set, under
// the recording lock, and only then end it. The handler queues a message for
// every member of that set under the same lock, so a subscription ended while
// still a member could be queued to after its feed had gone. Nothing would
// deliver that message or discard it, and the unsubscribe func would keep it
// alive for as long as its caller kept the func.
// TestSubscription_EndReleasesItsQueue pins that ending discards what was
// queued before the end; this pins that nothing is queued after it.
//
// It holds the recording lock, then unsubscribes or closes. While the lock is
// held the subscription cannot leave the set, so it must not end: msgs must
// stay open. Once the lock is released the call must return with msgs closed.
// The pass side cannot flake: unsubscribe and Close block on the lock before
// they can end anything. The fail side relies on an end made first finishing
// within the hold window, which it does with plenty to spare. What it pins is
// that the end waits for the lock. An end that takes the lock, lets it go
// again, ends the subscription, and only then takes the lock once more to
// leave the set is not caught.
func TestCapture_SubscriptionLeavesTheSetBeforeItEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(capture *smsfake.CaptureServer, unsubscribe func())
	}{
		{"unsubscribe", func(_ *smsfake.CaptureServer, unsubscribe func()) { unsubscribe() }},
		{"Close", func(capture *smsfake.CaptureServer, _ func()) { capture.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCapture(t)
			msgs, unsubscribe := capture.Subscribe()
			release := capture.HoldRecording(t)

			ended := make(chan struct{})
			go func() {
				defer close(ended)
				tc.end(capture, unsubscribe)
				requireClosed(t, msgs, "the subscription, once "+tc.name+" returned")
			}()

			time.Sleep(smsfake.HoldWindow)
			if !stillOpen(msgs) {
				t.Fatalf("%s ended the subscription while the recording lock was held, before it could leave the server's set; want it out of the set first, then ended", tc.name)
			}

			release()
			returnsWithin(t, tc.name+" once the recording lock was released", func() { <-ended })
		})
	}
}
