package driver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The queue must render clips one at a time, in enqueue order — so the ack
// finishes before the answer starts (no overlapping afplay).
func TestSpeechQueueSerial(t *testing.T) {
	var mu sync.Mutex
	var order []byte
	q := newSpeechQueue(func(text []byte, _ string, _ float64) {
		time.Sleep(time.Millisecond) // expose overlap if the queue isn't serial
		mu.Lock()
		order = append(order, text[0])
		mu.Unlock()
	})
	d1 := q.enqueue([]byte{1}, "", 1)
	d2 := q.enqueue([]byte{2}, "", 1)
	d3 := q.enqueue([]byte{3}, "", 1)
	<-d1
	<-d2
	<-d3
	if string(order) != string([]byte{1, 2, 3}) {
		t.Fatalf("play order = %v, want [1 2 3] (serial, in order)", order)
	}
}

// cancelQueued drains pending items without rendering them and closes their done
// channels so SpeakSync callers are not stranded.
func TestSpeechQueueCancelQueued(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var rendered []int

	q := newSpeechQueue(func(text []byte, _ string, _ float64) {
		if text[0] == 0 {
			close(started) // signal that clip 0 is rendering
			<-release      // block until the test says go
		} else {
			rendered = append(rendered, int(text[0]))
		}
	})

	// Clip 0 will block the worker while clips 1 and 2 queue up.
	q.enqueue([]byte{0}, "", 1)
	<-started // worker is now inside render for clip 0
	d1 := q.enqueue([]byte{1}, "", 1)
	d2 := q.enqueue([]byte{2}, "", 1)

	q.cancelQueued() // drop 1 and 2; close their done channels
	close(release)   // let clip 0 finish

	// done channels for cancelled clips must be closed (not block).
	mustClose := func(ch <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s not closed after cancelQueued", name)
		}
	}
	mustClose(d1, "d1")
	mustClose(d2, "d2")

	// Clips 1 and 2 must not have been rendered.
	if len(rendered) != 0 {
		t.Fatalf("expected no renders for cancelled clips, got %v", rendered)
	}
}

// Adversary: a single clip renders exactly once, and its done channel is
// close-only (not a value) and closed exactly once.
func TestSpeechQueueSingleClipAndCloseOnce(t *testing.T) {
	var n int32
	q := newSpeechQueue(func(text []byte, _ string, _ float64) { atomic.AddInt32(&n, 1) })
	done := q.enqueue([]byte{1}, "", 1)
	<-done
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("single clip: render called %d times, want 1", got)
	}
	select {
	case _, ok := <-done:
		if ok {
			t.Fatal("done channel delivered a value; want close-only")
		}
	default:
		t.Fatal("done channel not closed after the clip finished")
	}
}
