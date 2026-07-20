package telephony

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// meetingsTodayEvent is one entry in meetings_today_events.json: a VADEvent
// emitted by the real pipeline, positioned by inference-window index.
type meetingsTodayEvent struct {
	WindowIndex int    `json:"window_index"`
	Kind        string `json:"kind"`
}

// meetingsTodayGolden mirrors internal/telephony/testdata/meetings_today_events.json.
type meetingsTodayGolden struct {
	Source     string               `json:"source"`
	SampleRate int                  `json:"sample_rate"`
	WindowSize int                  `json:"window_size"`
	Events     []meetingsTodayEvent `json:"events"`
}

// runRealPipeline replays ulawFrame bytes through the exact real-pipeline
// sequence the ticket specifies: decodeMuLaw -> windower -> sileroDetector.Detect
// -> vadMachine.step, chunked the way Twilio actually delivers audio (160-byte,
// 20ms frames — see internal/telephony/twilio/session.go's chunkSize) and
// returns every emitted VADEvent, tagged with the inference-window index that
// produced it.
func runRealPipeline(t *testing.T, ulaw []byte) []meetingsTodayEvent {
	t.Helper()

	det, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}
	cfg := defaultVADConfig()
	w := newWindower(cfg.WindowSize)
	m := newVADMachine(cfg)

	const chunkSize = 160 // Twilio's 20ms mu-law chunk at 8kHz — see twilio/session.go
	var events []meetingsTodayEvent
	windowIdx := 0
	for off := 0; off < len(ulaw); off += chunkSize {
		end := off + chunkSize
		if end > len(ulaw) {
			end = len(ulaw)
		}
		frame := ulaw[off:end]
		for _, win := range w.push(decodeMuLaw(frame)) {
			prob, err := det.Detect(win)
			if err != nil {
				t.Fatalf("Detect at window %d: %v", windowIdx, err)
			}
			if ev, emit := m.step(prob); emit {
				events = append(events, meetingsTodayEvent{WindowIndex: windowIdx, Kind: string(ev.Kind)})
			}
			windowIdx++
		}
	}
	return events
}

// meetingsTodayProbGolden mirrors testdata/meetings_today_goldens.json — the
// onnxruntime per-frame speech probabilities for the full recording.
type meetingsTodayProbGolden struct {
	Frames []struct {
		Index  int     `json:"index"`
		Output float32 `json:"output"`
	} `json:"frames"`
}

// TestSileroE2EPerFrameGolden validates that gonnx's per-frame probabilities
// on the real recording stay within 1e-3 of the onnxruntime reference, the
// same conformance delta used by TestSileroDetectorGolden.
func TestSileroE2EPerFrameGolden(t *testing.T) {
	const delta = 1e-3

	ulaw, err := os.ReadFile(filepath.Join("testdata", "meetings_today.ulaw"))
	if err != nil {
		t.Fatalf("reading recording fixture: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "meetings_today_goldens.json"))
	if err != nil {
		t.Fatalf("reading per-frame golden fixture: %v", err)
	}
	var golden meetingsTodayProbGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("unmarshalling per-frame golden fixture: %v", err)
	}
	if len(golden.Frames) == 0 {
		t.Fatal("per-frame golden fixture has no frames")
	}

	det, err := NewSileroDetector()
	if err != nil {
		t.Fatalf("NewSileroDetector: %v", err)
	}
	cfg := defaultVADConfig()
	w := newWindower(cfg.WindowSize)

	const chunkSize = 160
	windowIdx := 0
	for off := 0; off < len(ulaw); off += chunkSize {
		end := off + chunkSize
		if end > len(ulaw) {
			end = len(ulaw)
		}
		for _, win := range w.push(decodeMuLaw(ulaw[off:end])) {
			prob, err := det.Detect(win)
			if err != nil {
				t.Fatalf("Detect at window %d: %v", windowIdx, err)
			}
			if windowIdx < len(golden.Frames) {
				want := golden.Frames[windowIdx].Output
				diff := prob - want
				if diff < 0 {
					diff = -diff
				}
				if diff > delta {
					t.Errorf("frame %d: got prob %v, want %v (delta %v > %v)", windowIdx, prob, want, diff, delta)
				}
			}
			windowIdx++
		}
	}
	if windowIdx != len(golden.Frames) {
		t.Errorf("processed %d windows, golden has %d frames", windowIdx, len(golden.Frames))
	}
}

// TestSileroE2ETimelineGolden feeds the real "Can you show me my meetings
// today?" recording through the real, unmodified detector and state machine
// and asserts the emitted VADEvent sequence matches the committed golden
// exactly. A mis-wired detector, a windowing bug, or a state-machine
// regression each change this timeline and fail this test.
//
// SOP-154 retroactive RED-state verification: attempt 1's commit that
// reverted EndSilenceMS 1050ms -> 700ms updated this test's golden fixture
// (testdata/meetings_today_events.json, end-of-utterance window 248 -> 237)
// in the same commit as the production change, so no genuine RED state was
// ever demonstrated for that specific behavior at the time. Verified here,
// after the fact: temporarily setting defaultVADConfig's EndSilenceMS back
// to 1050 and running this test fails with
//
//	event 6: got {WindowIndex:248 Kind:end-of-utterance}, want {WindowIndex:237 Kind:end-of-utterance}
//
// i.e. exactly the pre-SOP-154 value, confirming the golden fixture and the
// production code are genuinely coupled and this test is not vacuous.
// Restoring EndSilenceMS to 700 makes it pass again with no other change.
func TestSileroE2ETimelineGolden(t *testing.T) {
	ulaw, err := os.ReadFile(filepath.Join("testdata", "meetings_today.ulaw"))
	if err != nil {
		t.Fatalf("reading recording fixture: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "meetings_today_events.json"))
	if err != nil {
		t.Fatalf("reading events golden fixture: %v", err)
	}
	var golden meetingsTodayGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("unmarshalling events golden fixture: %v", err)
	}
	if len(golden.Events) == 0 {
		t.Fatal("events golden fixture has no events")
	}

	got := runRealPipeline(t, ulaw)

	if len(got) != len(golden.Events) {
		t.Fatalf("emitted %d events, want %d\ngot:  %+v\nwant: %+v", len(got), len(golden.Events), got, golden.Events)
	}
	for i, want := range golden.Events {
		if got[i] != want {
			t.Errorf("event %d: got %+v, want %+v", i, got[i], want)
		}
	}
}
