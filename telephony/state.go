package telephony

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/iansmith/aatoolkit/telephony/assets"
)

// MaxSilenceMS is how long the session waits, hearing no VAD speech events,
// before starting call termination (SOP-125 Observable behavior #3).
//
// The clock runs from the caller's most recent speech onset, not their first
// (see withSpeechReset), so this is dead air after they genuinely stop -- 20s
// of it read as the engine having failed to notice, and 10s left no room to pause
// and think mid-call.
const MaxSilenceMS = 15000

// MaxUtteranceMS caps a single continuous utterance (SOP-156). While the
// caller speaks without reaching end-of-utterance, the utterance timer runs;
// on expiry the engine plays the forced-stop clip and hangs up. 45s is long enough
// for any genuine turn and short enough that a caller who holds the line open
// (a phone by a speaker) cannot run up an unbounded Twilio bill. Overridable
// per-process by AATOOLKIT_MAX_UTTERANCE_MS, and per-session by WithMaxUtteranceMS.
const MaxUtteranceMS = 45000

// MaxTurnMS caps a whole turn (SOP-161) -- the caller's entire stretch of
// speaking before the engine takes her turn, spanning any pauses in between.
// Unlike MaxUtteranceMS (which resets on every end-of-utterance), this timer
// runs from the turn's first speech onset until completeTurn, so a caller
// who breaks a long ramble into several sub-cap utterances is still bounded.
// 60s is a deliberately conservative start (tightening is a later tuning
// pass). Overridable per-process by AATOOLKIT_MAX_TURN_MS, and per-session by
// WithMaxTurnMS.
const MaxTurnMS = 60000

// MarkEchoGraceMS is the only tunable knob in the mark-echo timeout
// derivation: the margin added atop the farewell clip's own playout
// duration to give Twilio a chance to echo the mark back (SOP-125
// Observable behavior #4).
const MarkEchoGraceMS = 500

// MarkEchoTimeout derives how long to wait for Twilio's mark-echo after
// sending the farewell clip: the clip's own playout duration (μ-law is 1
// byte/sample at SampleRateHz) plus MarkEchoGraceMS. Genuinely derived from
// clip length -- not a hardcoded constant -- so a different clip yields a
// different timeout (see TestMarkEchoTimeoutDerivedFromClip).
func MarkEchoTimeout(clip []byte) time.Duration {
	clipMS := len(clip) * 1000 / SampleRateHz
	return time.Duration(clipMS)*time.Millisecond + MarkEchoGraceMS*time.Millisecond
}

// StartSTTService runs the sttService's main loop until ctx is cancelled.
// This is a seam to allow main.go to start the STT service without exporting
// the sttService.run method.
func StartSTTService(ctx context.Context, svc *sttService) {
	svc.run(ctx)
}

// SessionState is a state in the session's total (state, source) transition
// table (charter: single select loop, SOP-123). AwaitingFullResult,
// Terminating, and AwaitingMarkEcho exist in the enum ahead of the logic
// that drives them -- their transition handlers are stubs that log "not yet
// implemented" (SOP-115/G,H own that logic; out of scope here).
type SessionState int

const (
	StateIdle SessionState = iota
	StateListening
	StateAwaitingFullResult
	StateSpeaking
	StateTerminating
	StateAwaitingMarkEcho
	StateClosed

	numStates = int(StateClosed) + 1
)

// AllStates enumerates every SessionState, in declaration order. Used by
// TestTransitionTableIsTotal to iterate the full state space.
var AllStates = []SessionState{
	StateIdle,
	StateListening,
	StateAwaitingFullResult,
	StateSpeaking,
	StateTerminating,
	StateAwaitingMarkEcho,
	StateClosed,
}

// inProgressStates are the states of a live, non-terminating call -- the ones
// the termination timers (idle, utterance cap) are wired into. Excludes
// Terminating/AwaitingMarkEcho (already terminating) and Closed (terminal).
var inProgressStates = []SessionState{
	StateIdle,
	StateListening,
	StateAwaitingFullResult,
}

func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateListening:
		return "Listening"
	case StateAwaitingFullResult:
		return "AwaitingFullResult"
	case StateSpeaking:
		return "Speaking"
	case StateTerminating:
		return "Terminating"
	case StateAwaitingMarkEcho:
		return "AwaitingMarkEcho"
	case StateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("SessionState(%d)", int(s))
	}
}

// InputSource identifies which channel or timer fired to produce a
// transitionEvent. It is the second axis of the (state, source) transition
// table.
type InputSource int

const (
	SourceTwilioData InputSource = iota
	SourceTwilioControl
	SourceVADEvent
	SourceSTTResult
	SourceIdleTimer
	SourceMarkEchoTimer
	SourceUtteranceTimer
	SourceSimTurnTimer
	SourceTurnTimer

	numSources = int(SourceTurnTimer) + 1
)

// AllSources enumerates every InputSource, in declaration order. Used by
// TestTransitionTableIsTotal to iterate the full state space.
var AllSources = []InputSource{
	SourceTwilioData,
	SourceTwilioControl,
	SourceVADEvent,
	SourceSTTResult,
	SourceIdleTimer,
	SourceMarkEchoTimer,
	SourceUtteranceTimer,
	SourceSimTurnTimer,
	SourceTurnTimer,
}

func (s InputSource) String() string {
	switch s {
	case SourceTwilioData:
		return "TwilioData"
	case SourceTwilioControl:
		return "TwilioControl"
	case SourceVADEvent:
		return "VADEvent"
	case SourceSTTResult:
		return "STTResult"
	case SourceIdleTimer:
		return "IdleTimer"
	case SourceMarkEchoTimer:
		return "MarkEchoTimer"
	case SourceUtteranceTimer:
		return "UtteranceTimer"
	default:
		return fmt.Sprintf("InputSource(%d)", int(s))
	}
}

// transitionEvent carries whatever payload accompanied the InputSource that
// fired: []byte for TwilioData, ControlEvent for TwilioControl, VADEvent for
// VADEvent, STTResult for STTResult, and nil for the two timer sources.
type transitionEvent struct {
	source  InputSource
	payload any
}

// transitionHandler applies one (state, source) transition and returns the
// next state. Handlers may run side effects (TurnSink callbacks, logging,
// forwarding a frame) but must never block -- run()'s select loop calls them
// synchronously (charter R8: no blocking sends anywhere in the loop).
type transitionHandler func(s *Session, ev transitionEvent) SessionState

// transitionTable is total: every (state, source) pair must resolve to a
// non-nil handler -- see TestTransitionTableIsTotal. Indexed [state][source]
// using each enum's zero-based iota value.
type transitionTable [numStates][numSources]transitionHandler

// transitions is the package-level total transition table, built once at
// init from buildTransitionTable.
var transitions = buildTransitionTable()

// TransitionHandlerDefined reports whether the transition table has an
// explicit (non-nil) entry for (state, source). Exported so
// TestTransitionTableIsTotal can assert totality without reaching into
// package-private table internals.
func TransitionHandlerDefined(state SessionState, source InputSource) bool {
	if int(state) < 0 || int(state) >= len(AllStates) {
		return false
	}
	if int(source) < 0 || int(source) >= len(AllSources) {
		return false
	}
	return transitions[state][source] != nil
}

// buildTransitionTable constructs the total (state, source) transition
// table. Every entry defaults to handleUnexpected (loud warn, remain in
// state) and is then overridden for the (state, source) pairs this ticket
// implements.
func buildTransitionTable() transitionTable {
	var t transitionTable
	for _, st := range AllStates {
		for _, src := range AllSources {
			t[st][src] = handleUnexpected
		}
	}

	// Idle: no call audio has arrived yet.
	t[StateIdle][SourceTwilioData] = handleDataFrame
	t[StateIdle][SourceTwilioControl] = handleControlEvent
	t[StateIdle][SourceSTTResult] = handleSTTResult

	// Listening: actively forwarding audio through VAD.
	t[StateListening][SourceTwilioData] = handleDataFrame
	t[StateListening][SourceTwilioControl] = handleControlEvent
	t[StateListening][SourceVADEvent] = withSpeechReset(handleListeningVADEvent)
	t[StateListening][SourceSTTResult] = handleSTTResult

	// Terminating: never actually dwelt in -- handleIdleTimer performs the
	// whole send-farewell/send-mark/arm-mark-echo-timer sequence atomically
	// and returns AwaitingMarkEcho directly (SOP-125). Its row stays entirely
	// stubbed since no event is ever dispatched while "in" it.
	for _, src := range AllSources {
		t[StateTerminating][src] = handleNotImplemented
	}

	// AwaitingMarkEcho: waiting for Twilio to echo the farewell mark back, or
	// for the mark-echo timeout to fire (SOP-125). Every other source is a
	// stub here -- no further call audio/VAD/STT activity is expected once
	// the farewell has been sent.
	for _, src := range AllSources {
		t[StateAwaitingMarkEcho][src] = handleNotImplemented
	}
	t[StateAwaitingMarkEcho][SourceTwilioControl] = handleMarkEchoControlEvent
	t[StateAwaitingMarkEcho][SourceMarkEchoTimer] = handleMarkEchoTimeout

	// Idle timer: fires after MaxSilenceMS of no VAD speech and starts call
	// termination (SOP-125 Observable behavior #3). Wired into every state
	// where a call is actively in progress -- not Terminating,
	// AwaitingMarkEcho (both mid-termination already), or Closed (terminal).
	// Idle timer (silence → farewell, SOP-125), the max-utterance cap (speech
	// too long → forced-stop, SOP-156), and the max-turn cap (a whole turn
	// too long, across pauses → forced-stop, SOP-161) are the three
	// termination timers, wired into the same set of in-progress states by
	// definition -- not Terminating/AwaitingMarkEcho (already terminating) or
	// Closed (terminal). One slice, so a sixth live state can never wire one
	// timer and forget the others.
	for _, st := range inProgressStates {
		t[st][SourceIdleTimer] = handleIdleTimer
		t[st][SourceUtteranceTimer] = handleUtteranceTimeout
		t[st][SourceTurnTimer] = handleTurnTimeout
	}

	// AwaitingFullResult: this utterance's full pass was dispatched -- acting
	// on its transcript is SOP-115/G scope. Its result returns the session to
	// Listening, so the caller's next utterance dispatches its own pass.
	t[StateAwaitingFullResult][SourceTwilioData] = handleDataFrame
	t[StateAwaitingFullResult][SourceTwilioControl] = handleControlEvent
	t[StateAwaitingFullResult][SourceVADEvent] = withSpeechReset(handleAwaitingFullResultVADEvent)
	t[StateAwaitingFullResult][SourceSTTResult] = handleSTTResult

	// StateSpeaking: playing the thinking bed during sim-turn (SOP-157).
	// VAD events are absorbed UNwrapped by withSpeechReset (PRD D4.5): a real
	// barge-in must not be routed through handleSpeechOnset/completeTurn --
	// that would arm the utterance timer and flip turnActive back to true
	// mid-bed, which is exactly the "handling" the ticket says barge-in must
	// not get here. Twilio data and control events are absorbed (no further
	// changes). When the sim-turn timer fires, return to Listening.
	t[StateSpeaking][SourceTwilioData] = handleDataFrame
	t[StateSpeaking][SourceTwilioControl] = handleControlEvent
	t[StateSpeaking][SourceVADEvent] = handleUnexpected
	t[StateSpeaking][SourceSTTResult] = handleSTTResult
	t[StateSpeaking][SourceSimTurnTimer] = handleSimTurnTimeout
	// Idle/utterance timers are cancelled during speaking, so they won't fire.
	// If they somehow do (stale timer), absorb with handleUnexpected (already set).

	// Closed: terminal. Every source is absorbed with no state change.
	for _, src := range AllSources {
		t[StateClosed][src] = handleClosed
	}

	return t
}

// handleDataFrame appends one inbound media payload to the session's turn
// buffer (SOP-124 Observable behavior #1) and relays it toward the VAD
// pipeline (via the session's non-blocking forward buffer -- see
// Session.run). Only Idle transitions to Listening; every other state that
// wires this handler (Listening, AwaitingFullResult) keeps accumulating
// audio without losing its place in the current utterance.
func handleDataFrame(s *Session, ev transitionEvent) SessionState {
	frame, _ := ev.payload.([]byte)
	s.turnBuf = append(s.turnBuf, frame...)
	select {
	case s.forwardCh <- frame:
	default:
		log.Printf("telephony: session %s: WARN VAD forward buffer full, dropping frame", s.CallSID)
	}
	if s.State() == StateIdle {
		return StateListening
	}
	return s.State()
}

// handleControlEvent handles a Twilio control-plane event. A "stop" event
// transitions cleanly to Closed (farewell/mark-echo teardown is SOP-115/H
// scope -- out of scope here, so stop goes straight to Closed). Anything
// else is unexpected in the states this ticket implements and is logged
// loudly without changing state.
func handleControlEvent(s *Session, ev transitionEvent) SessionState {
	cev, _ := ev.payload.(ControlEvent)
	if cev.Kind == controlKindStop {
		s.completeTurn(TriggerCallEnd)
		log.Printf("telephony: session %s: stop received, closing", s.CallSID)
		return StateClosed
	}
	log.Printf("telephony: session %s: WARN unexpected control event %q in state %s", s.CallSID, cev.Kind, s.State())
	return s.State()
}

// handleSpeechOnset is VADSpeech's handling, identical in every live state:
// the caller has started an utterance. This is the "now speaking" edge of the
// two-phase timer model (SOP-156). The idle/silence timer is cancelled -- it
// watches silence, and the caller is no longer silent, so it must not fire
// mid-utterance (the bug that hung a real caller up at 15s) -- and the
// max-utterance cap is armed to bound this utterance. VADSpeech fires once per
// utterance, so the cap measures the whole utterance from its start. Sets
// turnActive = true (SOP-154) -- a no-op write on every onset after the
// turn's first, since nothing clears it again until completion. Never
// changes state; the recognition pass in flight is orthogonal.
//
// !s.turnActive, checked here before the assignment below, is exactly "this
// is the first onset of a new turn" -- so the turn-level cap (SOP-161) arms
// here, guarded by that check, rather than on every onset the way the
// per-utterance cap does.
func handleSpeechOnset(s *Session) SessionState {
	s.cancelIdleTimer()
	s.armUtteranceTimer()
	if !s.turnActive {
		s.armTurnTimer()
	}
	s.turnActive = true
	if s.turnSink != nil {
		s.turnSink.OnSpeechStart()
	}
	return s.State()
}

// withSpeechReset wraps one state's VAD handler so VADSpeech and VADTurnEnd
// are handled uniformly -- by handleSpeechOnset and completeTurn
// respectively -- across every live VAD-consuming state, and every other VAD
// kind falls through to the state's own handler. It is applied at the
// transition table so the rule is visible on each row that needs it --
// rather than repeated inside each handler, where a state could silently
// omit it.
//
// VADSpeech's uniform handling exists because it used to reach only
// Listening's handler: no transition leads back to Listening once a session
// leaves it, so the idle timer never reset again and every call was
// terminated MaxSilenceMS after its caller's first word, however long they
// kept talking (observed live in build/logs/server-2026-07-16-11-19-21.log
// -- one "speech start" logged across a 17-second call, with termination
// armed from it). VADTurnEnd needs the same uniform treatment for the same
// reason (SOP-154): a full pass can still be in flight (StateAwaitingFullResult)
// when TurnEndSilenceMS of trailing silence elapses, and a turn-end embedded
// in only one state's switch would be silently dropped in every other one.
//
// VADTurnEnd does NOT call completeTurn() directly when a full pass is still
// in flight (StateAwaitingFullResult): completing now would flush and reset
// turnTranscripts before that pass's result arrives, and handleSTTResult's
// unconditional FullPass accumulation would then attribute the in-flight
// pass's text to whatever turn is active when it lands -- silently bleeding
// trailing words from the just-completed turn into the next one. Instead,
// completion is deferred via turnEndPending; handleSTTResult finishes it once
// the pass returns (state.go).
func withSpeechReset(h transitionHandler) transitionHandler {
	return func(s *Session, ev transitionEvent) SessionState {
		if vev, ok := ev.payload.(VADEvent); ok {
			switch vev.Kind {
			case VADSpeech:
				return handleSpeechOnset(s)
			case VADTurnEnd:
				if s.State() == StateAwaitingFullResult {
					s.turnEndPending = true
					return s.State()
				}
				s.completeTurn(TriggerSilenceTurnEnd)
				return s.sendBedAndEnterSpeaking()
			}
		}
		return h(s, ev)
	}
}

// handleListeningVADEvent dispatches turn-taking boundaries to the session's
// TurnSink directly from the transition table (dispatchTurnSink is gone --
// SOP-123 subsumes its work here). VADEndOfUtterance dispatches the one full
// pass immediately. VADSilence carries no meaning here. VADSpeech and
// VADTurnEnd never reach this handler -- withSpeechReset intercepts both for
// every live state.
func handleListeningVADEvent(s *Session, ev transitionEvent) SessionState {
	vev, _ := ev.payload.(VADEvent)
	if vev.Kind == VADEndOfUtterance {
		return dispatchFullPass(s)
	}
	return s.State()
}

// dispatchFullPass notifies the TurnSink of end-of-utterance and dispatches
// the FullPass STTRequest for this utterance's turn buffer, transitioning to
// StateAwaitingFullResult. Shared by every path that ends an utterance with a
// full pass: a direct VADEndOfUtterance from Listening, and the
// second-utterance-while-first-is-in-flight path once the first pass's
// result is (eventually) awaited.
//
// The turn buffer is cleared once the audio is dispatched: it holds *this*
// utterance, and the next one starts empty. dispatchSTT copies the audio into
// the request, so the in-flight pass is unaffected by the reset; assigning nil
// rather than turnBuf[:0] means later appends allocate fresh storage instead of
// overwriting the bytes the copy was taken from. Without this the buffer is
// append-only for the whole call, and every pass re-sends everything said so
// far -- growing latency per pass and repeating earlier text in each
// transcript.
func dispatchFullPass(s *Session) SessionState {
	s.onUtteranceEnd()
	s.dispatchSTT(FullPass, s.turnBuf)
	s.turnBuf = nil
	return StateAwaitingFullResult
}

// handleAwaitingFullResultVADEvent covers StateAwaitingFullResult's VAD
// boundary: this utterance's full pass is still in flight, so a further
// VADEndOfUtterance still notifies the TurnSink -- callers rely on
// OnEndOfUtterance firing once per utterance regardless of state -- but does
// not dispatch another pass.
//
// Reaching here at all means the caller completed a whole second utterance
// (speech, then enough silence for VAD to call it) before the first pass's
// result came back, and that utterance is dropped. The window is small: a
// full pass over one utterance's audio returns in about a second, while
// VADEndOfUtterance needs MaxSilenceMS of quiet to fire. Dispatching a second
// pass here instead would be worse -- dispatchSTT advances sttReqID, so the
// first utterance's result would arrive stale and be discarded, trading a
// dropped short utterance for a dropped long one. Overlapping passes need
// per-request correlation rather than a single awaited id (SOP-115/H, with
// barge-in). VADSpeech and VADTurnEnd never reach here: withSpeechReset
// intercepts both.
func handleAwaitingFullResultVADEvent(s *Session, ev transitionEvent) SessionState {
	vev, _ := ev.payload.(VADEvent)
	if vev.Kind != VADEndOfUtterance {
		return s.State()
	}
	// A second utterance closed while the first's pass is still in flight. It
	// dispatches no new pass, but it IS the end of an utterance, so the
	// end-of-utterance bookkeeping must still run -- otherwise the cap armed for
	// this second utterance stays armed and the idle timer stays cancelled after
	// the caller has finished, and a subsequent silence force-stops the call
	// instead of ending it with the farewell (SOP-156).
	s.onUtteranceEnd()
	return s.State()
}

// handleSTTResult validates an inbound STTResult's correlation identity,
// logs it, and — for FullPass results — either accumulates the transcript
// into the turn buffer or flushes the buffer if the turn-end policy fires
// (SOP-150). A late STTResult whose RequestID does not match the session's
// current awaited request id is silently discarded (D5 logical discard —
// Charter R8).
func handleSTTResult(s *Session, ev transitionEvent) SessionState {
	res, _ := ev.payload.(STTResult)
	if res.SessionID != s.CallSID {
		log.Printf("telephony: session %s: WARN unexpected STT result: mismatch SessionID %q, expected %q -- dropped",
			s.CallSID, res.SessionID, s.CallSID)
		return s.State()
	}

	if res.RequestID != s.sttReqID {
		log.Printf("telephony: session %s: discard STT result: RequestID %d does not match awaited %d",
			s.CallSID, res.RequestID, s.sttReqID)
		return s.State()
	}

	log.Printf("telephony: session %s: STT result %s: %q", s.CallSID, res.Kind, res.Text)

	var turnCompleted bool
	if res.Kind == FullPass {
		if s.turnEndPolicy != nil && s.turnEndPolicy.IsEndOfTurn(res.Text) {
			s.completeTurn(TriggerStopword)
			turnCompleted = true
		} else {
			s.turnTranscripts = append(s.turnTranscripts, strings.TrimSpace(res.Text))
			// A VADTurnEnd that fired while this pass was still in flight
			// deferred completion (turnEndPending, set by withSpeechReset) so
			// this text lands in the turn it belongs to, not the next one.
			if s.turnEndPending {
				s.completeTurn(TriggerSilenceTurnEnd)
				turnCompleted = true
			}
		}
	}

	if res.Kind == FullPass && s.State() == StateAwaitingFullResult {
		if turnCompleted {
			return s.sendBedAndEnterSpeaking()
		}
		return StateListening
	}
	return s.State()
}

func handleIdleTimer(s *Session, ev transitionEvent) SessionState {
	s.completeTurn(TriggerIdleTimeout)
	return s.terminateWithClip(assets.FarewellULaw)
}

// handleUtteranceTimeout runs when the max-utterance cap fires (SOP-156): the
// caller has spoken continuously past MaxUtteranceMS, so cut them off. It plays
// the forced-stop clip and terminates through the same mark-echo flow as the
// farewell -- only the clip differs, so the mark-echo timeout is derived from
// the forced-stop clip's own length, and the caller hears the whole clip before
// the line drops. Any transcripts fused so far are flushed, since the call is
// ending.
func handleUtteranceTimeout(s *Session, ev transitionEvent) SessionState {
	log.Printf("telephony: session %s: max-utterance cap reached, forcing stop", s.CallSID)
	s.completeTurn(TriggerUtteranceCap)
	return s.terminateWithClip(assets.AudioForcedStopULaw)
}

// handleTurnTimeout runs when the max-turn cap fires (SOP-161): the caller's
// whole turn -- possibly spanning several utterances and pauses -- has run
// longer than MaxTurnMS, so cut them off. Same shape as
// handleUtteranceTimeout: play the forced-stop clip and terminate through the
// mark-echo flow. Any transcripts fused so far are flushed, since the call is
// ending.
func handleTurnTimeout(s *Session, ev transitionEvent) SessionState {
	log.Printf("telephony: session %s: max-turn cap reached, forcing stop", s.CallSID)
	s.completeTurn(TriggerTurnCap)
	return s.terminateWithClip(assets.AudioForcedStopULaw)
}

// onUtteranceEnd is the end-of-utterance bookkeeping shared by every path a
// caller's utterance can close on: it notifies the TurnSink and reconciles the
// two-phase termination timers (SOP-156) -- cancel the max-utterance cap and
// re-arm the idle/silence timer, so a caller who now stays quiet gets the
// farewell after MaxSilenceMS rather than the forced-stop after MaxUtteranceMS.
// It must run on EVERY end-of-utterance, dispatching path or not.
func (s *Session) onUtteranceEnd() {
	if s.turnSink != nil {
		s.turnSink.OnEndOfUtterance()
	}
	s.cancelUtteranceTimer()
	s.armIdleTimer()
}

// handleSimTurnTimeout runs when the sim-turn bed timer expires (SOP-157):
// the thinking bed has finished playing, so return to Listening and re-arm
// the idle timer to listen for the next turn.
func handleSimTurnTimeout(s *Session, ev transitionEvent) SessionState {
	s.armIdleTimer()
	return StateListening
}

// handleMarkEchoControlEvent covers AwaitingMarkEcho's happy path (SOP-125
// Observable behavior #4): Twilio's echo of the farewell mark closes the
// underlying transport and moves to Closed. Anything else -- a stray "stop",
// or a mark echo whose name doesn't match the one just sent -- is unexpected
// here and is logged loudly without changing state.
func handleMarkEchoControlEvent(s *Session, ev transitionEvent) SessionState {
	cev, _ := ev.payload.(ControlEvent)
	if cev.Kind != controlKindMark || cev.MarkName != farewellMarkName {
		log.Printf("telephony: session %s: WARN unexpected control event %q (mark=%q) in state %s", s.CallSID, cev.Kind, cev.MarkName, s.State())
		return s.State()
	}
	s.closeTransport()
	return StateClosed
}

// handleMarkEchoTimeout covers AwaitingMarkEcho's timeout path (SOP-125
// Observable behavior #4): Twilio never echoed the mark within
// MarkEchoTimeout, so the peer did not honor the mark protocol -- log
// loudly, close the transport anyway, and move to Closed.
func handleMarkEchoTimeout(s *Session, ev transitionEvent) SessionState {
	log.Printf("telephony: session %s: WARN peer did not honor mark protocol -- closing anyway", s.CallSID)
	s.closeTransport()
	return StateClosed
}

// handleUnexpected is the default handler for a (state, source) pair this
// ticket does not otherwise drive: it logs loudly and remains in state, per
// the ticket's requirement that unexpected inputs never panic or hang the
// select loop.
func handleUnexpected(s *Session, ev transitionEvent) SessionState {
	log.Printf("telephony: session %s: WARN unexpected input source=%s in state=%s", s.CallSID, ev.source, s.State())
	return s.State()
}

// notImplKey identifies a (state, source) pair handleNotImplemented has
// already reported, so each distinct gap is logged once per session.
type notImplKey struct {
	state  SessionState
	source InputSource
}

// handleNotImplemented is the stub handler for the termination states'
// unreachable rows (SOP-115/G,H scope) -- out of scope here per the ticket,
// but the table must still be total.
//
// Each (state, source) pair is reported once per session. A per-frame source
// would otherwise repeat itself at the frame rate: AwaitingMarkEcho takes
// inbound media for the farewell's whole playout, which at 50 frames/sec
// drowns the control plane in ~100 identical lines. Logging once keeps the
// gap visible to SOP-115/G,H without the noise.
func handleNotImplemented(s *Session, ev transitionEvent) SessionState {
	if s.notImplLogged == nil {
		s.notImplLogged = make(map[notImplKey]bool)
	}
	state := s.State()
	key := notImplKey{state: state, source: ev.source}
	if s.notImplLogged[key] {
		return state
	}
	s.notImplLogged[key] = true
	log.Printf("telephony: session %s: state=%s source=%s: not yet implemented (further occurrences of this pair suppressed)", s.CallSID, state, ev.source)
	return state
}

// handleClosed absorbs any input once the session is Closed.
func handleClosed(s *Session, ev transitionEvent) SessionState {
	return StateClosed
}
