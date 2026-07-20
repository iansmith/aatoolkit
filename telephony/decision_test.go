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

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("recorded events: got %d, want 1 (%+v)", len(evs), evs)
	}
	e := evs[0]
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
