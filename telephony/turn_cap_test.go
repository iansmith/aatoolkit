package telephony_test

import (
	"context"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

// SOP-161 Phase 0. These tests pin the turn-level cap: a whole turn (which
// may span several utterances separated by pauses) is bounded by MaxTurnMS,
// distinct from SOP-156's per-utterance cap (which resets on every
// end-of-utterance). All timing is driven by the injected fakeClock — no
// sleeps, no waiting for a real deadline.

// turnTestMS is this suite's max-turn cap. Distinct from every other timer
// duration used in this package (idle 9000/15000, utterance 300/45000) so
// fakeClock fires exactly the timer a test means to.
const turnTestMS = 1300

func turnTestDur() time.Duration { return turnTestMS * time.Millisecond }

// assertNoClip fails the test if any bytes arrive on dataOut within a short
// window — used to prove the turn cap did NOT fire (it was cancelled or
// never armed for this path).
func assertNoClip(t *testing.T, dataOut telephony.TwilioDataPlaneOutput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	frame, err := dataOut.Recv(ctx)
	if err == nil {
		t.Fatalf("unexpected clip frame on dataOut: %d bytes (want none)", len(frame))
	}
}

// startTurnCapSession builds a session with the injected clock, a spy sink,
// data/STT in/out, StopwordPolicy, and the turn cap set to this suite's test
// duration.
func startTurnCapSession(t *testing.T, name string, det *fakeDetector, clock *fakeClock) (*telephony.Session, telephony.TwilioDataPlaneInput, telephony.TwilioDataPlaneOutput, telephony.STTInput, telephony.STTOutput, *spySink) {
	t.Helper()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](100)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](100)
	sink := &spySink{}
	s := telephony.NewSession(context.Background(), name,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithMaxTurnMS(turnTestMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, dataIn, dataOut, sttIn, sttOut, sink
}

// TestTurnCap_UnderCapSingleUtteranceCompletesNormally is the ticket's first
// Test Expectation: a turn under MaxTurnMS (single utterance, silence
// turn-end) completes normally — no forced-stop clip on dataOut, and the cap
// timer (armed at the turn's first onset) is cancelled by completeTurn. Fails
// on current code — WithMaxTurnMS/the turn cap don't exist yet, so nothing is
// armed for turnTestDur and assertNoClip's post-fire check is vacuous, not
// proof of cancellation.
func TestTurnCap_UnderCapSingleUtteranceCompletesNormally(t *testing.T) {
	clock := newFakeClock()
	probs := speechThenSilenceProbs(1, telephony.TurnEndSilenceWindows()+10)
	det := &fakeDetector{probs: probs}
	s, dataIn, dataOut, sttIn, sttOut, sink := startTurnCapSession(t, "turn-undercap-single", det, clock)
	defer s.Close()

	for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	clock.waitArmed(t, turnTestDur()) // proves the cap armed at the turn's first onset

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: "turn-undercap-single",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello there",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := 0; i <= telephony.TurnEndSilenceWindows()+10; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	turns := sink.turnTexts()
	if len(turns) != 1 || turns[0] != "hello there" {
		t.Fatalf("OnTurnComplete: got %v, want [\"hello there\"]", turns)
	}
	if s.TurnActive() {
		t.Fatal("TurnActive: got true, want false after turn completion")
	}
	assertNoClip(t, dataOut) // no forced-stop before firing the cap's stale deadline

	// The cap must have been cancelled by completeTurn: firing its deadline
	// now must be a no-op.
	clock.fire(t, turnTestDur())
	assertNoClip(t, dataOut)
}

// TestTurnCap_MultiUtteranceUnderCapCompletesNormally is the ticket's second
// Test Expectation: a turn spanning 2+ utterances, each individually well
// under the utterance cap and separated by pauses, still completes normally
// on the stopword — proving the turn cap doesn't spuriously fire on a
// multi-utterance turn that stays under budget. Fails on current code for the
// same reason as the single-utterance case above.
func TestTurnCap_MultiUtteranceUnderCapCompletesNormally(t *testing.T) {
	clock := newFakeClock()
	one := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	probs := append(append([]float32{}, one...), one...)
	det := &fakeDetector{probs: probs}
	s, dataIn, dataOut, sttIn, sttOut, sink := startTurnCapSession(t, "turn-undercap-multi", det, clock)
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-undercap-multi", "hello")
	clock.waitArmed(t, turnTestDur()) // armed once, at the first utterance's onset
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-undercap-multi", "done")

	// The stopword completes the turn, which now enters StateAwaitingResponse
	// (AATK-24) instead of returning straight to Listening -- no response
	// input is wired in this fixture, so the session simply waits there.
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	turns := sink.turnTexts()
	if len(turns) != 1 || turns[0] != "hello" {
		t.Fatalf("OnTurnComplete: got %v, want [\"hello\"]", turns)
	}
	if s.TurnActive() {
		t.Fatal("TurnActive: got true, want false after turn completion")
	}
	assertNoClip(t, dataOut)

	clock.fire(t, turnTestDur()) // cap's stale deadline; must be a no-op
	assertNoClip(t, dataOut)
}

// TestTurnCap_AccumulatesAcrossPausesAndForcesStop is the ticket's third Test
// Expectation and the core of the ticket: a turn whose accumulated wall-clock
// exceeds MaxTurnMS, spanning multiple utterances separated by pauses (none
// of which individually ends the turn), forces a stop with the forced-stop
// clip and ends the call. Fails on current code — no turn timer exists to
// fire, so no clip ever reaches dataOut.
func TestTurnCap_AccumulatesAcrossPausesAndForcesStop(t *testing.T) {
	clock := newFakeClock()
	one := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	probs := append(append([]float32{}, one...), one...)
	det := &fakeDetector{probs: probs}
	s, dataIn, dataOut, sttIn, sttOut, _ := startTurnCapSession(t, "turn-overcap", det, clock)
	defer s.Close()

	// Two utterances, neither the stopword: the turn stays active across
	// both, spanning the pause between them.
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-overcap", "hello")
	clock.waitArmed(t, turnTestDur())
	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-overcap", "still talking")

	if !s.TurnActive() {
		t.Fatal("TurnActive: got false, want true — turn must still be open across the pause")
	}

	// The turn's accumulated wall-clock now exceeds MaxTurnMS.
	clock.fire(t, turnTestDur())

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	var got []byte
	for len(got) < len(assets.AudioForcedStopULaw) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv forced-stop frame: %v (got %d/%d bytes)", err, len(got), len(assets.AudioForcedStopULaw))
		}
		got = append(got, frame...)
	}
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); st == telephony.StateAwaitingMarkEcho || st == telephony.StateClosed {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("after the turn cap fired, state = %s, want AwaitingMarkEcho or Closed", s.State())
}

// TestTurnCap_FiresWhileAwaitingFullResult is an adversary gap test: the
// ticket's file map wires SourceTurnTimer into all of inProgressStates
// (StateIdle, StateListening, StateAwaitingFullResult), not just Listening.
// This drives the cap to fire while a full STT pass is still outstanding
// (state == StateAwaitingFullResult, the one in-progress state none of the
// other new tests exercise) and confirms the forced-stop still fires. Fails
// on current code the same way the other accumulation test does — no timer
// exists to fire.
func TestTurnCap_FiresWhileAwaitingFullResult(t *testing.T) {
	clock := newFakeClock()
	probs := speechThenSilenceProbs(3, telephony.EndSilenceWindows()+3)
	det := &fakeDetector{probs: probs}
	s, dataIn, dataOut, sttIn, _, _ := startTurnCapSession(t, "turn-awaitingfullresult", det, clock)
	defer s.Close()

	for i := 0; i < telephony.EndSilenceWindows()+6; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	clock.waitArmed(t, turnTestDur())

	// Wait for the full-pass dispatch, but never answer it -- the session
	// stays in StateAwaitingFullResult with the turn still open.
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	_, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	waitForSessionState(t, s, telephony.StateAwaitingFullResult)

	clock.fire(t, turnTestDur())

	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	var got []byte
	for len(got) < len(assets.AudioForcedStopULaw) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv forced-stop frame: %v (got %d/%d bytes)", err, len(got), len(assets.AudioForcedStopULaw))
		}
		got = append(got, frame...)
	}
	cancel()
}

// TestTurnCap_ArmedOnceNotReArmedBySecondOnset is the ticket's fourth Test
// Expectation: the cap arms once per turn, at the turn's first speech onset,
// and does not re-arm on a second onset within the same turn. fakeClock.fire
// reports how many timers it released for the exact duration it fired — a
// re-arm would leave 2 armed for turnTestDur instead of 1. Fails on current
// code (no timer armed at all, so fire's precondition — an armed timer for
// turnTestDur — is never met and the test fatals in waitArmed).
func TestTurnCap_ArmedOnceNotReArmedBySecondOnset(t *testing.T) {
	clock := newFakeClock()
	// First utterance: speech, then exactly enough silence to cross
	// end-of-utterance (but not turn-end), followed by a second utterance's
	// speech onset. The silence count matches EndSilenceWindows() exactly so
	// the first loop below consumes the whole segment and the second loop
	// lands on the appended voiced windows, not leftover silence.
	probs := append(
		speechThenSilenceProbs(3, telephony.EndSilenceWindows()),
		voicedProbs(3)...,
	)
	det := &fakeDetector{probs: probs}
	s, dataIn, dataOut, sttIn, sttOut, sink := startTurnCapSession(t, "turn-armonce", det, clock)
	defer s.Close()

	// First utterance up through end-of-utterance.
	for i := 0; i < telephony.EndSilenceWindows()+3; i++ {
		sendData(t, dataIn, windowFrame(0x80), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	clock.waitArmed(t, turnTestDur()) // armed at the turn's first onset

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	req, err := sttIn.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("STT request not received: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	err = sttOut.Send(ctx, telephony.STTResult{
		SessionID: "turn-armonce",
		RequestID: req.RequestID,
		Kind:      telephony.FullPass,
		Text:      "hello",
	})
	cancel()
	if err != nil {
		t.Fatalf("send STT result: %v", err)
	}
	waitForSessionState(t, s, telephony.StateListening)

	// Second utterance's onset, within the same (still-open) turn.
	for i := 0; i < 3; i++ {
		sendData(t, dataIn, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart2(t, sink)

	if !s.TurnActive() {
		t.Fatal("TurnActive: got false, want true — still mid-turn")
	}

	// Exactly one timer must be armed for turnTestDur — a re-arm on the
	// second onset would leave two, and fire would release both.
	released := clock.fire(t, turnTestDur())
	if released != 1 {
		t.Fatalf("timers released for turnTestDur: got %d, want 1 (cap re-armed on the second onset)", released)
	}

	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	var got []byte
	for len(got) < len(assets.AudioForcedStopULaw) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv forced-stop frame: %v (got %d/%d bytes)", err, len(got), len(assets.AudioForcedStopULaw))
		}
		got = append(got, frame...)
	}
	cancel()
}

// waitSpeechStart2 blocks until the sink has seen at least 2 speech onsets.
func waitSpeechStart2(t *testing.T, sink *spySink) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if start, _ := sink.counts(); start >= 2 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("fewer than 2 speech onsets observed within the backstop")
}

// TestTurnCap_CancelledOnBothCompletionTriggers is the ticket's fifth Test
// Expectation: completeTurn cancels the turn cap unconditionally, regardless
// of which trigger fired. Checked on 2 distinct trigger paths — silence
// turn-end and stopword — each leaving no armed timerTurn behind (advancing
// the clock past MaxTurnMS after normal completion must not produce a
// forced-stop). Fails on current code the same way the under-cap tests above
// do: nothing is ever armed for turnTestDur, so clock.fire's precondition is
// never met.
func TestTurnCap_CancelledOnBothCompletionTriggers(t *testing.T) {
	t.Run("SilenceTurnEnd", func(t *testing.T) {
		clock := newFakeClock()
		probs := speechThenSilenceProbs(1, telephony.TurnEndSilenceWindows()+10)
		det := &fakeDetector{probs: probs}
		s, dataIn, dataOut, sttIn, sttOut, sink := startTurnCapSession(t, "turn-cancel-silence", det, clock)
		defer s.Close()

		for i := 0; i < telephony.EndSilenceWindows()+2; i++ {
			sendData(t, dataIn, windowFrame(0x80), recvTimeout)
			time.Sleep(2 * time.Millisecond)
		}
		clock.waitArmed(t, turnTestDur())

		ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
		req, err := sttIn.Recv(ctx)
		cancel()
		if err != nil {
			t.Fatalf("STT request not received: %v", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
		err = sttOut.Send(ctx, telephony.STTResult{
			SessionID: "turn-cancel-silence",
			RequestID: req.RequestID,
			Kind:      telephony.FullPass,
			Text:      "hello",
		})
		cancel()
		if err != nil {
			t.Fatalf("send STT result: %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		for i := 0; i <= telephony.TurnEndSilenceWindows()+10; i++ {
			sendData(t, dataIn, windowFrame(0x80), recvTimeout)
			time.Sleep(2 * time.Millisecond)
		}
		if _, eou := waitEOU(t, sink); eou < 1 {
			t.Fatal("no end-of-utterance observed")
		}
		if s.TurnActive() {
			t.Fatal("TurnActive: got true, want false after silence turn-end")
		}

		clock.fire(t, turnTestDur())
		assertNoClip(t, dataOut)
	})

	t.Run("Stopword", func(t *testing.T) {
		clock := newFakeClock()
		one := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
		probs := append(append([]float32{}, one...), one...)
		det := &fakeDetector{probs: probs}
		s, dataIn, dataOut, sttIn, sttOut, _ := startTurnCapSession(t, "turn-cancel-stopword", det, clock)
		defer s.Close()

		driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-cancel-stopword", "hello")
		clock.waitArmed(t, turnTestDur())
		driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "turn-cancel-stopword", "done")

		// The stopword completes the turn into StateAwaitingResponse (AATK-24)
		// instead of Listening -- no response input is wired here.
		waitForSessionState(t, s, telephony.StateAwaitingResponse)
		if s.TurnActive() {
			t.Fatal("TurnActive: got true, want false after stopword")
		}

		clock.fire(t, turnTestDur())
		assertNoClip(t, dataOut)
	})
}
