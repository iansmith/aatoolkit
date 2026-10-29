package twilio

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// realtimePlayoutMarkPrefix begins the name of the mark the engine writes after
// each agent turn when WithCallerSilenceTimeout is set (AATK-163), so it can
// tell when that turn's audio has finished playing to the caller. The call's
// stream SID and a per-call sequence number complete it.
//
// Per turn rather than per call, because two turns' marks can be in flight at
// once and the wire carries nothing but the name: one name for both would
// collapse them, which is the re-arm WithMarkRequestChan warns a consumer
// about. The stream SID is there for the reason realtimeFarewellMarkPrefix
// gives — a consumer's own marks travel the same wire, and this is the one
// string on the call a consumer's mark name cannot accidentally be.
const realtimePlayoutMarkPrefix = "aatoolkit-playout-"

// callerSilence is the caller-silence bound for one call (AATK-163): how long
// the caller may say nothing once the agent's audio has finished playing to
// them. One object per call, built by HandleStreamRealtime, nil when the
// option is unset or not positive.
//
// WHAT STARTS, STOPS AND RESETS THE COUNT.
//
//   - START: the carrier echoing a playout mark, or that mark's bound elapsing
//     without an echo (a carrier that ignores marks must not switch the bound
//     off, so a timed-out mark counts as played). The mark is requested when
//     the drain sees a response.done that does not end in a function call
//     while the caller is not mid-utterance, and it is written only if agent
//     audio has reached the carrier since the caller last spoke. Its echo
//     starts the count only if nothing has happened since it was written —
//     no caller speech and no further agent audio — and no reply is pending.
//   - STOP: the caller starting to speak (the barge-in Clear Bridge.Run sends
//     for every speech_started); agent audio written to the carrier; a
//     response.done that ends in a function call, which leaves the reply
//     pending until the next response.done that does not.
//   - RESET: none mid-count. Each echo that starts the count grants a whole
//     bound again, so every agent turn that plays restarts it (Ian's decision,
//     2026-10-28).
//
// It is NOT running at call open. The first start is the first playout echo,
// so a call where the agent never speaks is bounded only by WithIdleTimeout
// or the context. The engine sets no default; the consumer picks the value.
//
// THE COUNT HAS TWO DRIVERS, the filler's split inverted. The stops that must
// not be lost come from carrierMediaSink, which Bridge.Run calls for every
// speech_started and every audio delta with no drop in between: callerSpoke
// from Clear, agentAudio from inside Media's write slot. A stop missed there
// would hang up on a talking caller. The start, and the function-call stop,
// come from observe on the server-event drain, which is lossy by
// construction. Every loss there only fails to start the count, with one
// exception recorded below.
//
// Two counters keep the drivers honest with each other. starts counts the
// sink's speech starts and heard the drain's; a mark request carries heard,
// and the claim inside the write slot refuses it unless starts still agrees,
// so a speech start the drain has not yet seen voids the mark. gen numbers
// every caller speech and every agent frame; a mark captures it inside the
// write slot, so its echo knows whether anything followed it on the wire.
//
// FAIL-OPEN CASES, where the call stays up bounded only by WithIdleTimeout: a
// speech_started dropped by Bridge.publishEvent leaves heard behind starts for
// the rest of the call; a dropped speech_stopped skips marks until the next
// caller turn; a dropped turn-ending response.done skips that turn; a caller
// turn the backend never answers with audio leaves the count stopped; and an
// echo that overtakes the arming of the very mark it answers is unmatched.
// The unsafe residual: a function-call response.done dropped while the count
// runs lets it run through the tool round trip.
//
// LINE NOISE that the backend's VAD hears as speech produces speech_started,
// which stops this count exactly as it resets the idle guard. This bound does
// not end such a call.
//
// LOCK ORDER: writeSem → markTracker.mu → callerSilence.mu, and writeSem →
// callerSilence.mu. mu is a leaf: nothing here calls into the tracker or the
// sink while holding it.
type callerSilence struct {
	after time.Duration

	// dueCh tells the select loop a playout mark is wanted, and firedCh that
	// the count ran out. One slot each, sent without blocking, so a burst of
	// signals coalesces into one and no sender ever waits.
	dueCh   chan struct{}
	firedCh chan struct{}

	mu sync.Mutex
	// starts counts callerSpoke calls (the sink's view of speech starts) and
	// heard the speech_started events observe saw (the drain's).
	starts, heard uint64
	// wantHeard is heard as of the outstanding mark request.
	wantHeard uint64
	// gen is bumped by every caller speech and every agent frame. A claimed
	// mark carries it, and its echo starts the count only if it still matches.
	gen uint64
	// seq numbers the playout marks, so each turn's mark has its own name.
	seq uint64
	// speaking is the drain's latch: set by speech_started, cleared by
	// speech_stopped.
	speaking bool
	// audio is set by agent audio and cleared by caller speech and by a claim:
	// whether there is agent audio on the wire that no playout mark follows.
	audio bool
	// pending is set by a response.done ending in a function call and cleared
	// by the next one that does not.
	pending bool
	// want is the outstanding mark request.
	want bool
	// running is set while the countdown's timer is armed, and due once it has
	// fired and the select loop has not yet acted on it.
	running, due bool
	// stopped latches when the call ends, or when end decides to end it.
	stopped bool
	timer   *time.Timer
}

// newCallerSilence builds the bound for one call, or nil when after is zero or
// negative. nil, not an object that declines to act, is what keeps a call
// without the option byte-identical to one from before it existed: no timer,
// no mark, and two select cases on nil channels that never fire.
func newCallerSilence(after time.Duration) *callerSilence {
	if after <= 0 {
		return nil
	}
	return &callerSilence{
		after:   after,
		dueCh:   make(chan struct{}, 1),
		firedCh: make(chan struct{}, 1),
	}
}

// signalOnce sends on a one-slot channel without blocking; a signal already
// pending is the same signal.
func signalOnce(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// callerSpoke records that the caller started speaking. Called only by
// carrierMediaSink.Clear, which Bridge.Run calls once for every speech_started.
func (s *callerSilence) callerSpoke() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	s.audio = false
	s.gen++
	s.stopTimerLocked()
}

// agentAudio records that a frame of the backend's audio reached the carrier.
// Called inside the write slot, as the afterWrite of that frame, so it is
// ordered against a playout mark's claim exactly as the two are on the wire.
func (s *callerSilence) agentAudio() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audio = true
	s.gen++
	s.stopTimerLocked()
}

// observe drives the drain's half from one server event. It never blocks.
func (s *callerSilence) observe(ev ServerEvent) {
	if s == nil {
		return
	}
	switch ev.Type {
	case realtime.EventSpeechStarted:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.heard++
		s.speaking = true
	case realtime.EventSpeechStopped:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.speaking = false
	case realtime.EventResponseDone:
		toolCall := realtime.ResponseEndedInFunctionCall(ev.Raw)
		s.mu.Lock()
		defer s.mu.Unlock()
		if toolCall {
			// The reply is still coming, after the tool round trip.
			s.pending = true
			s.want = false
			s.stopTimerLocked()
			return
		}
		s.pending = false
		if s.speaking {
			// The caller holds the turn: this response was cut short by
			// them, and whatever it played is not what they are answering.
			return
		}
		s.want = true
		s.wantHeard = s.heard
		signalOnce(s.dueCh)
	}
}

// takeRequest consumes the outstanding mark request, returning the drain's
// speech count as of the request.
func (s *callerSilence) takeRequest() (tag uint64, ok bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.want || s.stopped {
		return 0, false
	}
	s.want = false
	return s.wantHeard, true
}

// nextMarkName is the name of the next playout mark on the call.
func (s *callerSilence) nextMarkName(streamSID string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return realtimePlayoutMarkPrefix + streamSID + "-" + strconv.FormatUint(s.seq, 10)
}

// claim decides, inside the write slot, whether a playout mark requested with
// tag may be written now, and returns the generation its echo must match. It
// refuses when the sink has counted a speech start the drain had not seen when
// it asked, or when no agent audio has reached the carrier since the caller
// last spoke or since the last mark.
func (s *callerSilence) claim(tag uint64) (gen uint64, ok bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.starts != tag || !s.audio {
		return 0, false
	}
	s.audio = false
	return s.gen, true
}

// played is a playout mark's resolution — its echo, or its bound elapsing —
// reported by the markTracker under its own lock. It starts the count only if
// nothing has happened since the mark was written and no reply is pending.
func (s *callerSilence) played(gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || gen != s.gen || s.pending || s.running {
		return
	}
	s.running = true
	s.due = false
	s.timer = time.AfterFunc(s.after, func() { s.fire(gen) })
}

// fire is the count running out. gen is the generation that started it; a
// mismatch means caller speech or agent audio overtook it while the timer was
// already running its function, which Stop cannot undo.
func (s *callerSilence) fire(gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || gen != s.gen || !s.running {
		return
	}
	s.running = false
	s.timer = nil
	s.due = true
	signalOnce(s.firedCh)
}

// end decides, on the select loop, that the call ends on this bound, and
// latches the bound off. It re-checks under the lock because caller speech
// can land between fire's signal and the select loop acting on it, and that
// caller is no longer silent.
func (s *callerSilence) end() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || !s.due {
		return false
	}
	s.stopped = true
	s.stopTimerLocked()
	return true
}

// stop latches the bound off at the end of the call.
func (s *callerSilence) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.stopTimerLocked()
}

// stopTimerLocked cancels the countdown, pending or fired-but-unacted. Called
// with s.mu held.
func (s *callerSilence) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.running = false
	s.due = false
}

// markDue is the channel that signals a playout mark is wanted, or nil.
func (s *callerSilence) markDue() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.dueCh
}

// expired is the channel that signals the count ran out, or nil.
func (s *callerSilence) expired() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.firedCh
}

// handlePlayoutMark writes the playout mark the drain asked for, from
// HandleStreamRealtime's select loop and through the sink's write slot, for
// the reason handleMarkRequest gives: a mark is a position on the wire, and
// only the slot orders it after the audio already written.
//
// Bounded, and fatal to the call when it fails, exactly as a consumer's mark
// is: the write runs on the loop that observes every way the call can end.
// A request the claim refuses writes nothing and is not an error.
func handlePlayoutMark(ctx context.Context, sink *carrierMediaSink, s *callerSilence) error {
	tag, ok := s.takeRequest()
	if !ok {
		return nil
	}
	name := s.nextMarkName(sink.streamSID)
	return boundedSend(ctx, "playout mark", func(c context.Context) error {
		return sink.playoutMark(c, name, tag)
	})
}
