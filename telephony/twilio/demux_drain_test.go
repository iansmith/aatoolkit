package twilio

import (
	"context"
	"testing"
	"time"
)

// A frame already buffered in the data plane when the pump's context is
// cancelled must still be received (drained), never dropped in favor of
// cancellation. session.go's teardown cancels the pump context and then closes
// the tap; if Recv preferred cancellation over a buffered frame, a media frame
// delivered just before a stop would vanish from both the session and the tap.
// That is the root cause of the load-sensitive TestTap_WiredToDataPlane flake:
// with a cancelled context AND a buffered frame both select cases are ready, so
// a plain select picks 50/50 and loses the frame under load.
//
// Loop enough that a 50/50 regression is caught with overwhelming probability
// (0.5^iters); the buffered case must win every time.
func TestDataPlaneRecvPrefersBufferedFrameOverCancellation(t *testing.T) {
	p := newDataPlane(1, time.Now) // clock never read: never full, so never drops

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: ctx.Done() is ready on every iteration

	const iters = 2000
	for i := range iters {
		if err := p.Send(context.Background(), Frame{Chunk: i, Payload: []byte{0x01}}); err != nil {
			t.Fatalf("iter %d: Send: %v", i, err)
		}
		f, err := p.Recv(cancelled)
		if err != nil {
			t.Fatalf("iter %d: Recv returned cancellation while a frame was buffered (%v) — the buffered frame was dropped", i, err)
		}
		if f.Chunk != i {
			t.Fatalf("iter %d: Recv returned frame chunk %d, want %d", i, f.Chunk, i)
		}
	}
}

// When the buffer is empty, a cancelled context must still make Recv return
// promptly with the cancellation error — draining buffered frames must not turn
// into ignoring cancellation, or the pump would never exit on teardown.
func TestDataPlaneRecvHonorsCancellationWhenEmpty(t *testing.T) {
	p := newDataPlane(1, time.Now)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Recv(cancelled); err == nil {
		t.Fatal("Recv on an empty plane with a cancelled context returned nil error, want cancellation")
	}
}
