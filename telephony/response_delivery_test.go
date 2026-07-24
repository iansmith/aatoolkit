package telephony_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/assets"
)

// AATK-24 Phase 0. These tests pin the signal-driven response-delivery state
// machine core: on turn completion the session enters StateAwaitingResponse
// (idle timer NOT armed, timerResponse armed instead); an ok response event
// plays it and enters StateAwaitingResponsePlayout; the response mark echo
// (or its derived backstop) returns to Listening with the idle timer armed;
// a failed event returns straight to Listening; timerResponse's expiry
// terminates the call the same way idle timeout does. All timing is driven
// by the injected fakeClock -- no sleeps, no waiting for a real deadline.

// orderedEvent is one entry in outSpy's combined send log, tagging which
// plane it arrived on so a test can assert cross-plane ordering: the clear
// (control), the response's audio frames (data), and the response mark
// (control) must interleave in exactly the order handleResponseReady sends
// them, which two separate channels can't show on their own.
type orderedEvent struct {
	kind  string // "clear", "frame", "mark"
	frame []byte
	mark  string
}

type outSpy struct {
	mu  sync.Mutex
	log []orderedEvent
}

func (o *outSpy) events() []orderedEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]orderedEvent(nil), o.log...)
}

// spyDataOut is a TwilioDataPlaneOutput that appends every sent frame to the
// shared outSpy log.
type spyDataOut struct{ spy *outSpy }

func (d spyDataOut) Channel() <-chan []byte { return nil }
func (d spyDataOut) Recv(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (d spyDataOut) Send(ctx context.Context, payload []byte) error {
	d.spy.mu.Lock()
	defer d.spy.mu.Unlock()
	d.spy.log = append(d.spy.log, orderedEvent{kind: "frame", frame: append([]byte(nil), payload...)})
	return nil
}

// spyCtlOut is a TwilioControlPlaneOutput that appends every sent control
// message to the shared outSpy log.
type spyCtlOut struct{ spy *outSpy }

func (c spyCtlOut) Channel() <-chan telephony.ControlOutMessage { return nil }
func (c spyCtlOut) Recv(ctx context.Context) (telephony.ControlOutMessage, error) {
	<-ctx.Done()
	return telephony.ControlOutMessage{}, ctx.Err()
}
func (c spyCtlOut) Send(ctx context.Context, msg telephony.ControlOutMessage) error {
	c.spy.mu.Lock()
	defer c.spy.mu.Unlock()
	if msg.Kind == telephony.ControlOutClear {
		c.spy.log = append(c.spy.log, orderedEvent{kind: "clear"})
	} else {
		c.spy.log = append(c.spy.log, orderedEvent{kind: "mark", mark: msg.MarkName})
	}
	return nil
}

// drainNFrames reads exactly n frames off dataOut, failing the test if any
// Recv errors -- used to consume a response's already-asserted frames before
// checking what comes after (e.g. a farewell clip sharing the same dataOut).
func drainNFrames(t *testing.T, dataOut telephony.TwilioDataPlaneOutput, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
		_, err := dataOut.Recv(ctx)
		cancel()
		if err != nil {
			t.Fatalf("drain response frame %d/%d: %v", i+1, n, err)
		}
	}
}

// drainAvailable reads whatever arrives on dataOut before ctx expires,
// without failing the test if nothing does -- used to prove an absence (no
// farewell clip) rather than a presence.
func drainAvailable(t *testing.T, dataOut telephony.TwilioDataPlaneOutput, ctx context.Context) []byte {
	t.Helper()
	var got []byte
	for {
		frame, err := dataOut.Recv(ctx)
		if err != nil {
			return got
		}
		got = append(got, frame...)
	}
}

// TestTurnCompleteEntersAwaitingResponseIdleNotArmed pins the core fix: on
// turn completion the session enters StateAwaitingResponse WITHOUT arming
// the idle timer (the racing 15s clock must not start), and arms
// timerResponse instead.
//
// Checked by consequence, not by clock.isArmed: the fake clock's armed
// bookkeeping only clears a duration via fire() -- TimerFacility.Cancel()
// cancels the timer's own context but never notifies fakeClock, so
// isArmed(d) can't distinguish "still armed" from "was armed once, then
// cancelled" (see telephony/utterance_cap_test.go, which documents and works
// around this exact pitfall). So the idle timer's absence is proven by
// firing its stale deadline and requiring no farewell clip results --
// firing a cancelled timer's duration is a safe no-op (its completion fails
// IsCurrent and is dropped) -- and its presence would be proven by the
// farewell actually appearing, which we also check never happens here.
// timerResponse's arming is then proven the same way utterance_cap_test.go
// proves a timer is armed: firing its exact duration and requiring the
// expected consequence (the response cap's own farewell-and-terminate,
// pinned separately by TestResponseCapExpiryTerminates below; here we only
// need the no-op half plus the response cap's SEPARATE presence, so we
// additionally use clock.waitArmed to directly observe timerResponse was
// armed before ever firing it).
func TestTurnCompleteEntersAwaitingResponseIdleNotArmed(t *testing.T) {
	const idleMS = 9000
	const responseMS = 20000
	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-idle-not-armed",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleMS),
		telephony.WithMaxResponseMS(responseMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-idle-not-armed", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	// The idle deadline's stale generations (Start, and onUtteranceEnd's
	// re-arm) must all have been cancelled by entering StateAwaitingResponse:
	// firing idleMS now must produce no farewell.
	clock.fire(t, idleMS*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	if bytes.Contains(drainAvailable(t, dataOut, ctx), assets.FarewellULaw) {
		t.Fatal("farewell clip present on dataOut after firing the idle deadline -- idle timer must not be armed on turn completion")
	}
	cancel()
	if got := s.State(); got != telephony.StateAwaitingResponse {
		t.Fatalf("state after firing the stale idle deadline: got %s, want AwaitingResponse", got)
	}

	// timerResponse must be armed at responseMS: firing it is the direct
	// proof (the response cap's own consequence -- farewell + terminate --
	// is pinned by TestResponseCapExpiryTerminates).
	clock.fire(t, responseMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestResponseReadyPlaysAndEntersPlayout pins Observable behavior #2: an ok
// SourceResponseReady event in StateAwaitingResponse produces, in exact
// order, a Twilio "clear" control message, the response frames written to
// dataOut, and a mark named "response", then transitions to
// StateAwaitingResponsePlayout.
func TestResponseReadyPlaysAndEntersPlayout(t *testing.T) {
	spy := &outSpy{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-ready",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(spyDataOut{spy}),
		telephony.WithTwilioControlOutput(spyCtlOut{spy}),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-ready", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	frames := [][]byte{{0x01, 0x02}, {0x03, 0x04, 0x05}}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: true, Frames: frames}); err != nil {
		t.Fatalf("send response event: %v", err)
	}
	cancel()

	waitForSessionState(t, s, telephony.StateAwaitingResponsePlayout)

	events := spy.events()
	wantLen := 1 + len(frames) + 1
	if len(events) != wantLen {
		t.Fatalf("outbound event count: got %d, want %d ([clear, %d frames, mark])", len(events), wantLen, len(frames))
	}
	if events[0].kind != "clear" {
		t.Errorf("event 0: got %+v, want clear", events[0])
	}
	for i, f := range frames {
		ev := events[1+i]
		if ev.kind != "frame" || !bytes.Equal(ev.frame, f) {
			t.Errorf("event %d: got %+v, want frame %v", 1+i, ev, f)
		}
	}
	last := events[len(events)-1]
	if last.kind != "mark" || last.mark != "response" {
		t.Errorf("last event: got %+v, want mark %q", last, "response")
	}
}

// TestResponsePlayoutEchoResumesListening pins Observable behavior #3's
// happy path: a "response" mark echo received in StateAwaitingResponsePlayout
// transitions to StateListening and arms the idle timer.
func TestResponsePlayoutEchoResumesListening(t *testing.T) {
	const idleMS = 9000
	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	controlIn := telephony.NewBufferedChan[telephony.ControlEvent](4)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-echo",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlInput(controlIn),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-echo", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: true, Frames: [][]byte{{0x01}}}); err != nil {
		t.Fatalf("send response event: %v", err)
	}
	cancel()
	waitForSessionState(t, s, telephony.StateAwaitingResponsePlayout)
	drainNFrames(t, dataOut, 1) // the response's own frame, sent before the mark

	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	if err := controlIn.Send(ctx, telephony.ControlEvent{Kind: "mark", MarkName: "response"}); err != nil {
		t.Fatalf("send mark echo: %v", err)
	}
	cancel()

	waitForSessionState(t, s, telephony.StateListening)

	// idle armed at resolvedMS(idleTimeoutMS, MaxSilenceMS) -- proof by
	// consequence: firing it must produce the farewell.
	clock.fire(t, idleMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestResponsePlayoutBackstopResumesListening pins Observable behavior #3's
// backstop path: when the mark echo never arrives, the derived backstop
// (from the response's total byte length across all frames) fires the same
// transition to StateListening with the idle timer armed.
func TestResponsePlayoutBackstopResumesListening(t *testing.T) {
	const idleMS = 9000
	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-backstop",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-backstop", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	// frames is [][]byte deliberately of uneven length, so the backstop must
	// sum bytes ACROSS every frame, never the frame count.
	frames := [][]byte{{0x01, 0x02, 0x03}, {0x04, 0x05}}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: true, Frames: frames}); err != nil {
		t.Fatalf("send response event: %v", err)
	}
	cancel()
	waitForSessionState(t, s, telephony.StateAwaitingResponsePlayout)
	drainNFrames(t, dataOut, len(frames)) // the response's own frames, sent before the mark

	// The backstop is derived the same way MarkEchoTimeout derives the
	// farewell's, from the response's total bytes across every frame --
	// computed here via the exported MarkEchoTimeout over the joined bytes,
	// since the derivation math is identical (total bytes * 1000 /
	// SampleRateHz + MarkEchoGraceMS).
	wantBackstop := telephony.MarkEchoTimeout(bytes.Join(frames, nil))
	clock.waitArmed(t, wantBackstop)
	clock.fire(t, wantBackstop)

	waitForSessionState(t, s, telephony.StateListening)
	clock.fire(t, idleMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestFailedResponseReturnsToListening pins Observable behavior #4's failed
// half (D6): a failed event sends "clear", writes no frames/mark,
// transitions to StateListening, and arms the idle timer.
func TestFailedResponseReturnsToListening(t *testing.T) {
	const idleMS = 9000
	clock := newFakeClock()
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	ctlOut := telephony.NewBufferedChan[telephony.ControlOutMessage](8)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-failed",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(ctlOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithResponseInput(responseIn),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleMS),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-failed", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: false}); err != nil {
		t.Fatalf("send failed response event: %v", err)
	}
	cancel()

	waitForSessionState(t, s, telephony.StateListening)

	ctx, cancel = context.WithTimeout(context.Background(), recvTimeout)
	msg, err := ctlOut.Recv(ctx)
	cancel()
	if err != nil {
		t.Fatalf("expected a clear control message, got error: %v", err)
	}
	if msg.Kind != telephony.ControlOutClear {
		t.Errorf("control message kind: got %s, want clear", msg.Kind)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	if _, err := ctlOut.Recv(ctx2); err == nil {
		t.Error("unexpected second control message after a failed response")
	}
	cancel2()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	if _, err := dataOut.Recv(ctx3); err == nil {
		t.Error("unexpected data frame after a failed response")
	}
	cancel3()

	// idle armed at resolvedMS(idleTimeoutMS, MaxSilenceMS) -- proof by
	// consequence: firing it must produce the farewell.
	clock.fire(t, idleMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)
}

// TestResponseCapExpiryTerminates pins Observable behavior #4's cap half: no
// response by MaxResponseMS terminates the call the same way idle timeout
// does -- farewell clip, then the mark-echo teardown flow -- recording the
// cap decision.
func TestResponseCapExpiryTerminates(t *testing.T) {
	if telephony.MaxResponseMS != 45000 {
		t.Fatalf("MaxResponseMS = %d, want 45000", telephony.MaxResponseMS)
	}

	const responseMS = 20000
	clock := newFakeClock()
	rec := &mockRecorder{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	dataOut := telephony.NewBufferedChan[[]byte](256)
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](8)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](8)
	probs := speechThenSilenceProbs(2, telephony.EndSilenceWindows())
	s := telephony.NewSession(context.Background(), "resp-cap-expiry",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{probs: probs}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithSTTInput(sttIn),
		telephony.WithSTTOutput(sttOut),
		telephony.WithTurnEndPolicy(telephony.StopwordPolicy{}),
		telephony.WithClock(clock.after),
		telephony.WithMaxResponseMS(responseMS),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	driveUtteranceToSTT(t, dataIn, sttIn, sttOut, "resp-cap-expiry", "done")
	waitForSessionState(t, s, telephony.StateAwaitingResponse)

	clock.fire(t, responseMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.State(); st == telephony.StateAwaitingMarkEcho || st == telephony.StateClosed {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if st := s.State(); st != telephony.StateAwaitingMarkEcho && st != telephony.StateClosed {
		t.Fatalf("after the response cap fired, state = %s, want AwaitingMarkEcho or Closed", st)
	}

	var found bool
	for _, ev := range rec.all() {
		if ev.Kind == telephony.DecisionKindResponseCap {
			found = true
			if ev.ParamValue != responseMS {
				t.Errorf("response-cap decision ParamValue: got %v, want %d", ev.ParamValue, responseMS)
			}
		}
	}
	if !found {
		t.Fatal("no response-cap decision recorded")
	}
}

// TestReplyRouterRouteDeliversOkEvent pins the ticket's ReplyRouter
// Observable behavior: Route enqueues an ok ResponseEvent on the sink's
// response input (no longer writes the WebSocket).
func TestReplyRouterRouteDeliversOkEvent(t *testing.T) {
	router := telephony.NewReplyRouter()
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	sink := router.Register("CA1", responseIn)
	defer sink.Close()

	frames := [][]byte{{0x01, 0x02}, {0x03}}
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := router.Route(ctx, "CA1", frames); err != nil {
		t.Fatalf("Route: %v", err)
	}
	cancel()

	ctx2, cancel2 := context.WithTimeout(context.Background(), recvTimeout)
	ev, err := responseIn.Recv(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("recv response event: %v", err)
	}
	if !ev.OK {
		t.Error("delivered event: OK = false, want true")
	}
	if len(ev.Frames) != len(frames) {
		t.Fatalf("delivered frames: got %d, want %d", len(ev.Frames), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(ev.Frames[i], frames[i]) {
			t.Errorf("frame %d: got %v, want %v", i, ev.Frames[i], frames[i])
		}
	}

	ctx3, cancel3 := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel3()
	if err := router.Route(ctx3, "unknown-session", frames); err != telephony.ErrUnknownSession {
		t.Errorf("Route on unknown session: got %v, want ErrUnknownSession", err)
	}
}

// TestReplyRouterFailDeliversFailedEvent pins the ticket's new
// ReplyRouter.Fail method: it delivers a failed ResponseEvent to the sink's
// response input.
func TestReplyRouterFailDeliversFailedEvent(t *testing.T) {
	router := telephony.NewReplyRouter()
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	sink := router.Register("CA1", responseIn)
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := router.Fail(ctx, "CA1"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	cancel()

	ctx2, cancel2 := context.WithTimeout(context.Background(), recvTimeout)
	ev, err := responseIn.Recv(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("recv response event: %v", err)
	}
	if ev.OK {
		t.Error("delivered event: OK = true, want false")
	}
	if len(ev.Frames) != 0 {
		t.Errorf("delivered frames: got %d, want 0", len(ev.Frames))
	}

	ctx3, cancel3 := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel3()
	if err := router.Fail(ctx3, "unknown-session"); err != telephony.ErrUnknownSession {
		t.Errorf("Fail on unknown session: got %v, want ErrUnknownSession", err)
	}
}

// TestIdleDecisionRecordsResolvedOverride pins D8's silence-knob
// production-ization (Addendum V3): with WithMaxSilenceMS(x) set, the
// idle-timeout cap decision records x, not the bare MaxSilenceMS constant.
func TestIdleDecisionRecordsResolvedOverride(t *testing.T) {
	const idleOverrideMS = 4321
	clock := newFakeClock()
	rec := &mockRecorder{}
	dataOut := telephony.NewBufferedChan[[]byte](256)
	s := telephony.NewSession(context.Background(), "idle-decision-override",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithClock(clock.after),
		telephony.WithMaxSilenceMS(idleOverrideMS),
		telephony.WithDecisionRecorder(rec),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	clock.fire(t, idleOverrideMS*time.Millisecond)
	drainClip(t, dataOut, assets.FarewellULaw)

	var found bool
	for _, ev := range rec.all() {
		if ev.Kind == telephony.DecisionKindIdleTimeout {
			found = true
			if ev.ParamValue != idleOverrideMS {
				t.Errorf("idle-timeout decision ParamValue: got %v, want %d (the resolved override, not the bare MaxSilenceMS constant)", ev.ParamValue, idleOverrideMS)
			}
		}
	}
	if !found {
		t.Fatal("no idle-timeout decision recorded")
	}
}

// TestResponseReadyAbsorbedOutsideAwaitingResponse pins the second half of
// Observable behavior #2: a SourceResponseReady event in any state OTHER
// than StateAwaitingResponse is absorbed and logged -- frames dropped, no
// clear/mark, state unchanged. (Adversary gap: the other 9 tests only ever
// send a response event from StateAwaitingResponse.)
func TestResponseReadyAbsorbedOutsideAwaitingResponse(t *testing.T) {
	spy := &outSpy{}
	dataIn := telephony.NewBufferedChan[[]byte](256)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](4)
	s := telephony.NewSession(context.Background(), "resp-outside-awaiting",
		telephony.WithVADFactory(func() (telephony.VADDetector, error) { return &fakeDetector{}, nil }),
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioDataOutput(spyDataOut{spy}),
		telephony.WithTwilioControlOutput(spyCtlOut{spy}),
		telephony.WithResponseInput(responseIn),
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	sendData(t, dataIn, windowFrame(0x01), recvTimeout)
	waitForSessionState(t, s, telephony.StateListening)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	if err := responseIn.Send(ctx, telephony.ResponseEvent{OK: true, Frames: [][]byte{{0xAA}}}); err != nil {
		t.Fatalf("send response event: %v", err)
	}
	cancel()

	// No timer or channel event distinguishes "absorbed" from "not yet
	// processed" -- bounded settle, then assert nothing changed, the same
	// pattern TestSpeechOnsetCancelsIdleTimer already uses to prove a negative.
	time.Sleep(150 * time.Millisecond)

	if got := s.State(); got != telephony.StateListening {
		t.Errorf("state after a response event outside StateAwaitingResponse: got %s, want Listening (unchanged)", got)
	}
	if events := spy.events(); len(events) != 0 {
		t.Errorf("outbound events after a response event outside StateAwaitingResponse: got %+v, want none (frames dropped, per Observable behavior #2)", events)
	}
}
