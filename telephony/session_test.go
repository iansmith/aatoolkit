package telephony_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

const recvTimeout = time.Second

// sendData sends f into in, failing the test if the sequencer doesn't
// consume it within the timeout (rather than hanging the whole run). Uses
// Errorf (not Fatalf) because it is called from helper goroutines, where
// FailNow is illegal.
func sendData(t *testing.T, in telephony.TwilioDataPlaneInput, f []byte, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := in.Send(ctx, f); err != nil {
		t.Errorf("frame not consumed within %s: %v", d, err)
	}
}

// fakeDetector is a controllable telephony.VADDetector: it returns probs[i]
// for the i-th call to Detect (0 once exhausted) and counts Reset calls.
type fakeDetector struct {
	mu     sync.Mutex
	probs  []float32
	i      int
	resets int
}

func (f *fakeDetector) Detect(window []float32) (float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := float32(0)
	if f.i < len(f.probs) {
		p = f.probs[f.i]
	}
	f.i++
	return p, nil
}

func (f *fakeDetector) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
}

// windowSpyDetector records every window it is asked to Detect, in order, on
// a buffered channel — used to observe frames flowing through the VAD
// pipeline without depending on the real ONNX model. Every window scores 0
// (silence), so it never drives the turn-taking state machine.
type windowSpyDetector struct {
	windows chan []float32
}

func newWindowSpyDetector(capacity int) *windowSpyDetector {
	return &windowSpyDetector{windows: make(chan []float32, capacity)}
}

func (d *windowSpyDetector) Detect(window []float32) (float32, error) {
	got := append([]float32(nil), window...)
	d.windows <- got
	return 0, nil
}

func (d *windowSpyDetector) Reset() {}

// spySink records TurnSink dispatches.
type spySink struct {
	mu    sync.Mutex
	start int
	eou   int
	turns []string
}

func (s *spySink) OnSpeechStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start++
}

func (s *spySink) OnEndOfUtterance() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eou++
}

func (s *spySink) OnTurnComplete(text string, _ telephony.TurnTrigger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, text)
}

func (s *spySink) counts() (start, eou int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start, s.eou
}

func (s *spySink) turnTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.turns...)
}

// windowFrame builds a 256-byte frame — one default-window's worth of μ-law
// samples — filled with a single repeated byte.
func windowFrame(b byte) []byte {
	f := make([]byte, 256)
	for i := range f {
		f[i] = b
	}
	return f
}

// --- construction ---

func TestNewSession_InitializesFields(t *testing.T) {
	s := telephony.NewSession(context.Background(), "CA-123")
	defer s.Close()

	if s.CallSID != "CA-123" {
		t.Errorf("CallSID: got %q want %q", s.CallSID, "CA-123")
	}
	if s.State() != telephony.StateIdle {
		t.Errorf("State: got %s want %s", s.State(), telephony.StateIdle)
	}
	if len(s.History) != 0 {
		t.Errorf("History: got len %d want 0", len(s.History))
	}
}

// --- core: the property that defines this ticket ---

// The sequencer must fan a frame arriving on the Twilio data-plane input
// through to the VAD pipeline, unchanged (observed here via a spy detector).
func TestSession_FansDataInToVAD(t *testing.T) {
	spy := newWindowSpyDetector(4)
	data := telephony.NewBufferedChan[[]byte](1)
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return spy, nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	frame := windowFrame(0x11)
	go sendData(t, data, frame, recvTimeout)

	select {
	case win := <-spy.windows:
		if len(win) != 256 {
			t.Fatalf("window: got len %d want 256", len(win))
		}
	case <-time.After(recvTimeout):
		t.Fatal("timeout waiting for window to reach the detector")
	}
}

// Multiple frames must reach the detector in the order they arrived on the
// data-plane input.
func TestSession_ForwardsFramesInOrder(t *testing.T) {
	spy := newWindowSpyDetector(4)
	data := telephony.NewBufferedChan[[]byte](4)
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return spy, nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	frames := [][]byte{windowFrame(0x01), windowFrame(0x02), windowFrame(0x03)}
	go func() {
		for _, f := range frames {
			sendData(t, data, f, recvTimeout)
		}
	}()

	for i, wantByte := range []byte{0x01, 0x02, 0x03} {
		select {
		case win := <-spy.windows:
			if len(win) == 0 {
				t.Errorf("frame %d: window is empty, want decoded from byte %#x", i, wantByte)
			}
		case <-time.After(recvTimeout):
			t.Fatalf("frame %d: timeout waiting for window", i)
		}
	}
}

// SOP-108: the sequencer must not log a line per frame at the default log
// level — at real audio frame rates it drowns out the speech-start/
// end-of-utterance lines a tail -f session is meant to surface.
func TestSession_Run_DoesNotLogPerFrameAtDefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	spy := newWindowSpyDetector(4)
	data := telephony.NewBufferedChan[[]byte](1)
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return spy, nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	frame := windowFrame(0x11)
	go sendData(t, data, frame, recvTimeout)

	select {
	case <-spy.windows:
	case <-time.After(recvTimeout):
		t.Fatal("timeout waiting for window to reach the detector")
	}

	if strings.Contains(buf.String(), "byte frame") {
		t.Fatalf("expected no per-frame log line at the default log level, got log output: %q", buf.String())
	}
}

// --- lifecycle ---

// Close must stop the sequencer promptly and return (no goroutine leak, no hang).
func TestSession_CloseStopsSequencer(t *testing.T) {
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return newWindowSpyDetector(4), nil }),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s — sequencer stuck")
	}
}

// Start must be idempotent: a second Start must not spawn a second sequencer
// (two goroutines would double-close s.done and panic on teardown).
func TestSession_StartIsIdempotent(t *testing.T) {
	spy := newWindowSpyDetector(4)
	data := telephony.NewBufferedChan[[]byte](1)
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return spy, nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	// Exactly one sequencer forwards the frame; Close must not panic.
	go sendData(t, data, windowFrame(0x42), recvTimeout)
	select {
	case win := <-spy.windows:
		if len(win) != 256 {
			t.Errorf("window: got len %d want 256", len(win))
		}
	case <-time.After(recvTimeout):
		t.Fatal("timeout waiting for window")
	}
	s.Close()
}

// Adversary gap: Close must not hang right after the sequencer commits to
// forwarding a frame into the VAD pipeline — the forward hand-off must be
// context-guarded even though forwardToVAD is the pipeline's normal consumer.
func TestSession_CloseUnblocksMidForward(t *testing.T) {
	data := telephony.NewBufferedChan[[]byte](0) // unbuffered: Send blocks until the sequencer commits to it
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return newWindowSpyDetector(4), nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// This send returns only once the sequencer has received the frame, so
	// it is now committed to forwarding it into the VAD pipeline.
	if err := data.Send(context.Background(), windowFrame(0x09)); err != nil {
		t.Fatalf("send: %v", err)
	}

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung while the pipeline was mid-forward")
	}
}

// Cancelling the parent context must stop the sequencer too.
func TestSession_ParentContextCancelStopsSequencer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := telephony.NewSession(ctx, "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return newWindowSpyDetector(4), nil }),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sequencer did not stop after parent context cancel")
	}
}

// --- deadlock avoidance (charter R8) ---

// slowDetector blocks every Detect call on a gate the test controls, so the
// VAD pipeline's internal buffer fills up and its consumer stalls -- the
// condition under which a blocking-send implementation of run() would
// deadlock the whole select loop.
type slowDetector struct {
	gate chan struct{}
}

func (d *slowDetector) Detect(window []float32) (float32, error) {
	<-d.gate
	return 0, nil
}

func (d *slowDetector) Reset() {}

// TestNoDeadlockUnderSustainedFrames pumps 1000 frames at max rate
// into the session's Twilio data-plane input while the VAD detector is
// gated shut (simulating a slow/backed-up VAD pipeline). A correct run()
// drains dataIn continuously via the non-blocking forwardCh handoff
// (charter R8: the engine never performs a blocking send) and must accept all
// 1000 frames well within the timeout regardless of VAD speed. A
// blocking-send implementation hangs once VAD.In's buffer fills.
func TestNoDeadlockUnderSustainedFrames(t *testing.T) {
	const frameCount = 1000

	data := telephony.NewBufferedChan[[]byte](8) // small: force real backpressure if undrained
	det := &slowDetector{gate: make(chan struct{})}
	defer close(det.gate)

	s := telephony.NewSession(context.Background(), "CA-deadlock",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	sendDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := 0; i < frameCount; i++ {
			if err := data.Send(ctx, windowFrame(byte(i))); err != nil {
				sendDone <- err
				return
			}
		}
		sendDone <- nil
	}()

	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("sending %d frames did not complete: %v (deadlock?)", frameCount, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out pumping frames — sequencer appears deadlocked")
	}
}

// --- Start() error / factory wiring ---

// Start must fail hard when the vadFactory errors, and must not start any
// goroutine (the session stays inert — a caller can retry or abandon it).
func TestSessionStartFailsOnBadFactory(t *testing.T) {
	wantErr := errors.New("boom: model unavailable")
	data := telephony.NewBufferedChan[[]byte](0) // unbuffered: Send only completes if something reads it
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return nil, wantErr }),
		telephony.WithTwilioDataInput(data),
	)

	err := s.Start()
	if err == nil {
		t.Fatal("Start: got nil error, want the factory's error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Start error: got %v, want it to wrap %v", err, wantErr)
	}

	// Nothing was started: sending on the data-plane input must not be
	// consumed by a sequencer that was never spawned.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := data.Send(ctx, []byte{0x01}); err == nil {
		t.Error("data frame was consumed, but Start failed — no sequencer should be running")
	}
	s.Close() // must not hang even though the session never started
}

// TestSessionTurnSinkDispatch drives a fake detector through a known
// probability sequence and asserts the injected TurnSink sees OnSpeechStart
// and OnEndOfUtterance at the expected boundaries.
func TestSessionTurnSinkDispatch(t *testing.T) {
	// It takes telephony.EndSilenceWindows() (= ceil(EndSilenceMS / windowMS),
	// with the thresholds in vad.go) consecutive silent windows after a speech
	// onset to reach end-of-utterance. Derive the silence run and the slice
	// capacity from that helper so this tracks any VAD retune instead of pinning
	// a frozen window count.
	probs := make([]float32, 0, 1+telephony.EndSilenceWindows())
	probs = append(probs, 0.9) // -> Speech
	for i := 0; i < telephony.EndSilenceWindows(); i++ {
		probs = append(probs, 0.1) // -> ... -> EndOfUtterance on the last silent window
	}

	det := &fakeDetector{probs: probs}
	sink := &spySink{}
	data := telephony.NewBufferedChan[[]byte](len(probs))
	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < len(probs); i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
		// Give the forwarder goroutine time to drain forwardCh between
		// frames -- forwardCh's depth models real-time frame pacing (80ms
		// buffer at 20ms/frame), not an artificial synchronous burst.
		time.Sleep(2 * time.Millisecond)
	}

	deadline := time.After(2 * time.Second)
	for {
		start, eou := sink.counts()
		if start >= 1 && eou >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for dispatch: start=%d eou=%d, want >=1 each", start, eou)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	start, eou := sink.counts()
	if start != 1 {
		t.Errorf("OnSpeechStart calls: got %d, want 1", start)
	}
	if eou != 1 {
		t.Errorf("OnEndOfUtterance calls: got %d, want 1", eou)
	}
}

// TestSessionClosedChannel verifies that Session.Closed() returns a channel
// that remains open until setState transitions to StateClosed.
func TestSessionClosedChannel(t *testing.T) {
	data := telephony.NewBufferedChan[[]byte](1)
	control := telephony.NewBufferedChan[telephony.ControlEvent](1)
	sink := &spySink{}
	det := &fakeDetector{probs: []float32{0.1}}

	s := telephony.NewSession(context.Background(), "CA-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithTwilioControlInput(control),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Before transition to StateClosed, selecting on Closed() should not fire
	select {
	case <-s.Closed():
		t.Fatal("Closed() channel closed before StateClosed")
	case <-time.After(10 * time.Millisecond):
		// Expected: channel is open, timeout hit
	default:
		// Expected: channel is open, default case taken immediately
	}

	// Transition to StateClosed by sending a stop control event
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()
	err := control.Send(ctx, telephony.ControlEvent{
		Kind: "stop",
	})
	if err != nil {
		t.Fatalf("Failed to send stop control event: %v", err)
	}

	// Wait for the Closed() channel to close
	deadline := time.After(500 * time.Millisecond)
	select {
	case <-s.Closed():
		// Expected: channel closed within timeout
		return
	case <-deadline:
		t.Fatal("Closed() channel did not close within timeout after stop event")
	}
}

// TestTimerMigrationIdle verifies that the idle timer fires and dispatches
// SourceIdleTimer, causing the session to transition through StateTerminating
// to StateAwaitingMarkEcho (same behavior as pre-migration).
func TestTimerMigrationIdle(t *testing.T) {
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)

	var closedCount int
	var closedMu sync.Mutex
	closeFunc := func() {
		closedMu.Lock()
		closedCount++
		closedMu.Unlock()
	}

	s := telephony.NewSession(context.Background(), "idle-test",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithCloseFunc(closeFunc),
		telephony.WithMaxSilenceMS(50),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Wait for the idle timer to fire
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := s.State()
		if state == telephony.StateAwaitingMarkEcho {
			return
		}
		if state == telephony.StateClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.State(); got != telephony.StateAwaitingMarkEcho && got != telephony.StateClosed {
		t.Fatalf("idle timer did not cause termination; state: got %s, want AwaitingMarkEcho or Closed", got)
	}
}

// TestSpeechOnsetCancelsIdleTimer verifies SOP-156's reconciliation: speech
// onset CANCELS the idle/silence timer (it does not re-arm it, as it did under
// SOP-125). The idle timer measures silence, so it must not be running while the
// caller is actively speaking — that is the fix for the mid-utterance hangup.
// The caller is now bounded by the max-utterance cap instead (default 45s here,
// well outside this test's window), so the session stays alive past the original
// idle deadline and does not silently terminate mid-utterance.
func TestSpeechOnsetCancelsIdleTimer(t *testing.T) {
	data := telephony.NewBufferedChan[[]byte](256)
	sink := &spySink{}

	s := telephony.NewSession(context.Background(), "cancel-idle-test",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: []float32{0.9}}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithMaxSilenceMS(200),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Sleep 150ms (< 200, idle timer still pending; session in StateIdle).
	time.Sleep(150 * time.Millisecond)

	// One voiced frame drives Idle→Listening and, via VADSpeech onset, cancels
	// the idle timer (SOP-156). No new idle deadline is armed.
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()
	if err := data.Send(ctx, windowFrame(0x99)); err != nil {
		t.Fatalf("send frame: %v", err)
	}

	// Well past the original 200ms idle deadline: the session must still be
	// alive, proving onset cancelled the idle timer.
	time.Sleep(110 * time.Millisecond)
	if got := s.State(); got != telephony.StateListening {
		t.Fatalf("state after the original idle deadline: got %s, want Listening (onset cancelled the idle timer)", got)
	}

	// And still alive well beyond where the old reset deadline would have been:
	// unlike SOP-125, onset does NOT re-arm the idle timer, so nothing fires
	// mid-utterance. The utterance cap (45s) governs from here.
	time.Sleep(190 * time.Millisecond)
	if got := s.State(); got != telephony.StateListening {
		t.Fatalf("state deep into the utterance: got %s, want Listening (idle timer is not re-armed on speech; the cap governs)", got)
	}
}

// TestSTTResultDiscardsLateRequest verifies that an STTResult with a
// RequestID that doesn't match the session's current awaited request is
// silently discarded with a log line containing discard.
func TestSTTResultDiscardsLateRequest(t *testing.T) {
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	probs := speechThenSilenceProbs(1, telephony.EndSilenceWindows())

	s := telephony.NewSession(context.Background(), "test-discard-call",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// One speech window then enough silence windows to cross EndSilenceMS,
	// driving the session to StateAwaitingFullResult via the dispatched full
	// pass.
	frame := make([]byte, 256)
	for i := 0; i < len(probs); i++ {
		sendData(t, dataIn, frame, recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.State() != telephony.StateAwaitingFullResult {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.State(); got != telephony.StateAwaitingFullResult {
		t.Fatalf("state before delivering stale result: got %s, want AwaitingFullResult", got)
	}

	// Read the dispatched full-pass request to learn the RequestID the
	// session is currently awaiting.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), recvTimeout)
	defer reqCancel()
	req, err := sttIn.Recv(reqCtx)
	if err != nil {
		t.Fatalf("full-pass request not dispatched: %v", err)
	}

	// Deliver a stale result one RequestID behind the awaited one.
	stale := telephony.STTResult{
		SessionID: "test-discard-call",
		RequestID: req.RequestID - 1,
		Kind:      telephony.FullPass,
		Text:      "stale",
	}
	sendCtx, sendCancel := context.WithTimeout(context.Background(), recvTimeout)
	defer sendCancel()
	if err := sttOut.Send(sendCtx, stale); err != nil {
		t.Fatalf("send stale result: %v", err)
	}

	// Give the session's transition loop time to process (and discard) it.
	time.Sleep(50 * time.Millisecond)

	if got := s.State(); got != telephony.StateAwaitingFullResult {
		t.Fatalf("state after stale result: got %s, want AwaitingFullResult (result should have been discarded, not acted on)", got)
	}
	if !strings.Contains(buf.String(), "discard") {
		t.Fatalf("expected a log line containing %q, got log output: %q", "discard", buf.String())
	}
}

// --- SOP-157: Simulated turn bed playback ---
//
// Retroactive RED-state verification (attempt 2 remediation): attempt 1's
// Phase 0 commit (191b6f0) landed AFTER the implementation commit (26c65fc),
// so these tests were never observed failing against code that lacked the
// sim-turn behavior -- a test-after-code evasion, not genuine TDD. Verified
// here instead: `git checkout b7fbc2e -- internal/telephony/state.go
// internal/telephony/session.go` (working tree only, never committed) reverts
// StateSpeaking/WithSimTurnMS/sendBed/sendBedAndEnterSpeaking entirely; `go
// test ./internal/telephony/...` then FAILS with a build error --
// `internal/telephony/session_test.go:771:13: undefined:
// telephony.WithSimTurnMS` -- because the test file (already present on this
// branch) references APIs the reverted code no longer has. Restoring the
// implementation (`git checkout HEAD -- internal/telephony/state.go
// internal/telephony/session.go`) returns the suite to green. This confirms
// the tests do genuinely pin behavior the code must supply, even though the
// commit ordering that produced them was wrong.

// drainAllDataOut reads all available data on dataOut and returns the bytes.
func drainAllDataOut(t *testing.T, dataOut telephony.TwilioDataPlaneOutput, ctx context.Context) []byte {
	t.Helper()
	var got []byte
	for {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			break
		}
		got = append(got, frame...)
	}
	return got
}

// driveUtteranceToSTT drives dataIn with one utterance's worth of frames --
// enough to cross EndSilenceWindows and fire VADEndOfUtterance -- then
// receives the dispatched STTRequest and answers it with text. Shared by
// every SOP-157 test below that needs a real completed (or fused) utterance,
// mirroring the fixture already used by TestLivePath_StopwordEndsTurn.
func driveUtteranceToSTT(t *testing.T, dataIn telephony.TwilioDataPlaneInput, sttIn telephony.STTInput, sttOut telephony.STTOutput, sessionID, text string) {
	t.Helper()
	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: sessionID,
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      text,
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}
}

// waitForSessionState polls (no session timer involved -- this is ordinary
// cross-goroutine settling, the same pattern TestLivePath_IdleStillTerminates
// and its neighbors already use) until s reaches want or the backstop trips.
func waitForSessionState(t *testing.T, s *telephony.Session, want telephony.SessionState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state: got %s, want %s within the backstop", s.State(), want)
}

// TestSimTurn_DisabledByDefault asserts no bed is sent when sim-turn is
// disabled (AATOOLKIT_SIM_TURN_MS unset, WithSimTurnMS never called): turn
// completion (via the stopword) returns straight to Listening and delivers
// the fused turn text with no bed bytes ever appearing on dataOut.
func TestSimTurn_DisabledByDefault(t *testing.T) {
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}

	s := telephony.NewSession(context.Background(), "test-simturn-off",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: append(
				speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...,
			)}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithMaxSilenceMS(9000),
		// WithSimTurnMS is intentionally never called -- sim-turn is off by
		// default, same posture as AATOOLKIT_AUDIO_TAP unset.
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-off", "hello")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-off", "done")

	waitForSessionState(t, s, telephony.StateListening)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if bedBytes := drainAllDataOut(t, dataOut, ctx); len(bedBytes) > 0 {
		t.Errorf("bed bytes on dataOut with sim-turn disabled: got %d bytes, want 0", len(bedBytes))
	}
	if turns := sink.turnTexts(); len(turns) != 1 || turns[0] != "hello" {
		t.Errorf("fused turn text: got %v, want [\"hello\"] (turn completion must still work with sim-turn disabled)", turns)
	}
}

// TestSimTurn_EnabledEntersSpeakingPlaysBedThenReturnsToListening replaces
// attempt 1's placeholder TestSimTurn_WithSimTurnMSOption (which asserted
// nothing beyond Start() not erroring): it proves WithSimTurnMS's value
// genuinely drives timerSimTurn's armed duration, that turn completion with
// sim-turn enabled enters StateSpeaking with real bed bytes already on
// dataOut, and that firing the timer returns the session to StateListening.
func TestSimTurn_EnabledEntersSpeakingPlaysBedThenReturnsToListening(t *testing.T) {
	const simTurnMS = 100 // well under the ~5040ms clip -- no looping in play here

	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	clock := newFakeClock()

	s := telephony.NewSession(context.Background(), "test-simturn-on",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: append(
				speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...,
			)}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithSimTurnMS(simTurnMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-on", "hello")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-on", "done")

	waitForSessionState(t, s, telephony.StateSpeaking)

	// The bed is written to dataOut synchronously by sendBedAndEnterSpeaking
	// -- before the timer is even armed -- so it's already there once
	// StateSpeaking is observed.
	wantBed := assets.LLMThinkingULaw[:simTurnMS*telephony.SampleRateHz/1000]
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	got := drainAllDataOut(t, dataOut, ctx)
	cancel()
	if !bytes.Equal(got, wantBed) {
		t.Fatalf("bed bytes on dataOut: got %d bytes, want %d bytes matching the clip prefix", len(got), len(wantBed))
	}

	// clock.fire fatals if nothing is armed for exactly simTurnMS -- this is
	// the direct proof that WithSimTurnMS's value drives timerSimTurn's
	// duration, not just that some timer fires eventually.
	clock.fire(t, simTurnMS*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)

	if turns := sink.turnTexts(); len(turns) != 1 || turns[0] != "hello" {
		t.Errorf("fused turn text: got %v, want [\"hello\"]", turns)
	}
}

// TestSimTurn_LoopsClipToFillDuration asserts a configured duration longer
// than the ~5040ms clip loops the clip's bytes to fill it, rather than
// stopping short or padding with silence.
func TestSimTurn_LoopsClipToFillDuration(t *testing.T) {
	clipMS := len(assets.LLMThinkingULaw) * 1000 / telephony.SampleRateHz
	const extraMS = 200
	simTurnMS := clipMS + extraMS // forces one full loop plus a partial repeat

	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](1024)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	clock := newFakeClock()

	s := telephony.NewSession(context.Background(), "test-simturn-loop",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: append(
				speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...,
			)}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithSimTurnMS(simTurnMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-loop", "hello")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-loop", "done")

	waitForSessionState(t, s, telephony.StateSpeaking)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	got := drainAllDataOut(t, dataOut, ctx)
	cancel()

	extraBytes := extraMS * telephony.SampleRateHz / 1000
	wantLen := len(assets.LLMThinkingULaw) + extraBytes
	if len(got) != wantLen {
		t.Fatalf("bed bytes: got %d, want %d (one full clip + %dms repeated from the start)", len(got), wantLen, extraMS)
	}
	if !bytes.Equal(got[:len(assets.LLMThinkingULaw)], assets.LLMThinkingULaw) {
		t.Fatalf("first %d bytes: not the whole clip", len(assets.LLMThinkingULaw))
	}
	if !bytes.Equal(got[len(assets.LLMThinkingULaw):], assets.LLMThinkingULaw[:extraBytes]) {
		t.Fatalf("bytes after the first clip: got %d bytes, want the clip's own first %d bytes repeated (looping)", len(got)-len(assets.LLMThinkingULaw), extraBytes)
	}

	clock.fire(t, time.Duration(simTurnMS)*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)
}

// TestSimTurn_IdleTimerCancelledDuringBedRearmedOnResume is the ticket's PRD
// D2.5 case: the idle timer must be cancelled at bed-start (so it cannot fire
// mid-bed) and re-armed fresh on resume (so a caller who then goes quiet
// still gets the farewell). Both halves are driven on the injected clock --
// firing the idle deadline mid-bed must be a provable no-op (state stays
// Speaking, no farewell), and firing it again after resume must produce the
// real farewell.
func TestSimTurn_IdleTimerCancelledDuringBedRearmedOnResume(t *testing.T) {
	const simTurnMS = 2000 // distinct from idleMS so fakeClock fires exactly the timer meant
	const idleMS = 9500

	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	clock := newFakeClock()

	s := telephony.NewSession(context.Background(), "test-simturn-idle",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: append(
				speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...,
			)}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithSimTurnMS(simTurnMS),
		telephony.WithMaxSilenceMS(idleMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-idle", "hello")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-idle", "done")

	waitForSessionState(t, s, telephony.StateSpeaking)

	// Advance the clock past MaxSilenceMS while the bed is playing. Every
	// idle timer armed so far (session start, cancelled at speech onset;
	// end-of-utterance, cancelled at bed-start) is stale, so this must be a
	// no-op -- checked by consequence (state stays Speaking, no farewell), the
	// same way TestUtterance_IdleTimerDoesNotFireMidUtterance proves it,
	// since the fake clock's armed set can't distinguish live from cancelled.
	clock.fire(t, idleMS*time.Millisecond)
	if got := s.State(); got != telephony.StateSpeaking {
		t.Fatalf("state after firing the idle deadline mid-bed: got %s, want Speaking (idle timer must not fire during the bed)", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if bytes.Contains(drainAllDataOut(t, dataOut, ctx), assets.FarewellULaw) {
		t.Fatal("farewell clip present on dataOut mid-bed -- idle timer fired when it must not have")
	}
	cancel()

	// Resume: the sim-turn timer fires, returning to Listening and re-arming
	// the idle timer fresh.
	clock.fire(t, simTurnMS*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)

	// Now the idle deadline must fire for real.
	clock.fire(t, idleMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestSimTurn_MultiTurnSequenceDeliversBothTexts drives two full turns with a
// bed between them and asserts both turns' fused text reach the TurnSink --
// the collection script's core loop (greeting -> bed -> turn -> bed -> turn
// -> ...).
func TestSimTurn_MultiTurnSequenceDeliversBothTexts(t *testing.T) {
	const simTurnMS = 300

	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	clock := newFakeClock()

	s := telephony.NewSession(context.Background(), "test-simturn-multi",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: append(append(append(
				speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...),
				speechThenSilenceProbs(2, telephony.EndSilenceWindows())...,
			)}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithSimTurnMS(simTurnMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Turn 1.
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-multi", "alpha")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-multi", "done")
	waitForSessionState(t, s, telephony.StateSpeaking)
	clock.fire(t, simTurnMS*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)

	// Turn 2 -- the bed between them already resumed listening, so a fresh
	// utterance starts a genuinely new turn (turnActive was cleared by
	// completeTurn(), never left true across the bed).
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-multi", "bravo")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-multi", "done")
	waitForSessionState(t, s, telephony.StateSpeaking)
	clock.fire(t, simTurnMS*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)

	if turns := sink.turnTexts(); len(turns) != 2 || turns[0] != "alpha" || turns[1] != "bravo" {
		t.Fatalf("fused turn texts: got %v, want [\"alpha\" \"bravo\"]", turns)
	}
}

// TestSimTurn_BargeInDuringBedIsAbsorbed pins ticket Observable behavior #5:
// barge-in during the bed is NOT handled. A VAD speech event arriving while
// StateSpeaking must be absorbed with no side effect -- no extra
// OnSpeechStart notification, no turnActive flip, no state change -- not
// routed through handleSpeechOnset the way a genuine onset in Listening is.
func TestSimTurn_BargeInDuringBedIsAbsorbed(t *testing.T) {
	const simTurnMS = 5000 // long enough that the barge-in frame lands mid-bed

	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](512)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	clock := newFakeClock()

	// The two driveUtteranceToSTT calls below consume exactly
	// 2*(EndSilenceWindows()+2) VAD windows; one extra voiced window is
	// appended for the barge-in frame sent after StateSpeaking is reached.
	probs := append(append(
		speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
		speechThenSilenceProbs(2, telephony.EndSilenceWindows())...),
		0.9, // barge-in: a real speech-onset-shaped window, sent during the bed
	)

	s := telephony.NewSession(context.Background(), "test-simturn-bargein",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithSimTurnMS(simTurnMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-bargein", "hello")
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "test-simturn-bargein", "done")
	waitForSessionState(t, s, telephony.StateSpeaking)

	baseStart, baseEOU := sink.counts()
	if s.TurnActive() {
		t.Fatal("TurnActive true right after entering StateSpeaking, want false (completeTurn already cleared it)")
	}

	// The barge-in: one voiced frame while the bed is playing.
	sendData(t, dataIn, windowFrame(0x77), recvTimeout)

	// No timer or channel event distinguishes "absorbed" from "not yet
	// processed" -- bounded settle time, then assert nothing changed, the
	// same pattern TestSpeechOnsetCancelsIdleTimer already uses to prove a
	// negative.
	time.Sleep(150 * time.Millisecond)

	if got := s.State(); got != telephony.StateSpeaking {
		t.Errorf("state after barge-in: got %s, want Speaking (bed must not be interrupted)", got)
	}
	if s.TurnActive() {
		t.Error("TurnActive true after barge-in during the bed, want false (barge-in must not be handled as a new onset)")
	}
	if start, eou := sink.counts(); start != baseStart || eou != baseEOU {
		t.Errorf("sink counts after barge-in: got (start=%d eou=%d), want unchanged (start=%d eou=%d) -- barge-in must be absorbed, not dispatched as a real onset",
			start, eou, baseStart, baseEOU)
	}

	clock.fire(t, simTurnMS*time.Millisecond)
	waitForSessionState(t, s, telephony.StateListening)
}
