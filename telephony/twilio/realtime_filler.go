package twilio

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// armTrigger names what asked for a countdown, which is the whole of what the
// machine needs to remember about its own cause: only one of the two is worth
// a log line, and only start knows whether that countdown survived to play.
//
// A type rather than a bool because armedByCallOpen and armedByTurn read at the
// call site, where a bare true/false would not — and because it is carried in
// the pending start's closure rather than on the struct, so naming it costs
// nothing that a field would have cost.
type armTrigger int

const (
	// armedByTurn is observe's pair: the caller stopped speaking, or a
	// response ended in a function call and the tool round trip begins. Both
	// mean the caller has finished talking and is now waiting.
	armedByTurn armTrigger = iota
	// armedByCallOpen is the call itself opening, with nothing said on either
	// side yet (AATK-128). Before it the machine was never armed until the
	// caller spoke, so a backend that completed its handshake and then
	// produced nothing left the caller on an open line, uncovered, until the
	// idle guard dropped it.
	armedByCallOpen
)

// filler plays the consumer's loop to the caller across the gap between the
// caller's last word and the backend's first reply frame (AATK-108). It is one
// state machine per call: armed by an event, started by a delay, stopped by
// the reply.
//
// THE STATE MACHINE HAS TWO DRIVERS, and which one does what is the whole
// design rather than an accident of where the code sits.
//
// ARMING is driven by observe, on the server-event drain goroutine, because
// the two events that arm it — speech_stopped, and the response.done that
// ended in a function call — reach this package nowhere else. (AATK-128 adds a
// third trigger, armedByCallOpen, from HandleStreamRealtime's own setup
// goroutine before either loop is running; it is not an event and so has
// nothing to do with this stream's losses.) That stream is
// lossy by construction (Bridge.publishEvent drops rather than parking Run's
// read loop), and it is lossy in the safe direction: a dropped arm costs the
// caller a loop that does not play, which is exactly the silence this option
// exists to replace and never worse than it.
//
// STOPPING is driven by carrierMediaSink, because it must not be lossy in any
// direction. A stop the relay misses puts loop frames on the wire underneath
// the reply. Media and Clear are called by Bridge.Run for every audio delta
// and every speech_started, unconditionally and without any drop in between,
// so the sink is where the guarantee lives.
//
// The split has one seam worth naming, since the type comment would otherwise
// read as though it had none. Bridge.Run publishes an event before it
// dispatches it, so an arm running on the drain goroutine can land AFTER the
// sink has already stopped the machine for a later event — leaving a pending
// start that the sink is not going to cancel, because the event that would
// have cancelled it has already gone by. It needs a wait whose only following
// audio is a single delta, or a speech_started immediately after a
// function-call response.done; a reply's continuing deltas each cancel it
// again. The cost when it does happen is bounded — the loop starts once and is
// stopped by the next delta or the next turn — which is why this is recorded
// rather than defended against with the cross-goroutine sequencing that
// closing it would take.
//
// There is a second window of the same shape and the same cost, on the other
// side: arm returns early when the loop is playing, deliberately leaving the
// generation alone, so an arm that lands between a frame's write failing and
// play's deferred endEpisode taking the lock is swallowed — endEpisode then
// clears playing under a generation the arm did not change, and no timer was
// ever set. Microseconds wide, and it needs a coincident carrier write
// failure. Like the seam above it costs one wait its loop, which is the
// silence this option replaces rather than anything worse, and it self-heals
// at the next arm.
//
// observe therefore says NOTHING about audio deltas or speech_started, even
// though it sees both. Acting on them there would be redundant with the sink
// at best, and at worst wrong: observe runs on the drain goroutine while Media
// runs on Bridge.Run's, so a stop issued from observe could win that race and
// clear the machine's playing flag before Media reads it — and Media reads it
// to decide whether the carrier is owed a clear. The loop would then stop with
// frames still queued at the carrier and no clear to discard them, which is
// the reply-behind-the-loop delay this option exists to hide. The only stop
// observe issues is on a turn-ending response.done, where no clear is owed.
//
// Not reachable at all when the consumer supplied no usable FillerConfig:
// newFiller returns nil and every method below tolerates a nil receiver, so an
// unconfigured call runs no timer and starts no goroutine.
type filler struct {
	// loop is the consumer's audio, held by reference rather than copied: it
	// is read-only here and can be a clip of some size. A consumer that
	// mutates it after passing it in changes what the caller hears, which is
	// why FillerConfig's doc says not to.
	loop  []byte
	delay time.Duration

	// sink is set once by attach, before any goroutine that could read it
	// exists. The two point at each other — the sink stops the filler, the
	// filler writes through the sink — so one of them has to be completed
	// after construction; see the attach call in HandleStreamRealtime.
	sink *carrierMediaSink

	// ctx ends when the call does, and cancel is what shutdown pulls. It
	// bounds the play goroutine's wait for the carrier write slot: without
	// it, a wedged carrier write would strand that goroutine past the end of
	// the call, since Media and Clear hold the slot on the call's own
	// unbounded context.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// gen invalidates work scheduled by a state the machine has since left.
	// Every arm, disarm and stop bumps it; a pending start and a running play
	// goroutine each carry the gen they were created under and give up the
	// moment it no longer matches. It is what makes a cancelled start
	// genuinely cancelled even though time.Timer.Stop cannot un-fire a timer
	// that is already running its function.
	gen uint64
	// timer is the pending start, nil when nothing is pending.
	timer *time.Timer
	// playing is true between the delay elapsing and the loop being stopped.
	// It is what Media reads to decide whether the carrier is owed a clear.
	playing bool
	// off is the read position in loop, in bytes. It survives across frames
	// within one episode and resets at the start of each, so the wrap is
	// seamless and the loop always begins where the consumer's clip does.
	off int
	// stopped latches at the end of the call: nothing arms or plays after it.
	stopped bool
}

// newFiller builds the state machine for one call, or nil when the consumer
// asked for nothing usable. A zero Delay ("never start") and an empty Loop
// ("nothing to play") are both inert, and inert means nil here rather than a
// live object that declines to act — that is what makes an unconfigured call
// byte-identical to one from before this option existed, with no timer armed
// and no goroutine started.
func newFiller(ctx context.Context, cfg FillerConfig) *filler {
	if cfg.Delay <= 0 || len(cfg.Loop) == 0 {
		return nil
	}
	c, cancel := context.WithCancel(ctx)
	return &filler{loop: cfg.Loop, delay: cfg.Delay, ctx: c, cancel: cancel}
}

// attach completes construction by handing the filler the sink it writes
// through. Called once, from the call's own setup goroutine, before the bridge
// is running and therefore before anything can observe a half-built filler.
func (f *filler) attach(s *carrierMediaSink) {
	if f == nil {
		return
	}
	f.sink = s
}

// observe drives the arming half of the machine from one server event. Called
// for every event the bridge reads, on the drain goroutine — see the two
// drivers described on the type.
func (f *filler) observe(ev ServerEvent) {
	if f == nil {
		return
	}
	switch ev.Type {
	case realtime.EventSpeechStopped:
		// The caller has finished a sentence. Everything from here to the
		// reply's first frame is the wait this option exists to fill.
		f.arm(armedByTurn)
	case realtime.EventResponseDone:
		// A response that ended in a function call is not the reply: it is
		// the start of a second wait, the tool round trip plus the second
		// LLM leg, which the caller hears as one continuous silence with the
		// first. Any other response.done ends the turn, so the loop stops.
		if realtime.ResponseEndedInFunctionCall(ev.Raw) {
			f.arm(armedByTurn)
		} else {
			f.stop()
		}
	}
	// Audio deltas and speech_started are deliberately absent: see the type
	// comment. Both reach carrierMediaSink unconditionally, and the sink is
	// what acts on them.
}

// arm starts the wait before the loop may play, and — this is the fix for the
// "reset on a tool call" defect — leaves an already-running wait alone rather
// than restarting it. A loop already PLAYING keeps playing; a countdown already
// PENDING keeps counting from its first arm. Re-arming (the function-call
// response.done announcing the tool round trip, or a second speech_stopped when
// a noisy room makes the VAD reopen the turn) means the wait CONTINUES, never
// that it begins again — restarting a pending countdown was what pushed the
// loop's start a whole Delay later than the caller's silence began.
//
// by names which of the two waits this is. It rides the pending start's own
// closure, exactly as gen does, so a re-arm that finds a countdown already
// pending leaves the original label alone along with the original countdown.
func (f *filler) arm(by armTrigger) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	if f.timer != nil {
		// A countdown is already pending, so the wait is already being timed;
		// re-arming must not push its start back. start nils f.timer the moment
		// it fires, so a non-nil timer here means genuinely still-pending, never
		// a stale fired one. (Playing is handled by the guard below, where the
		// timer is already nil.)
		return
	}
	if f.playing {
		// Already playing, so there is nothing to schedule and — this is the
		// part that has to come first — nothing to invalidate. gen is the
		// token the running play goroutine carries; bumping it here would
		// make its next frame decline and the goroutine return, while playing
		// stayed true and no timer was armed. The machine would be dead: no
		// goroutine, no pending start, and every later arm returning at this
		// same guard. That is precisely the case the re-arm exists for — the
		// function call announced seconds into a wait the loop is already
		// filling — so it would go silent for exactly the longest gap this
		// ticket measured.
		//
		// Any timer still held here has already fired — start is what set
		// playing — so leaving it alone costs nothing. start's own playing
		// check is defensive rather than load-bearing: AfterFunc fires once,
		// and this early return neither bumps gen nor replaces the timer, so
		// no second start carrying a live generation exists to be caught by
		// it.
		return
	}
	// After the guard above, never before it: cancelPendingLocked bumps the
	// generation, and bumping it while a loop is playing is the round-1 defect
	// this function's early return exists to prevent.
	f.cancelPendingLocked()
	gen := f.gen
	f.timer = time.AfterFunc(f.delay, func() { f.start(gen, by) })
}

// stop ends the loop, whether it was pending or playing, and reports whether
// it was PLAYING — which is to say whether frames are queued at the carrier
// that the caller is about to be owed a clear for. The clear itself is the
// caller's to send: carrierMediaSink.Media sends one, and Clear is already
// sending its own.
func (f *filler) stop() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.playing
	f.playing = false
	f.cancelPendingLocked()
	return was
}

// shutdown latches the machine off at the end of the call and releases the
// play goroutine, which may be parked waiting for the carrier's write slot.
func (f *filler) shutdown() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.stopped = true
	f.playing = false
	f.cancelPendingLocked()
	f.mu.Unlock()
	f.cancel()
}

// cancelPendingLocked drops any pending start and invalidates whatever the
// current generation had scheduled. Called with f.mu held.
func (f *filler) cancelPendingLocked() {
	f.gen++
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
}

// start is the delay elapsing: the backend has said nothing for Delay, so the
// loop begins. gen is the generation the pending start was created under; a
// mismatch means the machine moved on while the timer was already running its
// function, which Stop cannot undo.
func (f *filler) start(gen uint64, by armTrigger) {
	f.mu.Lock()
	if f.stopped || f.gen != gen || f.playing {
		f.mu.Unlock()
		return
	}
	// This timer has fired (start IS its function), so clear the handle: arm
	// distinguishes a genuinely-pending countdown (f.timer != nil) from a
	// playing or idle machine by it, and a stale fired timer left here would
	// make the next arm mistake "idle" for "pending" and never start the loop.
	f.timer = nil
	f.playing = true
	f.off = 0
	f.mu.Unlock()

	if by == armedByCallOpen {
		// Once per call, and only when the condition actually happened: the
		// backend completed its handshake and then produced no audio for the
		// whole of Delay, with the caller never having spoken. Named so a call
		// that sounded dead at the open is greppable afterwards — and
		// deliberately NOT under the "filler audio:" prefix play's write-error
		// line carries, which three tests in this package grep for as the
		// signature of a frame that failed to reach the carrier.
		log.Printf("twilio: realtime: silent backend at call open: playing the filler loop")
	}

	go f.play(gen)
}

// play writes the loop to the carrier, one frame per MuLawFrameMS, until the
// machine leaves this generation or the call ends.
//
// The pacing is not a nicety. Twilio buffers outbound media, so a loop written
// as fast as the socket accepts it would sit in that buffer ahead of the real
// reply and delay it by the buffered length — and it would still do so after
// the clear, because the clear cannot un-send what the carrier has already
// begun playing. One frame per frame is what keeps the switch to the reply
// inside a single frame's time.
func (f *filler) play(gen uint64) {
	// However this goroutine leaves, the episode leaves with it. That is not
	// tidiness: playing is what arm reads to decide there is already a loop
	// running, and what Media reads to decide the carrier is owed a clear. A
	// goroutine that returned with playing still set would leave a machine
	// with nothing running, nothing scheduled, and every later arm bailing out
	// at its "already playing" guard — dead for the rest of the call — while
	// the next reply sent a clear that discarded audio the carrier had every
	// right to play. Most declines reach this already stopped — the three
	// inside writeFrame's own closure do — and endEpisode's generation check
	// is what makes those a no-op. One does not: fillerMedia declines when
	// EncodeMediaB64 fails, on a machine that is still playing under a
	// matching generation, and there endEpisode acts. That is the wanted
	// answer, since nothing else would end the episode, but the two cases are
	// worth telling apart rather than describing as one.
	defer f.endEpisode(gen)

	ticker := time.NewTicker(telephony.MuLawFrameMS * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C:
			written, err := f.writeFrame(gen)
			if err != nil {
				// The carrier write failed. Stop adding to the damage and
				// leave the ending to whichever path observes it — but do not
				// assume there is one: a filler frame fails precisely while
				// the backend is SILENT, which is when no reply frame is
				// coming to report anything. Hence the defer above.
				//
				// A cancelled context is NOT that failure, and is not logged.
				// It means this machine's own context ended — shutdown, or the
				// call's context — while the frame was queueing for the write
				// slot, which is the call ending normally. The select above
				// catches that first whenever it wins the race; this catches
				// the tick that got in ahead of it. AATK-128 made the race
				// common rather than rare: a loop now plays on every call whose
				// backend is quiet at the open, so at teardown there is usually
				// a play goroutine to lose it, and the line it wrote is
				// indistinguishable from a real carrier failure.
				if !errors.Is(err, context.Canceled) {
					log.Printf("twilio: realtime: filler audio: %v", err)
				}
				return
			}
			if !written {
				return
			}
		}
	}
}

// endEpisode marks the loop stopped, but only if the machine is still in the
// generation the caller was playing under. The guard is what makes this safe
// to defer unconditionally: a play goroutine that exits because stop() already
// ran finds the generation moved on and changes nothing, so it cannot stomp a
// wait that has since been re-armed or a loop that has since restarted.
//
// The bump is belt-and-braces rather than load-bearing, and is recorded as
// such so it is not later mistaken for the guard: at the only point this
// function acts, arm cannot have left a pending timer behind — it returns
// early while playing is set, without arming one. Deleting the bump leaves
// every test green.
func (f *filler) endEpisode(gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gen != gen {
		return
	}
	f.playing = false
	f.gen++
}

// writeFrame writes one frame of the loop, reporting whether it wrote. false
// with a nil error means play should return. Usually that is because the
// machine has left this generation — the reply arrived, the caller spoke, or
// the call ended — but not always: fillerMedia also declines when its encode
// fails, on a machine still in this generation, which is the case play's own
// defer is written around.
//
// The generation check runs INSIDE the carrier's write slot, which is the
// property the whole stop path rests on. carrierMediaSink.Media stops the
// filler and then sends its clear; the stop takes only f.mu, but the clear
// takes the slot, and a frame whose decision to write was taken outside the
// slot could be admitted after that clear and land underneath the reply.
// Deciding inside it means the last word belongs to whichever of the two got
// there first, and either order is correct: a frame ahead of the clear is
// discarded by it, and a frame behind it finds playing already false and is
// never written at all.
func (f *filler) writeFrame(gen uint64) (bool, error) {
	return f.sink.fillerMedia(f.ctx, func() (string, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stopped || f.gen != gen || !f.playing {
			return "", false
		}
		frame := f.frameLocked()
		f.off = (f.off + len(frame)) % len(f.loop)
		return base64.StdEncoding.EncodeToString(frame), true
	})
}

// frameLocked reads one MuLawFrameMS frame from the loop at the current
// offset, wrapping around the end.
//
// The loop is a ring of bytes rather than a list of frames, and deliberately:
// a consumer's clip is whatever length it is, so a frame-list reading would
// have to either pad the last frame with silence — a click, once per pass,
// which is exactly the artefact a caller notices — or refuse clips that are
// not a whole number of frames. Reading across the wrap costs one copy per
// frame and has neither problem.
//
// Called with f.mu held.
func (f *filler) frameLocked() []byte {
	frame := make([]byte, 0, defaultFrameBytes)
	off := f.off
	for len(frame) < defaultFrameBytes {
		i := off % len(f.loop)
		take := min(defaultFrameBytes-len(frame), len(f.loop)-i)
		frame = append(frame, f.loop[i:i+take]...)
		off += take
	}
	return frame
}
