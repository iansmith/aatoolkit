package telephony

import (
	"context"
	"sync"
	"testing"
)

func TestReplyRouter_RoutesToRegisteredSession(t *testing.T) {
	router := NewReplyRouter()
	captured := &captureOutput{}
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

	if len(captured.written) != len(frames) {
		t.Fatalf("expected %d writes, got %d", len(frames), len(captured.written))
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
				output := &captureOutput{}
				sink := router.Register(sessionID, output)
				if sink != nil {
					router.Deregister(sessionID)
				}
			}
		}()
	}
	wg.Wait()
}

type captureOutput struct {
	mu      sync.Mutex
	written [][]byte
}

func (c *captureOutput) Channel() <-chan []byte { return nil }
func (c *captureOutput) Recv(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *captureOutput) Send(ctx context.Context, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := make([]byte, len(payload))
	copy(frame, payload)
	c.written = append(c.written, frame)
	return nil
}
