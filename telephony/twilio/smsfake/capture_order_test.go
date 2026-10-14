package smsfake_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
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

// TestCapture_SubscriptionLeavesTheSetBeforeItEnds guards the order
// unsubscribe and Close end a subscription in: they take it out of the
// server's set, under the recording lock, and only then end it. The handler
// queues a message for every member of that set under the same lock, so a
// subscription ended while still a member could be queued to after its feed
// had gone. Nothing would deliver that message or discard it, and the
// unsubscribe func would keep it alive for as long as its caller kept the
// func. TestSubscription_EndReleasesItsQueue pins that ending discards what
// was queued before the end. This test does not pin that nothing is queued
// after it, only that the end waits for the recording lock, as the last
// paragraph says.
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

// TestCapture_SubscribeWhileCloseRunsIsEndedByClose pins that Close's promise
// covers a subscription made while Close is running: once Close returns, that
// subscription has ended too, and the server holds nothing. The code keeps it
// by a pairing. Close marks the server closed in the same hold of the lock in
// which it takes the subscription set, and Subscribe checks for that and joins
// the set in one hold. So a Subscribe either joins before the set is taken,
// and Close ends it, or finds the server closed and ends its own subscription.
// A Close that took the set, ended what it took, and only then marked itself
// closed would let a Subscribe made in between join a set nobody ends: its
// msgs would stay open, and a subscriber ranging over it would never stop.
//
// It stops Close partway. One subscription's feed cannot finish ending while
// the test holds its lock, so Close, ending it, waits. Once Close has told it
// to stop, Close has taken the set, and the test subscribes. Then it lets
// Close finish, and on Close's goroutine, straight after Close returns, the
// new subscription must be closed. Neither side relies on timing: the
// Subscribe is made after Close has taken the set and before it can return.
// Subscribe's half of the pairing is not pinned by construction, because
// there is no point between its check and its join at which a test can stop
// it.
func TestCapture_SubscribeWhileCloseRunsIsEndedByClose(t *testing.T) {
	capture := newCapture(t)
	held, _ := capture.Subscribe() // Close ends it
	stopping, release := capture.HoldEnding(t, held)

	madeDuringClose := make(chan (<-chan smsfake.Message), 1)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		capture.Close()
		requireClosed(t, <-madeDuringClose, "a subscription made while Close ran, once Close returned")
	}()

	returnsWithin(t, "Close telling the held subscription to stop", func() { <-stopping })
	var msgs <-chan smsfake.Message
	returnsWithin(t, "Subscribe while Close is held", func() { msgs, _ = capture.Subscribe() })
	madeDuringClose <- msgs

	release()
	returnsWithin(t, "Close once the held subscription could end", func() { <-closed })
	requireSubscribers(t, capture, 0, "after Close")
}

// TestCapture_CloseWaitsForInFlightPost pins that Close does not return while
// a POST's handler is still running: it waits for the POST to be answered,
// and returns with its message recorded. A Close that shut only the listener,
// or cut the connections too, would return at once, and the POST would be
// recorded after it or dropped.
//
// The POST goes over a connection of the test's own, sent with Expect:
// 100-continue and its body held back. The server sends 100 Continue only
// when the handler first reads the body, so its arrival shows the handler has
// begun and is waiting on its form. A body held back by the client gives no
// such sign: the client's transport sends nothing until it can read the body,
// so the server has no request yet, and httptest's Close cuts a connection
// that has not sent one.
//
// Once 100 Continue has arrived, Close must not return within the hold window;
// once the body is sent, it must, with the POST answered 201 and its message
// in Captured. The pass side cannot flake: httptest's Close waits for every
// connection with a request in progress to end. The fail side relies on a
// Close that does not wait returning within the hold window, which it does
// with plenty to spare.
func TestCapture_CloseWaitsForInFlightPost(t *testing.T) {
	capture := newCapture(t)

	conn, err := net.Dial("tcp", capture.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the capture server: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(smsfake.SubscribeWait)); err != nil {
		t.Fatalf("bound the connection: %v", err)
	}

	body := url.Values{"Body": {"in flight"}}.Encode()
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n",
		messagesPath(subscribeAccount), capture.Listener.Addr(), urlencoded, len(body)); err != nil {
		t.Fatalf("send the POST's headers: %v", err)
	}
	reply := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reply, nil)
	if err != nil {
		t.Fatalf("read the server's reply to the headers: %v", err)
	}
	if interim.StatusCode != http.StatusContinue {
		t.Fatalf("the server replied %d to the headers, want %d, sent when the handler reads the body", interim.StatusCode, http.StatusContinue)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		capture.Close()
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a POST's handler was still reading its form, want it to wait for the POST")
	case <-time.After(smsfake.HoldWindow):
	}

	if _, err := io.WriteString(conn, body); err != nil {
		t.Fatalf("send the POST's body: %v", err)
	}
	returnsWithin(t, "Close once the in-flight POST's body was sent", func() { <-closed })
	if got := requireOneRecorded(t, capture)[0].Body; got != "in flight" {
		t.Errorf("recorded Body = %q, want %q, the POST in flight when Close was called", got, "in flight")
	}
	resp, err := http.ReadResponse(reply, nil)
	if err != nil {
		t.Fatalf("read the response to the in-flight POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("the in-flight POST was answered %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}
