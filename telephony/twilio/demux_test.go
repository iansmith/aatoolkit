package twilio_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

func mediaFrame(streamSID string, chunk int) twilio.Frame {
	return twilio.Frame{Event: twilio.EventMedia, StreamSID: streamSID, Chunk: chunk}
}

// Happy: 3 media frames all arrive on the data plane, none on the control plane.
func TestDemux_RoutesMediaToData(t *testing.T) {
	d := twilio.NewDemux()
	ctx := context.Background()

	frames := []twilio.Frame{
		mediaFrame("MZ1", 1),
		mediaFrame("MZ1", 2),
		mediaFrame("MZ1", 3),
	}
	for _, f := range frames {
		if err := d.RouteFrame(ctx, f); err != nil {
			t.Fatalf("RouteFrame: %v", err)
		}
	}

	for i, want := range frames {
		select {
		case got := <-d.Data.Channel():
			if got.Chunk != want.Chunk {
				t.Fatalf("data plane frame %d: got chunk %d, want %d", i, got.Chunk, want.Chunk)
			}
		default:
			t.Fatalf("data plane frame %d: expected a frame, got none", i)
		}
	}

	select {
	case f := <-d.Control.Channel():
		t.Fatalf("control plane should be empty, got %+v", f)
	default:
	}
}

// Happy: start, stop, mark, clear all arrive on the control plane.
func TestDemux_RoutesControlEvents(t *testing.T) {
	d := twilio.NewDemux()
	ctx := context.Background()

	events := []twilio.EventType{twilio.EventStart, twilio.EventStop, twilio.EventMark, twilio.EventClear}
	for _, ev := range events {
		if err := d.RouteFrame(ctx, twilio.Frame{Event: ev, StreamSID: "MZ1"}); err != nil {
			t.Fatalf("RouteFrame(%s): %v", ev, err)
		}
	}

	for i, want := range events {
		select {
		case got := <-d.Control.Channel():
			if got.Event != want {
				t.Fatalf("control plane frame %d: got event %q, want %q", i, got.Event, want)
			}
		default:
			t.Fatalf("control plane frame %d: expected a frame, got none", i)
		}
	}

	select {
	case f := <-d.Data.Channel():
		t.Fatalf("data plane should be empty, got %+v", f)
	default:
	}
}

// Edge: with depth 2, sending A, B, C leaves B, C in the buffer — A is evicted.
func TestDemux_DropOldest(t *testing.T) {
	data := twilio.NewDataPlane(2)
	ctx := context.Background()

	a := mediaFrame("MZ1", 1)
	b := mediaFrame("MZ1", 2)
	c := mediaFrame("MZ1", 3)

	for _, f := range []twilio.Frame{a, b, c} {
		if err := data.Send(ctx, f); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	got1, err := data.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv 1: %v", err)
	}
	got2, err := data.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv 2: %v", err)
	}

	if got1.Chunk != b.Chunk || got2.Chunk != c.Chunk {
		t.Fatalf("buffer contents: got [%d, %d], want [%d, %d]", got1.Chunk, got2.Chunk, b.Chunk, c.Chunk)
	}

	select {
	case extra := <-data.Channel():
		t.Fatalf("expected buffer to contain exactly 2 frames, got extra: %+v", extra)
	default:
	}
}

// Edge: drops are logged edge-triggered — exactly one line when dropping starts,
// one when it stops (with count), no per-frame log lines.
func TestDropIsLoudOnEdges(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	data := twilio.NewDataPlane(2)
	ctx := context.Background()

	// Fill the buffer, then overflow it repeatedly — every one of these
	// Sends evicts the oldest frame since nothing is draining the buffer.
	for i := 1; i <= 5; i++ {
		if err := data.Send(ctx, mediaFrame("MZ1", i)); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	// Drain the buffer so the next Send has room — this is what ends the
	// drop episode.
	if _, err := data.Recv(ctx); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if _, err := data.Recv(ctx); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if err := data.Send(ctx, mediaFrame("MZ1", 6)); err != nil {
		t.Fatalf("Send 6: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 log lines (start + stop), got %d: %q", len(lines), lines)
	}
}

// Edge: non-consecutive chunk values across media frames produce a loud log line.
func TestDemux_ChunkGapDetected(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	d := twilio.NewDemux()
	ctx := context.Background()

	for _, chunk := range []int{1, 2, 5} {
		if err := d.RouteFrame(ctx, mediaFrame("MZ1", chunk)); err != nil {
			t.Fatalf("RouteFrame(chunk=%d): %v", chunk, err)
		}
	}

	if !strings.Contains(buf.String(), "gap") {
		t.Fatalf("expected a log line about the chunk gap, got: %q", buf.String())
	}
}

// Error: filling the control plane to capacity and sending one more is fatal —
// a fatal-level log line and a teardown signal.
func TestDemux_ControlPlaneFatalOnFull(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	const depth = 16
	control := twilio.NewControlPlane(depth)
	ctx := context.Background()

	for i := 0; i < depth; i++ {
		if err := control.Send(ctx, twilio.Frame{Event: twilio.EventMark, StreamSID: "MZ1"}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	signaler, ok := control.(twilio.TeardownSignaler)
	if !ok {
		t.Fatalf("control plane does not implement TeardownSignaler")
	}

	select {
	case <-signaler.Teardown():
		t.Fatal("teardown signaled before control plane overflowed")
	default:
	}

	if err := control.Send(ctx, twilio.Frame{Event: twilio.EventMark, StreamSID: "MZ1"}); err == nil {
		t.Fatal("expected an error sending into a full control plane")
	}

	select {
	case <-signaler.Teardown():
	case <-time.After(time.Second):
		t.Fatal("expected teardown signal after control plane overflow")
	}

	if !strings.Contains(strings.ToLower(buf.String()), "fatal") {
		t.Fatalf("expected a fatal-level log line, got: %q", buf.String())
	}
}
