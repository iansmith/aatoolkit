package telephony

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony/assets"
)

// ControlEvent is a Twilio control-plane signal (e.g. "stop") routed to a
// Session's controlIn field, decoupled from twilio.Frame to avoid an import
// cycle (internal/telephony/twilio already imports internal/telephony).
// Twilio-side adapters translate twilio.Frame into ControlEvent.
type ControlEvent struct {
	Kind     string
	MarkName string
	CallSID  string
}

// controlKindStop is the ControlEvent.Kind value for a Twilio "stop" signal.
const controlKindStop = "stop"

// controlKindMark is the ControlEvent.Kind value for a Twilio "mark" echo
// (SOP-125): Twilio echoes back a mark once it has finished playing the
// audio queued before it, which is how AwaitingMarkEcho learns the farewell
// clip actually played.
const controlKindMark = "mark"

// farewellMarkName is the mark name sent alongside the farewell clip, and
// the name expected back on its echo.
const farewellMarkName = "farewell"

// Timer names for the TimerFacility (Charter R2: one mechanism)
const (
	timerIdle      = "idle"
	timerMarkEcho  = "markEcho"
	timerUtterance = "utterance"
	timerSimTurn   = "simTurn"
	timerTurn      = "turn"
)

// sttDispatchDepth is sttDispatchCh's buffer size: at most one FullPass is
// ever pending per utterance, so a small fixed buffer is generous headroom
// without being a call-duration-scaled tuning knob.
const sttDispatchDepth = 4

// TwilioDataPlaneInput is the receive side of the channel a Session reads
// inbound media payloads from (SOP-120's demux data plane, adapted).
type TwilioDataPlaneInput = ServiceInput[[]byte]

// TwilioControlPlaneInput is the receive side of the channel a Session reads
// Twilio control-plane signals from (SOP-120's demux control plane, adapted).
type TwilioControlPlaneInput = ServiceInput[ControlEvent]

// ControlOutKind identifies which Twilio control-plane message a
// ControlOutMessage carries: EncodeMark or EncodeClear (SOP-125). EncodeStop
// is never sent here -- it's a client-side function (twilio/stream.go), not
// part of the server's outbound vocabulary; the server ends a call by
// closing the WebSocket instead.
type ControlOutKind string

const (
	ControlOutMark  ControlOutKind = "mark"
	ControlOutClear ControlOutKind = "clear"
)

// ControlOutMessage is the send-side counterpart to ControlEvent: a Twilio
// control-plane message a Session writes out via TwilioControlPlaneOutput.
type ControlOutMessage struct {
	Kind     ControlOutKind
	MarkName string
}

// TwilioDataPlaneOutput is the send side of the channel a Session writes
// outbound media frames to (SOP-116 ServiceOutput pattern). The concrete
// implementation (internal/telephony/twilio/output.go) encodes each payload
// via EncodeMedia and writes it to the WebSocket; this package only sees the
// generic interface to avoid an import cycle (twilio already imports
// telephony).
type TwilioDataPlaneOutput = ServiceOutput[[]byte]

// TwilioControlPlaneOutput is the send side of the channel a Session writes
// outbound control-plane messages to (mark/clear). The concrete
// implementation (internal/telephony/twilio/output.go) encodes each message
// via EncodeMark/EncodeClear and writes it to the WebSocket.
type TwilioControlPlaneOutput = ServiceOutput[ControlOutMessage]

// Session is the per-call coordination point. It owns the service-interface
// inputs that drive the pipeline, the call's identity and conversation
// history, and the cancel func that tears the call down. A single sequencer
// goroutine per session drives the entire pipeline via one select loop (see
// run) -- there is no separate VAD-event dispatcher goroutine; turn-taking
// dispatch happens inline from the transition table.
//
// A Session is driven by one owning goroutine: Start and Close are not safe
// to call concurrently with each other.
type Session struct {
	CallSID string
	History []Message

	vadFactory       func() (VADDetector, error)
	turnSink         TurnSink
	decisionRecorder DecisionRecorder
	turnEndPolicy    TurnEndPolicy
	vadCfg           vadConfig
	vadEventObserver func(ev VADEvent, emitted bool)

	// turnTranscripts accumulates FullPass STT transcript text for the
	// current turn (SOP-150). Distinct from turnBuf, which is raw audio.
	turnTranscripts []string

	// turnActive is true while a turn is in progress (from speech onset until
	// turn completion). Source of truth for "a turn is in progress."
	turnActive bool

	// turnEndPending is set when VADTurnEnd fires while a full pass is still
	// in flight (StateAwaitingFullResult): completing now would flush
	// turnTranscripts before that pass's result arrives, so completion is
	// deferred until handleSTTResult processes it (state.go).
	turnEndPending bool

	// dataIn, controlIn, sttOut, and sttIn are the service-interface inputs
	// driving the select loop's total (state, source) transition table. Any
	// of them may be nil (unwired) -- run()'s select treats a nil channel as
	// a case that never fires, so an unwired source is simply inert rather
	// than a crash.
	dataIn    TwilioDataPlaneInput
	controlIn TwilioControlPlaneInput
	sttOut    STTOutput

	// dataOut and ctlOut are where the SOP-125 termination flow writes the
	// farewell clip's audio frames and its mark, respectively. Both may be
	// nil (unwired), in which case sendClip/sendMarkAndArmEcho are no-ops
	// for that plane.
	dataOut TwilioDataPlaneOutput
	ctlOut  TwilioControlPlaneOutput

	// closeFunc tears down the underlying transport (e.g. the Twilio
	// WebSocket) once the session reaches Closed via the termination flow.
	// nil is a safe no-op -- Session itself is transport-agnostic.
	closeFunc func()

	// idleTimeoutMS overrides MaxSilenceMS for this session when > 0 (test
	// seam only -- MaxSilenceMS's real multi-second default is too slow to
	// wait out in a test).
	idleTimeoutMS int

	// utteranceTimeoutMS overrides MaxUtteranceMS for this session when > 0
	// (SOP-156). The 45s production default is impractical to wait out in a
	// test, and the operator sets a small value (e.g. 10s) for live testing.
	utteranceTimeoutMS int

	// turnTimeoutMS overrides MaxTurnMS for this session when > 0 (SOP-161).
	// The 60s production default is impractical to wait out in a test, and
	// the operator sets a small value for live testing.
	turnTimeoutMS int

	// simTurnMS is the duration for playing the thinking bed when sim-turn
	// is enabled (SOP-157). <= 0 means sim-turn is disabled.
	simTurnMS int

	// sttIn is where drainSTTDispatch sends full-pass STTRequests (SOP-124).
	// sttDispatchCh is dispatchSTT's non-blocking handoff to the dedicated
	// drain goroutine -- a single drainer preserves FIFO dispatch order,
	// which per-call goroutines racing on sttIn.Send cannot.
	sttIn         STTInput
	sttDispatchCh chan STTRequest
	sttReqID      int

	// turnBuf accumulates raw inbound μ-law frame bytes for the current
	// utterance (SOP-124 Observable behavior #1: the state machine owns this
	// buffer, not a service).
	turnBuf []byte

	// timerFacility manages all named timers (idle, utterance, markEcho).
	// Single mechanism per Charter R2.
	timerFacility *TimerFacility

	// clock is how this session's timers wait out their durations; nil means
	// the wall clock. See WithClock.
	clock func(time.Duration) <-chan time.Time

	// vad wraps the VAD goroutine behind the service-interface planes: In
	// accepts raw frames, Out emits VADEvents. Constructed in Start once the
	// detector factory resolves.
	vad *vadService

	// forwardCh is the session's own, fully-owned channel for handing frames
	// from run() to the VAD-forwarder goroutine. Because Session owns both
	// ends, run() can select-send against it without ever blocking (charter
	// R8) -- unlike vad.In, a blocking Send whose buffer may be full.
	forwardCh chan []byte

	// notImplLogged records which (state, source) pairs handleNotImplemented
	// has already reported this session, so a per-frame source reports the
	// gap once instead of once per frame. AwaitingMarkEcho receives inbound
	// media for the whole 2s farewell playout -- at 50 frames/sec that is
	// ~100 identical lines, which buries the control plane in the log.
	// Written only from run()'s single select loop (charter: one loop owns
	// every transition), so it needs no lock.
	notImplLogged map[notImplKey]bool

	stateMu    sync.Mutex
	state      SessionState
	closedOnce sync.Once
	closedCh   chan struct{}

	cancel  context.CancelFunc
	ctx     context.Context
	wg      sync.WaitGroup
	done    chan struct{}
	started bool
}

// SessionOption configures optional Session behavior at construction time.
type SessionOption func(*Session)

// WithVADFactory overrides how Start constructs the vadDetector used by this
// session's VAD goroutine. Production code doesn't need this — Start defaults
// to NewSileroDetector — but tests use it to inject a fake detector without
// touching the real ONNX model.
func WithVADFactory(f func() (VADDetector, error)) SessionOption {
	return func(s *Session) { s.vadFactory = f }
}

// WithTurnSink overrides the TurnSink that receives this session's VAD
// boundary events. Start defaults to a logging TurnSink when none is given.
func WithTurnSink(sink TurnSink) SessionOption {
	return func(s *Session) { s.turnSink = sink }
}

// WithDecisionRecorder wires the DecisionRecorder that receives one
// DecisionEvent per parameterized voice-input choice (M1: end-of-utterance).
// Unset, a session uses a no-op recorder (NewSession default) and records
// nothing. The session owns the recorder's lifecycle: Close flushes it.
func WithDecisionRecorder(r DecisionRecorder) SessionOption {
	return func(s *Session) { s.decisionRecorder = r }
}

// ProdTurnSink is the production TurnSink: it logs turn completions with
// structured logging (turn number, text, timestamp). Satisfies the TurnSink
// interface defined in vad.go.
type ProdTurnSink struct {
	CallSID string
	turnNum int
	turnMu  sync.Mutex
}

func (p *ProdTurnSink) OnSpeechStart() {
	// No-op in production; VAD internally tracks this.
}

func (p *ProdTurnSink) OnEndOfUtterance() {
	// No-op in production; VAD internally tracks this.
}

func (p *ProdTurnSink) OnTurnComplete(text string, trigger TurnTrigger) {
	p.turnMu.Lock()
	p.turnNum++
	turnNum := p.turnNum
	p.turnMu.Unlock()
	log.Printf("telephony: session %s: turn complete (turn=%d trigger=%s text=%q)", p.CallSID, turnNum, trigger, text)
}

// TurnEndPolicy decides whether a FullPass transcript closes the current
// turn. If IsEndOfTurn returns true the accumulated buffer is flushed
// (excluding the triggering utterance) and the stopword is consumed. A
// session with no policy injected still flushes on call end.
type TurnEndPolicy interface {
	IsEndOfTurn(transcript string) bool
}

// StopwordPolicy closes the turn when the normalized transcript equals
// exactly "done". Normalization: lowercase, trim whitespace, strip
// trailing punctuation (.!?,). Matching is exact after normalization.
type StopwordPolicy struct{}

func (StopwordPolicy) IsEndOfTurn(transcript string) bool {
	return normalizeTranscript(transcript) == "done"
}

func normalizeTranscript(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, ".!?,")
	return s
}

// WithTurnEndPolicy injects the policy that decides whether a FullPass
// transcript closes the current turn. Without a policy, turns are only
// flushed on call end.
func WithTurnEndPolicy(p TurnEndPolicy) SessionOption {
	return func(s *Session) { s.turnEndPolicy = p }
}

// WithVADConfig overrides this session's VAD config (e.g. EndSilenceMS).
// Unset (zero-value) fields are filled from defaultVADConfig() by
// Session.Start via withDefaults, same as an entirely omitted config.
//
// Production never calls this -- every live call runs defaultVADConfig(),
// which is why vad.go's DefaultVADConfig doc says the config is
// process-wide, not per-session. This option exists for SOP-153's replay
// harness: `probeset replay --end-silence-ms N` needs to drive one Session
// at a caller-chosen threshold per invocation without mutating the
// package-wide default (which would race concurrent replays and leak into
// any live call sharing the process).
func WithVADConfig(cfg VADConfig) SessionOption {
	return func(s *Session) { s.vadCfg = cfg }
}

// WithVADEventObserver registers f to be called once per inference window
// this session's VAD processes, settling that window's fate (see
// newVADService's onWindow param in vad.go): emitted=false fires as soon as
// the window is known to have produced no VADEvent; emitted=true and the
// real event fire only once that event is guaranteed-delivered onto this
// session's VAD output channel, i.e. once run()'s select loop is guaranteed
// to see it, not merely once it was computed.
//
// Production never calls this either. It exists alongside WithVADConfig for
// SOP-153's replay harness: Replay needs to know every window it fed has
// been fully accounted for -- including any VADEndOfUtterance it produced
// having actually reached the sequencer -- before it can safely conclude no
// more STT dispatches are coming and end the call. A real completion signal
// in place of a fixed sleep.
func WithVADEventObserver(f func(ev VADEvent, emitted bool)) SessionOption {
	return func(s *Session) { s.vadEventObserver = f }
}

// WithTwilioDataInput wires in the Twilio data-plane demux output (SOP-120)
// as this session's source of inbound media payloads.
func WithTwilioDataInput(in TwilioDataPlaneInput) SessionOption {
	return func(s *Session) { s.dataIn = in }
}

// WithTwilioControlInput wires in the Twilio control-plane demux output
// (SOP-120) as this session's source of control-plane signals.
func WithTwilioControlInput(in TwilioControlPlaneInput) SessionOption {
	return func(s *Session) { s.controlIn = in }
}

// WithSTTOutput wires in the STT service's result output as this session's
// source of recognition results.
func WithSTTOutput(out STTOutput) SessionOption {
	return func(s *Session) { s.sttOut = out }
}

// WithSTTInput wires in the STT service's request input as the destination
// for this session's dispatched full-pass STTRequests (SOP-124).
func WithSTTInput(in STTInput) SessionOption {
	return func(s *Session) { s.sttIn = in }
}

// WithTwilioDataOutput wires in the destination for the farewell clip's
// outbound audio frames (SOP-125).
func WithTwilioDataOutput(out TwilioDataPlaneOutput) SessionOption {
	return func(s *Session) { s.dataOut = out }
}

// WithTwilioControlOutput wires in the destination for the outbound mark
// sent alongside the farewell clip (SOP-125).
func WithTwilioControlOutput(out TwilioControlPlaneOutput) SessionOption {
	return func(s *Session) { s.ctlOut = out }
}

// WithCloseFunc overrides how the session tears down its underlying
// transport once Closed via the termination flow. Production wires this to
// the Twilio WebSocket's close; tests inject a fake to observe it was
// called without a real connection.
func WithCloseFunc(f func()) SessionOption {
	return func(s *Session) { s.closeFunc = f }
}

// WithClock replaces the passage of time for this session's timers (idle,
// utterance, markEcho) with after. nil, the default, means the wall clock.
//
// Test seam only, and the reason it exists: a timer-driven transition can
// otherwise only be observed by sleeping past its deadline. Such a test
// asserts on the scheduler rather than the code, and -- worse -- silently
// stops proving anything the moment the deadline it was meant to outlast
// grows past the sleep. With a fake clock the test fires the timer itself and
// the assertion is exact.
func WithClock(after func(time.Duration) <-chan time.Time) SessionOption {
	return func(s *Session) { s.clock = after }
}

// WithMaxSilenceMS overrides MaxSilenceMS for this session. Test seam only:
// MaxSilenceMS's real multi-second default is impractical to wait out in a
// test.
func WithMaxSilenceMS(ms int) SessionOption {
	return func(s *Session) { s.idleTimeoutMS = ms }
}

// WithMaxUtteranceMS overrides MaxUtteranceMS for this session (SOP-156).
// Test seam and the live-testing knob behind AATOOLKIT_MAX_UTTERANCE_MS.
func WithMaxUtteranceMS(ms int) SessionOption {
	return func(s *Session) { s.utteranceTimeoutMS = ms }
}

// WithMaxTurnMS overrides MaxTurnMS for this session (SOP-161). Test seam
// and the live-testing knob behind AATOOLKIT_MAX_TURN_MS.
func WithMaxTurnMS(ms int) SessionOption {
	return func(s *Session) { s.turnTimeoutMS = ms }
}

// WithSimTurnMS enables sim-turn bed playback (SOP-157) for the configured
// duration (ms). <= 0 disables it (production default).
func WithSimTurnMS(ms int) SessionOption {
	return func(s *Session) { s.simTurnMS = ms }
}

// NewSession builds a session for callSID with a cancel derived from ctx. It
// does not start the sequencer or the VAD goroutine — call Start for that.
func NewSession(ctx context.Context, callSID string, opts ...SessionOption) *Session {
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		CallSID:       callSID,
		forwardCh:     make(chan []byte, ComputeDepth(DataPlaneBufferMS, MuLawFrameMS)),
		sttDispatchCh: make(chan STTRequest, sttDispatchDepth),
		closedCh:      make(chan struct{}),
		state:         StateIdle,
		cancel:        cancel,
		ctx:           ctx,
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Default the recorder to a no-op so every call site can Record/Close
	// unconditionally (mirrors how the tap treats a nil *Tap as a no-op).
	if s.decisionRecorder == nil {
		s.decisionRecorder = noopRecorder{}
	}
	// Built after the options so WithClock can supply the passage of time.
	s.timerFacility = NewTimerFacilityWithClock(ctx, s.clock)
	return s
}

// Start spawns the sequencer and the VAD-forward goroutine for this session.
// It is idempotent: calling it again after the first success is a no-op
// returning nil.
//
// Start constructs the session's vadDetector via its vadFactory (defaulting
// to the production NewSileroDetector), failing hard: a factory error is
// returned and nothing is started — no goroutines are spawned, so a caller
// that gets an error can retry or abandon the session cleanly.
func (s *Session) Start() error {
	if s.started {
		return nil
	}

	factory := s.vadFactory
	if factory == nil {
		factory = NewSileroDetector
	}
	det, err := factory()
	if err != nil {
		return err
	}

	if s.turnSink == nil {
		s.turnSink = logTurnSink{callSID: s.CallSID}
	}
	cfg := s.vadCfg.withDefaults()
	// Validated against the production MaxSilenceMS constant, not the
	// session's idleTimeoutMS test-seam override (see WithMaxSilenceMS):
	// this is a config sanity check on the real 700/5000/15000 ordering the
	// ticket's design rests on, not a runtime assertion against every
	// possible test override -- several existing tests intentionally
	// compress idleTimeoutMS to fast-forward termination flows unrelated to
	// VAD timing, and there is no equivalent per-session override for
	// EndSilenceMS/TurnEndSilenceMS to keep such a session internally
	// consistent even if this checked idleTimeoutMS instead.
	if err := cfg.validateOrdering(MaxSilenceMS); err != nil {
		return fmt.Errorf("Session.Start: %w", err)
	}
	depth := ComputeDepth(DataPlaneBufferMS, MuLawFrameMS)
	if s.vadEventObserver != nil {
		s.vad = newVADService(s.CallSID, det, cfg, depth, depth, s.vadEventObserver)
	} else {
		s.vad = newVADService(s.CallSID, det, cfg, depth, depth)
	}
	s.vadCfg = cfg

	s.armIdleTimer()

	s.started = true
	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.run() }()
	go func() { defer s.wg.Done(); s.forwardToVAD() }()
	go func() { defer s.wg.Done(); s.drainSTTDispatch() }()
	go func() {
		s.wg.Wait()
		close(s.done)
	}()
	return nil
}

// run is the sequencer: a single select loop with one receive-case per
// service input this session was wired with (Twilio data plane, Twilio
// control plane, VAD events, STT results). Every input's payload is
// dispatched through the total (state, source) transition table (state.go),
// which returns the next state. the engine never performs a blocking send from
// this loop (charter R8): the only outbound handoff, to VAD, goes through
// forwardCh via a non-blocking select-send in handleDataFrame.
//
// A nil, unwired input's Channel() is nil, and a nil channel in a select
// case simply never fires — so an unwired source is inert rather than a
// crash. run() stops when the session's context is cancelled.
func (s *Session) run() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame, ok := <-dataChannel(s.dataIn):
			if !ok {
				return
			}
			s.dispatch(SourceTwilioData, frame)
		case cev, ok := <-controlChannel(s.controlIn):
			if !ok {
				return
			}
			s.dispatch(SourceTwilioControl, cev)
		case ev, ok := <-s.vad.Out.Channel():
			if !ok {
				return
			}
			s.dispatch(SourceVADEvent, ev)
		case res, ok := <-sttChannel(s.sttOut):
			if !ok {
				return
			}
			s.dispatch(SourceSTTResult, res)
		case completion := <-s.timerFacility.Completions():
			if !s.timerFacility.IsCurrent(completion) {
				continue
			}
			switch completion.Name {
			case timerIdle:
				s.dispatch(SourceIdleTimer, nil)
			case timerMarkEcho:
				s.dispatch(SourceMarkEchoTimer, nil)
			case timerUtterance:
				s.dispatch(SourceUtteranceTimer, nil)
			case timerSimTurn:
				s.dispatch(SourceSimTurnTimer, nil)
			case timerTurn:
				s.dispatch(SourceTurnTimer, nil)
			}
		}
	}
}

// dispatch looks up the transition handler for the session's current state
// and source, applies it, and stores the resulting next state.
func (s *Session) dispatch(source InputSource, payload any) {
	next := transitions[s.State()][source](s, transitionEvent{source: source, payload: payload})
	s.setState(next)
}

// dataChannel returns in's receive channel, or nil if in is unwired (nil).
// A nil channel disables its select case rather than panicking.
func dataChannel(in TwilioDataPlaneInput) <-chan []byte {
	if in == nil {
		return nil
	}
	return in.Channel()
}

// controlChannel returns in's receive channel, or nil if in is unwired.
func controlChannel(in TwilioControlPlaneInput) <-chan ControlEvent {
	if in == nil {
		return nil
	}
	return in.Channel()
}

// sttChannel returns out's receive channel, or nil if out is unwired.
func sttChannel(out STTOutput) <-chan STTResult {
	if out == nil {
		return nil
	}
	return out.Channel()
}

// armIdleTimer starts the idle timer at idleTimeoutMS (test seam) or
// MaxSilenceMS (production default). Armed at Start, cancelled at speech onset
// (the caller is no longer silent), and re-armed by dispatchFullPass when the
// utterance ends -- so it measures silence, never active speech (SOP-156).
func (s *Session) armIdleTimer() {
	ms := s.idleTimeoutMS
	if ms <= 0 {
		ms = MaxSilenceMS
	}
	s.timerFacility.Arm(s.ctx, timerIdle, time.Duration(ms)*time.Millisecond)
}

// cancelIdleTimer stops and clears the idle timer, if armed.
func (s *Session) cancelIdleTimer() {
	s.timerFacility.Cancel(timerIdle)
}

// armUtteranceTimer starts the max-utterance cap (SOP-156) at utteranceTimeoutMS
// (test seam) or MaxUtteranceMS (production default). Called on speech onset --
// which fires exactly once per utterance, since vadMachine only emits VADSpeech
// from a non-speaking state and stays speaking until end-of-utterance -- so this
// caps the whole utterance from its start, not "time since the last resume".
func (s *Session) armUtteranceTimer() {
	ms := s.utteranceTimeoutMS
	if ms <= 0 {
		ms = MaxUtteranceMS
	}
	s.timerFacility.Arm(s.ctx, timerUtterance, time.Duration(ms)*time.Millisecond)
}

// cancelUtteranceTimer stops and clears the utterance cap, if armed.
func (s *Session) cancelUtteranceTimer() {
	s.timerFacility.Cancel(timerUtterance)
}

// armTurnTimer starts the max-turn cap (SOP-161) at turnTimeoutMS (test
// seam) or MaxTurnMS (production default). Called at a turn's first speech
// onset (guarded by !turnActive in handleSpeechOnset), so it bounds the whole
// turn from its start, not any single utterance within it.
func (s *Session) armTurnTimer() {
	ms := s.turnTimeoutMS
	if ms <= 0 {
		ms = MaxTurnMS
	}
	s.timerFacility.Arm(s.ctx, timerTurn, time.Duration(ms)*time.Millisecond)
}

// cancelTurnTimer stops and clears the turn cap, if armed.
func (s *Session) cancelTurnTimer() {
	s.timerFacility.Cancel(timerTurn)
}

// armMarkEchoTimer starts the mark-echo timer, derived from the real
// farewell clip's playout duration (SOP-125 Observable behavior #4).
func (s *Session) armMarkEchoTimer(clip []byte) {
	s.timerFacility.Arm(s.ctx, timerMarkEcho, MarkEchoTimeout(clip))
}

// cancelMarkEchoTimer stops and clears the mark-echo timer, if armed.
func (s *Session) cancelMarkEchoTimer() {
	s.timerFacility.Cancel(timerMarkEcho)
}

// armSimTurnTimer starts the bed-playback timer for sim-turn (SOP-157).
func (s *Session) armSimTurnTimer() {
	if s.simTurnMS <= 0 {
		return
	}
	s.timerFacility.Arm(s.ctx, timerSimTurn, time.Duration(s.simTurnMS)*time.Millisecond)
}

// cancelSimTurnTimer stops and clears the sim-turn timer, if armed.
func (s *Session) cancelSimTurnTimer() {
	s.timerFacility.Cancel(timerSimTurn)
}

// farewellFrameBytes is one 20ms (MuLawFrameMS) frame's worth of μ-law audio
// (1 byte/sample at SampleRateHz) -- the same chunking Twilio itself uses for
// inbound media frames. Used to chunk both the farewell clip (sendClip) and
// the sim-turn thinking bed (sendBed).
const farewellFrameBytes = SampleRateHz * MuLawFrameMS / 1000

// sendClip writes a μ-law clip out on dataOut, one MuLawFrameMS-sized frame at
// a time. A nil dataOut (unwired -- e.g. in tests that don't care about this
// plane) is a no-op. Used by both terminal clips: the farewell (idle timeout)
// and the forced-stop (SOP-156 utterance cap). Called synchronously from a
// timer handler: a terminal, once-per-call flow, not the steady-state media
// path run() must never block in (charter R8), and dataOut's buffer is sized
// generously enough to absorb the whole clip without blocking on a draining
// consumer.
func (s *Session) sendClip(clip []byte) {
	if s.dataOut == nil {
		return
	}
	for i := 0; i < len(clip); i += farewellFrameBytes {
		end := i + farewellFrameBytes
		if end > len(clip) {
			end = len(clip)
		}
		if err := s.dataOut.Send(s.ctx, clip[i:end]); err != nil {
			log.Printf("telephony: session %s: WARN clip send failed: %v", s.CallSID, err)
			return
		}
	}
}

// sendMarkAndArmEcho sends the farewell mark on ctlOut (a nil ctlOut is a
// no-op) and arms the mark-echo timeout regardless, so AwaitingMarkEcho
// always has a bounded wait even when the control-plane output is unwired.
func (s *Session) sendMarkAndArmEcho(clip []byte) {
	if s.ctlOut != nil {
		msg := ControlOutMessage{Kind: ControlOutMark, MarkName: farewellMarkName}
		if err := s.ctlOut.Send(s.ctx, msg); err != nil {
			log.Printf("telephony: session %s: WARN mark send failed: %v", s.CallSID, err)
		}
	}
	s.armMarkEchoTimer(clip)
}

// terminateWithClip plays clip on the data plane and moves the call into
// AwaitingMarkEcho, sending the termination mark with an echo timeout derived
// from clip. Shared by the farewell (idle timeout) and the forced-stop
// (SOP-156 utterance cap) -- they differ only in the clip.
//
// setState happens BEFORE the mark send (not after, via dispatch()'s usual
// post-handler assignment) so an observer that receives the mark on ctlOut can
// never see a State() that still reads the old state (SOP-125 code review: this
// ordering was the source of a ~1-in-50 flake in TestTermination_MarkEchoReceived).
func (s *Session) terminateWithClip(clip []byte) SessionState {
	s.sendClip(clip)
	s.setState(StateAwaitingMarkEcho)
	s.sendMarkAndArmEcho(clip)
	return StateAwaitingMarkEcho
}

// closeTransport cancels the mark-echo timer and tears down the underlying
// transport via closeFunc (a nil closeFunc is a safe no-op) once
// AwaitingMarkEcho reaches Closed, whether via a genuine mark echo or a
// mark-echo timeout.
func (s *Session) closeTransport() {
	s.cancelMarkEchoTimer()
	if s.closeFunc != nil {
		s.closeFunc()
	}
}

// dispatchSTT hands an STTRequest to drainSTTDispatch via a non-blocking
// select-send so a slow/unwired STT sidecar cannot stall the sequencer
// (Charter R8: run()'s select never performs a blocking send) -- mirrors
// handleDataFrame's non-blocking send to forwardCh. audio is copied so later
// turnBuf appends on the run() goroutine cannot race the drain goroutine's
// Send. Routing every request through the single drainSTTDispatch goroutine
// (rather than a bare goroutine per call, as a per-call send would) preserves
// FIFO dispatch order across the requests it hands off.
func (s *Session) dispatchSTT(kind STTPassKind, audio []byte) {
	if s.sttIn == nil {
		return
	}
	s.sttReqID++
	req := STTRequest{
		SessionID: s.CallSID,
		RequestID: s.sttReqID,
		Kind:      kind,
		Audio:     append([]byte(nil), audio...),
	}
	select {
	case s.sttDispatchCh <- req:
	default:
		log.Printf("telephony: session %s: WARN STT dispatch buffer full, dropping %v request", s.CallSID, kind)
	}
}

// recordVADDecision records one VAD-boundary DecisionEvent. ev supplies the
// audio position (StreamWindowIndex * windowMS), the detector probability, and
// the silence count; the caller names the kind, the gating param and its
// resolved value, the effect, and the STT request id (0 when none is
// associated). One record site for every VAD-boundary decision (SOP-164).
func (s *Session) recordVADDecision(kind, param string, value any, ev VADEvent, effect string, requestID int) {
	s.decisionRecorder.Record(DecisionEvent{
		AudioMS:      ev.StreamWindowIndex * s.vadCfg.windowMS(),
		Type:         DecisionTypeVAD,
		Kind:         kind,
		Param:        param,
		ParamValue:   value,
		Prob:         ev.Prob,
		SilenceCount: ev.SilenceCount,
		RequestID:    requestID,
		Effect:       effect,
	})
}

// recordEndOfUtterance records the end-of-utterance (EndSilenceMS) decision.
// dispatched distinguishes the two paths an utterance can close on: the normal
// path that dispatched a FullPass (effect names the request), and the dropped
// path -- a second utterance closed while a pass was still in flight, so no new
// pass was sent (SOP-164). sttReqID names the relevant pass either way.
func (s *Session) recordEndOfUtterance(ev VADEvent, dispatched bool) {
	var effect string
	var reqID int
	if dispatched {
		effect = fmt.Sprintf("utterance closed; dispatched STT request %d", s.sttReqID)
		reqID = s.sttReqID
	} else {
		effect = fmt.Sprintf("utterance closed; dropped (STT pass %d still in flight)", s.sttReqID)
	}
	s.recordVADDecision(DecisionKindEndOfUtter, DecisionParamEndSilence, s.vadCfg.EndSilenceMS, ev, effect, reqID)
}

// drainSTTDispatch relays STTRequests from sttDispatchCh to sttIn in FIFO
// order, isolated from run()'s select loop so a slow/backed-up STT sidecar
// stalls only this goroutine (Charter R8) -- mirrors forwardToVAD's pattern.
func (s *Session) drainSTTDispatch() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case req, ok := <-s.sttDispatchCh:
			if !ok {
				return
			}
			if err := s.sttIn.Send(s.ctx, req); err != nil {
				return
			}
		}
	}
}

// forwardToVAD drains forwardCh and relays each frame into the VAD service's
// input via a blocking Send. It is a goroutine isolated from run()'s select
// loop specifically so that a slow/backed-up VAD pipeline stalls only this
// goroutine, never the sequencer (charter R8) -- run() keeps servicing every
// other select-case regardless of how long a given Send blocks here.
func (s *Session) forwardToVAD() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame, ok := <-s.forwardCh:
			if !ok {
				return
			}
			if err := s.vad.In.Send(s.ctx, frame); err != nil {
				return
			}
		}
	}
}

// State returns the session's current SessionState. Safe to call from any
// goroutine, including concurrently with the running sequencer.
func (s *Session) State() SessionState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state
}

// TurnActive reports whether a turn is currently in progress. Safe to call
// from any goroutine: turnActive is written only from the sequencer
// goroutine, always followed later in the same event's handling by
// setState's Lock/Unlock (called unconditionally after every transition,
// run()), so acquiring stateMu here -- the same mutex State() uses --
// establishes happens-before for turnActive's most recent write too, the
// same way it does for state itself.
func (s *Session) TurnActive() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.turnActive
}

// Closed returns a channel that closes when the session transitions to StateClosed.
// Safe to call from any goroutine.
func (s *Session) Closed() <-chan struct{} {
	return s.closedCh
}

// setState sets the session's current SessionState. Called only from the
// sequencer goroutine (run), but guarded so State() readers never race it.
func (s *Session) setState(next SessionState) {
	s.stateMu.Lock()
	s.state = next
	s.stateMu.Unlock()

	if next == StateClosed {
		s.closedOnce.Do(func() { close(s.closedCh) })
	}
}

// flushTurnTranscripts delivers accumulated turn transcripts through the
// TurnSink and clears the buffer. Parts are joined with a single space
// (each was trimmed on accumulation). An empty buffer is a no-op
// (SOP-150 Observable behavior 4). trigger identifies which completion path
// (state.go) is flushing, for TurnSink.OnTurnComplete's structured log line
// (SOP-162 DoD).
func (s *Session) flushTurnTranscripts(trigger TurnTrigger) {
	if len(s.turnTranscripts) == 0 {
		return
	}
	text := strings.Join(s.turnTranscripts, " ")
	s.turnTranscripts = nil
	if s.turnSink != nil {
		s.turnSink.OnTurnComplete(text, trigger)
	}
}

// sendBedAndEnterSpeaking plays the thinking bed if sim-turn is enabled,
// cancels the idle timer, arms the sim-turn timer, and transitions to
// StateSpeaking. Returns StateSpeaking if enabled, StateListening otherwise.
// Meant to be called after completeTurn() when a turn ends and we would
// normally return to Listening.
func (s *Session) sendBedAndEnterSpeaking() SessionState {
	if s.simTurnMS <= 0 {
		return StateListening
	}
	s.sendBed()
	s.cancelIdleTimer()
	s.armSimTurnTimer()
	return StateSpeaking
}

// sendBed plays the thinking bed on the data plane, looping the clip as needed
// to fill the configured duration (simTurnMS).
func (s *Session) sendBed() {
	if s.dataOut == nil || s.simTurnMS <= 0 || len(assets.LLMThinkingULaw) == 0 {
		// The empty-clip guard isn't reachable with the real embedded asset,
		// but without it an empty clip would spin the outer loop below
		// forever: the inner loop never executes, sentMS never advances, and
		// this runs synchronously on the single sequencer goroutine (run()) --
		// a permanent hang of the whole session, not just a dropped bed.
		return
	}
	totalMS := s.simTurnMS

	var sentMS int
	for sentMS < totalMS {
		for i := 0; i < len(assets.LLMThinkingULaw); i += farewellFrameBytes {
			end := i + farewellFrameBytes
			if end > len(assets.LLMThinkingULaw) {
				end = len(assets.LLMThinkingULaw)
			}
			if err := s.dataOut.Send(s.ctx, assets.LLMThinkingULaw[i:end]); err != nil {
				return
			}
			frameMS := (end - i) * 1000 / SampleRateHz
			sentMS += frameMS
			if sentMS >= totalMS {
				return
			}
		}
	}
}

// completeTurn flushes the current turn's transcripts and clears the active
// flag, marking the end of a turn. Called from every turn-completion path
// (VADTurnEnd, stopword, Twilio stop, idle timeout, utterance cap, turn cap)
// with the trigger identifying which one.
// turnEndPending is cleared here too: whichever path completes the turn,
// deferred or immediate, this is that completion. The turn-level cap
// (SOP-161) is cancelled unconditionally here too, regardless of trigger --
// mirroring how Close() cancels every timer unconditionally rather than
// per-path -- so no current or future completion path can leave it armed.
func (s *Session) completeTurn(trigger TurnTrigger) {
	s.flushTurnTranscripts(trigger)
	s.turnActive = false
	s.turnEndPending = false
	s.cancelTurnTimer()
}

// Close cancels the session and, if the sequencer was started, waits for its
// goroutines to exit before releasing the VAD service. Safe to call on a
// session that was never started.
func (s *Session) Close() {
	s.cancel()
	if s.started {
		<-s.done
		s.cancelIdleTimer()
		s.cancelUtteranceTimer()
		s.cancelMarkEchoTimer()
		s.cancelSimTurnTimer()
		s.cancelTurnTimer()
		s.vad.Close()
	}
	// Flush the decision record after the sequencer has drained (so no further
	// Record can arrive) and regardless of started, so a never-started session
	// still writes a clean, empty record. Idempotent -- a second Close no-ops.
	if err := s.decisionRecorder.Close(); err != nil {
		log.Printf("telephony: session %s: decision recorder close: %v", s.CallSID, err)
	}
}
