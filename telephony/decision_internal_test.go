package telephony

import "testing"

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
