package telephony_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// mockRecorder is a DecisionRecorder that captures events for assertions.
type mockRecorder struct {
	mu     sync.Mutex
	events []telephony.DecisionEvent
	closed bool
}

func (m *mockRecorder) Record(ev telephony.DecisionEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *mockRecorder) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockRecorder) all() []telephony.DecisionEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]telephony.DecisionEvent(nil), m.events...)
}

// startSession builds a Session with the given options, starts it, and fails
// the test on a start error -- the setup repeated at the top of every
// decision-recorder test in this file.
func startSession(t *testing.T, sessionID string, opts ...telephony.SessionOption) *telephony.Session {
	t.Helper()
	s := telephony.NewSession(context.Background(), sessionID, opts...)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// pumpSilenceFrames sends n windowFrame(0x80) frames spaced 2ms apart -- the
// speech-then-silence drive repeated (with varying counts and timeouts)
// across the decision-recorder tests.
func pumpSilenceFrames(t *testing.T, dataIn telephony.TwilioDataPlaneInput, n int, timeout time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		sendData(t, dataIn, windowFrame(0x80), timeout)
		time.Sleep(2 * time.Millisecond)
	}
}

// filterByKind returns the events of the given Kind, preserving order.
func filterByKind(evs []telephony.DecisionEvent, kind string) []telephony.DecisionEvent {
	var out []telephony.DecisionEvent
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// waitTurnComplete blocks until the sink has recorded at least one turn, with
// a deadlock backstop a passing test never reaches.
func waitTurnComplete(t *testing.T, sink *spySink) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sink.turnTexts()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if len(sink.turnTexts()) == 0 {
		t.Fatal("turn did not complete within the backstop")
	}
}

// driveOneTurn drives one complete user turn through a started session's wired
// channels: an utterance to its end, the STT result carrying text for the pass
// that utterance dispatched, then held silence until the turn closes. Shared by
// every test that needs a session with exactly one completed turn behind it.
func driveOneTurn(
	t *testing.T,
	sid string,
	dataIn *telephony.BufferedChan[[]byte],
	sttIn *telephony.BufferedChan[telephony.STTRequest],
	sttOut *telephony.BufferedChan[telephony.STTResult],
	sink *spySink,
	text string,
) {
	t.Helper()
	pumpSilenceFrames(t, dataIn, telephony.EndSilenceWindows()+2, recvTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: sid, RequestID: req.RequestID, Kind: telephony.FullPass, Text: text,
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the result settle before silence resumes
	pumpSilenceFrames(t, dataIn, telephony.TurnEndSilenceWindows()+11, recvTimeout)
	waitTurnComplete(t, sink)
}

// fieldCheck pairs a precomputed assertion outcome with its failure message,
// letting a run of independent field checks be table-driven instead of a
// chain of if statements.
type fieldCheck struct {
	ok  bool
	msg string
}

// checkFields reports every failing check via t.Error, preserving the
// "check everything, report every failure" behavior of a chain of t.Errorf
// calls.
func checkFields(t *testing.T, checks []fieldCheck) {
	t.Helper()
	for _, c := range checks {
		if !c.ok {
			t.Error(c.msg)
		}
	}
}

// TestDecisionRecorder_EndOfUtterance drives one utterance through a live
// session and asserts exactly one end-of-utterance DecisionEvent is recorded,
// naming EndSilenceMS, its resolved value, an audio-position time, and the STT
// request the utterance dispatched.
func TestDecisionRecorder_EndOfUtterance(t *testing.T) {
	rec := &mockRecorder{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	probs := speechThenSilenceProbs(1, telephony.EndSilenceWindows())
	s := startSession(t, "test-decrec",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithDecisionRecorder(rec),
	)
	defer s.Close()

	pumpSilenceFrames(t, dataIn, telephony.EndSilenceWindows()+2, 5*time.Second)

	// The STT dispatch confirms VADEndOfUtterance fired; recordEndOfUtterance
	// runs right after it on the same sequencer goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received (end-of-utterance never fired): %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the record settle after the enqueue

	// A single utterance now records more than the end-of-utterance decision
	// (M2 added speech-start + silence), so pick out the EOU event rather than
	// asserting a total count.
	evs := rec.all()
	eou := filterByKind(evs, telephony.DecisionKindEndOfUtter)
	if len(eou) != 1 {
		t.Fatalf("end-of-utterance events: got %d, want 1 (all: %+v)", len(eou), evs)
	}
	e := eou[0]
	want := telephony.DefaultVADConfig().EndSilenceMS
	checkFields(t, []fieldCheck{
		{e.Type == telephony.DecisionTypeVAD, fmt.Sprintf("Type: got %q, want %q", e.Type, telephony.DecisionTypeVAD)},
		{e.Kind == telephony.DecisionKindEndOfUtter, fmt.Sprintf("Kind: got %q, want %q", e.Kind, telephony.DecisionKindEndOfUtter)},
		{e.Param == telephony.DecisionParamEndSilence, fmt.Sprintf("Param: got %q, want %q", e.Param, telephony.DecisionParamEndSilence)},
		{e.ParamValue == want, fmt.Sprintf("ParamValue: got %v, want %d", e.ParamValue, want)},
		{e.RequestID == req.RequestID, fmt.Sprintf("RequestID: got %d, want %d", e.RequestID, req.RequestID)},
		{e.AudioMS > 0 && e.AudioMS%32 == 0, fmt.Sprintf("AudioMS: got %d, want a positive multiple of 32 (window-clock ms)", e.AudioMS)},
		{strings.Contains(e.Effect, "STT request"), fmt.Sprintf("Effect: got %q, want a mention of the dispatched STT request", e.Effect)},
	})
}

// waitCapEvent blocks until exactly one type="cap" DecisionEvent of the given
// kind has been recorded, and returns it. Polls rec (the cap is recorded on the
// session's sequencer goroutine, inside completeTurn, before terminateWithClip)
// with a deadlock backstop a passing test never reaches. Asserts on the literal
// wire values ("cap", the kind strings) so the test pins the recorded JSON
// contract directly and compiles against current code — where nothing records a
// cap decision, so the backstop is the RED signal (M4 / SOP-166).
func waitCapEvent(t *testing.T, rec *mockRecorder, kind string) telephony.DecisionEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var found []telephony.DecisionEvent
		for _, ev := range rec.all() {
			if ev.Type == "cap" && ev.Kind == kind {
				found = append(found, ev)
			}
		}
		switch {
		case len(found) == 1:
			return found[0]
		case len(found) > 1:
			t.Fatalf("cap events of kind %q: got %d, want 1 (all: %+v)", kind, len(found), rec.all())
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %q cap event recorded within the backstop (all: %+v)", kind, rec.all())
	return telephony.DecisionEvent{}
}

// TestDecisionRecorder_UtteranceCap drives a caller past the per-utterance cap
// and asserts the forced utterance-end is recorded as a type="cap" decision
// naming MaxUtteranceMS, its resolved (overridden) value, and an audio position
// carried from the last VAD window — the cap fires from a timer, not a VADEvent,
// so the position comes from the session's tracked stream-window (M4 behavior 4).
func TestDecisionRecorder_UtteranceCap(t *testing.T) {
	rec := &mockRecorder{}
	clock := newFakeClock()
	det := &fakeDetector{probs: voicedProbs(50)}
	data := telephony.NewBufferedChan[[]byte](8)
	sink := &spySink{}
	s := telephony.NewSession(context.Background(), "deccap-utt",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithClock(clock.after),
		telephony.WithMaxUtteranceMS(utteranceTestMS),
		telephony.WithMaxSilenceMS(idleTestMS),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < 3; i++ { // onset: begins the utterance, arms the cap
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart(t, sink) // ensures a VAD window (hence a stream position) was seen

	clock.fire(t, utteranceTestDur()) // the cap expires while still speaking

	e := waitCapEvent(t, rec, "utterance-cap")
	if e.Param != "MaxUtteranceMS" {
		t.Errorf("Param: got %q, want MaxUtteranceMS", e.Param)
	}
	if e.ParamValue != utteranceTestMS {
		t.Errorf("ParamValue: got %v, want %d (the resolved override)", e.ParamValue, utteranceTestMS)
	}
	if e.AudioMS <= 0 || e.AudioMS%32 != 0 {
		t.Errorf("AudioMS: got %d, want a positive multiple of 32 (last-window position after speech)", e.AudioMS)
	}
	if !strings.Contains(e.Effect, "forced utterance end") {
		t.Errorf("Effect: got %q, want a mention of the forced utterance end", e.Effect)
	}
}

// TestDecisionRecorder_TurnCap drives a caller past the whole-turn cap and
// asserts the forced turn-end is recorded as a type="cap" decision naming
// MaxTurnMS and its resolved value.
func TestDecisionRecorder_TurnCap(t *testing.T) {
	rec := &mockRecorder{}
	clock := newFakeClock()
	det := &fakeDetector{probs: voicedProbs(50)}
	data := telephony.NewBufferedChan[[]byte](8)
	sink := &spySink{}
	s := telephony.NewSession(context.Background(), "deccap-turn",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithMaxTurnMS(turnTestMS),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < 3; i++ { // onset: opens the turn, arms the turn cap
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart(t, sink)

	clock.fire(t, turnTestDur()) // the whole-turn cap expires

	e := waitCapEvent(t, rec, "turn-cap")
	if e.Param != "MaxTurnMS" {
		t.Errorf("Param: got %q, want MaxTurnMS", e.Param)
	}
	if e.ParamValue != turnTestMS {
		t.Errorf("ParamValue: got %v, want %d (the resolved override)", e.ParamValue, turnTestMS)
	}
	if e.AudioMS <= 0 || e.AudioMS%32 != 0 {
		t.Errorf("AudioMS: got %d, want a positive multiple of 32 (last-window position after speech)", e.AudioMS)
	}
	if !strings.Contains(e.Effect, "forced turn end") {
		t.Errorf("Effect: got %q, want a mention of the forced turn end", e.Effect)
	}
}

// TestDecisionRecorder_IdleTimeout fires the idle/silence timer with no speech
// and asserts the call-ending timeout is recorded as a type="cap" decision
// naming MaxSilenceMS. ParamValue is the production MaxSilenceMS constant, NOT
// the WithMaxSilenceMS test-seam override (which only exists to make the timer
// fire fast) — the idle cap is not an operator-tunable knob, unlike the
// utterance/turn caps (M4 behavior 3). The injected clock fires the timer, so no
// real 15 s wait.
func TestDecisionRecorder_IdleTimeout(t *testing.T) {
	rec := &mockRecorder{}
	clock := newFakeClock()
	det := &fakeDetector{}
	s := telephony.NewSession(context.Background(), "deccap-idle",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleTestMS),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	clock.fire(t, idleTestDur()) // silence deadline reached with no speech at all

	e := waitCapEvent(t, rec, "idle-timeout")
	if e.Param != "MaxSilenceMS" {
		t.Errorf("Param: got %q, want MaxSilenceMS", e.Param)
	}
	// AATK-24 (D8 silence-knob production-ization, Addendum V3): the idle
	// cap decision now follows the resolved bound (the WithMaxSilenceMS
	// override, here idleTestMS), not the bare MaxSilenceMS constant --
	// distinct from Session.Start's validateOrdering check, which is
	// unaffected and still validates against the bare constant.
	if e.ParamValue != idleTestMS {
		t.Errorf("ParamValue: got %v, want %d (the resolved override, not the bare MaxSilenceMS constant)", e.ParamValue, idleTestMS)
	}
	if !strings.Contains(e.Effect, "idle") {
		t.Errorf("Effect: got %q, want a mention of the idle call-end", e.Effect)
	}
}

// fakeNow is a mutable monotonic clock for the injected decision clock
// (WithDecisionClock). A test advances it explicitly between a dispatch and its
// result so the recorded latency is exact and deterministic -- no wall clock.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeNow() *fakeNow { return &fakeNow{t: time.Unix(0, 0)} }

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// waitEventType blocks until at least one DecisionEvent of the given type has
// been recorded and returns the first, with a deadlock backstop a passing test
// never reaches. Used to observe the stt_dispatch / stt_result records, each of
// which appears once on the single-utterance path (M5 / SOP-167).
func waitEventType(t *testing.T, rec *mockRecorder, typ string) telephony.DecisionEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range rec.all() {
			if ev.Type == typ {
				return ev
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %q event recorded within the backstop (all: %+v)", typ, rec.all())
	return telephony.DecisionEvent{}
}

// TestDecisionRecorder_STTDispatchAndResult drives one utterance to an STT
// dispatch, advances the injected clock by a known delta, then delivers the
// matching STTResult, and asserts both the stt_dispatch and stt_result decisions
// are recorded -- the latter correlated by request id and carrying the exact
// latency, the transcript, and whisper's own audio duration (M5 / SOP-167).
func TestDecisionRecorder_STTDispatchAndResult(t *testing.T) {
	const sessionID = "test-sttdec"
	rec := &mockRecorder{}
	fnow := newFakeNow()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	probs := speechThenSilenceProbs(1, telephony.EndSilenceWindows())
	s := startSession(t, sessionID,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithDecisionRecorder(rec),
		telephony.WithDecisionClock(fnow.now),
	)
	defer s.Close()

	pumpSilenceFrames(t, dataIn, telephony.EndSilenceWindows()+2, 5*time.Second)

	// The dispatch appears on sttIn; receiving it proves dispatchSTT (and its
	// stt_dispatch record + stored dispatch instant) have already run.
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received (end-of-utterance never dispatched): %v", err)
	}

	dispatch := waitEventType(t, rec, "stt_dispatch")
	checkFields(t, []fieldCheck{
		{dispatch.RequestID == req.RequestID, fmt.Sprintf("stt_dispatch RequestID: got %d, want %d", dispatch.RequestID, req.RequestID)},
		{dispatch.AudioBytes > 0, fmt.Sprintf("stt_dispatch AudioBytes: got %d, want > 0", dispatch.AudioBytes)},
	})

	// Advance the clock, then deliver the result: latency must be exactly the delta.
	fnow.advance(800 * time.Millisecond)
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: sessionID,
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello world",
		Duration:  1.5,
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	result := waitEventType(t, rec, "stt_result")
	checkFields(t, []fieldCheck{
		{result.RequestID == req.RequestID, fmt.Sprintf("stt_result RequestID: got %d, want %d", result.RequestID, req.RequestID)},
		{result.LatencyMS == 800, fmt.Sprintf("stt_result LatencyMS: got %d, want 800", result.LatencyMS)},
		{result.Text == "hello world", fmt.Sprintf("stt_result Text: got %q, want %q", result.Text, "hello world")},
		{result.STTDurSec == 1.5, fmt.Sprintf("stt_result STTDurSec: got %v, want 1.5", result.STTDurSec)},
	})
}

// TestTranscriptSummary_WrittenAtClose drives one full user turn (utterance ->
// STT result -> silence turn-end) and asserts the conversation transcript
// summary is both printed to the live writer and written to <sid>.transcript.txt
// at Close, with the turn's utterance bracketed (SOP-168).
func TestTranscriptSummary_WrittenAtClose(t *testing.T) {
	dir := t.TempDir()
	const sid = "test-transcript"
	var live bytes.Buffer

	probs := speechThenSilenceProbs(1, telephony.TurnEndSilenceWindows()+10)
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	s := startSession(t, sid,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTranscriptOutput(dir, sid, &live),
	)

	driveOneTurn(t, sid, dataIn, sttIn, sttOut, sink, "hello world")

	s.Close()

	const want = "user  --> [hello world]\n"
	if live.String() != want {
		t.Errorf("live transcript: got %q, want %q", live.String(), want)
	}
	got, err := os.ReadFile(filepath.Join(dir, sid+".transcript.txt"))
	if err != nil {
		t.Fatalf("read transcript file: %v", err)
	}
	if string(got) != want {
		t.Errorf("transcript file: got %q, want %q", string(got), want)
	}
}

// TestWithFileDecisionRecorderFromEnv_Gating covers the AATOOLKIT_EVENT_LOG
// gate (DoD: "no files written and the no-op recorder is used" when off or no
// dir). Close flushes even a session that never Started, and the recorder
// writes its header on Close regardless of event count, so no audio-driving is
// needed to observe whether a recorder was wired.
func TestWithFileDecisionRecorderFromEnv_Gating(t *testing.T) {
	newSess := func(dir string) *telephony.Session {
		return telephony.NewSession(context.Background(), "CAgate",
			telephony.WithFileDecisionRecorderFromEnv(dir, "MZgate", "CAgate", "sim", telephony.DefaultVADConfig(), io.Discard))
	}

	t.Run("enabled with a dir writes the record", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AATOOLKIT_EVENT_LOG", "1")
		newSess(dir).Close()
		if _, err := os.Stat(filepath.Join(dir, "MZgate.events.header.json")); err != nil {
			t.Errorf("enabled: expected header written, got %v", err)
		}
	})

	t.Run("disabled writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AATOOLKIT_EVENT_LOG", "0")
		newSess(dir).Close()
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("disabled: expected no files, got %d", len(entries))
		}
	})

	t.Run("enabled but no dir writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AATOOLKIT_EVENT_LOG", "on")
		// dir "" -> no-op option; a stray dir is passed only to prove nothing
		// is written to it.
		telephony.NewSession(context.Background(), "CAgate",
			telephony.WithFileDecisionRecorderFromEnv("", "MZgate", "CAgate", "sim", telephony.DefaultVADConfig(), io.Discard)).Close()
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("no dir: expected no files, got %d", len(entries))
		}
	})
}

// flushFixture is a fresh FileDecisionRecorder (its own temp dir and live
// buffer) with the same two-event fixture already recorded. Each
// TestFileRecorder_FlushAndLiveFeed subtest builds its own so it is runnable
// and correct in isolation.
type flushFixture struct {
	dir  string
	live *bytes.Buffer
	r    *telephony.FileDecisionRecorder
	in   []telephony.DecisionEvent
}

// newFlushFixture builds a flushFixture: a new recorder over a new temp dir,
// with the fixture's two events already recorded. ParamValue/AudioMS in the
// fixture are arbitrary inputs the recorder serializes verbatim — NOT the VAD
// default (which is DefaultVADConfig().EndSilenceMS) -- any values work here,
// so they are deliberately literals, decoupled from the tuned default.
func newFlushFixture(t *testing.T) flushFixture {
	t.Helper()
	dir := t.TempDir()
	var live bytes.Buffer
	r := telephony.NewFileDecisionRecorder(dir, "MZstream1", "CAcall1", "sim", telephony.DefaultVADConfig(), &live)
	if r == nil {
		t.Fatal("NewFileDecisionRecorder returned nil for a non-empty dir")
	}
	in := []telephony.DecisionEvent{
		{Type: "vad", Kind: "end-of-utterance", Param: "EndSilenceMS", ParamValue: 700, AudioMS: 640, RequestID: 1, Effect: "utterance closed; dispatched STT request 1"},
		{Type: "vad", Kind: "end-of-utterance", Param: "EndSilenceMS", ParamValue: 700, AudioMS: 1280, RequestID: 2, Effect: "utterance closed; dispatched STT request 2"},
	}
	for _, e := range in {
		r.Record(e)
	}
	return flushFixture{dir: dir, live: &live, r: r, in: in}
}

// TestFileRecorder_FlushAndLiveFeed checks the concrete recorder both live-feeds
// each event to its writer as it arrives and, on Close, flushes a homogeneous
// JSONL plus a separate header file. Split into one subtest per property --
// the properties are independent (live-feed, jsonl flush, header contents,
// close-idempotency) -- each subtest builds its own fixture and drives it to
// whatever state (Close or not) it needs, so each runs correctly in isolation.
func TestFileRecorder_FlushAndLiveFeed(t *testing.T) {
	t.Run("live-feeds each event as it arrives", func(t *testing.T) {
		f := newFlushFixture(t)
		if got := strings.Count(f.live.String(), "\n"); got != len(f.in) {
			t.Errorf("live feed lines: got %d, want %d\n%s", got, len(f.in), f.live.String())
		}
	})

	t.Run("flushes a homogeneous jsonl on close", func(t *testing.T) {
		f := newFlushFixture(t)
		if err := f.r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(f.dir, "MZstream1.events.jsonl"))
		if err != nil {
			t.Fatalf("read jsonl: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != len(f.in) {
			t.Fatalf("jsonl lines: got %d, want %d", len(lines), len(f.in))
		}
		for i, ln := range lines {
			var ev telephony.DecisionEvent
			if err := json.Unmarshal([]byte(ln), &ev); err != nil {
				t.Fatalf("line %d is not valid json: %v", i, err)
			}
			if ev.Seq != i+1 {
				t.Errorf("line %d seq: got %d, want %d", i, ev.Seq, i+1)
			}
		}
	})

	t.Run("writes a header file on close", func(t *testing.T) {
		f := newFlushFixture(t)
		if err := f.r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		hdrData, err := os.ReadFile(filepath.Join(f.dir, "MZstream1.events.header.json"))
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		var hdr map[string]any
		if err := json.Unmarshal(hdrData, &hdr); err != nil {
			t.Fatalf("header is not valid json: %v", err)
		}
		if hdr["call_sid"] != "CAcall1" {
			t.Errorf("header call_sid: got %v, want CAcall1", hdr["call_sid"])
		}
		if hdr["label"] != "sim" {
			t.Errorf("header label: got %v, want sim", hdr["label"])
		}
		if _, ok := hdr["vad_config"]; !ok {
			t.Errorf("header missing vad_config")
		}
	})

	t.Run("close is idempotent", func(t *testing.T) {
		f := newFlushFixture(t)
		if err := f.r.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := f.r.Close(); err != nil {
			t.Fatalf("second Close (must be idempotent): %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Shadow model-route decisions (AATK-71).
// ---------------------------------------------------------------------------

// routeFixture is a live session wired with a capturing recorder and a spy
// sink, plus the plumbing needed to drive one complete user turn through it
// (utterance -> STT result -> silence turn-end). The four model-route tests
// each need that same drive before a verdict has a turn to attach to, so it is
// assembled once here rather than repeated per test.
type routeFixture struct {
	session *telephony.Session
	rec     *mockRecorder
	sink    *spySink
	dataIn  *telephony.BufferedChan[[]byte]
	sttIn   *telephony.BufferedChan[telephony.STTRequest]
	sttOut  *telephony.BufferedChan[telephony.STTResult]
}

// newRouteFixture builds and starts the session. The caller drives it with
// driveTurn and closes it (Close is the synchronisation point a verdict's
// recording is observable after -- see ReportModelRoute).
func newRouteFixture(t *testing.T, sid string) routeFixture {
	t.Helper()
	f := routeFixture{
		rec:    &mockRecorder{},
		sink:   &spySink{},
		dataIn: telephony.NewBufferedChan[[]byte](256),
		sttIn:  telephony.NewBufferedChan[telephony.STTRequest](100),
		sttOut: telephony.NewBufferedChan[telephony.STTResult](100),
	}
	probs := speechThenSilenceProbs(1, telephony.TurnEndSilenceWindows()+10)
	f.session = startSession(t, sid,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTurnSink(f.sink),
		telephony.WithTwilioDataInput(f.dataIn),
		telephony.WithSTTInput(f.sttIn),
		telephony.WithSTTOutput(f.sttOut),
		telephony.WithDecisionRecorder(f.rec),
	)
	return f
}

// driveTurn runs one utterance to its STT result and then holds silence until
// the turn closes, so the session has exactly one completed turn.
func (f routeFixture) driveTurn(t *testing.T) {
	t.Helper()
	driveOneTurn(t, f.session.CallSID, f.dataIn, f.sttIn, f.sttOut, f.sink, "hello world")
}

// TestModelRoute_SilentByDefault drives a full turn through a session no
// consumer reports a verdict on -- the behavior of every existing consumer --
// and asserts the event stream carries nothing of the new kind: zero
// model_route events, and no trace of either new wire key anywhere in the
// serialized stream, which is what "byte-identical to the pre-change stream"
// amounts to for a recording made from the same input.
func TestModelRoute_SilentByDefault(t *testing.T) {
	f := newRouteFixture(t, "route-silent")
	f.driveTurn(t)
	f.session.Close()

	evs := f.rec.all()
	if n := len(filterByKind(evs, telephony.DecisionKindModelRoute)); n != 0 {
		t.Errorf("model_route events with no verdict reported: got %d, want 0", n)
	}
	if len(evs) == 0 {
		t.Fatal("no decision events recorded at all -- the drive never produced a turn")
	}
	body, err := json.Marshal(evs)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	for _, key := range []string{"model_route", "model-route", "route_reason", "RouteTier"} {
		if bytes.Contains(body, []byte(key)) {
			t.Errorf("unreported-verdict stream contains %q; it must be identical to the pre-change stream: %s", key, body)
		}
	}
}

// TestModelRoute_OneEventPerReportedVerdict reports a single verdict for a
// completed turn and asserts exactly one event carrying the tier (in the
// existing Param/ParamValue vocabulary), the reason, and an effect that says
// on the record that nothing acted on it.
func TestModelRoute_OneEventPerReportedVerdict(t *testing.T) {
	f := newRouteFixture(t, "route-one")
	f.driveTurn(t)
	f.session.ReportModelRoute("large", "caller asked for a multi-step comparison")
	f.session.Close()

	got := filterByKind(f.rec.all(), telephony.DecisionKindModelRoute)
	if len(got) != 1 {
		t.Fatalf("model_route events: got %d, want 1 (all: %+v)", len(got), f.rec.all())
	}
	e := got[0]
	checkFields(t, []fieldCheck{
		{e.Type == "model_route", fmt.Sprintf("Type: got %q, want %q", e.Type, "model_route")},
		{e.Kind == "model-route", fmt.Sprintf("Kind: got %q, want %q", e.Kind, "model-route")},
		{e.Param == "RouteTier", fmt.Sprintf("Param: got %q, want %q", e.Param, "RouteTier")},
		{e.ParamValue == "large", fmt.Sprintf("ParamValue: got %v, want %q", e.ParamValue, "large")},
		{e.RouteReason == "caller asked for a multi-step comparison", fmt.Sprintf("RouteReason: got %q", e.RouteReason)},
		{e.Effect != "", "Effect: got empty, want a statement that the verdict was shadow-only"},
		{strings.Contains(e.Effect, "shadow"), fmt.Sprintf("Effect: got %q, want it to name shadow mode", e.Effect)},
		{e.AudioMS > 0 && e.AudioMS%32 == 0, fmt.Sprintf("AudioMS: got %d, want a positive multiple of 32 (window-clock ms)", e.AudioMS)},
	})
}

// TestModelRoute_SecondVerdictForSameTurnIgnored pins the chosen behavior for a
// consumer that reports twice for one turn: the first verdict wins and the
// second is dropped. A shadow corpus exists to measure what fraction of turns
// would escalate, so a duplicate must not double-count that turn's label.
func TestModelRoute_SecondVerdictForSameTurnIgnored(t *testing.T) {
	f := newRouteFixture(t, "route-twice")
	f.driveTurn(t)
	f.session.ReportModelRoute("small", "first")
	f.session.ReportModelRoute("large", "second")
	f.session.Close()

	got := filterByKind(f.rec.all(), telephony.DecisionKindModelRoute)
	if len(got) != 1 {
		t.Fatalf("model_route events after two reports for one turn: got %d, want 1 (all: %+v)", len(got), f.rec.all())
	}
	if got[0].ParamValue != "small" || got[0].RouteReason != "first" {
		t.Errorf("kept verdict: got tier=%v reason=%q, want the first (small/first)", got[0].ParamValue, got[0].RouteReason)
	}
}

// TestModelRoute_VocabularyPlacement asserts the new kind joins the existing
// DecisionKind* family rather than starting a parallel one: it has the wire
// value the recorded contract names, and it collides with none of the kinds
// already in the vocabulary.
func TestModelRoute_VocabularyPlacement(t *testing.T) {
	checkFields(t, []fieldCheck{
		{telephony.DecisionTypeModelRoute == "model_route", fmt.Sprintf("DecisionTypeModelRoute: got %q, want %q", telephony.DecisionTypeModelRoute, "model_route")},
		{telephony.DecisionKindModelRoute == "model-route", fmt.Sprintf("DecisionKindModelRoute: got %q, want %q", telephony.DecisionKindModelRoute, "model-route")},
		{telephony.DecisionParamRouteTier == "RouteTier", fmt.Sprintf("DecisionParamRouteTier: got %q, want %q", telephony.DecisionParamRouteTier, "RouteTier")},
	})
	assertNoCollision(t, "DecisionKindModelRoute", telephony.DecisionKindModelRoute, []string{
		telephony.DecisionKindSpeechStart, telephony.DecisionKindSilence,
		telephony.DecisionKindEndOfUtter, telephony.DecisionKindTurnEnd,
		telephony.DecisionKindUtteranceCap, telephony.DecisionKindTurnCap,
		telephony.DecisionKindIdleTimeout, telephony.DecisionKindResponseCap,
	})
	assertNoCollision(t, "DecisionParamRouteTier", telephony.DecisionParamRouteTier, []string{
		telephony.DecisionParamSpeechThresh, telephony.DecisionParamSilenceThresh,
		telephony.DecisionParamEndSilence, telephony.DecisionParamTurnEndSilence,
		telephony.DecisionParamMaxUtterance, telephony.DecisionParamMaxTurn,
		telephony.DecisionParamMaxSilence, telephony.DecisionParamMaxResponse,
	})
}

// assertNoCollision fails if next repeats a value already in the vocabulary --
// the check that a new constant joined the family rather than shadowing a
// member of it.
func assertNoCollision(t *testing.T, name, next string, existing []string) {
	t.Helper()
	for _, v := range existing {
		if v == next {
			t.Errorf("%s collides with an existing value %q", name, v)
		}
	}
}

// TestModelRoute_ReplayStability drives the same scripted input twice, reporting
// the same verdict at the same point in each run, and asserts the route events
// come out identical -- same order, same fields, same AudioMS. That is what
// makes a shadow corpus usable as an evaluation set, and it holds because the
// event is anchored on the turn's audio-clock position rather than on when the
// consumer got around to reporting.
func TestModelRoute_ReplayStability(t *testing.T) {
	run := func(sid string) []telephony.DecisionEvent {
		f := newRouteFixture(t, sid)
		f.driveTurn(t)
		f.session.ReportModelRoute("large", "same verdict both runs")
		f.session.Close()
		return filterByKind(f.rec.all(), telephony.DecisionKindModelRoute)
	}

	first := run("route-replay-1")
	second := run("route-replay-2")

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("route events per run: got %d and %d, want 1 each", len(first), len(second))
	}
	b1, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal run 1: %v", err)
	}
	b2, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal run 2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("route events differ across identical runs:\nrun1: %s\nrun2: %s", b1, b2)
	}
}
