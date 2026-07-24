package telephony_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	s := telephony.NewSession(context.Background(), "test-decrec",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(2 * time.Millisecond)
	}

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
	var eou []telephony.DecisionEvent
	for _, ev := range evs {
		if ev.Kind == telephony.DecisionKindEndOfUtter {
			eou = append(eou, ev)
		}
	}
	if len(eou) != 1 {
		t.Fatalf("end-of-utterance events: got %d, want 1 (all: %+v)", len(eou), evs)
	}
	e := eou[0]
	if e.Type != telephony.DecisionTypeVAD {
		t.Errorf("Type: got %q, want %q", e.Type, telephony.DecisionTypeVAD)
	}
	if e.Kind != telephony.DecisionKindEndOfUtter {
		t.Errorf("Kind: got %q, want %q", e.Kind, telephony.DecisionKindEndOfUtter)
	}
	if e.Param != telephony.DecisionParamEndSilence {
		t.Errorf("Param: got %q, want %q", e.Param, telephony.DecisionParamEndSilence)
	}
	if want := telephony.DefaultVADConfig().EndSilenceMS; e.ParamValue != want {
		t.Errorf("ParamValue: got %v, want %d", e.ParamValue, want)
	}
	if e.RequestID != req.RequestID {
		t.Errorf("RequestID: got %d, want %d", e.RequestID, req.RequestID)
	}
	if e.AudioMS <= 0 || e.AudioMS%32 != 0 {
		t.Errorf("AudioMS: got %d, want a positive multiple of 32 (window-clock ms)", e.AudioMS)
	}
	if !strings.Contains(e.Effect, "STT request") {
		t.Errorf("Effect: got %q, want a mention of the dispatched STT request", e.Effect)
	}
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
	s := telephony.NewSession(context.Background(), sessionID,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithDecisionRecorder(rec),
		telephony.WithDecisionClock(fnow.now),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(2 * time.Millisecond)
	}

	// The dispatch appears on sttIn; receiving it proves dispatchSTT (and its
	// stt_dispatch record + stored dispatch instant) have already run.
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received (end-of-utterance never dispatched): %v", err)
	}

	dispatch := waitEventType(t, rec, "stt_dispatch")
	if dispatch.RequestID != req.RequestID {
		t.Errorf("stt_dispatch RequestID: got %d, want %d", dispatch.RequestID, req.RequestID)
	}
	if dispatch.AudioBytes <= 0 {
		t.Errorf("stt_dispatch AudioBytes: got %d, want > 0", dispatch.AudioBytes)
	}

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
	if result.RequestID != req.RequestID {
		t.Errorf("stt_result RequestID: got %d, want %d", result.RequestID, req.RequestID)
	}
	if result.LatencyMS != 800 {
		t.Errorf("stt_result LatencyMS: got %d, want 800", result.LatencyMS)
	}
	if result.Text != "hello world" {
		t.Errorf("stt_result Text: got %q, want %q", result.Text, "hello world")
	}
	if result.STTDurSec != 1.5 {
		t.Errorf("stt_result STTDurSec: got %v, want 1.5", result.STTDurSec)
	}
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
	s := telephony.NewSession(context.Background(), sid,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTranscriptOutput(dir, sid, &live),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// One utterance -> end-of-utterance -> STT dispatch.
	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{SessionID: sid, RequestID: req.RequestID, Kind: telephony.FullPass, Text: "hello world"})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Continued silence -> turn-end -> completeTurn captures the turn.
	for i := 0; i <= telephony.TurnEndSilenceWindows()+10; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sink.turnTexts()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if len(sink.turnTexts()) == 0 {
		t.Fatal("turn did not complete within the backstop")
	}

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

// TestFileRecorder_FlushAndLiveFeed checks the concrete recorder both live-feeds
// each event to its writer as it arrives and, on Close, flushes a homogeneous
// JSONL plus a separate header file.
func TestFileRecorder_FlushAndLiveFeed(t *testing.T) {
	dir := t.TempDir()
	var live bytes.Buffer
	r := telephony.NewFileDecisionRecorder(dir, "MZstream1", "CAcall1", "sim", telephony.DefaultVADConfig(), &live)
	if r == nil {
		t.Fatal("NewFileDecisionRecorder returned nil for a non-empty dir")
	}

	// ParamValue/AudioMS here are arbitrary fixture inputs the recorder serializes
	// verbatim — NOT the VAD default (which is DefaultVADConfig().EndSilenceMS). This
	// test exercises the recorder's flush/live-feed, so any values work; they are
	// deliberately literals, decoupled from the tuned default.
	in := []telephony.DecisionEvent{
		{Type: "vad", Kind: "end-of-utterance", Param: "EndSilenceMS", ParamValue: 700, AudioMS: 640, RequestID: 1, Effect: "utterance closed; dispatched STT request 1"},
		{Type: "vad", Kind: "end-of-utterance", Param: "EndSilenceMS", ParamValue: 700, AudioMS: 1280, RequestID: 2, Effect: "utterance closed; dispatched STT request 2"},
	}
	for _, e := range in {
		r.Record(e)
	}

	if got := strings.Count(live.String(), "\n"); got != len(in) {
		t.Errorf("live feed lines: got %d, want %d\n%s", got, len(in), live.String())
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "MZstream1.events.jsonl"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(in) {
		t.Fatalf("jsonl lines: got %d, want %d", len(lines), len(in))
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

	hdrData, err := os.ReadFile(filepath.Join(dir, "MZstream1.events.header.json"))
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

	if err := r.Close(); err != nil {
		t.Fatalf("second Close (must be idempotent): %v", err)
	}
}
