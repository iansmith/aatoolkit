package twilio

import (
	"bytes"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// Call-level tests for the caller-silence bound on the realtime path
// (AATK-163, WithCallerSilenceTimeout).
//
// The bound measures the CALLER, which nothing on this path measured before:
// WithIdleTimeout is reset by every backend event, so it times the backend
// going quiet. This one counts from the moment the agent's reply has finished
// playing to the caller — the echo of the playout mark the engine writes after
// each agent turn — and is stopped by caller speech, by agent audio, and by a
// reply that is still pending behind a tool call.
//
// Every assertion is made against the carrier's own wire or the error the call
// returns, the two places a consumer can see the ending.

// callerSilenceTestLimit is the caller-silence bound most tests here configure.
// Short enough that a window of several multiples of it is still a fast test,
// long enough that a few milliseconds of scheduling cannot be mistaken for it.
const callerSilenceTestLimit = 300 * time.Millisecond

// agentTurnPayload is the delta every agent turn in this file is made of. Its
// fill is distinct from every other fill this package's wire assertions look
// for, so a count of it is a count of agent audio and nothing else.
func agentTurnPayload() string { return distinctCarrierPayload(0x3c) }

// countOf counts the records in recs that match pred.
func countOf(recs []carrierWireRecord, pred func(carrierWireRecord) bool) int {
	n := 0
	for _, r := range recs {
		if pred(r) {
			n++
		}
	}
	return n
}

// isPlayoutMark matches the marks the engine writes after an agent turn on h's
// call, and no mark a consumer or the farewell writes.
func isPlayoutMark(h *realtimeHarness) func(carrierWireRecord) bool {
	prefix := realtimePlayoutMarkPrefix + h.streamSID + "-"
	return func(r carrierWireRecord) bool { return strings.HasPrefix(r.markName, prefix) }
}

// playoutMarks returns the playout marks in a capture, in wire order.
func playoutMarks(h *realtimeHarness, recs []carrierWireRecord) []carrierWireRecord {
	pred := isPlayoutMark(h)
	var out []carrierWireRecord
	for _, r := range recs {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// waitPlayoutMarks blocks until at least n playout marks are on the wire and
// returns them in wire order.
func waitPlayoutMarks(t *testing.T, h *realtimeHarness, wire func() []carrierWireRecord, n int) []carrierWireRecord {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(playoutMarks(h, wire())) >= n })
	return playoutMarks(h, wire())
}

// playAgentAudio has the backend speak frames deltas of agent audio and waits
// until every one has reached the carrier, so whatever the test emits next is
// read by the bridge after them.
func playAgentAudio(t *testing.T, be *fakeRealtimeBackend, wire func() []carrierWireRecord, frames int) {
	t.Helper()
	p := agentTurnPayload()
	before := countOf(wire(), hasPayload(p))
	for range frames {
		be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": p})
	}
	waitFor(t, 5*time.Second, func() bool { return countOf(wire(), hasPayload(p)) >= before+frames })
}

// endTurn emits the response.done that ends an agent turn without a function
// call.
func endTurn(t *testing.T, be *fakeRealtimeBackend) {
	t.Helper()
	be.emitOnce(t, map[string]string{"type": "response.done"})
}

// greet is one whole agent turn: frames of audio, then the response.done that
// ends it.
func greet(t *testing.T, be *fakeRealtimeBackend, wire func() []carrierWireRecord, frames int) {
	t.Helper()
	playAgentAudio(t, be, wire, frames)
	endTurn(t, be)
}

// startCount plays one short agent turn, waits for its playout mark, and
// echoes it as a carrier does once that audio has played — the instant the
// caller-silence count starts. It returns that instant, taken just BEFORE the
// echo is sent, so a duration measured from it is never shorter than the one
// the engine measured.
func startCount(t *testing.T, be *fakeRealtimeBackend, h *realtimeHarness, wire func() []carrierWireRecord) time.Time {
	t.Helper()
	n := len(playoutMarks(h, wire()))
	greet(t, be, wire, 5)
	mark := waitPlayoutMarks(t, h, wire, n+1)[n]
	at := time.Now()
	echoMarkFromCarrier(t, h, mark.markName)
	return at
}

func emitSpeechStarted(t *testing.T, be *fakeRealtimeBackend) {
	t.Helper()
	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_started"})
}

func emitSpeechStopped(t *testing.T, be *fakeRealtimeBackend) {
	t.Helper()
	be.emitOnce(t, map[string]string{"type": "input_audio_buffer.speech_stopped"})
}

// waitServerEvent reads the consumer's server-event channel until an event
// matching pred arrives. It is the happens-before edge a test needs when the
// ORDER between a server event and a carrier frame is the subject: the drain
// runs the engine's own observers before it offers an event to the consumer,
// so an event read here has already been observed.
func waitServerEvent(t *testing.T, events <-chan ServerEvent, pred func(ServerEvent) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if pred(ev) {
				return
			}
		case <-deadline:
			t.Fatal("the server event this test waits on never reached the consumer's channel")
		}
	}
}

// waitEndedAfter waits for the call to end and fails unless it ended at least
// bound after from and with the caller-silence sentinel. It returns the error.
func waitEndedAfter(t *testing.T, h *realtimeHarness, from time.Time, bound time.Duration) error {
	t.Helper()
	err := h.waitDone(bound + 5*time.Second)
	if took := time.Since(from); took < bound {
		t.Fatalf("the call ended %v after the echo that started the count, before its %v bound: %v", took, bound, err)
	}
	if !errors.Is(err, ErrCallerSilenceTimeout) {
		t.Fatalf("the ending must be identifiable with errors.Is(err, ErrCallerSilenceTimeout), got: %v", err)
	}
	return err
}

// --- DoD 1: no caller speech ends the call, after the greeting has played ---

// TestCallerSilence_EndsAtTheLimitAfterTheGreetingHasPlayed is the ticket's
// central case. The agent greets, the caller says nothing, and the call ends
// one limit after the greeting has finished PLAYING — not after the backend
// finished SENDING it, which happens in a burst seconds earlier — with the
// farewell first and an error a consumer can tell apart from the idle bound.
//
// The echo is withheld across a window longer than the limit and shorter than
// the mark's own bound, which is the case that tells the two clocks apart: an
// implementation that started counting when the mark was written ends this
// call inside the window.
func TestCallerSilence_EndsAtTheLimitAfterTheGreetingHasPlayed(t *testing.T) {
	const d = callerSilenceTestLimit
	const (
		silenceLine = "twilio: realtime: caller silence timeout: playing the farewell before ending the call"
		idleLine    = "twilio: realtime: idle timeout: playing the farewell before ending the call"
	)
	buf := captureLog(t)

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d), WithFarewellAudio(farewellTestClip()))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	// One second of greeting, written as fast as the carrier accepts it.
	const greetingFrames = 50
	greet(t, be, wire, greetingFrames)
	mark := waitPlayoutMarks(t, h, wire, 1)[0]

	got := wire()
	markAt := slices.IndexFunc(got, isPlayoutMark(h))
	if n := countOf(got[:markAt], hasPayload(agentTurnPayload())); n != greetingFrames {
		t.Fatalf("the playout mark must follow the whole greeting on the wire: %d of %d frames precede it\n%+v",
			n, greetingFrames, got)
	}

	// More than twice the limit, less than the mark's bound (the greeting's
	// playout plus telephony.MarkEchoGraceMS): the carrier is still playing the
	// greeting, so the caller has had nothing to answer yet.
	time.Sleep(700 * time.Millisecond)
	assertStillRunning(t, h, "the count must not run while the greeting is still playing")
	if slices.IndexFunc(wire(), isFarewellFrame) >= 0 {
		t.Fatalf("the farewell must not start before the greeting has played:\n%+v", wire())
	}

	echoAt := time.Now()
	echoMarkFromCarrier(t, h, mark.markName)

	waitForWireRecord(t, wire, isFarewellFrame)
	got = wire()
	first := got[slices.IndexFunc(got, isFarewellFrame)]
	if waited := first.at.Sub(echoAt); waited < d || waited > d+time.Second {
		t.Fatalf("the farewell must start one limit after the greeting played: started %v after the echo, limit %v",
			waited, d)
	}

	waitForWireRecord(t, wire, isFarewellMark(h))
	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)

	err := h.waitDone(5 * time.Second)
	if !errors.Is(err, ErrCallerSilenceTimeout) {
		t.Fatalf("the ending must be identifiable with errors.Is(err, ErrCallerSilenceTimeout), got: %v", err)
	}
	if errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("a caller-silence ending must not also read as the idle timeout: %v", err)
	}
	if want := "twilio: realtime: caller silence timeout: no caller speech for 300ms"; err.Error() != want {
		t.Fatalf("the ending's message:\n got %q\nwant %q", err.Error(), want)
	}

	waitForWireRecord(t, wire, isClosed)
	got = wire()
	closedAt := slices.IndexFunc(got, isClosed)
	if n := len(farewellFrames(got[:closedAt])); n != len(farewellFills) {
		t.Fatalf("every farewell frame must reach the carrier before the close, %d of %d did:\n%+v",
			n, len(farewellFills), got)
	}
	if slices.IndexFunc(got[:closedAt], isFarewellMark(h)) < 0 {
		t.Fatalf("the farewell's mark must reach the carrier before the close:\n%+v", got)
	}

	if n := strings.Count(buf.String(), silenceLine); n != 1 {
		t.Fatalf("the caller-silence farewell must be logged exactly once, %q appeared %d times\nlog: %q",
			silenceLine, n, buf.String())
	}
	if strings.Contains(buf.String(), idleLine) {
		t.Fatalf("a caller-silence ending must not log the idle farewell line\nlog: %q", buf.String())
	}
}

// --- DoD 2: a caller who keeps speaking is not ended ------------------------

// TestCallerSilence_ACallerWhoKeepsSpeakingIsNotEnded pins DoD 2. The count is
// running, then the caller starts speaking again and again, each start well
// inside the limit, for several limits.
//
// Guards the lossless stop: delete callerSpoke from carrierMediaSink.Clear and
// the count started by the greeting runs out at one limit.
func TestCallerSilence_ACallerWhoKeepsSpeakingIsNotEnded(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	startCount(t, be, h, wire)
	be.emitEvery(t, d/3, map[string]string{"type": "input_audio_buffer.speech_started"})

	time.Sleep(3*d + d/2)
	assertStillRunning(t, h, "a caller who keeps speaking within the limit must not be hung up on")
}

// TestCallerSilence_OneUtteranceLongerThanTheLimitIsNotCut pins that caller
// speech STOPS the count rather than resetting it: one speech_started and then
// nothing at all — no speech_stopped, no reply — for three limits. A caller
// mid-sentence for longer than the limit is still talking.
//
// Mutation, exactly as run: delete BOTH the gen bump and the stopTimerLocked
// call from callerSpoke, leaving starts++ and audio=false. Either one alone
// voids the pending fire, so deleting only the stop leaves this green.
func TestCallerSilence_OneUtteranceLongerThanTheLimitIsNotCut(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	startCount(t, be, h, wire)
	emitSpeechStarted(t, be)

	time.Sleep(3 * d)
	assertStillRunning(t, h, "a caller speaking for longer than the limit must not be cut off")
}

// --- DoD 3: agent speech and a pending reply do not count -------------------

// TestCallerSilence_AgentAudioStopsTheCount pins "stops at response start". The
// count is running, and a second reply starts streaming before it runs out,
// with no response.done yet: the agent is talking, so the caller is not silent
// in any sense the bound means. When that turn ends, a second playout mark is
// written under a new name, and its echo starts the count again.
//
// Mutation, exactly as run: delete the gen bump and the stopTimerLocked call
// from callerSilence.agentAudio, keeping audio=true (deleting the whole hook
// also stops the greeting's mark ever being written, which fails startCount
// rather than this test's own assertion).
func TestCallerSilence_AgentAudioStopsTheCount(t *testing.T) {
	const d = callerSilenceTestLimit
	buf := captureLog(t)

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	echoAt := startCount(t, be, h, wire)
	playAgentAudio(t, be, wire, 50)

	time.Sleep(time.Until(echoAt.Add(3 * d)))
	assertStillRunning(t, h, "agent audio must stop the count")

	endTurn(t, be)
	marks := waitPlayoutMarks(t, h, wire, 2)
	if marks[0].markName == marks[1].markName {
		t.Fatalf("each agent turn's playout mark must carry its own name, both were %q", marks[0].markName)
	}
	if strings.Contains(buf.String(), "re-arming") {
		t.Fatalf("a second playout mark must not re-arm the first\nlog: %q", buf.String())
	}

	at := time.Now()
	echoMarkFromCarrier(t, h, marks[1].markName)
	waitEndedAfter(t, h, at, d)
}

// TestCallerSilence_AnUnansweredCallerTurnDoesNotCount pins that a caller turn
// the backend never answers leaves the count stopped: the caller spoke, so the
// silence that began at the greeting's echo is over, and nothing the agent has
// said since has finished playing to start a new one.
func TestCallerSilence_AnUnansweredCallerTurnDoesNotCount(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	startCount(t, be, h, wire)
	emitSpeechStarted(t, be)
	emitSpeechStopped(t, be)

	time.Sleep(3 * d)
	assertStillRunning(t, h, "a caller turn with no reply must not restart the count")
}

// TestCallerSilence_AFunctionCallKeepsTheReplyPending pins that the tool round
// trip is not caller silence: a response that ends in a function call means
// the reply is still coming. Two orderings, because the count can be running
// when the function call arrives, or about to start.
func TestCallerSilence_AFunctionCallKeepsTheReplyPending(t *testing.T) {
	const d = callerSilenceTestLimit

	// Both orderings finish the same way: the reply is spoken, its turn ends,
	// its mark is echoed, and only that echo starts a count that runs out.
	reply := func(t *testing.T, be *fakeRealtimeBackend, h *realtimeHarness, wire func() []carrierWireRecord) {
		t.Helper()
		waitEndedAfter(t, h, startCount(t, be, h, wire), d)
	}

	// Running: the function call arrives a third of the way into a running
	// count. Mutation: delete the stopTimerLocked call in observe's
	// function-call branch.
	t.Run("running", func(t *testing.T) {
		be := newFakeRealtimeBackend(t)
		h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
		waitBackendReady(t, be, h)
		wire := h.captureCarrierWire(t)

		echoAt := startCount(t, be, h, wire)
		time.Sleep(d / 3)
		emitFunctionCallDone(t, be)

		time.Sleep(time.Until(echoAt.Add(3 * d)))
		assertStillRunning(t, h, "a function call must stop a running count while its reply is pending")
		if n := len(playoutMarks(h, wire())); n != 1 {
			t.Fatalf("a function-call response.done must write no playout mark, got %d marks:\n%+v", n, wire())
		}
		reply(t, be, h, wire)
	})

	// Before the echo: the greeting's turn has ended and its mark is on the
	// wire, then the function call arrives, then the greeting's echo. The echo
	// must not start a count while a reply is pending. Mutation: delete the
	// !pending condition from callerSilence.played.
	t.Run("before the echo", func(t *testing.T) {
		events := make(chan ServerEvent, 256)
		be := newFakeRealtimeBackend(t)
		h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d), WithServerEventChan(events))
		waitBackendReady(t, be, h)
		wire := h.captureCarrierWire(t)

		greet(t, be, wire, 5)
		mark := waitPlayoutMarks(t, h, wire, 1)[0]
		emitFunctionCallDone(t, be)
		waitServerEvent(t, events, func(ev ServerEvent) bool { return realtime.ResponseEndedInFunctionCall(ev.Raw) })

		echoAt := time.Now()
		echoMarkFromCarrier(t, h, mark.markName)

		time.Sleep(time.Until(echoAt.Add(3 * d)))
		assertStillRunning(t, h, "a playout echo must not start the count while a reply is pending")
		reply(t, be, h, wire)
	})
}

// TestCallerSilence_BargeInDoesNotStartTheCount pins barge-in. The caller
// interrupts the greeting after its playout mark is on the wire, and Twilio —
// emptying its buffer on the clear — echoes every mark still queued behind the
// discarded audio at once. That echo says nothing about the caller having
// heard the greeting, and the caller is talking, so it must not start the
// count.
func TestCallerSilence_BargeInDoesNotStartTheCount(t *testing.T) {
	const d = callerSilenceTestLimit

	bargeIn := func(t *testing.T) (*fakeRealtimeBackend, *realtimeHarness, func() []carrierWireRecord) {
		t.Helper()
		be := newFakeRealtimeBackend(t)
		h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
		waitBackendReady(t, be, h)
		wire := h.captureCarrierWire(t)

		greet(t, be, wire, 5)
		mark := waitPlayoutMarks(t, h, wire, 1)[0]
		emitSpeechStarted(t, be)
		waitFor(t, 5*time.Second, func() bool {
			got := wire()
			i := slices.IndexFunc(got, isPlayoutMark(h))
			return i >= 0 && slices.IndexFunc(got[i:], isClear) >= 0
		})
		echoMarkFromCarrier(t, h, mark.markName)
		return be, h, wire
	}

	// Mutation: delete the gen == s.gen check from callerSilence.played.
	t.Run("immediate echo", func(t *testing.T) {
		_, h, _ := bargeIn(t)
		time.Sleep(3 * d)
		assertStillRunning(t, h, "the echo a clear releases must not start the count while the caller speaks")
	})

	// The backend's audio can trail the speech_started by a delta, and the
	// interrupted response then ends with a cancelled response.done. Any
	// playout mark written for it is echoed as a carrier would. Mutation:
	// delete the !speaking gate from callerSilence.observe.
	t.Run("trailing delta", func(t *testing.T) {
		be, h, wire := bargeIn(t)
		playAgentAudio(t, be, wire, 1)
		be.emitAny(t, map[string]any{"type": "response.done", "response": map[string]any{"status": "cancelled"}})

		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if marks := playoutMarks(h, wire()); len(marks) >= 2 {
				echoMarkFromCarrier(t, h, marks[1].markName)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		time.Sleep(3 * d)
		assertStillRunning(t, h, "a reply cancelled by barge-in must not start the count while the caller speaks")
	})
}

// TestCallerSilence_EchoOfAMarkOvertakenByLaterAudioDoesNotStartIt pins wire
// order. Reply A's mark is written, then reply B's audio is written BEHIND it
// before A's echo arrives. A's echo says A played, while B is still playing;
// only B's own mark can start the count.
//
// Mutation: delete the gen bump from callerSilence.agentAudio.
func TestCallerSilence_EchoOfAMarkOvertakenByLaterAudioDoesNotStartIt(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	greet(t, be, wire, 5)
	markA := waitPlayoutMarks(t, h, wire, 1)[0]

	// Two seconds of reply B, in one delta, written after A's mark.
	long := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x3d}, 2*telephony.SampleRateHz))
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": long})
	waitForWireRecord(t, wire, hasPayload(long))

	echoAt := time.Now()
	echoMarkFromCarrier(t, h, markA.markName)
	time.Sleep(time.Until(echoAt.Add(3 * d)))
	assertStillRunning(t, h, "the echo of a mark that later audio overtook must not start the count")

	endTurn(t, be)
	markB := waitPlayoutMarks(t, h, wire, 2)[1]
	at := time.Now()
	echoMarkFromCarrier(t, h, markB.markName)
	waitEndedAfter(t, h, at, d)
}

// TestCallerSilence_AReplyWithNoAudioDoesNotStartIt pins that a turn which put
// no audio on the line cannot end the wait it would have answered. The caller
// speaks, and the backend ends a response that carried no audio — an
// out-of-band or empty response. The caller is still waiting for a reply;
// nothing has been played to them to be silent after.
//
// Mutation: delete the audio condition from callerSilence.claim.
func TestCallerSilence_AReplyWithNoAudioDoesNotStartIt(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	startCount(t, be, h, wire)
	emitSpeechStarted(t, be)
	emitSpeechStopped(t, be)
	endTurn(t, be)

	time.Sleep(3 * d)
	if n := len(playoutMarks(h, wire())); n != 1 {
		t.Fatalf("a turn with no agent audio must write no playout mark, got %d marks:\n%+v", n, wire())
	}
	assertStillRunning(t, h, "a reply with no audio must not start the count")
}

// TestCallerSilence_UnechoedPlayoutMarkStartsTheCountAtItsBound pins the
// decision that a playout mark the carrier never echoes counts as played once
// its bound elapses. A carrier that ignores marks must not be able to switch
// the bound off.
//
// Measured from the greeting's first frame on the wire, which is where the
// playout clock the bound derives from started: the mark's bound is that
// audio's playout plus telephony.MarkEchoGraceMS, and the count is one limit
// after that. The tolerance covers the wire stamp landing after the write.
//
// Mutation: delete the played call from markTracker.expire's played branch.
func TestCallerSilence_UnechoedPlayoutMarkStartsTheCountAtItsBound(t *testing.T) {
	const (
		d      = callerSilenceTestLimit
		frames = 15 // 300 ms of greeting
	)

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	greet(t, be, wire, frames)
	waitPlayoutMarks(t, h, wire, 1)
	got := wire()
	first := got[slices.IndexFunc(got, hasPayload(agentTurnPayload()))]

	err := h.waitDone(5 * time.Second)
	ended := time.Now()
	if !errors.Is(err, ErrCallerSilenceTimeout) {
		t.Fatalf("an unechoed playout mark must still start the count, ending with the sentinel; got: %v", err)
	}
	floor := frames*telephony.MuLawFrameMS*time.Millisecond + telephony.MarkEchoGraceMS*time.Millisecond + d - 50*time.Millisecond
	if took := ended.Sub(first.at); took < floor {
		t.Fatalf("the count must start at the mark's bound, not before: the call ended %v after the greeting began, floor %v",
			took, floor)
	}
}

// --- DoD 4: unset is today's behaviour --------------------------------------

// TestCallerSilence_UnsetZeroOrNegativeIsByteIdenticalToToday pins DoD 4. A
// call that does not set the option, or sets it to zero or less, writes the
// carrier exactly the frames a call before this option wrote — no mark after a
// turn — spelled out as literals rather than derived from the encoders the
// implementation calls, and logs nothing about marks or caller silence.
//
// Mutation: delete the after <= 0 nil return from newCallerSilence.
func TestCallerSilence_UnsetZeroOrNegativeIsByteIdenticalToToday(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []RealtimeOption
	}{
		{"unset", nil},
		{"zero", []RealtimeOption{WithCallerSilenceTimeout(0)}},
		{"negative", []RealtimeOption{WithCallerSilenceTimeout(-time.Second)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)

			be := newFakeRealtimeBackend(t)
			h := silenceHarness(t, be.url(), tc.opts...)
			waitBackendReady(t, be, h)
			raw := h.captureCarrierRaw(t)

			payload := silencePayloadB64()
			be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": payload})
			waitFor(t, 5*time.Second, func() bool { return len(raw()) >= 1 })
			endTurn(t, be)
			emitSpeechStarted(t, be)
			waitFor(t, 5*time.Second, func() bool { return len(raw()) >= 2 })
			echoMarkFromCarrier(t, h, "nobody-asked-for-this")

			time.Sleep(time.Second)

			want := []string{
				`{"event":"media","streamSid":"` + h.streamSID + `","media":{"payload":"` + payload + `"}}`,
				`{"event":"clear","streamSid":"` + h.streamSID + `"}`,
			}
			if got := raw(); !slices.Equal(got, want) {
				t.Fatalf("the carrier must receive exactly today's frames:\n got %q\nwant %q", got, want)
			}
			assertStillRunning(t, h, "an unset caller-silence bound must not end the call")
			for _, absent := range []string{"caller silence", "mark"} {
				if strings.Contains(buf.String(), absent) {
					t.Fatalf("an unset caller-silence bound must log nothing about %q\nlog: %q", absent, buf.String())
				}
			}
		})
	}
}

// --- engine marks stay the engine's ----------------------------------------

// TestCallerSilence_EngineMarksNeverReachTheConsumer pins that the playout
// marks are the engine's own, on a call where the consumer has marks of its
// own in flight: their echoes, their timed-out records and a late echo after
// a mark's bound has fired are never delivered on the consumer's echo channel,
// and a late echo is not logged as matching nothing.
//
// Mutations: delete markTracker.echo's played branch (the first playout echo
// falls through to the consumer); delete the playout-prefix suppression in
// echo (the late echo is logged).
func TestCallerSilence_EngineMarksNeverReachTheConsumer(t *testing.T) {
	const d = 500 * time.Millisecond
	buf := captureLog(t)

	requests := make(chan string, testChanBuffer)
	echoes := make(chan MarkEcho, testChanBuffer)
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithMarkRequestChan(requests),
		WithMarkEchoChan(echoes),
		WithFarewellAudio(farewellTestClip()),
		WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	// The consumer's own mark, delivered as it always was.
	requests <- "c1"
	waitForWireRecord(t, wire, func(r carrierWireRecord) bool { return r.markName == "c1" })
	echoMarkFromCarrier(t, h, "c1")
	select {
	case rec := <-echoes:
		if rec != (MarkEcho{Name: "c1"}) {
			t.Fatalf("the consumer's own mark must be delivered as echoed, got %+v", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer's own mark echo never reached its channel")
	}

	// A playout mark the carrier echoes.
	startCount(t, be, h, wire)

	// A second turn, before the count runs out, whose mark is left to time out
	// and is then echoed late.
	greet(t, be, wire, 5)
	late := waitPlayoutMarks(t, h, wire, 2)[1]
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(buf.String(), "carrier did not honor mark protocol for \""+late.markName+"\"")
	})
	echoMarkFromCarrier(t, h, late.markName)

	waitForWireRecord(t, wire, isFarewellMark(h))
	echoMarkFromCarrier(t, h, realtimeFarewellMarkPrefix+h.streamSID)
	if err := h.waitDone(5 * time.Second); !errors.Is(err, ErrCallerSilenceTimeout) {
		t.Fatalf("the call must end on the caller-silence bound, got: %v", err)
	}
	if n := len(farewellFrames(wire())); n != len(farewellFills) {
		t.Fatalf("the farewell must play: %d of %d frames reached the carrier", n, len(farewellFills))
	}

	for len(echoes) > 0 {
		t.Fatalf("only the consumer's own mark may reach its echo channel, got %+v", <-echoes)
	}
	if strings.Contains(buf.String(), "matches no outstanding mark") {
		t.Fatalf("an engine playout mark's echo must never be logged as unmatched\nlog: %q", buf.String())
	}
}

// --- the two timers end with their own sentinels ----------------------------

// TestCallerSilence_AndIdleTimeoutEachEndWithTheirOwnSentinel pins that a
// consumer setting both bounds can tell which one ended the call. The bounds
// are an order of magnitude apart in each subtest, so the one that fires is
// never in doubt.
func TestCallerSilence_AndIdleTimeoutEachEndWithTheirOwnSentinel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		silence, idle time.Duration
		want, notWant error
	}{
		{"caller silence first", 300 * time.Millisecond, 3 * time.Second, ErrCallerSilenceTimeout, ErrIdleTimeout},
		{"idle first", 3 * time.Second, 500 * time.Millisecond, ErrIdleTimeout, ErrCallerSilenceTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newFakeRealtimeBackend(t)
			h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(tc.silence), WithIdleTimeout(tc.idle))
			waitBackendReady(t, be, h)
			wire := h.captureCarrierWire(t)

			startCount(t, be, h, wire)
			err := h.waitDone(10 * time.Second)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want an error matching %v, got: %v", tc.want, err)
			}
			if errors.Is(err, tc.notWant) {
				t.Fatalf("the error must not also match %v: %v", tc.notWant, err)
			}
		})
	}
}

// TestCallerSilence_TheFillersClearIsNotCallerSpeech pins the split between
// the barge-in Clear and the engine's own clears. The hold loop is playing at
// call open, the greeting's first frame stops it with the engine's own clear,
// and that clear must not be counted as the caller speaking — or the drain,
// which saw no speech_started, and the sink would disagree for the rest of the
// call and no playout mark would ever be written.
//
// Mutation: make stopFillerAndClear call the exported Clear.
func TestCallerSilence_TheFillersClearIsNotCallerSpeech(t *testing.T) {
	const d = callerSilenceTestLimit

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(),
		WithFillerAudio(FillerConfig{Loop: fillerTestLoop(), Delay: fillerTestDelay}),
		WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	waitFillerPlaying(t, wire, 3)
	greet(t, be, wire, 5)

	got := wire()
	if i := slices.IndexFunc(got, hasPayload(agentTurnPayload())); i < 1 || !got[i-1].clear {
		t.Fatalf("the greeting must interrupt the hold loop with a clear ahead of its first frame:\n%+v", got)
	}

	mark := waitPlayoutMarks(t, h, wire, 1)[0]
	at := time.Now()
	echoMarkFromCarrier(t, h, mark.markName)
	waitEndedAfter(t, h, at, d)
}

// TestCallerSilence_EachAgentTurnGrantsAFreshBound pins a DECISION, not a
// deletion: each agent turn whose audio has played grants the caller a whole
// limit again (restart), rather than the count pausing while the agent speaks
// and resuming with what was left (accumulate). Ian confirmed restart on
// 2026-10-28. No deletion turns this red; an accumulating implementation
// does, because it ends the call 0.4 of a limit after the second echo.
func TestCallerSilence_EachAgentTurnGrantsAFreshBound(t *testing.T) {
	const d = 500 * time.Millisecond

	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithCallerSilenceTimeout(d))
	waitBackendReady(t, be, h)
	wire := h.captureCarrierWire(t)

	firstEcho := startCount(t, be, h, wire)
	time.Sleep(time.Until(firstEcho.Add(d * 6 / 10)))
	secondEcho := startCount(t, be, h, wire)

	time.Sleep(time.Until(secondEcho.Add(d * 7 / 10)))
	assertStillRunning(t, h, "a second agent turn must grant the caller a whole limit again")
	waitEndedAfter(t, h, secondEcho, d)
}

// TestIdleTimeout_ErrorBytesAreUnchanged pins the idle ending's message now that
// it is built by the helper both timers share. TestSilentBackend_BothConditionsAreLoggedOnce
// pins its farewell log line.
func TestIdleTimeout_ErrorBytesAreUnchanged(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	h := silenceHarness(t, be.url(), WithIdleTimeout(300*time.Millisecond))

	err := h.waitDone(5 * time.Second)
	if want := "twilio: realtime: idle timeout: no backend activity for 300ms"; err == nil || err.Error() != want {
		t.Fatalf("the idle ending's message must be unchanged:\n got %v\nwant %q", err, want)
	}
}
