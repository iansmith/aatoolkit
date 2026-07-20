package telephony_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

// SOP-156 Phase 0. These tests pin the max-utterance cutoff: a single
// continuous utterance is capped, and on the cap the engine plays a forced-stop
// clip and hangs up through the same mark-echo flow as the farewell. They also
// pin the reconciliation of the idle timer, whose reset-on-onset-only behavior
// is what hung a real caller up mid-sentence (the motivating bug).
//
// All timing is driven by the injected fakeClock — no sleeps, no waiting.

const (
	// utteranceTestMS is this suite's max-utterance cap. Distinct from the
	// idle value so fakeClock can fire exactly the timer a test means to.
	utteranceTestMS = 300
	idleTestMS      = 9000
)

func utteranceTestDur() time.Duration { return utteranceTestMS * time.Millisecond }
func idleTestDur() time.Duration      { return idleTestMS * time.Millisecond }

// voicedProbs returns n above-SpeechThresh windows: continuous speech, never
// enough silence to end the utterance.
func voicedProbs(n int) []float32 {
	p := make([]float32, n)
	for i := range p {
		p[i] = 0.9
	}
	return p
}

// waitSpeechStart blocks until the sink has seen at least one onset, proving
// the session processed the speech frame (and, with it, did its onset-time
// timer bookkeeping) before the test inspects timer state.
func waitSpeechStart(t *testing.T, sink *spySink) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if start, _ := sink.counts(); start >= 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no speech onset observed within the backstop")
}

// waitEOU blocks until the sink has seen at least one end-of-utterance, and
// returns the (start, eou) counts.
func waitEOU(t *testing.T, sink *spySink) (int, int) {
	t.Helper()
	return waitEOUCount(t, sink, 1)
}

// waitEOUCount blocks until the sink has seen at least n end-of-utterance
// notifications, and returns the (start, eou) counts.
func waitEOUCount(t *testing.T, sink *spySink, n int) (int, int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if start, eou := sink.counts(); eou >= n {
			return start, eou
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, eou := sink.counts()
	t.Fatalf("only %d/%d end-of-utterance notifications within the backstop", eou, n)
	return 0, 0
}

// drainClip reads dataOut until the whole expected clip has arrived, and
// asserts the bytes are exactly it.
func drainClip(t *testing.T, dataOut telephony.TwilioDataPlaneOutput, want []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []byte
	for len(got) < len(want) {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			t.Fatalf("recv clip frame: %v (got %d/%d bytes)", err, len(got), len(want))
		}
		got = append(got, frame...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("clip on dataOut: %d bytes, not the expected clip (%d bytes)", len(got), len(want))
	}
}

// startUtteranceSession builds a session with the injected clock, a spy sink,
// data in/out, and the two caps set to the suite's distinct test durations.
func startUtteranceSession(t *testing.T, name string, det *fakeDetector, clock *fakeClock) (*telephony.Session, telephony.TwilioDataPlaneInput, telephony.TwilioDataPlaneOutput, *spySink) {
	t.Helper()
	data := telephony.NewBufferedChan[[]byte](8)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sink := &spySink{}
	s := telephony.NewSession(context.Background(), name,
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithClock(clock.after),
		telephony.WithMaxUtteranceMS(utteranceTestMS),
		telephony.WithMaxSilenceMS(idleTestMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, data, dataOut, sink
}

// TestUtterance_CapForcesStopAfterMaxMs is the point of the ticket: a caller
// who talks past the cap is cut off with the forced-stop clip, and the session
// heads into termination. Fails on current code — no utterance timer is armed,
// so fakeClock finds nothing to fire at the cap duration.
func TestUtterance_CapForcesStopAfterMaxMs(t *testing.T) {
	clock := newFakeClock()
	det := &fakeDetector{probs: voicedProbs(50)}
	s, data, dataOut, sink := startUtteranceSession(t, "utt-cap", det, clock)
	defer s.Close()

	for i := 0; i < 3; i++ { // onset: begins an utterance, arms the cap
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart(t, sink)

	clock.fire(t, utteranceTestDur()) // the cap expires while still speaking

	drainClip(t, dataOut, assets.AudioForcedStopULaw)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); st == telephony.StateAwaitingMarkEcho || st == telephony.StateClosed {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("after the cap fired, state = %s, want AwaitingMarkEcho or Closed", s.State())
}

// TestUtterance_IdleTimerDoesNotFireMidUtterance is the regression test for the
// reported bug. The idle/silence timer must NOT be armed while the caller is
// actively speaking — if it is, it counts down from the utterance's start and
// hangs the caller up mid-sentence. On current code the onset resets (arms) the
// idle timer, so this fails; the fix cancels it on onset and arms the utterance
// cap instead.
func TestUtterance_IdleTimerDoesNotFireMidUtterance(t *testing.T) {
	clock := newFakeClock()
	det := &fakeDetector{probs: voicedProbs(50)}
	s, data, dataOut, sink := startUtteranceSession(t, "utt-noidle", det, clock)
	defer s.Close()

	for i := 0; i < 3; i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart(t, sink)

	// Fire the idle/silence deadline while the caller is mid-utterance. It must
	// be a no-op: onset cancelled the idle timer, so its deadline is stale and
	// dispatches nothing. On the buggy code onset RE-ARMS idle, so this fire
	// terminates the call — the mid-sentence hangup.
	//
	// Checked by consequence, not by clock.isArmed: the fake clock's armed set
	// is append-only and never sees TimerFacility.Cancel, so "is it armed" can't
	// distinguish a live timer from a cancelled one. Firing it can — a cancelled
	// timer's completion fails IsCurrent and is dropped.
	clock.fire(t, idleTestDur())

	// The run loop is a single goroutine, so this second fire is processed after
	// the idle fire. The forced-stop can only reach dataOut if the idle fire did
	// NOT already terminate the call (which would have played the farewell and
	// left the cap unarmed). Requiring the forced-stop here is the proof the
	// caller was still on the line.
	clock.fire(t, utteranceTestDur())
	drainClip(t, dataOut, assets.AudioForcedStopULaw)
}

// TestUtterance_IdleTimerStillFiresOnSilence guards the behavior the fix must
// preserve: a caller who genuinely goes quiet still gets the farewell after
// MaxSilenceMS. After end-of-utterance the idle timer is armed; firing it runs
// the farewell and enters termination.
func TestUtterance_IdleTimerStillFiresOnSilence(t *testing.T) {
	clock := newFakeClock()
	// Enough voiced windows to establish speech, then enough silence to
	// cross end-of-utterance.
	probs := speechThenSilenceProbs(10, telephony.EndSilenceWindows()+3)
	det := &fakeDetector{probs: probs}
	s, data, dataOut, sink := startUtteranceSession(t, "utt-silence", det, clock)
	defer s.Close()

	for i := 0; i < len(probs); i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
		time.Sleep(2 * time.Millisecond) // let the VAD goroutine consume each window
	}
	// The utterance ended; the caller is now silent.
	if _, eou := waitEOU(t, sink); eou < 1 {
		t.Fatal("no end-of-utterance observed")
	}

	clock.fire(t, idleTestDur()) // silence deadline reached

	drainClip(t, dataOut, assets.FarewellULaw)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); st == telephony.StateAwaitingMarkEcho || st == telephony.StateClosed {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("after the silence deadline, state = %s, want AwaitingMarkEcho or Closed", s.State())
}

// TestUtterance_UnderCapDoesNotFire guards the normal turn: a caller who
// finishes an utterance before the cap is not cut off. Once end-of-utterance
// arrives the cap is cancelled, so nothing can force-stop the turn and no clip
// reaches the caller.
func TestUtterance_UnderCapDoesNotFire(t *testing.T) {
	clock := newFakeClock()
	probs := speechThenSilenceProbs(10, telephony.EndSilenceWindows()+3)
	det := &fakeDetector{probs: probs}
	s, data, dataOut, sink := startUtteranceSession(t, "utt-undercap", det, clock)
	defer s.Close()

	for i := 0; i < len(probs); i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	waitEOU(t, sink)

	// The utterance ended under the cap, so the cap was cancelled at
	// end-of-utterance and the idle timer re-armed. Fire the cap's stale
	// deadline: it must be a no-op — no forced-stop. Then fire the idle timer
	// (the real post-utterance timer) and require the FAREWELL, which proves
	// the session survived the cap fire and the post-EOU timer is idle, not the
	// cap. (Checked by consequence, not clock.isArmed — see the mid-utterance
	// test for why the append-only fake clock can't answer "is it armed".)
	clock.fire(t, utteranceTestDur())
	clock.fire(t, idleTestDur())
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestUtterance_SecondUtteranceInAwaitingFullResultReconcilesTimers is the
// regression test for a gap found in code review. A caller who completes a
// SECOND utterance before the first's STT pass returns closes it via
// handleAwaitingFullResultVADEvent, which dispatches no new pass. That path must
// still run the end-of-utterance timer reconciliation: without it the cap armed
// for the second utterance stays armed and the idle timer stays cancelled after
// the caller has finished, so a following silence would force-stop the call
// instead of ending it with the farewell.
func TestUtterance_SecondUtteranceInAwaitingFullResultReconcilesTimers(t *testing.T) {
	clock := newFakeClock()
	// Two full utterances back to back: each voiced, then silent to EOU.
	one := speechThenSilenceProbs(10, telephony.EndSilenceWindows()+3)
	probs := append(append([]float32{}, one...), one...)
	det := &fakeDetector{probs: probs}

	data := telephony.NewBufferedChan[[]byte](8)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	// STT input is wired so the full pass dispatches, but no result is ever fed
	// back, so the session stays in AwaitingFullResult while the second
	// utterance plays out.
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sink := &spySink{}
	s := telephony.NewSession(context.Background(), "utt-2nd",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return det, nil }),
		telephony.WithTurnSink(sink),
		telephony.WithTwilioDataInput(data),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithClock(clock.after),
		telephony.WithMaxUtteranceMS(utteranceTestMS),
		telephony.WithMaxSilenceMS(idleTestMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	for i := 0; i < len(probs); i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
		time.Sleep(2 * time.Millisecond)
	}
	waitEOUCount(t, sink, 2) // both utterances ended

	// The caller has finished. Fire the cap's stale deadline (no-op), then fire
	// idle and require the FAREWELL — which proves the second utterance's end
	// re-armed idle and cancelled the cap on the AwaitingFullResult path too.
	clock.fire(t, utteranceTestDur())
	clock.fire(t, idleTestDur())
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestUtterance_ForcedStopPlaysWholeClipThenCloses pins the "hang up on a
// timeout after the sound plays" contract: the forced-stop plays the full clip,
// then arms the mark-echo timer derived from THAT clip's length (not the
// farewell's), and closes on the mark echo or that timeout.
func TestUtterance_ForcedStopPlaysWholeClipThenCloses(t *testing.T) {
	clock := newFakeClock()
	det := &fakeDetector{probs: voicedProbs(50)}
	s, data, dataOut, sink := startUtteranceSession(t, "utt-clip", det, clock)
	defer s.Close()

	for i := 0; i < 3; i++ {
		sendData(t, data, windowFrame(byte(i+1)), recvTimeout)
	}
	waitSpeechStart(t, sink)
	clock.fire(t, utteranceTestDur())
	drainClip(t, dataOut, assets.AudioForcedStopULaw)

	// The post-playout wait is derived from the forced-stop clip, so a mark
	// echo timer at exactly MarkEchoTimeout(AudioForcedStopULaw) must be armed.
	wait := telephony.MarkEchoTimeout(assets.AudioForcedStopULaw)
	clock.waitArmed(t, wait)
	clock.fire(t, wait) // no echo comes; the timeout closes the call

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.State() == telephony.StateClosed {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("after mark-echo timeout, state = %s, want Closed", s.State())
}

// TestAssets_ForcedStopAndThinkingAreValidMuLaw is a vendored-asset guard: both
// embedded clips are present and a whole number of 8 kHz μ-law samples (μ-law
// is 1 byte/sample, so any non-empty length qualifies; the check is that they
// embedded at all and are not empty).
func TestAssets_ForcedStopAndThinkingAreValidMuLaw(t *testing.T) {
	for _, c := range []struct {
		name string
		clip []byte
	}{
		{"audio-forced-stop", assets.AudioForcedStopULaw},
		{"llm-thinking", assets.LLMThinkingULaw},
	} {
		if len(c.clip) == 0 {
			t.Errorf("%s embedded empty — the asset did not vendor", c.name)
		}
	}
}
