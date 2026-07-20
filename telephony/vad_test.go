package telephony

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// --- decodeMuLaw --------------------------------------------------------------

// μ-law 0xFF is the silence code and must decode to exactly 0.
func TestDecodeMuLaw_SilenceIsZero(t *testing.T) {
	got := decodeMuLaw([]byte{0xFF, 0xFF, 0xFF})
	if len(got) != 3 {
		t.Fatalf("want 3 samples, got %d", len(got))
	}
	for i, s := range got {
		if s != 0 {
			t.Errorf("sample %d = %v, want 0", i, s)
		}
	}
}

// Full-scale μ-law bytes normalize to near ±1, and every sample stays in range.
func TestDecodeMuLaw_NormalizedRange(t *testing.T) {
	got := decodeMuLaw([]byte{0x00, 0x80})
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d", len(got))
	}
	if got[0] > -0.9 {
		t.Errorf("0x00 → %v, want near -1", got[0])
	}
	if got[1] < 0.9 {
		t.Errorf("0x80 → %v, want near +1", got[1])
	}
	for _, s := range got {
		if s < -1.0001 || s > 1.0001 {
			t.Errorf("sample %v out of [-1,1]", s)
		}
	}
}

// --- config -------------------------------------------------------------------

// A zero/partial config is filled from defaults so windowing and EOU timing
// can't be silently disabled (a zero SampleRateHz would never end an utterance).
func TestVADConfig_WithDefaultsFillsZeroFields(t *testing.T) {
	if got := (vadConfig{}).withDefaults(); got != defaultVADConfig() {
		t.Errorf("zero config should equal defaults, got %+v", got)
	}
	custom := vadConfig{WindowSize: 128, SampleRateHz: 16000}.withDefaults()
	if custom.WindowSize != 128 || custom.SampleRateHz != 16000 {
		t.Errorf("set fields must be preserved, got %+v", custom)
	}
	if custom.EndSilenceMS != defaultVADConfig().EndSilenceMS {
		t.Errorf("unset EndSilenceMS should default, got %d", custom.EndSilenceMS)
	}
}

// --- windower -----------------------------------------------------------------

// A partial fill yields no window; crossing the size yields exactly one.
func TestWindower_YieldsFixedSizeWindows(t *testing.T) {
	w := newWindower(256)
	if got := w.push(make([]float32, 160)); len(got) != 0 {
		t.Fatalf("160 samples: want 0 windows, got %d", len(got))
	}
	got := w.push(make([]float32, 160)) // 320 buffered → one 256 window
	if len(got) != 1 || len(got[0]) != 256 {
		t.Fatalf("want one 256-sample window, got %d windows", len(got))
	}
}

// One push spanning several window sizes yields all complete windows.
func TestWindower_MultipleWindowsInOnePush(t *testing.T) {
	w := newWindower(100)
	got := w.push(make([]float32, 250)) // two 100-windows, 50 remain
	if len(got) != 2 {
		t.Fatalf("want 2 windows, got %d", len(got))
	}
	if got2 := w.push(make([]float32, 50)); len(got2) != 1 {
		t.Fatalf("want 1 window after remainder completes, got %d", len(got2))
	}
}

// Window contents must be the samples in order, with the remainder carried over.
func TestWindower_PreservesContentAndOrder(t *testing.T) {
	w := newWindower(4)
	got := w.push([]float32{1, 2, 3, 4, 5, 6})
	if len(got) != 1 {
		t.Fatalf("want 1 window, got %d", len(got))
	}
	for i, want := range []float32{1, 2, 3, 4} {
		if got[0][i] != want {
			t.Errorf("window[%d] = %v, want %v", i, got[0][i], want)
		}
	}
	got2 := w.push([]float32{7, 8}) // remainder {5,6} + {7,8}
	if len(got2) != 1 {
		t.Fatalf("want 1 window, got %d", len(got2))
	}
	for i, want := range []float32{5, 6, 7, 8} {
		if got2[0][i] != want {
			t.Errorf("window2[%d] = %v, want %v", i, got2[0][i], want)
		}
	}
}

// --- vadMachine ---------------------------------------------------------------

// windowMS = 256/8000*1000 = 32; EndSilenceMS 96 → 3 silence windows to EOU.
func vadTestCfg() vadConfig {
	return vadConfig{WindowSize: 256, SpeechThresh: 0.5, SilenceThresh: 0.35, EndSilenceMS: 96, SampleRateHz: 8000}
}

// Silence emits nothing; the first above-threshold window emits Speech.
func TestVADMachine_SpeechOnset(t *testing.T) {
	m := newVADMachine(vadTestCfg())
	if ev, ok := m.step(0.1); ok {
		t.Errorf("silence should emit nothing, got %v", ev)
	}
	ev, ok := m.step(0.9)
	if !ok || ev.Kind != VADSpeech {
		t.Errorf("onset: got (%v,%v), want Speech", ev, ok)
	}
}

// Speech → drop emits Silence; sustained silence past the hangover emits EOU.
func TestVADMachine_SilenceThenEndOfUtterance(t *testing.T) {
	m := newVADMachine(vadTestCfg())
	m.step(0.9) // enter speech
	ev, ok := m.step(0.1)
	if !ok || ev.Kind != VADSilence {
		t.Fatalf("first drop: got (%v,%v), want Silence", ev, ok)
	}
	if ev, ok := m.step(0.1); ok {
		t.Errorf("2nd silence window: want no event yet, got %v", ev)
	}
	ev, ok = m.step(0.1) // 3rd silence window → hangover reached
	if !ok || ev.Kind != VADEndOfUtterance {
		t.Errorf("3rd silence: got (%v,%v), want EndOfUtterance", ev, ok)
	}
}

// Speech resuming before the hangover cancels the pending end-of-utterance.
func TestVADMachine_ResumeCancelsEndOfUtterance(t *testing.T) {
	m := newVADMachine(vadTestCfg())
	m.step(0.9) // speech
	m.step(0.1) // silence 1 (Silence)
	m.step(0.9) // resume before hangover
	m.step(0.1) // silence 1 again
	m.step(0.1) // silence 2
	ev, ok := m.step(0.1)
	if !ok || ev.Kind != VADEndOfUtterance {
		t.Errorf("EOU should require full hangover after resume: got (%v,%v)", ev, ok)
	}
}

// After an end-of-utterance the machine is idle again, so the next speech
// starts a fresh utterance (emits Speech). Guards multi-turn calls.
func TestVADMachine_NewUtteranceAfterEndOfUtterance(t *testing.T) {
	m := newVADMachine(vadTestCfg())
	m.step(0.9) // speech
	m.step(0.1) // silence 1
	m.step(0.1) // silence 2
	if ev, ok := m.step(0.1); !ok || ev.Kind != VADEndOfUtterance {
		t.Fatalf("want EndOfUtterance to close the utterance, got (%v,%v)", ev, ok)
	}
	ev, ok := m.step(0.9)
	if !ok || ev.Kind != VADSpeech {
		t.Errorf("new utterance after EOU: got (%v,%v), want Speech", ev, ok)
	}
}

// A probability in the hysteresis dead zone does not flap the state.
func TestVADMachine_HysteresisDeadZoneNoFlap(t *testing.T) {
	m := newVADMachine(vadTestCfg())
	m.step(0.9) // speech
	if ev, ok := m.step(0.4); ok {
		t.Errorf("dead-zone prob should emit nothing while speaking, got %v", ev)
	}
}

// TestVADMachine_TurnEnd pins SOP-154 Observable behavior #3: a turn-end
// signal fires after TurnEndSilenceMS of trailing silence, derived from the
// same counter that drives EndSilenceMS -- not a second independent clock.
// EndSilenceMS (96ms/3 windows) fires end-of-utterance first; the same
// silence run then keeps counting, uninterrupted, until it crosses
// TurnEndSilenceMS (192ms/6 windows total), and not one window before.
func TestVADMachine_TurnEnd(t *testing.T) {
	cfg := vadTestCfg()
	cfg.TurnEndSilenceMS = 192
	m := newVADMachine(cfg)

	m.step(0.9) // speech
	ev, ok := m.step(0.1)
	if !ok || ev.Kind != VADSilence {
		t.Fatalf("silence 1: got (%v,%v), want Silence", ev, ok)
	}
	if _, ok := m.step(0.1); ok {
		t.Fatalf("silence 2: want no event yet")
	}
	ev, ok = m.step(0.1) // silence 3: crosses EndSilenceMS (96ms)
	if !ok || ev.Kind != VADEndOfUtterance {
		t.Fatalf("silence 3: got (%v,%v), want EndOfUtterance", ev, ok)
	}

	// Silence keeps accumulating on the SAME run post-EOU: windows 4 and 5
	// must emit nothing, and turn-end must not fire before window 6 (192ms).
	for i := 4; i <= 5; i++ {
		if ev, ok := m.step(0.1); ok {
			t.Fatalf("silence window %d: want no event before TurnEndSilenceMS, got %v", i, ev)
		}
	}
	ev, ok = m.step(0.1) // silence window 6: crosses TurnEndSilenceMS (192ms)
	if !ok || ev.Kind != VADTurnEnd {
		t.Fatalf("silence window 6: got (%v,%v), want VADTurnEnd", ev, ok)
	}

	// Fires at most once per silence run.
	if ev, ok := m.step(0.1); ok {
		t.Errorf("2nd turn-end should not fire: got %v", ev)
	}

	// A fresh speech onset resets everything for the next utterance/turn.
	ev, ok = m.step(0.9)
	if !ok || ev.Kind != VADSpeech {
		t.Errorf("onset after turn-end: got (%v,%v), want Speech", ev, ok)
	}
}

// --- runVAD goroutine ---------------------------------------------------------

type fakeDetector struct {
	probs  []float32
	i      int
	resets atomic.Int32
}

func (f *fakeDetector) Detect(window []float32) (float32, error) {
	p := float32(0)
	if f.i < len(f.probs) {
		p = f.probs[f.i]
	}
	f.i++
	return p, nil
}

func (f *fakeDetector) Reset() { f.resets.Add(1) }

// Frames arriving on In produce VADEvents on Out via the detector.
func TestRunVAD_EmitsSpeechFromFrames(t *testing.T) {
	cfg := vadTestCfg()
	cfg.WindowSize = 160 // one window per 160-sample (20ms @8k) frame
	in := make(chan []byte, 4)
	out := make(chan VADEvent, 8)
	det := &fakeDetector{probs: []float32{0.9}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runVAD(ctx, in, out, det, cfg)

	in <- make([]byte, 160)
	select {
	case ev := <-out:
		if ev.Kind != VADSpeech {
			t.Errorf("got %v, want Speech", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Speech event")
	}
}

// A window larger than one frame must be assembled across several frames
// before the detector runs (the real config: 256-sample window, 160/frame).
func TestRunVAD_AssemblesWindowAcrossFrames(t *testing.T) {
	cfg := vadTestCfg()
	cfg.WindowSize = 256 // needs two 160-sample frames to fill one window
	in := make(chan []byte, 4)
	out := make(chan VADEvent, 8)
	det := &fakeDetector{probs: []float32{0.9}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runVAD(ctx, in, out, det, cfg)

	in <- make([]byte, 160) // 160 samples buffered — no window yet
	in <- make([]byte, 160) // 320 total → one 256 window → Detect → Speech
	select {
	case ev := <-out:
		if ev.Kind != VADSpeech {
			t.Errorf("got %v, want Speech", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: window was not assembled across frames")
	}
}

// Cancelling the context stops the goroutine and resets the detector state.
func TestRunVAD_StopsAndResetsOnCancel(t *testing.T) {
	cfg := vadTestCfg()
	cfg.WindowSize = 160
	in := make(chan []byte, 4)
	out := make(chan VADEvent, 8)
	det := &fakeDetector{}
	ctx, cancel := context.WithCancel(context.Background())
	go runVAD(ctx, in, out, det, cfg)

	cancel()
	deadline := time.After(2 * time.Second)
	for det.resets.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("detector was not Reset after context cancel")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// --- vadService (SOP-116 pattern) -----------------------------------------------

// A vadService relays frames sent via VADInput to events received via
// VADOutput, stamping the constructor-fixed SessionID onto every event.
func TestVADService_SendRecv(t *testing.T) {
	cfg := vadConfig{
		WindowSize:    160, // one window per 160-sample (20ms @8k) frame
		SpeechThresh:  0.5,
		SilenceThresh: 0.35,
		SampleRateHz:  8000,
		EndSilenceMS:  40, // 2 silence windows to EOU
	}
	det := &fakeDetector{probs: []float32{0.9, 0.1, 0.1}} // speech, silence, EOU
	vs := newVADService("call-1", det, cfg, 4, 8)
	defer vs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := vs.In.Send(ctx, make([]byte, 160)); err != nil {
			t.Fatalf("send frame %d: %v", i, err)
		}
	}

	wantKinds := []VADKind{VADSpeech, VADSilence, VADEndOfUtterance}
	for i, want := range wantKinds {
		ev, err := vs.Out.Recv(ctx)
		if err != nil {
			t.Fatalf("recv event %d: %v", i, err)
		}
		if ev.Kind != want {
			t.Errorf("event %d: got %v, want %v", i, ev.Kind, want)
		}
		if ev.SessionID != "call-1" {
			t.Errorf("event %d: SessionID = %q, want %q", i, ev.SessionID, "call-1")
		}
	}
}

// TestDefaultVADConfigInvariants pins the ordering the VAD design rests on.
// These are tuning knobs, and a tuning pass is exactly when they get moved
// without anyone re-deriving what depends on them.
func TestDefaultVADConfigInvariants(t *testing.T) {
	c := defaultVADConfig()

	// utterance-end << turn-end (SOP-154 Observable behavior #4). The full
	// EndSilenceMS < TurnEndSilenceMS < MaxSilenceMS ordering invariant,
	// including rejection of a violating config, is covered by
	// TestVADConfig_ValidateOrdering below.
	if c.EndSilenceMS >= c.TurnEndSilenceMS {
		t.Errorf("EndSilenceMS (%d) must be < TurnEndSilenceMS (%d): a turn cannot end before the utterance that started it",
			c.EndSilenceMS, c.TurnEndSilenceMS)
	}

	// Windowing has to be sane, or every threshold above is measured in
	// windows of zero milliseconds.
	if c.windowMS() <= 0 {
		t.Fatalf("windowMS() = %d, want > 0", c.windowMS())
	}
	if windowsToCross(c.EndSilenceMS, c.windowMS()) < 1 {
		t.Errorf("EndSilenceMS (%d) is under one window (%dms): end-of-utterance would fire on the first silent window",
			c.EndSilenceMS, c.windowMS())
	}
}

// TestVADConfig_ValidateOrdering pins SOP-154 Observable behavior #4: the
// utterance-end << turn-end << idle ordering (EndSilenceMS < TurnEndSilenceMS
// < MaxSilenceMS) is asserted, not assumed, and a config that violates either
// leg is rejected rather than silently producing a broken timer race.
func TestVADConfig_ValidateOrdering(t *testing.T) {
	const maxSilenceMS = 15000

	valid := vadConfig{EndSilenceMS: 700, TurnEndSilenceMS: 5000}.withDefaults()
	if err := valid.validateOrdering(maxSilenceMS); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	endNotBeforeTurn := vadConfig{EndSilenceMS: 5000, TurnEndSilenceMS: 5000}.withDefaults()
	if err := endNotBeforeTurn.validateOrdering(maxSilenceMS); err == nil {
		t.Error("EndSilenceMS == TurnEndSilenceMS: want rejection, got nil error")
	}

	turnNotBeforeMax := vadConfig{EndSilenceMS: 700, TurnEndSilenceMS: maxSilenceMS}.withDefaults()
	if err := turnNotBeforeMax.validateOrdering(maxSilenceMS); err == nil {
		t.Error("TurnEndSilenceMS == MaxSilenceMS: want rejection, got nil error")
	}
}

// --- DefaultVADConfig (SOP-152) -----------------------------------------------

// DefaultVADConfig is the audio tap's only route to the running VAD config, and
// the tap stamps it into every recording's sidecar so a later replay knows which
// thresholds produced that call's log. It must therefore report
// defaultVADConfig() itself and never its own copy of the values: a copy goes
// stale the next time a threshold is tuned -- which is not hypothetical, f5cad49
// moved EndSilenceMS 700 -> 1050 and the turn-end work moves it again -- and a
// stale copy would mislabel every recording written after the drift, silently.
func TestDefaultVADConfig_MatchesInternal(t *testing.T) {
	if got, want := DefaultVADConfig(), defaultVADConfig(); got != want {
		t.Errorf("DefaultVADConfig() = %+v\n            want %+v\n(accessor has drifted from the config it reports)", got, want)
	}
}
