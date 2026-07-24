package telephony

import (
	"context"
	"sync"
	"testing"
)

func TestReplyRouter_RoutesToRegisteredSession(t *testing.T) {
	router := NewReplyRouter()
	captured := &captureResponseInput{}
	sessionID := "CA1"

	sink := router.Register(sessionID, captured)
	if sink == nil {
		t.Fatal("Register returned nil ReplySink")
	}

	frames := [][]byte{{0x00, 0x01}, {0x02, 0x03}}
	ctx := context.Background()
	err := router.Route(ctx, sessionID, frames)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if len(captured.events) != 1 {
		t.Fatalf("expected 1 delivered response event, got %d", len(captured.events))
	}
	if !captured.events[0].OK {
		t.Errorf("delivered event: OK = false, want true")
	}
	if len(captured.events[0].Frames) != len(frames) {
		t.Fatalf("expected %d frames, got %d", len(frames), len(captured.events[0].Frames))
	}
}

func TestReplyRouter_UnknownSessionDropsAndWarns(t *testing.T) {
	router := NewReplyRouter()
	ctx := context.Background()
	err := router.Route(ctx, "nope", [][]byte{{0x00}})
	if err == nil {
		t.Fatal("should return error for unknown session")
	}
	if err != ErrUnknownSession {
		t.Fatalf("expected ErrUnknownSession, got %v", err)
	}
}

func TestReplyRouter_ConcurrentRegisterDeregister(t *testing.T) {
	router := NewReplyRouter()
	var wg sync.WaitGroup
	const numGoroutines = 10
	const numOps = 100

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := 0; op < numOps; op++ {
				sessionID := "CONCURRENT"
				output := &captureResponseInput{}
				sink := router.Register(sessionID, output)
				if sink != nil {
					router.Deregister(sessionID)
				}
			}
		}()
	}
	wg.Wait()
}

// captureResponseInput is a ResponseInput a test can inspect directly,
// mirroring the pre-existing captureOutput fake's role for
// TwilioDataPlaneOutput before ReplySink carried a response input instead of
// dataOut.
type captureResponseInput struct {
	mu     sync.Mutex
	events []ResponseEvent
}

func (c *captureResponseInput) Channel() <-chan ResponseEvent { return nil }
func (c *captureResponseInput) Recv(ctx context.Context) (ResponseEvent, error) {
	<-ctx.Done()
	return ResponseEvent{}, ctx.Err()
}
func (c *captureResponseInput) Send(ctx context.Context, ev ResponseEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frames := make([][]byte, len(ev.Frames))
	for i, f := range ev.Frames {
		frames[i] = append([]byte(nil), f...)
	}
	c.events = append(c.events, ResponseEvent{OK: ev.OK, Frames: frames})
	return nil
}
