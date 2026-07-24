package telephony_test

import (
	"bytes"
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

// syncBuffer wraps bytes.Buffer with a mutex so a background session
// goroutine's log.Printf writes and a test's polling reads can safely race
// each other under -race: log.Logger itself serializes concurrent writers,
// but a bare bytes.Buffer has no such protection for a concurrent reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestTransitionTableIsTotal iterates every (state, source) pair and asserts
// the transition table has an explicit, non-nil entry for it -- a new state
// or source added without a corresponding table entry must fail this test.
func TestTransitionTableIsTotal(t *testing.T) {
	if len(telephony.AllStates) != 8 {
		t.Fatalf("AllStates: got %d states, want 8", len(telephony.AllStates))
	}
	if len(telephony.AllSources) != 11 {
		t.Fatalf("AllSources: got %d sources, want 11", len(telephony.AllSources))
	}
	for _, st := range telephony.AllStates {
		for _, src := range telephony.AllSources {
			if !telephony.TransitionHandlerDefined(st, src) {
				t.Errorf("no transition handler for (state=%s, source=%s)", st, src)
			}
		}
	}
}

// TestCallDroppedBeforeAnyAudio drives a session's control-plane input with
// a "stop" event before any media frame ever arrives, and asserts the state
// machine transitions cleanly to Closed with no panic and no hang.
func TestCallDroppedBeforeAnyAudio(t *testing.T) {
	control := telephony.NewBufferedChan[telephony.ControlEvent](1)
	if err := control.Send(context.Background(), telephony.ControlEvent{Kind: "stop", CallSID: "call-1"}); err != nil {
		t.Fatalf("seed control event: %v", err)
	}

	s := telephony.NewSession(context.Background(), "call-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioControlInput(control),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	deadline := time.After(2 * time.Second)
	for {
		if s.State() == telephony.StateClosed {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("state did not reach Closed within deadline; got %s", s.State())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestSTTResultOutOfOrderIsReported delivers an STTResult whose SessionID
// does not match the session it arrived on, and asserts the mismatch is
// dropped and logged loudly rather than acted on.
func TestSTTResultOutOfOrderIsReported(t *testing.T) {
	logBuf := &syncBuffer{}
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	})

	sttOut := telephony.NewBufferedChan[telephony.STTResult](1)
	if err := sttOut.Send(context.Background(), telephony.STTResult{SessionID: "wrong", Text: "hello"}); err != nil {
		t.Fatalf("seed STT result: %v", err)
	}

	s := telephony.NewSession(context.Background(), "correct",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithSTTOutput(sttOut),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(logBuf.Bytes(), []byte("unexpected")) || bytes.Contains(logBuf.Bytes(), []byte("mismatch")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected log output to report the out-of-order STT result (\"unexpected\" or \"mismatch\"), got: %q", logBuf.String())
}

// TestUnexpectedControlEventLogsAndStaysInState covers the "unexpected
// input" branch of the ticket's Observable behavior #2: a control-plane
// event that isn't "stop" has no wired handler in Idle/Listening, so it
// must route to the warn/debug handler -- logging LOUDLY and remaining in
// state, never panicking or silently dropping.
func TestUnexpectedControlEventLogsAndStaysInState(t *testing.T) {
	logBuf := &syncBuffer{}
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	})

	control := telephony.NewBufferedChan[telephony.ControlEvent](1)
	if err := control.Send(context.Background(), telephony.ControlEvent{Kind: "mark", CallSID: "call-2"}); err != nil {
		t.Fatalf("seed control event: %v", err)
	}

	s := telephony.NewSession(context.Background(), "call-2",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioControlInput(control),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(logBuf.Bytes(), []byte("unexpected")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("unexpected")) {
		t.Fatalf("expected log output to report the unexpected control event, got: %q", logBuf.String())
	}
	if got := s.State(); got == telephony.StateClosed {
		t.Errorf("state: got %s, want session to remain non-Closed on an unexpected control event", got)
	}
}

// TestClosedSessionIgnoresFurtherDataFrames covers a state-interaction gap:
// once a session has reached Closed (via a "stop" control event), any
// subsequent data frame must be absorbed with no state change and no panic
// -- Closed is terminal (buildTransitionTable wires every source in Closed
// to handleClosed).
func TestClosedSessionIgnoresFurtherDataFrames(t *testing.T) {
	control := telephony.NewBufferedChan[telephony.ControlEvent](1)
	data := telephony.NewBufferedChan[[]byte](2)

	s := telephony.NewSession(context.Background(), "call-3",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioControlInput(control),
		telephony.WithTwilioDataInput(data),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := control.Send(ctx, telephony.ControlEvent{Kind: "stop", CallSID: "call-3"}); err != nil {
		t.Fatalf("send stop: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.State() != telephony.StateClosed {
		time.Sleep(10 * time.Millisecond)
	}
	if s.State() != telephony.StateClosed {
		t.Fatalf("state before posting a late frame: got %s, want Closed", s.State())
	}

	if err := data.Send(ctx, windowFrame(0x01)); err != nil {
		t.Fatalf("send late frame: %v", err)
	}

	time.Sleep(200 * time.Millisecond) // give run() a chance to (mis)handle it
	if got := s.State(); got != telephony.StateClosed {
		t.Fatalf("state after late frame: got %s, want it to remain Closed", got)
	}
}

// speechThenSilenceProbs builds a probability sequence a fakeDetector can
// replay: speechN windows at 0.9 (above default SpeechThresh) followed by
// silenceN windows at 0.1 (below default SilenceThresh).
func speechThenSilenceProbs(speechN, silenceN int) []float32 {
	probs := make([]float32, 0, speechN+silenceN)
	for i := 0; i < speechN; i++ {
		probs = append(probs, 0.9)
	}
	for i := 0; i < silenceN; i++ {
		probs = append(probs, 0.1)
	}
	return probs
}

// --- call termination (SOP-125) ---------------------------------------------

// TestMarkEchoTimeoutDerivedFromClip guards that the mark-echo timeout is
// genuinely derived from the farewell clip's length, not a hardcoded
// constant: a synthetic clip of a different length must yield a different
// timeout, and the real clip's timeout must exceed its own playout duration
// (the grace margin must be positive).
func TestMarkEchoTimeoutDerivedFromClip(t *testing.T) {
	timeout1 := telephony.MarkEchoTimeout(assets.FarewellULaw)

	synthetic := make([]byte, len(assets.FarewellULaw)*2)
	timeout2 := telephony.MarkEchoTimeout(synthetic)

	if timeout2 == timeout1 {
		t.Fatalf("MarkEchoTimeout did not change when clip length changed: timeout1=%v (len=%d), timeout2=%v (len=%d) -- looks hardcoded",
			timeout1, len(assets.FarewellULaw), timeout2, len(synthetic))
	}

	clipOnly := time.Duration(len(assets.FarewellULaw)*1000/telephony.SampleRateHz) * time.Millisecond
	if timeout1 <= clipOnly {
		t.Fatalf("MarkEchoTimeout: got %v, want > clip-only duration %v (grace margin must be positive)", timeout1, clipOnly)
	}
}

// TestTermination_MarkEchoReceived mocks all services, triggers idle
// timeout, and asserts: farewell frames are sent on the data output, a mark
// is sent on the control output, and once the mark echo is delivered on the
// control input, the session reaches Closed and its close func is called.
func TestTermination_MarkEchoReceived(t *testing.T) {
	control := telephony.NewBufferedChan[telephony.ControlEvent](4)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)

	var closedCount int
	var closedMu sync.Mutex
	closeFunc := func() {
		closedMu.Lock()
		closedCount++
		closedMu.Unlock()
	}

	s := telephony.NewSession(context.Background(), "call-term-1",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioControlInput(control),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithCloseFunc(closeFunc),
		telephony.WithMaxSilenceMS(50),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Drain farewell frames off dataOut until the whole clip has arrived.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []byte
	for len(got) < len(assets.FarewellULaw) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv farewell frame: %v (got %d/%d bytes so far)", err, len(got), len(assets.FarewellULaw))
		}
		got = append(got, frame...)
	}
	if !bytes.Equal(got, assets.FarewellULaw) {
		t.Fatalf("farewell frames: got %d bytes not matching FarewellULaw (%d bytes)", len(got), len(assets.FarewellULaw))
	}

	mark, err := ctlOut.Recv(ctx)
	if err != nil {
		t.Fatalf("recv mark: %v", err)
	}
	if mark.Kind != telephony.ControlOutMark {
		t.Fatalf("control-out Kind: got %v, want ControlOutMark", mark.Kind)
	}

	if got := s.State(); got != telephony.StateAwaitingMarkEcho {
		t.Fatalf("state after farewell+mark: got %s, want AwaitingMarkEcho", got)
	}

	if err := control.Send(ctx, telephony.ControlEvent{Kind: "mark", MarkName: mark.MarkName, CallSID: "call-term-1"}); err != nil {
		t.Fatalf("send mark echo: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.State() != telephony.StateClosed {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.State(); got != telephony.StateClosed {
		t.Fatalf("state after mark echo: got %s, want Closed", got)
	}

	closedMu.Lock()
	n := closedCount
	closedMu.Unlock()
	if n != 1 {
		t.Fatalf("closeFunc call count: got %d, want 1", n)
	}
}

// TestTermination_MarkEchoTimeout mirrors TestTermination_MarkEchoReceived
// but never delivers the mark echo: the mark-echo timeout must fire on its
// own, log the mark-protocol warning, and still close the session.
func TestTermination_MarkEchoTimeout(t *testing.T) {
	logBuf := &syncBuffer{}
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	})

	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)

	var closedCount int
	var closedMu sync.Mutex
	closeFunc := func() {
		closedMu.Lock()
		closedCount++
		closedMu.Unlock()
	}

	s := telephony.NewSession(context.Background(), "call-term-2",
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

	// Drain the farewell clip and the mark, then deliberately never echo it.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []byte
	for len(got) < len(assets.FarewellULaw) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv farewell frame: %v", err)
		}
		got = append(got, frame...)
	}
	if _, err := ctlOut.Recv(ctx); err != nil {
		t.Fatalf("recv mark: %v", err)
	}

	// The mark-echo timeout (derived from the real FarewellULaw clip: ~2s +
	// MarkEchoGraceMS) must fire on its own and close the session anyway.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.State() != telephony.StateClosed {
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.State(); got != telephony.StateClosed {
		t.Fatalf("state after mark-echo timeout: got %s, want Closed", got)
	}

	if !bytes.Contains(logBuf.Bytes(), []byte("mark protocol")) {
		t.Fatalf("expected log output to mention \"mark protocol\", got: %q", logBuf.String())
	}

	closedMu.Lock()
	n := closedCount
	closedMu.Unlock()
	if n != 1 {
		t.Fatalf("closeFunc call count: got %d, want 1", n)
	}
}
