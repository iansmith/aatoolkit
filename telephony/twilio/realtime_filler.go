package twilio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/realtime"
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
// ended in a function call — reach this package nowhere else. That stream is
// lossy by construction (Bridge.publishEvent drops rather than parking Run's
// read loop), and it is lossy in the safe direction: a dropped arm costs the
// caller a loop that does not play, which is exactly the silence this option
// exists to replace and never worse than it.
//
// STOPPING a loop that is already playing is driven by carrierMediaSink,
// because it must not be lossy in any direction. A stop the relay misses puts
// loop frames on the wire underneath the reply. Media and Clear are called by
// Bridge.Run for every audio delta and every speech_started, unconditionally
// and without any drop in between, so the sink is where the guarantee lives.
// observe deliberately only DISARMS on those two events (see disarmPending):
// were it to stop a playing loop it could win the race against Media and
// swallow the clear that Media owes the carrier.
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
		f.arm()
	case realtime.EventResponseDone:
		// A response that ended in a function call is not the reply: it is
		// the start of a second wait, the tool round trip plus the second
		// LLM leg, which the caller hears as one continuous silence with the
		// first. Any other response.done ends the turn, so the loop stops.
		if responseEndedInFunctionCall(ev.Raw) {
			f.arm()
		} else {
			f.stop()
		}
	case realtime.EventAudioDelta, realtime.EventSpeechStarted:
		// Disarm only. Stopping a PLAYING loop on these two is the sink's
		// job, because the sink owns the clear that has to go with it.
		f.disarmPending()
	}
}

// arm starts, or restarts, the wait before the loop may play. A loop already
// playing keeps playing: re-arming mid-loop (the function-call case above)
// means the wait continues, not that it begins again.
func (f *filler) arm() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	f.gen++
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	if f.playing {
		return
	}
	gen := f.gen
	f.timer = time.AfterFunc(f.delay, func() { f.start(gen) })
}

// disarmPending cancels a start that has not happened yet and leaves a playing
// loop alone. See the type comment for why the two are separated.
func (f *filler) disarmPending() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelPendingLocked()
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
func (f *filler) start(gen uint64) {
	f.mu.Lock()
	if f.stopped || f.gen != gen || f.playing {
		f.mu.Unlock()
		return
	}
	f.playing = true
	f.off = 0
	f.mu.Unlock()

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
	ticker := time.NewTicker(telephony.MuLawFrameMS * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C:
			written, err := f.writeFrame(gen)
			if err != nil {
				// The carrier write failed. That ends the call by its own
				// path — the bridge's next write reports it — so this
				// goroutine only has to stop adding to the damage.
				log.Printf("twilio: realtime: filler audio: %v", err)
				return
			}
			if !written {
				return
			}
		}
	}
}

// writeFrame writes one frame of the loop, reporting whether it wrote. false
// with a nil error means the machine has left this generation — the reply
// arrived, the caller spoke, or the call ended — and play should return.
//
// The generation check runs INSIDE the carrier's write slot, which is the
// property the whole stop path rests on. carrierMediaSink.Media stops the
// filler and then sends its clear, both of which take that slot; a frame whose
// decision to write was taken outside it could be admitted after that clear
// and land underneath the reply. Deciding inside the slot means the last word
// belongs to whichever of the two got there first, and either order is
// correct: a frame ahead of the clear is discarded by it, and a frame behind
// it is never written at all.
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

// responseEndedInFunctionCall reports whether a response.done frame's output
// items include a function call — the case where the turn is not over and the
// caller's wait continues into the tool round trip.
//
// Read from the raw frame rather than from a modelled field because this
// package models only the subset of the protocol it acts on, and one bool
// about one event type is not a reason to grow a decoded shape for the
// response object. A frame that does not parse, or that carries no output
// items, reports false: the conservative answer is "the turn ended", which
// leaves the loop stopped rather than playing over whatever comes next.
func responseEndedInFunctionCall(raw json.RawMessage) bool {
	var probe struct {
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	for _, item := range probe.Response.Output {
		if item.Type == realtime.ItemTypeFunctionCall {
			return true
		}
	}
	return false
}
