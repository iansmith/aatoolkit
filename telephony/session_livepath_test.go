package telephony_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// TestLivePath_StopwordEndsTurn verifies that on the live Twilio path, saying
// "done" ends the turn: the fused transcript is flushed to the TurnSink.
func TestLivePath_StopwordEndsTurn(t *testing.T) {
	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	// Two speech utterances separated by silence to trigger two dispatches
	probs := append(speechThenSilenceProbs(2, telephony.EndSilenceWindows()),
		speechThenSilenceProbs(2, telephony.EndSilenceWindows())...)
	s := telephony.NewSession(context.Background(), "test-stopword",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnSink(sink),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Feed the first utterance: enough frames to cross EndSilenceWindows and
	// fire VADEndOfUtterance, the one point STT actually dispatches from
	// (onset-dispatch was removed -- see the ticket comment for why).
	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(2 * time.Millisecond)
	}

	// Wait for STT request and respond with "hello"
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}

	// Send normal response
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: "test-stopword",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello world",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Feed the second utterance the same way, to fire a second
	// VADEndOfUtterance and second dispatch.
	for i := 0; i <= telephony.EndSilenceWindows()+4; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for next STT request
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	req, err = sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("second STT request not received: %v", err)
	}

	// Send "done" response - this should trigger turn completion
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: "test-stopword",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "done",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Verify turn was completed with fused text. The stopword utterance's own
	// text ("done") is excluded from the fused turn -- SOP-150's protected
	// contract (TestTurnFusion_StopwordExcluded: feeding "hello" then "Done."
	// delivers exactly "hello", not "hello Done."). SOP-162 wires the live
	// path into this pre-existing behavior; it does not redefine it.
	turns := sink.turnTexts()
	if len(turns) != 1 {
		t.Errorf("OnTurnComplete calls: got %d, want 1", len(turns))
	}
	if len(turns) > 0 && turns[0] != "hello world" {
		t.Errorf("Turn text: got %q, want %q", turns[0], "hello world")
	}
	if got := s.State(); got != telephony.StateListening {
		t.Errorf("State: got %s, want StateListening", got)
	}
	if s.TurnActive() {
		t.Errorf("TurnActive: got true, want false after turn completion")
	}
}

// TestLivePath_SilenceTurnEndCompletesTurn verifies that VADTurnEnd completes the turn.
func TestLivePath_SilenceTurnEndCompletesTurn(t *testing.T) {
	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	// Need enough frames to cross TurnEndSilenceWindows (~156 windows)
	probs := speechThenSilenceProbs(1, telephony.TurnEndSilenceWindows()+10)
	s := telephony.NewSession(context.Background(), "test-silence",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnSink(sink),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Feed the utterance: enough frames to cross EndSilenceWindows and fire
	// VADEndOfUtterance, the one point STT actually dispatches from
	// (onset-dispatch was removed -- see the ticket comment for why).
	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(2 * time.Millisecond)
	}

	// Wait for STT request
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}

	// Send response
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: "test-silence",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello there",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// The VAD will naturally fire VADTurnEnd when enough silence windows accumulate
	// Send silence frames synchronously until VADTurnEnd fires
	for i := 0; i <= telephony.TurnEndSilenceWindows()+10; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	// Give a bit more time for VADTurnEnd event to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify turn was completed
	turns := sink.turnTexts()
	if len(turns) != 1 {
		t.Errorf("OnTurnComplete calls: got %d, want 1", len(turns))
	}
	if len(turns) > 0 && turns[0] != "hello there" {
		t.Errorf("Turn text: got %q, want %q", turns[0], "hello there")
	}
	if got := s.State(); got != telephony.StateListening {
		t.Errorf("State: got %s, want StateListening", got)
	}
	if s.TurnActive() {
		t.Errorf("TurnActive: got true, want false after turn completion")
	}
}

// TestLivePath_IdleStillTerminates verifies idle path still works after turn completion wiring.
func TestLivePath_IdleStillTerminates(t *testing.T) {
	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	var closedOnce sync.Once

	probs := speechThenSilenceProbs(1, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "test-idle",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnSink(sink),
		telephony.WithMaxSilenceMS(50),
		telephony.WithCloseFunc(func() {
			closedOnce.Do(func() {})
		}),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Feed one frame to transition to Listening
	sendData(t, dataIn, windowFrame(0x80), 5*time.Second)

	// Send enough silence frames to cross EndSilenceWindows so VADEndOfUtterance
	// actually fires (without sttIn wired, dispatchFullPass no-ops the STT send
	// but still needs the real VAD event to drive onUtteranceEnd's idle re-arm).
	for i := 0; i <= telephony.EndSilenceWindows()+4; i++ {
		sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for idle timer to fire (we set it to 50ms, so 150ms should be enough)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.State() == telephony.StateAwaitingMarkEcho {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// This session never wires WithSTTInput, so nothing is ever accumulated
	// into turnTranscripts -- idle termination must not deliver a turn on an
	// empty buffer. SOP-150's protected contract
	// (TestTurnFusion_EmptyBufferNoEmit_CallEnd: "call end with nothing
	// accumulated must not deliver"). SOP-162 routes idle termination through
	// completeTurn(); it does not redefine what an empty buffer does there.
	if len(sink.turnTexts()) != 0 {
		t.Errorf("empty buffer must not deliver a turn on idle termination, got %d calls", len(sink.turnTexts()))
	}
	if got := s.State(); got != telephony.StateAwaitingMarkEcho {
		t.Errorf("State: got %s, want StateAwaitingMarkEcho", got)
	}
}

// TestLivePath_IdleTerminatesFlushesBeforeClip is the ticket's Test
// Expectations case TestLivePath_IdleStillTerminates couldn't cover: this
// session DOES wire STT, so a real utterance completes and its text sits in
// turnTranscripts when the idle timer fires. terminateWithClip's sendClip
// writes the farewell clip to dataOut synchronously, AFTER completeTurn()
// has already returned (handleIdleTimer: completeTurn() then
// terminateWithClip(FarewellULaw), state.go) -- so by the time any byte is
// observable on dataOut, OnTurnComplete must already have fired. Asserts
// that ordering directly, not just that both eventually happened.
func TestLivePath_IdleTerminatesFlushesBeforeClip(t *testing.T) {
	const idleTestFlushMS = 9100 // distinct from other suites' timer durations

	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)
	clock := newFakeClock()

	probs := speechThenSilenceProbs(1, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "test-idle-flush",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithTurnSink(sink),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleTestFlushMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Drive one utterance to VADEndOfUtterance and answer its STT request, so
	// "hello" sits in turnTranscripts before the idle timer fires. Only the
	// idle timer itself is clock-driven below; the short pacing sleep here
	// is unrelated to any session timer -- it lets the forwarder goroutine
	// drain forwardCh between frames (a bounded buffer modeling real-time
	// frame pacing, not an artificial synchronous burst -- see sendData's
	// siblings elsewhere in this package).
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
		SessionID: "test-idle-flush",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	// Fire the idle timer deterministically -- no real wall-clock wait. The
	// first byte observed on dataOut is the farewell clip; if OnTurnComplete
	// had not already run, this check would race it -- it can't, because
	// sendClip only starts after completeTurn() has fully returned.
	clock.fire(t, idleTestFlushMS*time.Millisecond)

	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	_, err = dataOut.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("farewell clip not sent on dataOut: %v", err)
	}

	turns := sink.turnTexts()
	if len(turns) != 1 || turns[0] != "hello" {
		t.Errorf("OnTurnComplete before farewell clip: got %v, want [\"hello\"]", turns)
	}
}

// TestLivePath_UtteranceCapTerminatesFlushesBeforeClip is the utterance-cap
// counterpart of TestLivePath_IdleTerminatesFlushesBeforeClip: a real
// completed utterance's text sits in turnTranscripts, then a SECOND,
// continuous utterance runs past MaxUtteranceMS and forces a stop.
// handleUtteranceTimeout calls completeTurn() then
// terminateWithClip(AudioForcedStopULaw) (state.go), so the same
// synchronous ordering guarantee applies: the first byte observable on
// dataOut cannot precede OnTurnComplete.
func TestLivePath_UtteranceCapTerminatesFlushesBeforeClip(t *testing.T) {
	const utteranceTestFlushMS = 301 // distinct from other suites' timer durations

	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](4)
	clock := newFakeClock()

	probs := append(speechThenSilenceProbs(1, telephony.EndSilenceWindows()), voicedProbs(200)...)
	s := telephony.NewSession(context.Background(), "test-cap-flush",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithTurnSink(sink),
		telephony.WithClock(clock.after),
		telephony.WithMaxUtteranceMS(utteranceTestFlushMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Drive the first (short) utterance to VADEndOfUtterance and answer its
	// STT request, so "hello" sits in turnTranscripts. The cap timer only
	// fires when this test explicitly tells the fake clock to -- no risk of
	// it tripping mid-loop the way a real wall-clock timer could. The short
	// pacing sleep here is unrelated to any session timer -- see the
	// idle-timer variant above for why it's needed.
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
		SessionID: "test-cap-flush",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}

	// Feed a second, continuous utterance (no silence) to arm the cap timer
	// for it, then fire it deterministically -- no real wall-clock wait.
	go sendData(t, dataIn, windowFrame(0x80), 5*time.Second)
	clock.fire(t, utteranceTestFlushMS*time.Millisecond)

	// First byte on dataOut is the forced-stop clip; if OnTurnComplete had
	// not already run, this would race it -- it can't, for the same reason
	// as the idle-timer variant above.
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	_, err = dataOut.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("forced-stop clip not sent on dataOut: %v", err)
	}

	turns := sink.turnTexts()
	if len(turns) != 1 || turns[0] != "hello" {
		t.Errorf("OnTurnComplete before forced-stop clip: got %v, want [\"hello\"]", turns)
	}
}

// TestLivePath_UtteranceCapStillTerminates verifies utterance cap still works.
func TestLivePath_UtteranceCapStillTerminates(t *testing.T) {
	sink := &spySink{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)

	probs := make([]float32, 100)
	for i := range probs {
		probs[i] = 1
	}

	var closedOnce sync.Once

	s := telephony.NewSession(context.Background(), "test-utterance-cap",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) {
			return &fakeDetector{probs: probs}, nil
		}),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnSink(sink),
		telephony.WithMaxUtteranceMS(50),
		telephony.WithCloseFunc(func() {
			closedOnce.Do(func() {})
		}),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Feed continuous frames
	go sendData(t, dataIn, windowFrame(0x80), 5*time.Second)

	// Wait for utterance cap to fire (we set it to 50ms)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.State() == telephony.StateAwaitingMarkEcho {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Same empty-buffer contract as TestLivePath_IdleStillTerminates above:
	// no WithSTTInput wired here either, so nothing accumulates, and the
	// forced-stop path must not deliver a turn on an empty buffer
	// (SOP-150's TestTurnFusion_EmptyBufferNoEmit_CallEnd).
	if len(sink.turnTexts()) != 0 {
		t.Errorf("empty buffer must not deliver a turn on utterance-cap termination, got %d calls", len(sink.turnTexts()))
	}
	if got := s.State(); got != telephony.StateAwaitingMarkEcho {
		t.Errorf("State: got %s, want StateAwaitingMarkEcho", got)
	}
}
