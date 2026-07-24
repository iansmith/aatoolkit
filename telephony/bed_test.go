package telephony_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

// AATK-25 Phase 0. These tests pin the paced thinking bed: while
// StateAwaitingResponse waits for a response, a repeating bed-tick timer
// writes one chunk of assets.LLMThinkingULaw to dataOut per tick and
// re-arms, looping the clip. The bed timer is cancelled on every exit from
// StateAwaitingResponse (ok event, failed event, response-cap expiry), so no
// bed chunk survives the clear. All timing is driven by the injected
// fakeClock -- no sleeps, no waiting for a real deadline.

// bedTickTestMS mirrors the ticket's frozen bedTickMS constant (200). Tests
// assert against this literal, not a package export, since the ticket does
// not ask for bedTickMS to be exported.
const bedTickTestMS = 200

func bedChunkBytes() int { return bedTickTestMS * telephony.SampleRateHz / 1000 }

// TestBedTickWritesOneChunkAndRearms pins Observable behavior #1: on
// entering StateAwaitingResponse the session arms a repeating bed-tick
// timer; each tick writes one chunk of assets.LLMThinkingULaw (bedTickMS *
// SampleRateHz / 1000 bytes) to dataOut and re-arms, looping the clip. The
// session's state is unchanged by a bed tick.
func TestBedTickWritesOneChunkAndRearms(t *testing.T) {
	wantChunk := bedChunkBytes()

	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "bed-tick",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "bed-tick", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	clock.fire(t, bedTickTestMS*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	frame, err := dataOut.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("recv bed chunk: %v", err)
	}
	if len(frame) != wantChunk {
		t.Fatalf("bed chunk size: got %d bytes, want %d (bedTickMS * SampleRateHz / 1000)", len(frame), wantChunk)
	}
	if !bytes.Equal(frame, assets.LLMThinkingULaw[:wantChunk]) {
		t.Fatalf("bed chunk content: got %v, want the clip's first %d bytes", frame, wantChunk)
	}
	if got := s.State(); got != telephony.StateAwaitingResponse {
		t.Fatalf("state after one bed tick: got %s, want AwaitingResponse (unchanged)", got)
	}

	// The tick must re-arm: firing bedTickMS again produces a second chunk,
	// continuing the loop from where the first chunk left off.
	clock.fire(t, bedTickTestMS*time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), recvTimeout)
	frame2, err := dataOut.Recv(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("recv second bed chunk (timer did not re-arm): %v", err)
	}
	if len(frame2) != wantChunk {
		t.Fatalf("second bed chunk size: got %d bytes, want %d", len(frame2), wantChunk)
	}
	if !bytes.Equal(frame2, assets.LLMThinkingULaw[wantChunk:2*wantChunk]) {
		t.Fatalf("second bed chunk content: got %v, want the clip's next %d bytes (looping)", frame2, wantChunk)
	}
}

// TestBedCancelledOnResponse pins Observable behavior #2's ok-event exit: an
// ok SourceResponseReady event cancels the bed-tick timer, so firing its
// stale deadline afterward produces no further bed chunk. Checked by
// consequence, not by clock.isArmed: the fake clock's armed bookkeeping is
// append-only and never sees TimerFacility.Cancel(), so a cancelled timer's
// stale channel is still "armed" from fakeClock's point of view -- firing it
// is a safe no-op (see telephony/response_delivery_test.go).
func TestBedCancelledOnResponse(t *testing.T) {
	chunkBytes := bedChunkBytes()

	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "bed-cancel-response",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
		telephony.WithClock(clock.after),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "bed-cancel-response", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)
	clock.waitArmed(t, bedTickTestMS*time.Millisecond)

	frames := [][]byte{{0x01, 0x02}}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: true, Frames: frames}); err != nil {
		t.Fatalf("send response event: %v", err)
	}
	cancel()
	waitForSessionState(t, s, telephony.StateAwaitingResponsePlayout)
	drainNFrames(t, dataOut, len(frames)) // the response's own frame, sent before the mark

	clock.fire(t, bedTickTestMS*time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	got := drainAvailable(t, dataOut, ctx2)
	cancel2()
	if bytes.Contains(got, assets.LLMThinkingULaw[:chunkBytes]) {
		t.Fatal("bed chunk present on dataOut after an ok response event left StateAwaitingResponse -- bed timer must be cancelled")
	}
}

// TestBedCancelledOnFailure pins Observable behavior #2's failed-event exit:
// same as TestBedCancelledOnResponse, but for a failed ResponseEvent.
func TestBedCancelledOnFailure(t *testing.T) {
	chunkBytes := bedChunkBytes()

	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "bed-cancel-failure",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
		telephony.WithClock(clock.after),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "bed-cancel-failure", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)
	clock.waitArmed(t, bedTickTestMS*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: false}); err != nil {
		t.Fatalf("send failed response event: %v", err)
	}
	cancel()
	waitForSessionState(t, s, telephony.StateListening)

	clock.fire(t, bedTickTestMS*time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	got := drainAvailable(t, dataOut, ctx2)
	cancel2()
	if bytes.Contains(got, assets.LLMThinkingULaw[:chunkBytes]) {
		t.Fatal("bed chunk present on dataOut after a failed response event left StateAwaitingResponse -- bed timer must be cancelled")
	}
}

// TestBedCancelledOnCapExpiry pins Observable behavior #2's response-cap
// exit: timerResponse's expiry cancels the bed-tick timer the same way the
// two response-event exits do.
func TestBedCancelledOnCapExpiry(t *testing.T) {
	const responseMS = 20000
	chunkBytes := bedChunkBytes()

	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "bed-cancel-cap",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithMaxResponseMS(responseMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "bed-cancel-cap", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)
	clock.waitArmed(t, bedTickTestMS*time.Millisecond)

	clock.fire(t, responseMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)

	clock.fire(t, bedTickTestMS*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	got := drainAvailable(t, dataOut, ctx)
	cancel()
	if bytes.Contains(got, assets.LLMThinkingULaw[:chunkBytes]) {
		t.Fatal("bed chunk present on dataOut after the response cap terminated the wait -- bed timer must be cancelled")
	}
}
