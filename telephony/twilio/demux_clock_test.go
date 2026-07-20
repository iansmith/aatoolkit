package twilio

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// SOP-152 follow-up. The data plane reports how long a drop episode lasted,
// and that duration is the whole point of the log line: "dropped=3" says almost
// nothing on its own, while "dropped=3 duration=4s" is the difference between a
// momentary blip and a call that spent four seconds shedding the caller's
// audio.
//
// It was measured with time.Now()/time.Since read inside the plane, which made
// it unassertable -- a test could only sleep and hope, and no test did, so the
// arithmetic went unchecked entirely.

// fakeClock is a hand-advanced clock. It needs no mutex: dropOldestPlane reads
// it only under p.mu, and these tests drive Send/Recv from one goroutine.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// captureLog redirects the standard logger for one test and returns the buffer
// it writes into. It restores the previous writer rather than assuming stderr,
// so a caller that is itself capturing gets its output back.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// TestDataPlane_DropEpisodeReportsDurationFromInjectedClock drives a depth-1
// plane through one complete drop episode and requires the reported duration to
// be the one the test staged.
//
// The staged span is 40ms deliberately -- two 20ms frames, the smallest episode
// that can actually happen. It subsumes the obvious version of this test: a
// plane reading time.Now() internally reports microseconds of real elapsed
// time, which misses 40ms exactly as it misses 4s, and a duration only accurate
// to the second would report every realistic episode as "0s" -- true of a
// healthy plane and of one shedding every frame it sees. Asserting the small
// span catches both faults; asserting a large one catches only the first.
//
// dropped=1 rides along so this cannot pass on a plane that timed an episode it
// miscounted.
func TestDataPlane_DropEpisodeReportsDurationFromInjectedClock(t *testing.T) {
	logs := captureLog(t)
	clk := &fakeClock{t: testStreamStartedAt}
	p := newDataPlane(1, clk.now)
	ctx := context.Background()

	mustSend := func(f Frame) {
		t.Helper()
		if err := p.Send(ctx, f); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	mustSend(Frame{Event: EventMedia}) // fills the buffer
	mustSend(Frame{Event: EventMedia}) // full -> evicts, episode starts

	clk.advance(40 * time.Millisecond)

	if _, err := p.Recv(ctx); err != nil { // drain, so the next Send fits
		t.Fatalf("Recv: %v", err)
	}
	mustSend(Frame{Event: EventMedia}) // fits -> episode ends, duration logged

	if !strings.Contains(logs.String(), "duration=40ms") {
		t.Errorf("drop episode log = %q\nwant duration=40ms — the plane is not measuring with the clock it was handed",
			strings.TrimSpace(logs.String()))
	}
	if !strings.Contains(logs.String(), "dropped=1") {
		t.Errorf("drop episode log = %q\nwant dropped=1", strings.TrimSpace(logs.String()))
	}
}

// TestDataPlane_NoDropNoEpisodeLog is the contrast. The test above asserts the
// content of a log line, which a plane that logged constantly would satisfy;
// this pins the line to an episode having actually happened.
func TestDataPlane_NoDropNoEpisodeLog(t *testing.T) {
	logs := captureLog(t)
	clk := &fakeClock{t: testStreamStartedAt}
	p := newDataPlane(4, clk.now)
	ctx := context.Background()

	// Well inside the buffer, so nothing is ever evicted.
	for i := 0; i < 3; i++ {
		if err := p.Send(ctx, Frame{Event: EventMedia}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if strings.Contains(logs.String(), "drop") {
		t.Errorf("a plane that dropped nothing logged %q", strings.TrimSpace(logs.String()))
	}
}
