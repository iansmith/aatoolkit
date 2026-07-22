package telephony

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// TurnSink receives turn-taking boundaries dispatched from a session's VAD
// events. OnSpeechStart fires when the caller starts talking (SOP-95 wires
// this as a no-op stub — barge-in behavior beyond that is out of scope);
// OnEndOfUtterance fires when the caller's utterance is judged complete.
// TurnTrigger names which of completeTurn's seven call sites (state.go, six
// distinct trigger values — silence-turn-end covers two of the seven: the
// immediate and the deferred-while-a-pass-was-in-flight paths) ended a turn
// (SOP-162 DoD: "trigger type" in every TurnSink.OnTurnComplete
// log line).
type TurnTrigger string

const (
	TriggerStopword       TurnTrigger = "stopword"
	TriggerSilenceTurnEnd TurnTrigger = "silence-turn-end"
	TriggerIdleTimeout    TurnTrigger = "idle-timeout"
	TriggerUtteranceCap   TurnTrigger = "utterance-cap"
	TriggerTurnCap        TurnTrigger = "turn-cap"
	TriggerCallEnd        TurnTrigger = "call-end"
)

// OnTurnComplete delivers the fused text of a completed turn (SOP-150):
// multiple utterances joined with a single space, each part trimmed, plus
// the trigger that ended the turn (SOP-162). Implementations must return
// promptly: they are called synchronously from the session's single
// transition-table dispatch loop (state.go's handleSpeechOnset/
// dispatchFullPass) and must not block it.
type TurnSink interface {
	OnSpeechStart()
	OnEndOfUtterance()
	OnTurnComplete(text string, trigger TurnTrigger)
}

// logTurnSink is the default TurnSink: it just logs each boundary. Useful as
// a safe default before a real consumer (the Respond pipeline, out of scope
// for this ticket) is wired in.
type logTurnSink struct {
	callSID string
}

func (l logTurnSink) OnSpeechStart() { log.Printf("telephony: session %s: speech start", l.callSID) }
func (l logTurnSink) OnEndOfUtterance() {
	log.Printf("telephony: session %s: end of utterance", l.callSID)
}
func (l logTurnSink) OnTurnComplete(text string, trigger TurnTrigger) {
	log.Printf("telephony: session %s: turn complete (%s): %q", l.callSID, trigger, text)
}

// vadDetector runs voice-activity inference on one fixed-size window of PCM
// samples, returning the probability in [0,1] that the window contains speech.
// The concrete Silero-backed detector runs out-of-process over HTTP
// (silero_http.go); the VAD goroutine and its state machine are written against
// this interface so they stay testable with a fake detector. ctx bounds a
// single inference — the HTTP detector derives a per-inference deadline from it
// (SOP-147); a fake may ignore it.
type vadDetector interface {
	Detect(ctx context.Context, window []float32) (float32, error)
	Reset()
}

// VADDetector is an exported alias for vadDetector — identical type, just
// spellable from outside the package. WithVADFactory's parameter needs an
// exported name so external test packages (telephony_test) can supply a
// custom factory without this package exposing detector construction.
type VADDetector = vadDetector

// VADConfig is an exported alias for vadConfig, so the audio tap (SOP-152, in
// package twilio) can record the config this process runs. Same need and same
// shape as the VADDetector alias above: name the type outward without handing
// out construction. An alias rather than a parallel struct keeps one definition
// of the fields.
type VADConfig = vadConfig

// DefaultVADConfig returns the VAD config every live call runs. It is
// process-wide for production traffic: no live call passes WithVADConfig, so
// vadCfg is filled from defaultVADConfig() via withDefaults() for every real
// session. The tap records it so a later replay (SOP-153) knows which
// thresholds produced a recording's live log -- defaultVADConfig() is a
// moving target (f5cad49 took EndSilenceMS 700 -> 1050) and the value cannot
// be recovered after the fact. SOP-153's replay harness is the one caller
// that does override per-session, via WithVADConfig (session.go), so it can
// drive `--end-silence-ms` without touching this package-wide default.
func DefaultVADConfig() VADConfig {
	return defaultVADConfig()
}

// vadConfig tunes the VAD windowing and state machine.
type vadConfig struct {
	WindowSize       int     // samples per inference window (Silero v5 @8kHz = 256)
	SpeechThresh     float32 // prob >= this enters speech
	SilenceThresh    float32 // prob < this counts as silence (hysteresis; <= SpeechThresh)
	EndSilenceMS     int     // trailing silence duration that ends an utterance
	SampleRateHz     int     // audio sample rate (Twilio μ-law = 8000)
	TurnEndSilenceMS int     // trailing silence duration that ends a turn (~5s, SOP-154)
}

func defaultVADConfig() vadConfig {
	// EndSilenceMS 900 -> 700 (AATK-9): AATK-3 raised 700 -> 900 to kill a Whisper
	// phrase-break hallucination (D5, "...the plumber still hasn't called back" split
	// into "...has to do it." at 700), but that split was the AATK-8 cold-start VAD bug
	// -- Silero was fed a bare 256-sample window with no 64-sample context, so it ran
	// cold every frame and mis-placed the boundary. AATK-8 fixed the detector (feed
	// 320 = 64 context + 256 chunk); a 700/900/1050 re-sweep over the fix-clean set is
	// then content-identical, so 900 bought nothing. Reverted to 700 (SOP-154's value)
	// for lower turn-taking latency. Caveat: one voice, partial set -- AATK-5 re-sweeps
	// across the fuller re-collection before the tuning question closes.
	return vadConfig{WindowSize: 256, SpeechThresh: 0.5, SilenceThresh: 0.35, EndSilenceMS: 700, SampleRateHz: 8000, TurnEndSilenceMS: 5000}
}

// withDefaults fills any unset (non-positive) field from defaultVADConfig so a
// partial config can't silently break windowing or end-of-utterance timing —
// e.g. a zero SampleRateHz would make windowMS 0 and never end an utterance.
func (c vadConfig) withDefaults() vadConfig {
	d := defaultVADConfig()
	if c.WindowSize <= 0 {
		c.WindowSize = d.WindowSize
	}
	if c.SpeechThresh <= 0 {
		c.SpeechThresh = d.SpeechThresh
	}
	if c.SilenceThresh <= 0 {
		c.SilenceThresh = d.SilenceThresh
	}
	if c.EndSilenceMS <= 0 {
		c.EndSilenceMS = d.EndSilenceMS
	}
	if c.SampleRateHz <= 0 {
		c.SampleRateHz = d.SampleRateHz
	}
	if c.TurnEndSilenceMS <= 0 {
		c.TurnEndSilenceMS = d.TurnEndSilenceMS
	}
	return c
}

// windowMS is the wall-clock duration one inference window represents.
func (c vadConfig) windowMS() int {
	if c.SampleRateHz == 0 {
		return 0
	}
	return c.WindowSize * 1000 / c.SampleRateHz
}

// windowsToCross returns the number of consecutive windowMS-sized windows a
// running count needs to reach or exceed thresholdMS -- the shared
// ceil-to-window rounding rule every threshold comparison in
// vadMachine.step and windowsForMS uses, so they can't silently drift apart
// if the rounding rule ever changes.
func windowsToCross(thresholdMS, windowMS int) int {
	if windowMS <= 0 {
		return 0
	}
	return (thresholdMS + windowMS - 1) / windowMS
}

// windowsForMS is how many consecutive windows, at the production defaults,
// it takes to cross thresholdMS -- shared by EndSilenceWindows and
// TurnEndSilenceWindows so the two test seams below can't drift apart in how
// they derive a window count from a config threshold.
func windowsForMS(thresholdMS int) int {
	c := defaultVADConfig()
	return windowsToCross(thresholdMS, c.windowMS())
}

// EndSilenceWindows is how many consecutive silence windows the VAD needs to
// see before it declares end-of-utterance, at the production defaults.
//
// Test seam only. A test that needs to feed "enough silence for the caller to
// have finished" would otherwise hardcode the count -- EndSilenceMS divided by
// the window duration -- and that number rots silently the moment either value
// moves, failing as "end of utterance never fired" instead of pointing at the
// default that changed. Production code never needs this: vadMachine derives
// it from its own config.
func EndSilenceWindows() int {
	return windowsForMS(defaultVADConfig().EndSilenceMS)
}

// TurnEndSilenceWindows is how many consecutive silence windows the VAD
// needs to see, since the start of the current trailing-silence run, before
// it declares turn-end, at the production defaults.
//
// Test seam only, same rationale as EndSilenceWindows: a test that must feed
// "enough silence to end the turn" derives the count from config rather than
// hardcoding it.
func TurnEndSilenceWindows() int {
	return windowsForMS(defaultVADConfig().TurnEndSilenceMS)
}

// validateOrdering enforces the utterance-end << turn-end << idle ordering
// invariant (SOP-154 Observable behavior #4): EndSilenceMS < TurnEndSilenceMS
// < maxSilenceMS. A config that inverts any leg is a bug the config itself
// should reject, not a silently-broken timer race at runtime. Session.Start
// calls this against the resolved config and MaxSilenceMS; it is a method
// (rather than inlined at the call site) so a config that violates it can be
// exercised directly in a test, without a Session seam to inject one.
func (c vadConfig) validateOrdering(maxSilenceMS int) error {
	if c.EndSilenceMS >= c.TurnEndSilenceMS {
		return fmt.Errorf("EndSilenceMS (%d) >= TurnEndSilenceMS (%d); ordering invariant violated", c.EndSilenceMS, c.TurnEndSilenceMS)
	}
	if c.TurnEndSilenceMS >= maxSilenceMS {
		return fmt.Errorf("TurnEndSilenceMS (%d) >= MaxSilenceMS (%d); ordering invariant violated", c.TurnEndSilenceMS, maxSilenceMS)
	}
	return nil
}

// decodeMuLaw converts G.711 μ-law bytes (1 byte/sample) to normalized float32
// PCM in [-1, 1].
func decodeMuLaw(frame []byte) []float32 {
	out := make([]float32, len(frame))
	for i, b := range frame {
		out[i] = float32(muLawToLinear(b)) / 32768
	}
	return out
}

// muLawToLinear decodes one G.711 μ-law byte to a signed 16-bit PCM sample
// (standard Sun/G.711 reference algorithm; full-scale ≈ ±32124).
func muLawToLinear(u byte) int16 {
	const bias = 0x84
	u = ^u
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F
	sample := (int(mantissa)<<3 + bias) << exponent
	sample -= bias
	if sign != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// windower accumulates samples and yields fixed-size windows, retaining any
// remainder for the next push.
type windower struct {
	size int
	buf  []float32
}

func newWindower(size int) *windower { return &windower{size: size} }

// push appends samples and returns every complete window now available, in
// order. The unconsumed remainder is compacted to the front of buf so the
// backing array does not grow without bound over a long call.
func (w *windower) push(samples []float32) [][]float32 {
	w.buf = append(w.buf, samples...)
	var windows [][]float32
	consumed := 0
	for len(w.buf)-consumed >= w.size {
		win := make([]float32, w.size)
		copy(win, w.buf[consumed:consumed+w.size])
		windows = append(windows, win)
		consumed += w.size
	}
	if consumed > 0 {
		w.buf = append(w.buf[:0], w.buf[consumed:]...)
	}
	return windows
}

// vadMachine turns a stream of per-window speech probabilities into VADEvents,
// with hysteresis and an end-of-utterance hangover.
type vadMachine struct {
	cfg          vadConfig
	speaking     bool
	silenceCount int  // consecutive sub-SilenceThresh windows since speech last stopped -- never reset at end-of-utterance, so it also drives VADTurnEnd (SOP-154: one counter, not two independent clocks)
	voicedCount  int  // voiced windows accumulated since speech-start
	turnEndFired bool // VADTurnEnd fired for this silence run; suppressed until next speech onset
	windowIndex  int  // monotonic window counter since speech-start (0 at onset)
	streamWindow int  // monotonic window counter over the whole stream; never reset (audio-position clock)
}

func newVADMachine(cfg vadConfig) *vadMachine { return &vadMachine{cfg: cfg.withDefaults()} }

// windowMS is the wall-clock duration one inference window represents.
func (m *vadMachine) windowMS() int {
	return m.cfg.windowMS()
}

// step advances the machine by one window's speech probability, returning an
// event when a boundary is crossed.
func (m *vadMachine) step(prob float32) (VADEvent, bool) {
	// Count every window uniformly, before any branch, so streamWindow is the
	// stream-global audio-position clock regardless of which path this window
	// takes (onset, dead-zone, silence, or speech). windowIndex below stays
	// per-utterance and is managed inside the branches as before.
	m.streamWindow++
	if !m.speaking {
		if prob >= m.cfg.SpeechThresh {
			m.speaking = true
			m.silenceCount = 0
			m.voicedCount = 1
			m.turnEndFired = false
			m.windowIndex = 0
			return m.event(VADSpeech, prob), true
		}

		// Not speaking: trailing silence keeps accumulating on the same
		// counter that drove EndSilenceMS (SOP-154 Observable behavior #3) --
		// a dead-zone probability changes nothing here either (hysteresis),
		// matching the speaking branch below.
		if m.turnEndFired || prob >= m.cfg.SilenceThresh {
			return VADEvent{}, false
		}
		m.silenceCount++
		m.windowIndex++
		if m.silenceCount >= windowsToCross(m.cfg.TurnEndSilenceMS, m.windowMS()) {
			m.turnEndFired = true
			return m.event(VADTurnEnd, prob), true
		}
		return VADEvent{}, false
	}

	m.windowIndex++

	// Speaking. Clear speech (or a resume after a pause) cancels any pending
	// end-of-utterance; a dead-zone probability changes nothing (hysteresis).
	if prob >= m.cfg.SpeechThresh {
		m.silenceCount = 0
		m.voicedCount++
		return VADEvent{}, false
	}
	if prob >= m.cfg.SilenceThresh {
		return VADEvent{}, false
	}

	// Below the silence threshold: a silence window.
	m.silenceCount++

	// Check for end-of-utterance. silenceCount is NOT reset here: it keeps
	// counting the same trailing-silence run so a later VADTurnEnd measures
	// total silence since the caller stopped, not silence since EOU.
	if m.silenceCount >= windowsToCross(m.cfg.EndSilenceMS, m.windowMS()) {
		ev := m.event(VADEndOfUtterance, prob)
		m.speaking = false
		m.voicedCount = 0
		return ev, true
	}

	if m.silenceCount == 1 {
		return m.event(VADSilence, prob), true
	}
	return VADEvent{}, false
}

// event snapshots the machine's current state into a lossless VADEvent
// (charter R9). SessionID is left zero here — the vadService wrapper stamps
// it on relay (charter R10).
func (m *vadMachine) event(kind VADKind, prob float32) VADEvent {
	return VADEvent{
		Kind:              kind,
		Prob:              prob,
		VoicedCount:       m.voicedCount,
		SilenceCount:      m.silenceCount,
		WindowIndex:       m.windowIndex,
		StreamWindowIndex: m.streamWindow,
	}
}

// VADInput is the frames-in plane of a vadService (SOP-116 pattern): callers
// Send raw μ-law frames for the wrapped vadMachine to consume.
type VADInput = ServiceOutput[[]byte]

// VADOutput is the events-out plane of a vadService (SOP-116 pattern):
// callers Recv the VADEvents the wrapped vadMachine emits.
type VADOutput = ServiceInput[VADEvent]

// vadService wraps runVAD behind the VADInput/VADOutput service planes,
// stamping SessionID (fixed at construction, charter R10) onto every event
// it relays.
type vadService struct {
	In  VADInput
	Out VADOutput

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newVADService starts a runVAD goroutine behind buffered service channels
// and stamps sessionID onto every VADEvent it relays. Callers must call
// Close to stop the underlying goroutines.
//
// onWindow, if given (variadic so every existing call site compiles
// unchanged), is invoked exactly once per inference window processed,
// settling that window's fate one of two ways: emitted=false fires
// immediately after m.step() decides no event was produced (synchronous,
// runVAD's own goroutine); emitted=true and the real event fire only once
// that event has been placed onto out's channel -- i.e. delivery to
// whatever reads Out.Channel() (in production, Session.run's select) is
// guaranteed, not merely attempted. Either way, onWindow having fired for
// every window fed so far is a precise "nothing from this input is still
// in flight toward the sequencer" signal.
//
// This is SOP-153's replay harness's synchronization seam: Replay needs to
// know every VADEndOfUtterance a replayed recording will ever produce has
// actually reached the sequencer before it can safely conclude no more STT
// dispatches are coming and end the call, and a fixed sleep can't stand in
// for that signal (see internal/telephony/replay.go's vadProgressGate).
func newVADService(sessionID string, det vadDetector, cfg vadConfig, inDepth, outDepth int, onWindow ...func(ev VADEvent, emitted bool)) *vadService {
	ctx, cancel := context.WithCancel(context.Background())
	in := NewBufferedChan[[]byte](inDepth)
	raw := make(chan VADEvent, outDepth)
	out := NewBufferedChan[VADEvent](outDepth)

	notify := func(ev VADEvent, emitted bool) {
		for _, f := range onWindow {
			f(ev, emitted)
		}
	}

	svc := &vadService{In: in, Out: out, cancel: cancel}
	svc.wg.Add(2)
	go func() {
		defer svc.wg.Done()
		runVAD(ctx, in.ch, raw, det, cfg, func() { notify(VADEvent{}, false) })
	}()
	go func() {
		defer svc.wg.Done()
		for {
			select {
			case ev, ok := <-raw:
				if !ok {
					return
				}
				ev.SessionID = sessionID
				select {
				case out.ch <- ev:
					notify(ev, true)
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return svc
}

// vadCloseWaitBound caps how long Close waits for the service's goroutines
// to actually exit -- see Close's own comment for why this can't be an
// unconditional wait.
const vadCloseWaitBound = 5 * time.Second

// Close stops the service's goroutines and, best-effort, waits for both to
// actually exit (not merely be asked to via cancel) before returning -- so a
// caller that constructs a new vadService right after Close returns won't
// usually race the outgoing one's goroutines (e.g. SOP-153's replay
// harness, which starts and tears down a fresh Session, and therefore a
// fresh vadService, per recording in a tight loop within one process; a
// live call, one per Session for a process's whole lifetime, never
// exercised this path enough to surface it).
//
// The wait is bounded, not unconditional: VADDetector.Detect takes no
// context, so a detector that blocks on something Close cannot see or
// cancel (this package's own slowDetector test helper is exactly this, by
// design) would otherwise turn every Close into a permanent hang. Detect
// implementations that return promptly -- every real one, including
// production Silero -- exit well within the bound and Close waits for them
// as documented; a genuinely-blocked one just falls back to the pre-SOP-153
// behavior of not being waited for.
func (s *vadService) Close() {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(vadCloseWaitBound):
	}
}

// runVAD consumes μ-law frames from in, runs the detector once per window, and
// emits VADEvents on out. It resets the detector and returns when ctx is
// cancelled or in is closed.
//
// onNoEvent, if given, is called once per window that did NOT produce an
// event -- see newVADService's onWindow doc for why (SOP-153's replay
// synchronization seam).
func runVAD(ctx context.Context, in <-chan []byte, out chan<- VADEvent, det vadDetector, cfg vadConfig, onNoEvent ...func()) {
	cfg = cfg.withDefaults()
	w := newWindower(cfg.WindowSize)
	m := newVADMachine(cfg)
	defer det.Reset() // reset on every exit: ctx cancel, closed in, or cancel mid-send
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			for _, win := range w.push(decodeMuLaw(frame)) {
				prob, err := det.Detect(ctx, win)
				if err != nil {
					// Fail loud, don't silently drop (SOP-147): a detector error
					// means VAD can no longer classify audio, so end the VAD
					// goroutine with an attributable log rather than continuing to
					// feed the state machine no-event windows forever.
					log.Printf("telephony: VAD detector error, ending VAD: %v", err)
					return
				}
				ev, emit := m.step(prob)
				if !emit {
					for _, f := range onNoEvent {
						f()
					}
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}
