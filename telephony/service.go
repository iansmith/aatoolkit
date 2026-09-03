package telephony

import (
	"context"
	"fmt"
	"math"
	"time"
)

// DataPlaneBufferMS is the target buffer depth in milliseconds for the
// data plane (audio frames and recognition results). This buffer absorbs
// jitter between frame-driven services (audio in, VAD, STT) and the
// message-driven control plane (LLM, TTS, Twilio out).
const DataPlaneBufferMS = 80

// MuLawFrameMS is the duration of a single Twilio μ-law audio frame.
// Twilio sends 160-sample μ-law frames at 8 kHz = 20 ms per frame.
const MuLawFrameMS = 20

// SampleRateHz is the sample rate of μ-law audio from Twilio.
const SampleRateHz = 8000

// MuLawDuration is how long n bytes of μ-law audio take to play out: one byte
// per sample at SampleRateHz. It is the one definition of this conversion --
// callers derive playout pacing, recording gap lengths and elapsed-audio
// figures from it rather than restating the arithmetic.
//
// The result is exact, keeping the sub-millisecond remainder: 100 bytes is
// 12.5ms, not 12ms. Truncating to whole milliseconds first is a different
// function that agrees only when n is a multiple of 8, so it must not be
// substituted here.
func MuLawDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / SampleRateHz
}

// FrameMS derives the frame duration in milliseconds from the static
// sample rate and the standard Twilio frame size. It is computed once
// at init to allow future flexibility if frame size changes.
var FrameMS = MuLawFrameMS

// ServiceInput[T] is the generic interface for a service that accepts
// typed input. It wraps a channel and provides context-aware send/recv.
type ServiceInput[T any] interface {
	Channel() <-chan T
	Send(ctx context.Context, val T) error
	Recv(ctx context.Context) (T, error)
}

// ServiceOutput[T] is the generic interface for a service that produces
// typed output. It wraps a channel and provides context-aware send/recv.
type ServiceOutput[T any] interface {
	Channel() <-chan T
	Send(ctx context.Context, val T) error
	Recv(ctx context.Context) (T, error)
}

// BufferedChan[T] is a concrete service channel implementation that wraps
// a buffered Go channel. The buffer depth is derived from DataPlaneBufferMS
// and FrameMS: depth = ceil(DataPlaneBufferMS / FrameMS).
type BufferedChan[T any] struct {
	ch chan T
}

// NewBufferedChan creates a new BufferedChan with the specified depth.
func NewBufferedChan[T any](depth int) *BufferedChan[T] {
	return &BufferedChan[T]{
		ch: make(chan T, depth),
	}
}

// Channel returns the underlying chan T (read-only view for receivers).
// Callers can range over this or use it as the receive side of select.
func (bc *BufferedChan[T]) Channel() <-chan T {
	return bc.ch
}

// Send sends a value into the channel, respecting the context deadline.
// If the context is cancelled before the send completes, returns the
// context error.
func (bc *BufferedChan[T]) Send(ctx context.Context, val T) error {
	select {
	case bc.ch <- val:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("send cancelled: %w", ctx.Err())
	}
}

// Recv receives a value from the channel, respecting the context deadline.
// If the context is cancelled before a value is available, returns the
// context error and the zero value of T.
func (bc *BufferedChan[T]) Recv(ctx context.Context) (T, error) {
	var zero T
	select {
	case val := <-bc.ch:
		return val, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("recv cancelled: %w", ctx.Err())
	}
}

// ComputeDepth returns the buffer depth required for the given buffer
// duration (in milliseconds) and frame size (in milliseconds).
// depth = ceil(bufferMS / frameMS).
func ComputeDepth(bufferMS, frameMS int) int {
	return int(math.Ceil(float64(bufferMS) / float64(frameMS)))
}
