package twilio

import (
	"context"
	"errors"
	"testing"
	"time"
)

// AATK-15. These tests pin the stop→drain→close teardown boundary for the
// Twilio data plane: the producer closes the plane's channel as its last act,
// and the consumer drains every buffered frame before the closed sentinel is
// reported. That single edge gives both teardown barriers (input stopped, queue
// drained) with no context-vs-buffer race to lose a frame. See
// design/teardown-protocol.md for the protocol and its correctness proof.
//
// Internal (package twilio) rather than twilio_test: errPlaneClosed and the
// concrete *dropOldestPlane.Close() are unexported — the sentinel is the
// contract these tests exist to nail down.

// drainTestNow is the clock handed to a plane that never drops in these tests
// (depth is always chosen > the number of frames sent), so it is never read.
// A non-nil func is still required: newDataPlane panics on a nil clock at the
// first drop rather than resurrecting the wall clock.
func drainTestNow() time.Time { return time.Time{} }

// TestDataPlaneCloseDrainsThenSignalsClosed is the completeness half of the
// boundary: after Close, every frame Send-ed before the Close is still
// delivered by Recv, in order, and only once the channel is closed AND drained
// does Recv report errPlaneClosed. No pre-stop frame is lost at teardown.
func TestDataPlaneCloseDrainsThenSignalsClosed(t *testing.T) {
	ctx := context.Background()
	const K = 5
	p := newDataPlane(K+3, drainTestNow) // depth > K: nothing is dropped

	for i := 0; i < K; i++ {
		if err := p.Send(ctx, Frame{Event: EventMedia, Chunk: i}); err != nil {
			t.Fatalf("Send(chunk %d): %v", i, err)
		}
	}
	p.Close()

	for i := 0; i < K; i++ {
		f, err := p.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv %d after Close: unexpected error %v — buffered frames must drain before the closed sentinel", i, err)
		}
		if f.Chunk != i {
			t.Fatalf("Recv %d: chunk = %d, want %d — drain must preserve send order", i, f.Chunk, i)
		}
	}

	if _, err := p.Recv(ctx); !errors.Is(err, errPlaneClosed) {
		t.Fatalf("Recv after draining a closed plane: err = %v, want errPlaneClosed", err)
	}
}

// TestDataPlaneSendAfterCloseIsNoOp is the safety half: a Send racing in after
// Close must neither panic (send on a closed channel) nor enqueue a phantom
// frame. The very next Recv must be the closed sentinel, not chunk 99.
func TestDataPlaneSendAfterCloseIsNoOp(t *testing.T) {
	ctx := context.Background()
	p := newDataPlane(4, drainTestNow)
	p.Close()

	if err := p.Send(ctx, Frame{Event: EventMedia, Chunk: 99}); err != nil {
		t.Fatalf("Send after Close: err = %v, want a nil no-op", err)
	}

	f, err := p.Recv(ctx)
	if !errors.Is(err, errPlaneClosed) {
		t.Fatalf("Recv after a post-close Send: err = %v, frame chunk = %d — want errPlaneClosed with no phantom chunk-99 frame", err, f.Chunk)
	}
}

// TestDataPlaneRecvHonorsCancellationWhenOpenAndEmpty guards the distinction the
// sentinel must not blur: on an OPEN, empty plane a cancelled ctx is the
// hard-abort escape and Recv returns a cancellation error — NOT errPlaneClosed,
// which means only "closed and drained." Recv must key the sentinel on the
// channel-closed signal (the comma-ok), not on any receive returning a zero
// Frame.
func TestDataPlaneRecvHonorsCancellationWhenOpenAndEmpty(t *testing.T) {
	p := newDataPlane(4, drainTestNow) // open, never closed; empty buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Recv(ctx)
	if err == nil {
		t.Fatal("Recv on an open, empty plane with a cancelled ctx returned nil — cancellation must be honored")
	}
	if errors.Is(err, errPlaneClosed) {
		t.Fatalf("Recv returned errPlaneClosed for a cancelled ctx on an OPEN plane: %v — cancellation is the hard-abort escape, distinct from close+drained", err)
	}
}
