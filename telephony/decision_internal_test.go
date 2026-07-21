package telephony

import (
	"context"
	"strings"
	"testing"
)

// mockDecRecorder is an internal DecisionRecorder that captures events. The
// internal tests drive s.dispatch synchronously on the test goroutine (no
// Start, no run loop), so no locking is needed.
type mockDecRecorder struct {
	events []DecisionEvent
}

func (m *mockDecRecorder) Record(ev DecisionEvent) { m.events = append(m.events, ev) }
func (m *mockDecRecorder) Close() error            { return nil }

func filterDecisionKind(evs []DecisionEvent, kind string) []DecisionEvent {
	var out []DecisionEvent
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// newDecTestSession builds a session wired with rec, with a resolved default
// VAD config (Start would normally fill it; these tests dispatch directly), so
// param_value assertions see the real thresholds.
func newDecTestSession(t *testing.T, rec DecisionRecorder, opts ...SessionOption) *Session {
	t.Helper()
	s := NewSession(context.Background(), "dec-test", append([]SessionOption{WithDecisionRecorder(rec)}, opts...)...)
	s.vadCfg = defaultVADConfig()
	return s
}

// TestDecisionRecorder_SpeechOnset — VADSpeech records a speech-start decision
// naming SpeechThresh, with the crossing prob and audio position.
func TestDecisionRecorder_SpeechOnset(t *testing.T) {
	rec := &mockDecRecorder{}
	s := newDecTestSession(t, rec)
	defer s.Close()
	s.setState(StateListening)

	s.dispatch(SourceVADEvent, VADEvent{Kind: VADSpeech, Prob: 0.9, StreamWindowIndex: 3})

	evs := filterDecisionKind(rec.events, DecisionKindSpeechStart)
	if len(evs) != 1 {
		t.Fatalf("speech-start events: got %d, want 1 (%+v)", len(evs), rec.events)
	}
	e := evs[0]
	if e.Type != DecisionTypeVAD || e.Param != DecisionParamSpeechThresh {
		t.Errorf("type/param: got %q/%q, want %q/%q", e.Type, e.Param, DecisionTypeVAD, DecisionParamSpeechThresh)
	}
	if e.ParamValue != s.vadCfg.SpeechThresh {
		t.Errorf("param_value: got %v, want %v (SpeechThresh)", e.ParamValue, s.vadCfg.SpeechThresh)
	}
	if e.Prob != 0.9 {
		t.Errorf("prob: got %v, want 0.9", e.Prob)
	}
	if want := 3 * s.vadCfg.windowMS(); e.AudioMS != want {
		t.Errorf("audio_ms: got %d, want %d (StreamWindowIndex*windowMS)", e.AudioMS, want)
	}
	if e.Effect != "utterance opened" {
		t.Errorf("effect: got %q, want %q", e.Effect, "utterance opened")
	}
}

// TestDecisionRecorder_Silence — VADSilence records a silence decision naming
// SilenceThresh.
func TestDecisionRecorder_Silence(t *testing.T) {
	rec := &mockDecRecorder{}
	s := newDecTestSession(t, rec)
	defer s.Close()
	s.setState(StateListening)

	s.dispatch(SourceVADEvent, VADEvent{Kind: VADSilence, Prob: 0.2, StreamWindowIndex: 8, SilenceCount: 1})

	evs := filterDecisionKind(rec.events, DecisionKindSilence)
	if len(evs) != 1 {
		t.Fatalf("silence events: got %d, want 1 (%+v)", len(evs), rec.events)
	}
	e := evs[0]
	if e.Param != DecisionParamSilenceThresh || e.ParamValue != s.vadCfg.SilenceThresh {
		t.Errorf("param/value: got %q/%v, want %q/%v", e.Param, e.ParamValue, DecisionParamSilenceThresh, s.vadCfg.SilenceThresh)
	}
	if e.SilenceCount != 1 {
		t.Errorf("silence_count: got %d, want 1", e.SilenceCount)
	}
	if want := 8 * s.vadCfg.windowMS(); e.AudioMS != want {
		t.Errorf("audio_ms: got %d, want %d", e.AudioMS, want)
	}
}

// TestDecisionRecorder_TurnEnd — a silence-driven VADTurnEnd records a turn-end
// decision naming TurnEndSilenceMS.
func TestDecisionRecorder_TurnEnd(t *testing.T) {
	rec := &mockDecRecorder{}
	s := newDecTestSession(t, rec)
	defer s.Close()
	s.setState(StateListening)

	// SilenceCount is injected test input here (this drives the machine's event
	// directly, not the threshold-crossing); the recorder copies it through.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADTurnEnd, Prob: 0.02, StreamWindowIndex: 40, SilenceCount: 160})

	evs := filterDecisionKind(rec.events, DecisionKindTurnEnd)
	if len(evs) != 1 {
		t.Fatalf("turn-end events: got %d, want 1 (%+v)", len(evs), rec.events)
	}
	e := evs[0]
	if e.Type != DecisionTypeVAD || e.Param != DecisionParamTurnEndSilence {
		t.Errorf("type/param: got %q/%q, want %q/%q", e.Type, e.Param, DecisionTypeVAD, DecisionParamTurnEndSilence)
	}
	if e.ParamValue != s.vadCfg.TurnEndSilenceMS {
		t.Errorf("param_value: got %v, want %v (TurnEndSilenceMS)", e.ParamValue, s.vadCfg.TurnEndSilenceMS)
	}
	if want := 40 * s.vadCfg.windowMS(); e.AudioMS != want {
		t.Errorf("audio_ms: got %d, want %d", e.AudioMS, want)
	}
	if e.SilenceCount != 160 {
		t.Errorf("silence_count: got %d, want 160 (injected)", e.SilenceCount)
	}
	if e.Effect != "turn closed (silence-turn-end)" {
		t.Errorf("effect: got %q", e.Effect)
	}
}

// TestDecisionRecorder_DeferredTurnEndRecordedOnce — a VADTurnEnd that arrives
// while a FullPass is still in flight is deferred; it must be recorded exactly
// once, at the point the pass resolves and the turn actually completes.
func TestDecisionRecorder_DeferredTurnEndRecordedOnce(t *testing.T) {
	rec := &mockDecRecorder{}
	sttIn := NewBufferedChan[STTRequest](10)
	s := newDecTestSession(t, rec, WithSTTInput(sttIn))
	defer s.Close()
	s.setState(StateListening)

	// Utterance ends -> dispatch a pass (sttReqID 1), enter AwaitingFullResult.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance, StreamWindowIndex: 10})
	if s.State() != StateAwaitingFullResult {
		t.Fatalf("after EOU: state %s, want StateAwaitingFullResult", s.State())
	}
	// Turn-end arrives while the pass is still in flight: deferred, not yet
	// recorded.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADTurnEnd, StreamWindowIndex: 20, SilenceCount: 160})
	if got := len(filterDecisionKind(rec.events, DecisionKindTurnEnd)); got != 0 {
		t.Fatalf("turn-end recorded before the pass resolved: got %d, want 0", got)
	}
	// Pass resolves -> the deferred turn-end completes and is recorded once.
	s.dispatch(SourceSTTResult, STTResult{SessionID: s.CallSID, RequestID: 1, Kind: FullPass, Text: "hello"})
	if got := len(filterDecisionKind(rec.events, DecisionKindTurnEnd)); got != 1 {
		t.Fatalf("turn-end after the pass resolved: got %d, want exactly 1 (%+v)", got, rec.events)
	}
}

// TestDecisionRecorder_DroppedUtteranceRecorded — when a second utterance ends
// while the first FullPass is still in flight (StateAwaitingFullResult), the
// dropped end-of-utterance is recorded too, distinctly from the dispatched one.
func TestDecisionRecorder_DroppedUtteranceRecorded(t *testing.T) {
	rec := &mockDecRecorder{}
	sttIn := NewBufferedChan[STTRequest](10)
	s := newDecTestSession(t, rec, WithSTTInput(sttIn))
	defer s.Close()
	s.setState(StateListening)

	// First utterance ends: dispatches a pass (sttReqID -> 1) and enters
	// StateAwaitingFullResult.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance, StreamWindowIndex: 10})
	if s.State() != StateAwaitingFullResult {
		t.Fatalf("after first EOU: state %s, want StateAwaitingFullResult", s.State())
	}
	// Second utterance ends while that pass is still in flight (no STT result
	// was delivered): dropped, but still recorded.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance, StreamWindowIndex: 30})

	eou := filterDecisionKind(rec.events, DecisionKindEndOfUtter)
	var dispatched, dropped int
	for _, e := range eou {
		if strings.Contains(e.Effect, "dispatched") {
			dispatched++
		}
		if strings.Contains(e.Effect, "dropped") {
			dropped++
		}
	}
	if dispatched != 1 {
		t.Errorf("dispatched end-of-utterance events: got %d, want 1 (%+v)", dispatched, eou)
	}
	if dropped != 1 {
		t.Errorf("dropped end-of-utterance events: got %d, want 1 (%+v)", dropped, eou)
	}
}

// TestDecisionRecorder_StopwordTurnEnd — a turn completed by the stopword
// records a turn-end decision (effect names the stopword cause), filling the
// log gap that would otherwise leave stopword turns unrecorded (SOP-168).
func TestDecisionRecorder_StopwordTurnEnd(t *testing.T) {
	rec := &mockDecRecorder{}
	sttIn := NewBufferedChan[STTRequest](10)
	s := newDecTestSession(t, rec, WithSTTInput(sttIn), WithTurnEndPolicy(StopwordPolicy{}))
	defer s.Close()
	s.setState(StateListening)

	// Utterance ends -> dispatch a pass (sttReqID 1), enter AwaitingFullResult.
	s.dispatch(SourceVADEvent, VADEvent{Kind: VADEndOfUtterance, StreamWindowIndex: 10})
	// The STT result is the stopword -> the turn completes on the stopword.
	s.dispatch(SourceSTTResult, STTResult{SessionID: s.CallSID, RequestID: 1, Kind: FullPass, Text: "done"})

	te := filterDecisionKind(rec.events, DecisionKindTurnEnd)
	if len(te) != 1 {
		t.Fatalf("turn-end decisions: got %d, want 1 (%+v)", len(te), rec.events)
	}
	// Assert the meaningful contract (a turn-end naming the stopword cause), not
	// the event's Type -- the type taxonomy is SOP-171's to restructure.
	if !strings.Contains(te[0].Effect, "stopword") {
		t.Errorf("Effect: got %q, want a mention of the stopword cause", te[0].Effect)
	}
}

// TestComputeTotals pins the per-recording header totals (SOP-168): utterances
// from end-of-utterance events, turns from every recorded turn completion
// (silence-turn-end, stopword, turn-cap, utterance-cap) but NOT idle-timeout,
// and STT calls from stt_dispatch events.
func TestComputeTotals(t *testing.T) {
	events := []DecisionEvent{
		{Type: DecisionTypeVAD, Kind: DecisionKindSpeechStart},
		{Type: DecisionTypeVAD, Kind: DecisionKindEndOfUtter},
		{Type: DecisionTypeSTTDispatch},
		{Type: DecisionTypeSTTResult},
		{Type: DecisionTypeVAD, Kind: DecisionKindTurnEnd},                                   // silence turn-end
		{Type: DecisionTypeVAD, Kind: DecisionKindTurnEnd, Effect: "turn closed (stopword)"}, // stopword turn-end
		{Type: DecisionTypeCap, Kind: DecisionKindTurnCap},
		{Type: DecisionTypeCap, Kind: DecisionKindUtteranceCap},
		{Type: DecisionTypeCap, Kind: DecisionKindIdleTimeout}, // call-end, NOT a turn
		{Type: DecisionTypeVAD, Kind: DecisionKindEndOfUtter},  // second utterance
	}
	got := computeTotals(events)
	if got.Events != len(events) {
		t.Errorf("Events: got %d, want %d", got.Events, len(events))
	}
	if got.Utterances != 2 {
		t.Errorf("Utterances: got %d, want 2", got.Utterances)
	}
	if got.Turns != 4 {
		t.Errorf("Turns: got %d, want 4 (2 turn-end + turn-cap + utterance-cap; idle excluded)", got.Turns)
	}
	if got.STTCalls != 1 {
		t.Errorf("STTCalls: got %d, want 1", got.STTCalls)
	}
}

// TestVADEvent_StreamWindowIndexMonotonic drives the vadMachine across two
// utterances and asserts StreamWindowIndex is the never-reset stream clock
// while WindowIndex still resets to 0 at each speech onset. windowMS is 32 at
// the mu-law default (256 samples / 8000 Hz), the audio-position unit
// DecisionEvent.AudioMS is built on.
func TestVADEvent_StreamWindowIndexMonotonic(t *testing.T) {
	m := newVADMachine(defaultVADConfig())

	if got := m.windowMS(); got != 32 {
		t.Fatalf("windowMS: got %d, want 32", got)
	}

	endSil := windowsToCross(m.cfg.EndSilenceMS, m.windowMS())

	// Utterance 1: onset + speech, then enough trailing silence to end the
	// utterance; a short gap; utterance 2: onset + speech.
	var probs []float32
	probs = append(probs, 0.9, 0.9, 0.9)
	for i := 0; i < endSil+2; i++ {
		probs = append(probs, 0.0)
	}
	probs = append(probs, 0.9, 0.9)

	var events []VADEvent
	for _, p := range probs {
		if ev, ok := m.step(p); ok {
			events = append(events, ev)
		}
	}

	var onsets []VADEvent
	for _, e := range events {
		if e.Kind == VADSpeech {
			onsets = append(onsets, e)
		}
	}
	if len(onsets) < 2 {
		t.Fatalf("want >=2 speech onsets, got %d (events=%+v)", len(onsets), events)
	}

	// WindowIndex resets to 0 at each onset...
	if onsets[0].WindowIndex != 0 || onsets[1].WindowIndex != 0 {
		t.Errorf("WindowIndex at onsets: got %d, %d; want 0, 0", onsets[0].WindowIndex, onsets[1].WindowIndex)
	}
	// ...but StreamWindowIndex does not: the second onset is strictly later.
	if onsets[1].StreamWindowIndex <= onsets[0].StreamWindowIndex {
		t.Errorf("StreamWindowIndex did not advance across onsets: %d then %d",
			onsets[0].StreamWindowIndex, onsets[1].StreamWindowIndex)
	}

	// Monotonic non-decreasing across every emitted event.
	for i := 1; i < len(events); i++ {
		if events[i].StreamWindowIndex < events[i-1].StreamWindowIndex {
			t.Errorf("StreamWindowIndex decreased at %d: %d then %d",
				i, events[i-1].StreamWindowIndex, events[i].StreamWindowIndex)
		}
	}
}
