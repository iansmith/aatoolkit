package twilio

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// errPlaneClosed is the sentinel Recv returns once the data plane's channel is
// closed AND drained — the structural marker "everything up to Done, and
// nothing after" (AATK-15). It is distinct from a context-cancellation error:
// close+drained is the graceful teardown boundary, ctx.Done() is the hard-abort
// escape. See design/teardown-protocol.md.
var errPlaneClosed = errors.New("twilio: demux: data plane closed")

// controlPlaneDepth is the buffer depth for the control plane. Control
// events (start/stop/mark/clear) are rare, so a small depth is generous.
const controlPlaneDepth = 16

// responseInputDepth is the buffer depth for a session's response input: at
// most one response is ever pending per turn, so a small fixed buffer is
// generous headroom without being a call-duration-scaled tuning knob --
// mirrors sttDispatchDepth's rationale (telephony/session.go).
const responseInputDepth = 4

// TwilioDataPlaneInput is the SOP-116 ServiceInput pattern specialized to
// Frame for the Twilio data plane (audio media frames). Its concrete
// implementation (dropOldestPlane) evicts the oldest buffered frame rather
// than blocking when full.
type TwilioDataPlaneInput = telephony.ServiceInput[Frame]

// TwilioControlPlaneInput is the SOP-116 ServiceInput pattern specialized to
// Frame for the Twilio control plane (start/stop/mark/clear events). Its
// concrete implementation (controlPlaneInput) treats a full buffer as fatal
// rather than dropping or blocking indefinitely.
type TwilioControlPlaneInput = telephony.ServiceInput[Frame]

// TeardownSignaler is implemented by control-plane inputs that can signal a
// fatal, unrecoverable overflow requiring the call to be torn down.
type TeardownSignaler interface {
	// Teardown returns a channel that is closed exactly once, when the
	// control plane overflows.
	Teardown() <-chan struct{}
}

// dropOldestPlane is a TwilioDataPlaneInput that evicts the oldest buffered
// frame instead of blocking when full. It owns ch directly (rather than
// wrapping telephony.BufferedChan) so eviction can use a non-blocking
// receive on the channel it created — no extra goroutines, no secondary
// buffering structure.
type dropOldestPlane struct {
	ch chan Frame

	// now is this plane's clock. A drop episode's duration is measured
	// across two calls at times the plane does not choose, so unlike a
	// one-shot start instant it cannot be handed in as a value -- it has to
	// be a clock. Send implements telephony.ServiceInput[Frame], whose
	// signature is not ours to add a parameter to, so the injection point is
	// the constructor.
	now func() time.Time

	mu        sync.Mutex
	closed    bool
	dropping  bool
	dropCount int
	dropStart time.Time
}

var _ TwilioDataPlaneInput = (*dropOldestPlane)(nil)

// NewDataPlane returns a new drop-oldest TwilioDataPlaneInput with the given
// buffer depth: once full, Send evicts the oldest buffered frame to make
// room for the newest one.
//
// This is the one place the data plane's clock is read. Everything below it
// takes the reading, which is what lets a test assert a drop episode's
// reported duration exactly rather than sleeping and hoping.
func NewDataPlane(depth int) TwilioDataPlaneInput {
	return newDataPlane(depth, time.Now)
}

// newDataPlane is NewDataPlane with the clock injected. No default and no
// fallback: a nil now is a programming error that should panic at the first
// drop rather than silently resurrect the wall clock this seam exists to
// remove.
func newDataPlane(depth int, now func() time.Time) *dropOldestPlane {
	return &dropOldestPlane{ch: make(chan Frame, depth), now: now}
}

func (p *dropOldestPlane) Channel() <-chan Frame { return p.ch }

func (p *dropOldestPlane) Send(ctx context.Context, f Frame) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// After Close, the channel is closed: a send would panic. Drop instead —
	// teardown has stopped input, and no frame sent after the close boundary is
	// owed delivery (AATK-15).
	if p.closed {
		return nil
	}

	select {
	case p.ch <- f:
		p.recordNotDropping()
		return nil
	default:
	}

	// Full: evict the oldest frame to make room, then enqueue f. A
	// concurrent Recv may have already drained a slot between the two
	// selects below; in that case there's nothing to evict, and the insert
	// still succeeds without blocking.
	select {
	case <-p.ch:
		p.recordDropped()
	default:
	}

	// p.mu is held for the whole call, so no concurrent Send could have
	// refilled the slot just freed above — this always has room.
	p.ch <- f
	return nil
}

func (p *dropOldestPlane) Recv(ctx context.Context) (Frame, error) {
	var zero Frame
	select {
	case f, ok := <-p.ch:
		// A closed channel still delivers every buffered frame (ok == true)
		// before it reports closed (ok == false). So this drains in order and
		// only reports errPlaneClosed once the channel is closed AND empty —
		// giving both teardown barriers off one edge, with no context-vs-buffer
		// race. The comma-ok is load-bearing: keying the sentinel on "a receive
		// returned" instead would swallow a legitimate zero Frame (AATK-15).
		if !ok {
			return zero, errPlaneClosed
		}
		return f, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("recv cancelled: %w", ctx.Err())
	}
}

// Close closes the data plane's channel, marking the teardown boundary. It is
// idempotent (a second call is a no-op) and reuses p.mu so it serializes with
// Send: after Close returns, Send no-ops and Recv drains any buffered frames
// before reporting errPlaneClosed. Single-producer is what makes closing here
// safe — Go's rule is that only the sole sender closes (AATK-15).
func (p *dropOldestPlane) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.ch)
}

// recordDropped notes that one frame was evicted, logging the start of a
// new drop episode edge-triggered (not per frame). Called with p.mu held.
func (p *dropOldestPlane) recordDropped() {
	if !p.dropping {
		p.dropping = true
		p.dropStart = p.now()
		p.dropCount = 0
		log.Printf("twilio: demux: data plane drop started")
	}
	p.dropCount++
}

// recordNotDropping closes out a drop episode, if one was in progress,
// logging exactly one stop line with the count and duration. Called with
// p.mu held.
func (p *dropOldestPlane) recordNotDropping() {
	if !p.dropping {
		return
	}
	duration := p.now().Sub(p.dropStart)
	log.Printf("twilio: demux: data plane drop stopped: dropped=%d duration=%s", p.dropCount, duration)
	p.dropping = false
	p.dropCount = 0
}

// controlPlaneInput is a TwilioControlPlaneInput backed by a standard
// buffered channel. Unlike the data plane, a full control plane is fatal:
// Send logs at fatal level, signals Teardown exactly once, and returns an
// error instead of blocking or dropping.
type controlPlaneInput struct {
	ch           chan Frame
	teardown     chan struct{}
	teardownOnce sync.Once
}

var (
	_ TwilioControlPlaneInput = (*controlPlaneInput)(nil)
	_ TeardownSignaler        = (*controlPlaneInput)(nil)
)

// NewControlPlane returns a new TwilioControlPlaneInput with the given
// buffer depth. Sending into a full control plane is fatal — see Send.
func NewControlPlane(depth int) TwilioControlPlaneInput {
	return &controlPlaneInput{
		ch:       make(chan Frame, depth),
		teardown: make(chan struct{}),
	}
}

func (c *controlPlaneInput) Channel() <-chan Frame { return c.ch }

func (c *controlPlaneInput) Send(ctx context.Context, f Frame) error {
	select {
	case c.ch <- f:
		return nil
	default:
	}

	c.teardownOnce.Do(func() {
		log.Printf("twilio: demux: FATAL: control plane full (depth=%d) — tearing down call", cap(c.ch))
		close(c.teardown)
	})
	return fmt.Errorf("twilio: demux: control plane full, call torn down")
}

func (c *controlPlaneInput) Recv(ctx context.Context) (Frame, error) {
	var zero Frame
	select {
	case f := <-c.ch:
		return f, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("recv cancelled: %w", ctx.Err())
	}
}

func (c *controlPlaneInput) Teardown() <-chan struct{} { return c.teardown }

// Demux is a single WebSocket-reading goroutine's decode-and-route point: it
// decodes Twilio Media Streams frames and routes them by event type —
// EventMedia to the data plane, everything else to the control plane —
// while detecting gaps in the media chunk sequence. session.go integration
// (wiring these planes into a live session) is deferred to SOP-115/F.
type Demux struct {
	Data    TwilioDataPlaneInput
	Control TwilioControlPlaneInput

	mu        sync.Mutex
	lastChunk int
	haveChunk bool
}

// NewDemuxWithPlanes builds a Demux from existing data/control planes —
// used by tests that need non-default buffer depths.
func NewDemuxWithPlanes(data TwilioDataPlaneInput, control TwilioControlPlaneInput) *Demux {
	return &Demux{Data: data, Control: control}
}

// NewDemux builds a Demux with production buffer depths: the data plane
// sized per SOP-116's ComputeDepth(DataPlaneBufferMS, MuLawFrameMS), and a
// depth-16 control plane.
func NewDemux() *Demux {
	dataDepth := telephony.ComputeDepth(telephony.DataPlaneBufferMS, telephony.MuLawFrameMS)
	return NewDemuxWithPlanes(NewDataPlane(dataDepth), NewControlPlane(controlPlaneDepth))
}

// dataPlaneCloser is the close capability the data plane's concrete type
// (dropOldestPlane) provides beyond the telephony.ServiceInput[Frame] interface.
// CloseData type-asserts to it rather than widening ServiceInput, whose
// Send/Recv signatures are shared and not ours to grow (AATK-15).
type dataPlaneCloser interface{ Close() }

// CloseData closes the data plane if its concrete type supports it, marking the
// teardown boundary so the data pump drains and terminates on the resulting
// errPlaneClosed rather than on context cancellation. A no-op for a data plane
// that is not a closer.
func (d *Demux) CloseData() {
	if c, ok := d.Data.(dataPlaneCloser); ok {
		c.Close()
	}
}

// Route decodes raw and routes the resulting Frame to the correct plane.
// Called once per inbound WebSocket message from the demux's single reading
// goroutine.
func (d *Demux) Route(ctx context.Context, raw []byte) error {
	f, err := DecodeFrame(raw)
	if err != nil {
		return err
	}
	return d.RouteFrame(ctx, f)
}

// RouteFrame routes a decoded Frame to the correct plane, checking for
// chunk-sequence gaps on media frames first.
func (d *Demux) RouteFrame(ctx context.Context, f Frame) error {
	if f.Event == EventMedia {
		d.checkChunkGap(f)
		return d.Data.Send(ctx, f)
	}
	return d.Control.Send(ctx, f)
}

// checkChunkGap logs loudly when a media frame's Chunk isn't exactly one
// past the last-seen chunk.
func (d *Demux) checkChunkGap(f Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.haveChunk && f.Chunk != d.lastChunk+1 {
		log.Printf("twilio: demux: chunk gap detected: missing %d-%d (last=%d, got=%d)",
			d.lastChunk+1, f.Chunk-1, d.lastChunk, f.Chunk)
	}
	d.lastChunk = f.Chunk
	d.haveChunk = true
}
