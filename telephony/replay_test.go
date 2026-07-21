package telephony_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// fakeSTTServer stands up an httptest server shaped like the real whisper
// sidecar (multipart POST /v1/audio/transcriptions, verbose_json response
// echoing request_id) and returns a real telephony.STTClient pointed at it
// -- the same STTClient.Transcribe code path telephony.Replay's caller wires
// in production (main.go), just aimed at a fake backend instead of a real
// one, so these tests never depend on a live whisper server.
func fakeSTTServer(t *testing.T, textFor func(requestID string) string) *telephony.STTClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestID := r.FormValue("request_id")
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"text":     textFor(requestID),
			"language": "en",
			"duration": 1.0,
		}
		if requestID != "" {
			resp["request_id"] = requestID
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	return telephony.NewSTTClient(server.URL)
}

// fixedTextSTTServer is fakeSTTServer for tests that don't care which text
// comes back for which request, only how many FullPass results arrive.
func fixedTextSTTServer(t *testing.T, text string) *telephony.STTClient {
	t.Helper()
	return fakeSTTServer(t, func(string) string { return text })
}

// TestReplay_Deterministic asserts Observable behavior #1: same audio, same
// config, byte-identical JSON output, every run. Uses a fake VAD factory
// (telephony.WithVADFactory, the same test seam every other test in this
// package uses) rather than the real Silero model, because determinism here
// is a claim about Replay's own plumbing -- not about the real VAD, which
// TestReplay_MatchesProduction below exercises and pins separately -- and a
// fake, fast detector keeps this test from re-paying the real fixture's
// ~10s runtime twice.
func TestReplay_Deterministic(t *testing.T) {
	sttClient := fixedTextSTTServer(t, "hello there")

	// 8 windows of speech then enough silence to cross the default
	// telephony.EndSilenceWindows() (= ceil(EndSilenceMS / windowMS)) -- content
	// is irrelevant with a fake VAD factory, only the byte count (window
	// count) the fake detector is asked to classify matters.
	audio := bytes.Repeat([]byte{0x01}, 8*256)

	run := func(callSID string) []telephony.ReplayResult {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results, err := telephony.Replay(ctx, callSID, bytes.NewReader(audio), sttClient,
			telephony.WithVADFactory(func() (telephony.VADDetector, error) {
				return newFakeReplayDetector(8, 40), nil
			}),
		)
		if err != nil {
			t.Fatalf("Replay(%s): %v", callSID, err)
		}
		return results
	}

	out1 := run("CA-det-1")
	out2 := run("CA-det-2")

	b1, err := json.Marshal(out1)
	if err != nil {
		t.Fatalf("marshal out1: %v", err)
	}
	b2, err := json.Marshal(out2)
	if err != nil {
		t.Fatalf("marshal out2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("replays not byte-identical:\nrun1: %s\nrun2: %s", b1, b2)
	}
	if len(out1) == 0 {
		t.Fatal("replay produced no results -- test fixture never dispatched a FullPass")
	}
}

// TestReplay_MatchesProduction is the fidelity gate (ticket SOP-153 DoD:
// "Replay reproduces a captured call's live-log utterance boundaries at
// that call's config... without it every number the harness prints is
// about a pipeline that doesn't exist").
//
// The captured .ulaw is testdata/meetings_today.ulaw (SOP-96): a real
// recording of "Can you show me my meetings today?", already the oracle
// this package's own TestSileroE2ETimelineGolden (silero_e2e_test.go) pins
// against a snapshot of the real, unmodified decodeMuLaw -> windower ->
// sileroDetector.Detect -> vadMachine.step pipeline. That golden
// (testdata/meetings_today_events.json) records exactly one
// "end-of-utterance" event (the test reads its window from the golden, not a
// literal) for this recording at the current default -- i.e. its real "live log" is
// "one utterance." Replay is called here with no VAD override at all (the
// real, default NewSileroDetector Session.Start already uses), so this is
// literally the production VAD+STT path, driven from a byte stream instead
// of a live WebSocket, with no override to fake anything on the VAD side.
// This test does not re-derive the boundary independently; it relies on
// the already-established, separately-tested golden as the oracle and
// asserts Replay reproduces the *number of utterances* that boundary
// implies -- exactly one FullPass dispatched, not more (no phantom
// utterance) and not zero (no dropped utterance).
func TestReplay_MatchesProduction(t *testing.T) {
	ulaw, err := os.ReadFile(filepath.Join("testdata", "meetings_today.ulaw"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	sttClient := fixedTextSTTServer(t, "can you show me my meetings today")

	// Generous: this drives the real Silero ONNX model over a 10s
	// recording, and under -race that inference is measurably (several
	// times) slower than un-instrumented -- not a hang, just real compute.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	results, err := telephony.Replay(ctx, "CA-prod-fixture", bytes.NewReader(ulaw), sttClient)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Derive the expected FullPass count from the committed golden rather than
	// hardcoding a count or window: Replay dispatches one FullPass per
	// end-of-utterance event, so reading the fixture keeps this in lockstep with
	// the golden across a VAD retune (AATK-4).
	euWindows := goldenEndOfUtteranceWindows(t)
	if len(results) != len(euWindows) {
		t.Fatalf("got %d FullPass results, want %d (golden end-of-utterance windows %v): %+v", len(results), len(euWindows), euWindows, results)
	}
	if results[0].Text != "can you show me my meetings today" {
		t.Errorf("results[0].Text = %q, want the STT stub's canned reply", results[0].Text)
	}
}

// goldenEndOfUtteranceWindows reads the committed meetings_today events golden and
// returns the window index of every end-of-utterance event. Tests derive the
// expected utterance count (and the boundary they cite) from the fixture instead of
// hardcoding it, so they never drift out of sync with the golden on a VAD retune.
// A local anonymous struct is used because the golden type in silero_e2e_test.go
// lives in package telephony (internal), unreachable from this external test.
func goldenEndOfUtteranceWindows(t *testing.T) []int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "meetings_today_events.json"))
	if err != nil {
		t.Fatalf("read events golden: %v", err)
	}
	var golden struct {
		Events []struct {
			WindowIndex int    `json:"window_index"`
			Kind        string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse events golden: %v", err)
	}
	var windows []int
	for _, e := range golden.Events {
		if e.Kind == "end-of-utterance" {
			windows = append(windows, e.WindowIndex)
		}
	}
	return windows
}

// fakeReplayDetector is a minimal telephony.VADDetector: speechN windows
// above SpeechThresh, then silenceN windows below SilenceThresh (and
// silence forever once probs is exhausted) -- same shape as this package's
// internal fakeDetector (session_test.go), reimplemented here because that
// type lives in package telephony_test too, but replay_test.go needs a
// second, independently-parameterized instance per Replay() call (a
// fakeDetector is stateful and Replay's Session.Start constructs a fresh
// one per call via the factory).
type fakeReplayDetector struct {
	probs []float32
	i     int
}

func newFakeReplayDetector(speechN, silenceN int) *fakeReplayDetector {
	probs := make([]float32, 0, speechN+silenceN)
	for i := 0; i < speechN; i++ {
		probs = append(probs, 0.9)
	}
	for i := 0; i < silenceN; i++ {
		probs = append(probs, 0.1)
	}
	return &fakeReplayDetector{probs: probs}
}

func (f *fakeReplayDetector) Detect(window []float32) (float32, error) {
	p := float32(0)
	if f.i < len(f.probs) {
		p = f.probs[f.i]
	}
	f.i++
	return p, nil
}

func (f *fakeReplayDetector) Reset() {}

// TestBuild_StructuralLabels pins the ticket's own worked example: a
// complete-turn recording replaying to 3 utterances yields rows
// [u1]=incomplete, [u1,u2]=incomplete, [u1,u2,u3]=complete -- by
// construction, no hand-labelling.
func TestBuild_StructuralLabels(t *testing.T) {
	utterances := []string{"hello", "I need", "to book a flight"}
	rows := telephony.RowsFromUtterances("rec-1", telephony.LabelCompleteTurn, utterances, 700)

	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	wantLabels := []telephony.RowLabel{telephony.RowIncomplete, telephony.RowIncomplete, telephony.RowComplete}
	wantPrefixLens := []int{1, 2, 3}
	for i, row := range rows {
		if row.Label != wantLabels[i] {
			t.Errorf("row %d: label = %q, want %q", i, row.Label, wantLabels[i])
		}
		if len(row.Utterances) != wantPrefixLens[i] {
			t.Errorf("row %d: len(Utterances) = %d, want %d", i, len(row.Utterances), wantPrefixLens[i])
		}
		if row.EndSilenceMS != 700 {
			t.Errorf("row %d: EndSilenceMS = %d, want 700", i, row.EndSilenceMS)
		}
	}
}

// TestBuild_TruncatedRecording pins the ticket's other worked example: a
// truncated recording never reached a confirmed end, so every row --
// including the terminal one -- is incomplete.
func TestBuild_TruncatedRecording(t *testing.T) {
	utterances := []string{"hello", "I need", "to book a"}
	rows := telephony.RowsFromUtterances("rec-2", telephony.LabelTruncated, utterances, 700)

	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	for i, row := range rows {
		if row.Label != telephony.RowIncomplete {
			t.Errorf("row %d: label = %q, want %q (truncated recordings never yield a complete row)", i, row.Label, telephony.RowIncomplete)
		}
	}
}

// TestBuild_ConfigChangesRows drives the SAME audio through Replay at two
// different --end-silence-ms values and asserts they produce different
// utterance (and therefore row) counts -- the DoD claim that the dataset is
// audio-anchored, not frozen text: build re-replays at whatever config it's
// given rather than reusing a cached transcript.
//
// The fixture is two identical speech bursts separated by a 15-window
// silence gap (a fake VAD factory drives this, not the real Silero model --
// this test is about build/replay's config-sensitivity, not VAD fidelity,
// which TestReplay_MatchesProduction owns separately). At
// --end-silence-ms=200 (~7 windows to cross), the gap is long enough to end
// the first utterance mid-gap, yielding 2 utterances. At
// --end-silence-ms=1000 (~32 windows to cross), the same 15-window gap
// never crosses the threshold, so the two speech bursts fuse into 1
// utterance.
func TestBuild_ConfigChangesRows(t *testing.T) {
	const speechWindows = 5
	const gapWindows = 15
	audio := bytes.Repeat([]byte{0x01}, (2*speechWindows+gapWindows)*256)

	replayAt := func(endSilenceMS int) []telephony.ReplayResult {
		sttClient := fixedTextSTTServer(t, "utterance")
		probs := make([]float32, 0, 2*speechWindows+gapWindows)
		for i := 0; i < speechWindows; i++ {
			probs = append(probs, 0.9)
		}
		for i := 0; i < gapWindows; i++ {
			probs = append(probs, 0.1)
		}
		for i := 0; i < speechWindows; i++ {
			probs = append(probs, 0.9)
		}
		det := &fakeReplayDetector{probs: probs}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results, err := telephony.Replay(ctx, "CA-cfg", bytes.NewReader(audio), sttClient,
			telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
			telephony.WithVADConfig(telephony.VADConfig{EndSilenceMS: endSilenceMS}),
		)
		if err != nil {
			t.Fatalf("Replay(endSilenceMS=%d): %v", endSilenceMS, err)
		}
		return results
	}

	shortResults := replayAt(200)
	longResults := replayAt(1000)

	shortRows := telephony.RowsFromUtterances("rec-cfg", telephony.LabelCompleteTurn, textsOf(shortResults), 200)
	longRows := telephony.RowsFromUtterances("rec-cfg", telephony.LabelCompleteTurn, textsOf(longResults), 1000)

	if len(shortRows) == len(longRows) {
		t.Fatalf("row counts identical across configs (%d == %d): --end-silence-ms isn't changing the rows\nshort (200ms): %+v\nlong (1000ms): %+v",
			len(shortRows), len(longRows), shortResults, longResults)
	}
}

func textsOf(results []telephony.ReplayResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Text
	}
	return out
}

// TestScore_ReportsOutcomes pins Observable behavior #5: score reports the
// four named verifier outcomes, not raw accuracy.
func TestScore_ReportsOutcomes(t *testing.T) {
	rows := []telephony.DatasetRow{
		{RecordingID: "r1", Utterances: []string{"a"}, Label: telephony.RowIncomplete},
		{RecordingID: "r1", Utterances: []string{"a", "b"}, Label: telephony.RowComplete},
		{RecordingID: "r2", Utterances: []string{"c"}, Label: telephony.RowIncomplete},
		{RecordingID: "r3", Utterances: []string{"d"}, Label: telephony.RowIncomplete},
	}
	fixedVerdicts := map[string]telephony.VerifierOutcome{
		"r1|1": telephony.OutcomeProceed,
		"r1|2": telephony.OutcomeSpuriousRepair,
		"r2|1": telephony.OutcomeRepairFires,
		"r3|1": telephony.OutcomePartialAccepted,
	}
	verify := func(_ context.Context, _ string, row telephony.DatasetRow) (telephony.VerifierOutcome, error) {
		key := row.RecordingID + "|" + string(rune('0'+len(row.Utterances)))
		outcome, ok := fixedVerdicts[key]
		if !ok {
			t.Fatalf("no fixed verdict for %s", key)
		}
		return outcome, nil
	}

	report, err := telephony.Score(context.Background(), "prompts/v1.txt", rows, 700, verify)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}

	want := map[telephony.VerifierOutcome]int{
		telephony.OutcomeProceed:         1,
		telephony.OutcomeSpuriousRepair:  1,
		telephony.OutcomeRepairFires:     1,
		telephony.OutcomePartialAccepted: 1,
	}
	for outcome, count := range want {
		if report.Outcomes[outcome] != count {
			t.Errorf("Outcomes[%s] = %d, want %d", outcome, report.Outcomes[outcome], count)
		}
	}
	if report.RowCount != len(rows) {
		t.Errorf("RowCount = %d, want %d", report.RowCount, len(rows))
	}

	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if bytes.Contains(bytes.ToLower(b), []byte("accuracy")) {
		t.Errorf("ScoreReport JSON contains \"accuracy\": %s (DoD: score reports the four outcomes, not accuracy)", b)
	}
}
