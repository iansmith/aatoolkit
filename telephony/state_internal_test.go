package telephony

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAwaitingMarkEcho_AbsorbsInboundDataPlane pins SOP-169: while the farewell
// clip plays and the session waits for the mark echo (StateAwaitingMarkEcho),
// the caller keeps streaming media until the socket closes, so inbound
// data/VAD/STT are expected -- they must be absorbed silently, not logged as an
// unimplemented gap. The state must stay AwaitingMarkEcho (the mark-echo wait
// is not abandoned).
func TestAwaitingMarkEcho_AbsorbsInboundDataPlane(t *testing.T) {
	cases := []struct {
		name    string
		source  InputSource
		payload any
	}{
		{"twilio-data", SourceTwilioData, []byte{0x01, 0x02}},
		{"vad-event", SourceVADEvent, VADEvent{Kind: VADSpeech}},
		{"stt-result", SourceSTTResult, STTResult{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(context.Background(), "markecho-absorb")
			defer s.Close()
			s.setState(StateAwaitingMarkEcho)

			s.dispatch(tc.source, tc.payload)

			if got := s.State(); got != StateAwaitingMarkEcho {
				t.Errorf("state after %s: got %s, want StateAwaitingMarkEcho (mark-echo wait must not be abandoned)", tc.name, got)
			}
			if s.notImplLogged[notImplKey{state: StateAwaitingMarkEcho, source: tc.source}] {
				t.Errorf("%s in AwaitingMarkEcho was logged 'not yet implemented' — expected teardown input must be absorbed silently", tc.name)
			}
		})
	}
}

// countingSink counts TurnSink callbacks. The external session_test.go has
// its own spySink; this package-internal one exists because tests that reach
// the unexported transition table cannot import that one.
type countingSink struct {
	mu    sync.Mutex
	start int
	eou   int
}

func (c *countingSink) OnSpeechStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.start++
}

func (c *countingSink) OnEndOfUtterance() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eou++
}

func (c *countingSink) OnTurnComplete(string, TurnTrigger) {}

func (c *countingSink) speechStarts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start
}

// turnSpy is an internal TurnSink that records dispatches for direct
// handler-level tests (package telephony, not telephony_test).
type turnSpy struct {
	starts int
	eou    int
	turns  []string
}

func (s *turnSpy) OnSpeechStart()                            { s.starts++ }
func (s *turnSpy) OnEndOfUtterance()                         { s.eou++ }
func (s *turnSpy) OnTurnComplete(text string, _ TurnTrigger) { s.turns = append(s.turns, text) }

// noopDetector satisfies vadDetector for sessions that never process audio.
type noopDetector struct{}

func (noopDetector) Detect(context.Context, []float32) (float32, error) { return 0, nil }
func (noopDetector) Reset()                                             {}

// newTestSession builds a minimal Session suitable for direct handler calls.
// It is NOT Start()ed — no goroutines run.
func newTestSession(sink TurnSink, policy TurnEndPolicy) *Session {
	return NewSession(context.Background(), "test-call",
		WithVADFactory(func() (VADDetector, error) { return noopDetector{}, nil }),
		WithTurnSink(sink),
		WithTurnEndPolicy(policy),
	)
}

func sttEvent(sessionID string, requestID int, kind STTPassKind, text string) transitionEvent {
	return transitionEvent{
		source: SourceSTTResult,
		payload: STTResult{
			SessionID: sessionID,
			RequestID: requestID,
			Kind:      kind,
			Text:      text,
		},
	}
}

// notLiveStates are the states in which the caller cannot still be talking:
// the call is either mid-termination or over. Everything else is live and
// must reset the idle timer on a speech onset.
//
// StateIdle is here because a VAD event cannot precede the first media frame,
// and that frame moves Idle -> Listening; its VAD row is deliberately
// unwired.
//
// StateAwaitingResponse and StateAwaitingResponsePlayout (AATK-24) are here
// for a different reason: the caller CAN still be talking while a response
// is generated or played out, but barge-in is explicitly out of scope
// (deferrals 1/3) -- a VAD event there must not reset the idle timer or
// notify OnSpeechStart. The idle timer is deliberately cancelled on entry
// into StateAwaitingResponse and stays inert through the whole
// response-delivery cycle regardless of caller speech; only the return to
// Listening (via the response-playout echo or its backstop) re-arms it.
var notLiveStates = map[SessionState]bool{
	StateIdle:                    true,
	StateTerminating:             true,
	StateAwaitingMarkEcho:        true,
	StateClosed:                  true,
	StateAwaitingResponse:        true,
	StateAwaitingResponsePlayout: true,
}

// liveSpeechStates is derived from the state list rather than written out, so
// a state added later is covered without anyone remembering to add it here.
//
// A hand-maintained list would make the rule this file exists to enforce
// unenforceable: a new live state wired into the table without
// withSpeechReset would leave every test green -- the totality test only
// checks a handler exists -- while reintroducing the exact bug of hanging up
// on a caller mid-sentence. Adding a state now means classifying it in
// notLiveStates above, and getting that wrong fails loudly here instead of
// silently in production.
func liveSpeechStates() []SessionState {
	var live []SessionState
	for _, st := range AllStates {
		if !notLiveStates[st] {
			live = append(live, st)
		}
	}
	return live
}

// TestSpeechOnsetResetsIdleTimerInEveryLiveState pins the fix for a caller
// being hung up on mid-sentence: VADSpeech must reset the idle timer in every
// live state, not only Listening.
//
// Observed live (build/logs/server-2026-07-16-11-19-21.log): once a session
// completes one utterance it parks in StateAwaitingFullResult, whose VAD
// handler ignored VADSpeech -- and no transition leads back to Listening. The
// MaxSilenceMS clock therefore ran from the caller's *first* word of the call
// and never reset, terminating the call however long they kept talking. One
// "speech start" was logged across a 17-second call.
//
// This asserts on the idle timer's generation counter, never on elapsed time:
// re-arming a named timer supersedes its previous generation, so IsCurrent
// going false *is* the reset, observed without a clock. The armed duration is
// longer than any test run, so no wall-clock deadline can influence the
// result and this cannot flake.
func TestSpeechOnsetResetsIdleTimerInEveryLiveState(t *testing.T) {
	for _, st := range liveSpeechStates() {
		t.Run(st.String(), func(t *testing.T) {
			sink := &countingSink{}
			s := NewSession(context.Background(), "speech-resets-idle", WithTurnSink(sink))
			defer s.Close()

			// A duration no test run can reach: only the re-arm, never
			// elapsed time, can supersede this generation.
			gen := s.timerFacility.Arm(s.ctx, timerIdle, time.Hour)
			if !s.timerFacility.IsCurrent(TimerCompletion{Name: timerIdle, TimerID: gen}) {
				t.Fatalf("precondition: freshly armed idle timer is not current")
			}

			s.setState(st)
			s.dispatch(SourceVADEvent, VADEvent{Kind: VADSpeech})

			if s.timerFacility.IsCurrent(TimerCompletion{Name: timerIdle, TimerID: gen}) {
				t.Errorf("VADSpeech in %s left the idle timer's original generation current: the timer was never re-armed, so the caller is hung up on %dms after their first word however long they keep talking", st, MaxSilenceMS)
			}
			if got := sink.speechStarts(); got != 1 {
				t.Errorf("OnSpeechStart in %s: got %d, want 1 (callers rely on it per onset regardless of which pass is in flight)", st, got)
			}
		})
	}
}

// TestSpeechOnsetDoesNotChangeState guards the other half of the rule: a
// speech onset reports the caller is still there, it does not move the
// session out of whichever pass is in flight. Without this, wrapping the VAD
// handlers could silently reset each state back to Listening and discard a
// pending full pass.
func TestSpeechOnsetDoesNotChangeState(t *testing.T) {
	for _, st := range liveSpeechStates() {
		t.Run(st.String(), func(t *testing.T) {
			s := NewSession(context.Background(), "speech-keeps-state", WithTurnSink(&countingSink{}))
			defer s.Close()

			s.setState(st)
			s.dispatch(SourceVADEvent, VADEvent{Kind: VADSpeech})

			if got := s.State(); got != st {
				t.Errorf("VADSpeech in %s moved the session to %s; a speech onset must not disturb the pass in flight", st, got)
			}
		})
	}
}

// TestNotImplementedLogsOncePerStateSourcePair pins the fix for the
// stub-handler log burying the control plane: a per-frame source hitting a
// still-stubbed row must report once per (state, source) pair, not per frame.
//
// The original trigger was AwaitingMarkEcho taking inbound media for the
// farewell clip's whole 2s playout (build/logs/server-2026-07-16-12-45-12.log:
// 100 identical "state=AwaitingMarkEcho source=TwilioData" lines at 50
// frames/sec). SOP-169 later absorbed that data plane during teardown, so it no
// longer stubs -- but the per-pair suppression is a general guard for any
// still-stubbed termination row, exercised here via StateTerminating (which
// still stubs every source, SOP-115/G,H).
//
// Frame count and log content only; no clock is read.
func TestNotImplementedLogsOncePerStateSourcePair(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	s := NewSession(context.Background(), "notimpl-once", WithTurnSink(&countingSink{}))
	defer s.Close()

	s.setState(StateTerminating)

	// 100 frames stands in for a sustained per-frame source at ~50 frames/sec.
	const frames = 100
	for i := 0; i < frames; i++ {
		s.dispatch(SourceTwilioData, []byte{0xFF})
	}

	got := strings.Count(buf.String(), "not yet implemented")
	if got != 1 {
		t.Errorf("Terminating + %d media frames logged %d \"not yet implemented\" lines, want 1: a per-frame source repeats the same gap and buries the control plane", frames, got)
	}
	// The one line must say it stands for many. Without that, a single
	// occurrence reads as "this happened once" when it happened `frames`
	// times -- the reader would under-read the gap rather than over-read it.
	if !strings.Contains(buf.String(), "suppressed") {
		t.Errorf("the surviving line must say further occurrences are suppressed, or it reads as a one-off; got: %q", buf.String())
	}
}

// TestNotImplementedLogsEachDistinctPair guards the other half: collapsing
// repeats must not collapse *different* gaps into one. Each (state, source)
// pair SOP-115/G,H has yet to drive still has to report itself, or wiring one
// state would silently hide the next.
func TestNotImplementedLogsEachDistinctPair(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	s := NewSession(context.Background(), "notimpl-distinct", WithTurnSink(&countingSink{}))
	defer s.Close()

	// Two distinct stub pairs, each driven twice. Both live in StateTerminating
	// (still stubs every source, SOP-115/G,H); AwaitingMarkEcho's data plane is
	// now absorbed (SOP-169), so it no longer offers a stubbed data pair.
	for i := 0; i < 2; i++ {
		s.setState(StateTerminating)
		s.dispatch(SourceTwilioData, []byte{0xFF})
		s.dispatch(SourceVADEvent, VADEvent{Kind: VADSpeech})
	}

	if got := strings.Count(buf.String(), "not yet implemented"); got != 2 {
		t.Errorf("two distinct stub (state, source) pairs logged %d lines, want 2 (one each): suppression must be per pair, not global", got)
	}
	for _, want := range []string{"source=TwilioData", "source=VADEvent"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("expected a report naming %s, got: %q", want, buf.String())
		}
	}
}

// drainDispatch reads every STTRequest the session has queued on its internal
// dispatch channel, without needing the drain goroutine or a live sidecar.
// dispatchSTT hands off non-blockingly, so the requests are already sitting in
// the buffer by the time dispatch() returns.
func drainDispatch(s *Session) []STTRequest {
	var got []STTRequest
	for {
		select {
		case req := <-s.sttDispatchCh:
			got = append(got, req)
		default:
			return got
		}
	}
}

// feedAudio pushes n frames of a distinct byte value through the data plane,
// which is what accumulates turnBuf.
func feedAudio(s *Session, b byte, frames int) {
	for i := 0; i < frames; i++ {
		s.dispatch(SourceTwilioData, []byte{b, b, b, b})
	}
}

// TestSecondUtteranceDispatchesItsOwnFullPass pins multi-utterance STT: a
// caller who says two things gets two transcripts.
//
// Observed live (build/logs/server-2026-07-16-13-46-07.log): a 75-second call
// with eight end-of-utterance events produced exactly one STT request --
// voice-in logged a single POST. The session dispatched the full pass for the
// first utterance, parked in AwaitingFullResult, and never left: every later
// utterance notified the TurnSink and went nowhere. Everything the caller said
// after their opening sentence was silently dropped.
//
// No clock is read: the STT result is injected directly, as the router would.
func TestSecondUtteranceDispatchesItsOwnFullPass(t *testing.T) {
	s := NewSession(context.Background(), "multi-utterance",
		WithTurnSink(&countingSink{}), WithSTTInput(NewBufferedChan[STTRequest](sttDispatchDepth)))
	defer s.Close()
	s.setState(StateListening)

	// Utterance 1: audio, then the caller stops.
	feedAudio(s, 0x01, 3)
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance})
	if got := s.State(); got != StateAwaitingFullResult {
		t.Fatalf("after utterance 1's end: state %s, want AwaitingFullResult", got)
	}
	first := drainDispatch(s)
	if len(first) != 1 {
		t.Fatalf("utterance 1 dispatched %d STT requests, want 1", len(first))
	}

	// Its transcript comes back.
	s.dispatch(SourceSTTResult, STTResult{
		SessionID: s.CallSID, RequestID: s.sttReqID, Kind: FullPass, Text: "first",
	})

	// Utterance 2: the caller says something else.
	feedAudio(s, 0x02, 3)
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance})

	second := drainDispatch(s)
	if len(second) != 1 {
		t.Fatalf("utterance 2 dispatched %d STT requests, want 1: the session never left AwaitingFullResult, so everything after the caller's first sentence is dropped", len(second))
	}
	if second[0].Kind != FullPass {
		t.Errorf("utterance 2 dispatched a %v, want FullPass", second[0].Kind)
	}
}

// TestFullPassResultReturnsToListening states the mechanism the test above
// depends on, so a regression names the cause rather than the symptom.
func TestFullPassResultReturnsToListening(t *testing.T) {
	s := NewSession(context.Background(), "result-returns",
		WithTurnSink(&countingSink{}), WithSTTInput(NewBufferedChan[STTRequest](sttDispatchDepth)))
	defer s.Close()
	s.setState(StateListening)

	feedAudio(s, 0x01, 2)
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance})
	s.dispatch(SourceSTTResult, STTResult{
		SessionID: s.CallSID, RequestID: s.sttReqID, Kind: FullPass, Text: "done",
	})

	if got := s.State(); got != StateListening {
		t.Errorf("after a FullPass result: state %s, want Listening — the utterance is complete, so the session must be ready for the next one", got)
	}
}

// TestEachUtteranceSendsOnlyItsOwnAudio pins the turnBuf reset. Without it the
// buffer is append-only for the whole call, so utterance 2's pass re-sends
// utterance 1's audio too: whisper re-transcribes everything said so far, the
// transcript repeats itself, and each pass costs more than the last.
func TestEachUtteranceSendsOnlyItsOwnAudio(t *testing.T) {
	s := NewSession(context.Background(), "turnbuf-resets",
		WithTurnSink(&countingSink{}), WithSTTInput(NewBufferedChan[STTRequest](sttDispatchDepth)))
	defer s.Close()
	s.setState(StateListening)

	feedAudio(s, 0xAA, 3) // utterance 1: 12 bytes of 0xAA
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance})
	first := drainDispatch(s)

	s.dispatch(SourceSTTResult, STTResult{
		SessionID: s.CallSID, RequestID: s.sttReqID, Kind: FullPass, Text: "first",
	})

	feedAudio(s, 0xBB, 2) // utterance 2: 8 bytes of 0xBB
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance})
	second := drainDispatch(s)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want one request per utterance, got %d and %d", len(first), len(second))
	}
	if want := 12; len(first[0].Audio) != want {
		t.Errorf("utterance 1 audio: %d bytes, want %d", len(first[0].Audio), want)
	}
	if want := 8; len(second[0].Audio) != want {
		t.Errorf("utterance 2 audio: %d bytes, want %d — the turn buffer still holds utterance 1, so every pass re-sends the whole call", len(second[0].Audio), want)
	}
	for i, b := range second[0].Audio {
		if b != 0xBB {
			t.Fatalf("utterance 2 audio byte %d = %#x, want 0xBB: utterance 1's audio leaked into it", i, b)
		}
	}
}

// TestSessionStart_OrderingInvariant pins SOP-154 Observable behavior #4 at
// the real production entry point: Start returns an error if the resolved
// VAD config violates EndSilenceMS < TurnEndSilenceMS < MaxSilenceMS,
// matching Start's existing convention of returning (not panicking on) a
// construction-time failure -- the sibling factory-error branch three lines
// above this check does the same (session.go). There is no public
// SessionOption to inject a bad vadCfg (by design -- production has no such
// seam either), so this reaches into the unexported field directly; the pure
// vadConfig.validateOrdering logic itself is covered by
// TestVADConfig_ValidateOrdering in vad_test.go.
func TestSessionStart_OrderingInvariant(t *testing.T) {
	s := NewSession(context.Background(), "bad-ordering",
		WithVADFactory(func() (VADDetector, error) { return &fakeDetector{}, nil }),
	)
	s.vadCfg = vadConfig{EndSilenceMS: 5000, TurnEndSilenceMS: 5000}

	err := s.Start()
	if err == nil {
		t.Fatal("Start did not return an error on an ordering-invariant-violating config")
	}
	if !strings.Contains(err.Error(), "ordering invariant") {
		t.Errorf("error = %v, want it to mention the ordering invariant", err)
	}
	if s.started {
		t.Error("s.started is true after Start failed; nothing should be marked started")
	}
}

// TestSessionUsesTheWallClockByDefault pins the path production actually
// takes.
//
// Sessions build their facility with NewTimerFacilityWithClock(ctx, s.clock),
// and s.clock is nil unless WithClock sets it -- so real time reaches
// production solely through the constructor's nil guard. Nothing else asserts
// that guard: the facility's own tests construct through NewTimerFacility,
// which no production path uses. Dropping the guard as redundant would leave
// every other test green and ship an engine whose timers never fire, so no
// call would ever hang up.
//
// The timer is armed for an hour and never awaited; only that a real timer
// exists is under test, which costs no time at all.
func TestSessionUsesTheWallClockByDefault(t *testing.T) {
	s := NewSession(context.Background(), "default-clock", WithTurnSink(&countingSink{}))
	defer s.Close()

	if s.clock != nil {
		t.Fatal("a session with no WithClock must leave clock nil, so the facility falls back to the wall clock")
	}
	if s.timerFacility.after == nil {
		t.Fatal("the facility has no clock at all: timers would never fire, so a call would never time out")
	}

	gen := s.timerFacility.Arm(s.ctx, timerIdle, time.Hour)
	if gen < 0 {
		t.Fatal("arming against the default clock failed")
	}
	if !s.timerFacility.IsCurrent(TimerCompletion{Name: timerIdle, TimerID: gen}) {
		t.Error("a timer armed on the default clock is not current")
	}
}

// --- Normalization / stopword match (ticket table) ---

func TestNormalizeTranscript(t *testing.T) {
	tests := []struct {
		transcript string
		normalized string
		match      bool
	}{
		{" Done.", "done", true},
		{" done", "done", true},
		{"Done!", "done", true},
		{"DONE", "done", true},
		{"done,", "done", true},
		{"Okay, done.", "okay, done", false},
		{"I'm done with work.", "i'm done with work", false},
		{"done deal", "done deal", false},
	}
	for _, tt := range tests {
		t.Run(tt.transcript, func(t *testing.T) {
			got := normalizeTranscript(tt.transcript)
			if got != tt.normalized {
				t.Errorf("normalizeTranscript(%q) = %q, want %q", tt.transcript, got, tt.normalized)
			}
			if (got == "done") != tt.match {
				t.Errorf("match for %q: got %v, want %v", tt.transcript, got == "done", tt.match)
			}
		})
	}
}

// --- Fusion joins with a single space, each part trimmed ---

func TestTurnFusion_JoinAndTrim(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, " I hope you're having a good day today."))
	s.sttReqID = 2
	handleSTTResult(s, sttEvent("test-call", 2, FullPass, " I've got a problem with soccer practice today."))

	if len(spy.turns) != 0 {
		t.Fatalf("expected no turn delivery yet, got %d", len(spy.turns))
	}

	s.sttReqID = 3
	handleSTTResult(s, sttEvent("test-call", 3, FullPass, " Done."))

	if len(spy.turns) != 1 {
		t.Fatalf("expected 1 turn delivery, got %d", len(spy.turns))
	}
	want := "I hope you're having a good day today. I've got a problem with soccer practice today."
	if spy.turns[0] != want {
		t.Errorf("delivered text:\n  got:  %q\n  want: %q", spy.turns[0], want)
	}
}

// --- The stopword is excluded from the turn ---

func TestTurnFusion_StopwordExcluded(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, "hello"))
	s.sttReqID = 2
	handleSTTResult(s, sttEvent("test-call", 2, FullPass, "Done."))

	if len(spy.turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(spy.turns))
	}
	if spy.turns[0] != "hello" {
		t.Errorf("delivered: got %q, want %q", spy.turns[0], "hello")
	}
}

// --- Call end flushes without a stopword (idle timer) ---

func TestTurnFusion_IdleTimerFlushes(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, " first part."))
	s.sttReqID = 2
	handleSTTResult(s, sttEvent("test-call", 2, FullPass, " second part."))

	if len(spy.turns) != 0 {
		t.Fatalf("expected no turn delivery yet, got %d", len(spy.turns))
	}

	handleIdleTimer(s, transitionEvent{source: SourceIdleTimer})

	if len(spy.turns) != 1 {
		t.Fatalf("expected 1 turn delivery after idle timer, got %d", len(spy.turns))
	}
	want := "first part. second part."
	if spy.turns[0] != want {
		t.Errorf("delivered: got %q, want %q", spy.turns[0], want)
	}
}

// --- Call end flushes without a stopword (Twilio stop) ---

func TestTurnFusion_TwilioStopFlushes(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, " first part."))
	s.sttReqID = 2
	handleSTTResult(s, sttEvent("test-call", 2, FullPass, " second part."))

	handleControlEvent(s, transitionEvent{
		source:  SourceTwilioControl,
		payload: ControlEvent{Kind: controlKindStop},
	})

	if len(spy.turns) != 1 {
		t.Fatalf("expected 1 turn delivery after stop, got %d", len(spy.turns))
	}
	want := "first part. second part."
	if spy.turns[0] != want {
		t.Errorf("delivered: got %q, want %q", spy.turns[0], want)
	}
}

// --- Empty buffer emits nothing ---

func TestTurnFusion_EmptyBufferNoEmit_Stopword(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, " Done."))

	if len(spy.turns) != 0 {
		t.Fatalf("stopword with nothing before it must not deliver, got %d turns", len(spy.turns))
	}
}

func TestTurnFusion_EmptyBufferNoEmit_CallEnd(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, StopwordPolicy{})
	s.state = StateListening

	handleIdleTimer(s, transitionEvent{source: SourceIdleTimer})

	if len(spy.turns) != 0 {
		t.Fatalf("call end with nothing accumulated must not deliver, got %d turns", len(spy.turns))
	}
}

// --- No policy: accumulates but only flushes on call end ---

func TestTurnFusion_NoPolicyFlushesOnCallEnd(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, nil)
	s.state = StateListening

	s.sttReqID = 1
	handleSTTResult(s, sttEvent("test-call", 1, FullPass, " hello"))

	// "Done" is accumulated as normal text, not a stopword
	s.sttReqID = 2
	handleSTTResult(s, sttEvent("test-call", 2, FullPass, " Done."))

	if len(spy.turns) != 0 {
		t.Fatalf("no policy: expected no turn delivery on 'Done', got %d", len(spy.turns))
	}

	handleControlEvent(s, transitionEvent{
		source:  SourceTwilioControl,
		payload: ControlEvent{Kind: controlKindStop},
	})

	if len(spy.turns) != 1 {
		t.Fatalf("expected 1 turn on call end, got %d", len(spy.turns))
	}
	want := "hello Done."
	if spy.turns[0] != want {
		t.Errorf("delivered: got %q, want %q", spy.turns[0], want)
	}
}

// --- Turn lifecycle (SOP-154 Observable behavior #6) ---

// TestTurnLifecycle_ActiveOnOnsetClearedOnComplete pins the turn lifecycle:
// turnActive is the source of truth for "a turn is in progress." The first
// speech onset of a turn sets it; completeTurn() (called by every
// turn-completion path -- VADTurnEnd, stopword, Twilio stop, idle timeout,
// utterance cap) clears it; and a second onset after completion sets it
// again, proving the flag is reusable across turns rather than a one-shot
// latch.
func TestTurnLifecycle_ActiveOnOnsetClearedOnComplete(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, nil)
	s.state = StateListening

	if s.turnActive {
		t.Fatal("turnActive is true before any speech onset")
	}

	handleSpeechOnset(s, VADEvent{Kind: VADSpeech})
	if !s.turnActive {
		t.Fatal("turnActive is false after speech onset, want true")
	}

	s.dispatch(SourceVADEvent, VADEvent{Kind: VADTurnEnd})
	if s.turnActive {
		t.Fatal("turnActive is true after VADTurnEnd, want false")
	}

	handleSpeechOnset(s, VADEvent{Kind: VADSpeech})
	if !s.turnActive {
		t.Fatal("turnActive is false after a second speech onset following completion, want true")
	}
}

// TestTurnLifecycle_TurnEndDefersWhileFullPassInFlight guards against two
// bugs stacked on top of each other. First: VADTurnEnd being handled by only
// one state's switch -- a full pass can still be in flight
// (StateAwaitingFullResult) when TurnEndSilenceMS of trailing silence
// elapses, and withSpeechReset must intercept VADTurnEnd uniformly across
// every live VAD-consuming state, the same class of bug VADSpeech had before
// withSpeechReset existed (see that wrapper's doc comment). Second, sharper:
// completing the turn immediately in that state would flush turnTranscripts
// and clear turnActive BEFORE the in-flight pass's own text arrives --
// handleSTTResult accumulates FullPass text unconditionally, so that text
// would land in the buffer for whatever turn is active when the (now late)
// result shows up, silently bleeding trailing words into the next turn. This
// test drives both halves: VADTurnEnd defers completion, and the pass's
// result -- once delivered -- finishes it with its own text included, not
// dropped and not misattributed.
func TestTurnLifecycle_TurnEndDefersWhileFullPassInFlight(t *testing.T) {
	spy := &turnSpy{}
	s := newTestSession(spy, nil)
	s.state = StateAwaitingFullResult
	s.turnActive = true
	s.sttReqID = 1
	s.turnTranscripts = []string{"hello"}

	got := transitions[StateAwaitingFullResult][SourceVADEvent](s, transitionEvent{
		source:  SourceVADEvent,
		payload: VADEvent{Kind: VADTurnEnd},
	})

	if !s.turnActive {
		t.Fatal("turnActive is false immediately after VADTurnEnd with a pass in flight, want true (completion deferred)")
	}
	if !s.turnEndPending {
		t.Fatal("turnEndPending is false after VADTurnEnd with a pass in flight, want true")
	}
	if len(spy.turns) != 0 {
		t.Fatalf("delivered turns before the in-flight pass returns: got %v, want none (nothing flushed yet)", spy.turns)
	}
	if got != StateAwaitingFullResult {
		t.Errorf("state after VADTurnEnd: got %s, want AwaitingFullResult (the in-flight pass is untouched)", got)
	}

	// The deferred pass returns: its text must land in the SAME turn, not be
	// dropped and not bleed into whatever turn starts next.
	next := handleSTTResult(s, sttEvent("test-call", 1, FullPass, "world"))

	if s.turnActive {
		t.Error("turnActive is true after the deferred pass returned, want false")
	}
	if s.turnEndPending {
		t.Error("turnEndPending is true after the deferred pass returned, want false")
	}
	if len(spy.turns) != 1 || spy.turns[0] != "hello world" {
		t.Errorf("delivered turns: got %v, want [\"hello world\"] (the in-flight pass's own text must be included, not lost)", spy.turns)
	}
	// AATK-24: turn completion now enters StateAwaitingResponse (a response
	// is being generated), not Listening.
	if next != StateAwaitingResponse {
		t.Errorf("state after the deferred pass returned: got %s, want AwaitingResponse", next)
	}
}
