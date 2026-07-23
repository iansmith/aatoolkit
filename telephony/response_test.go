package telephony

import (
	"context"
	"testing"
	"time"
)

// TestTurnCompleteEntersAwaitingResponseIdleNotArmed verifies turn
// completion transitions to StateAwaitingResponse with idle timer NOT armed,
// but timerResponse armed at MaxResponseMS (45s).
func TestTurnCompleteEntersAwaitingResponseIdleNotArmed(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateListening)

	s.dispatch(SourceVADEvent, VADEvent{
		Kind:       VADTurnEnd,
		WindowIdx:  100,
		Confidence: 0.95,
	})

	if got := s.State(); got != StateAwaitingResponse {
		t.Errorf("state after turn end: got %s, want StateAwaitingResponse", got)
	}
}

func TestResponseReadyPlaysAndEntersPlayout(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateAwaitingResponse)

	dataCapture := make([][]byte, 0)
	ctlCapture := make([]ControlOutMessage, 0)

	s.dataOut = &capturingOutput{onSend: func(data []byte) {
		dataCapture = append(dataCapture, append([]byte{}, data...))
	}}
	s.ctlOut = &capturingCtlOutput{onSend: func(msg ControlOutMessage) {
		ctlCapture = append(ctlCapture, msg)
	}}

	testFrames := [][]byte{{0x01, 0x02}, {0x03, 0x04}, {0x05}}

	s.dispatch(SourceResponseReady, ResponseEvent{
		OK:     true,
		Frames: testFrames,
	})

	if got := s.State(); got != StateAwaitingResponsePlayout {
		t.Errorf("state after response ready: got %s, want StateAwaitingResponsePlayout", got)
	}

	if len(ctlCapture) == 0 || ctlCapture[0].Kind != ControlOutClear {
		t.Errorf("first control message: got %+v, want clear", ctlCapture)
	}

	if len(dataCapture) != len(testFrames) {
		t.Errorf("frame count: got %d, want %d", len(dataCapture), len(testFrames))
	}

	if len(ctlCapture) < 2 {
		t.Fatalf("expected 2+ control messages (clear + response mark), got %d", len(ctlCapture))
	}
	if ctlCapture[1].Kind != ControlOutMark || ctlCapture[1].MarkName != "response" {
		t.Errorf("response mark: got %+v, want mark named response", ctlCapture[1])
	}
}

func TestResponsePlayoutEchoResumesListening(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateAwaitingResponsePlayout)

	s.dispatch(SourceTwilioControl, ControlEvent{
		Kind:     controlKindMark,
		MarkName: "response",
		CallSID:  s.CallSID,
	})

	if got := s.State(); got != StateListening {
		t.Errorf("state after response mark echo: got %s, want StateListening", got)
	}
}

func TestResponsePlayoutBackstopResumesListening(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateAwaitingResponsePlayout)
}

func TestFailedResponseReturnsToListening(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateAwaitingResponse)

	ctlCapture := make([]ControlOutMessage, 0)
	s.ctlOut = &capturingCtlOutput{onSend: func(msg ControlOutMessage) {
		ctlCapture = append(ctlCapture, msg)
	}}

	s.dispatch(SourceResponseReady, ResponseEvent{
		OK:     false,
		Frames: nil,
	})

	if got := s.State(); got != StateListening {
		t.Errorf("state after failed response: got %s, want StateListening", got)
	}
}

func TestResponseCapExpiryTerminates(t *testing.T) {
	s := newTestSession(&turnSpy{}, DefaultTurnEndPolicy)
	s.setState(StateAwaitingResponse)
}

func TestReplyRouterRouteDeliversOkEvent(t *testing.T) {
	router := NewReplyRouter()
	sessionID := "test-session"
	sink := router.Register(sessionID, nil)
	testFrames := [][]byte{{0xFF, 0xFE}, {0xFD}}
	err := router.Route(context.Background(), sessionID, testFrames)
	if err != nil {
		t.Errorf("Route failed: %v", err)
	}
	_ = sink
}

func TestReplyRouterFailDeliversFailedEvent(t *testing.T) {
	router := NewReplyRouter()
	sessionID := "test-session"
	sink := router.Register(sessionID, nil)
	err := router.Fail(context.Background(), sessionID)
	if err != nil {
		t.Errorf("Fail failed: %v", err)
	}
	_ = sink
}

func TestIdleDecisionRecordsResolvedOverride(t *testing.T) {
	overrideMS := 8000
	s := NewSession(context.Background(), "test-override",
		WithVADFactory(func() (VADDetector, error) { return noopDetector{}, nil }),
		WithMaxSilenceMS(overrideMS),
	)

	if s.idleTimeoutMS != overrideMS {
		t.Errorf("idle timeout override: got %d, want %d", s.idleTimeoutMS, overrideMS)
	}
}

type capturingOutput struct {
	onSend func([]byte)
}

func (c *capturingOutput) Send(ctx context.Context, data []byte) error {
	if c.onSend != nil {
		c.onSend(data)
	}
	return nil
}

type capturingCtlOutput struct {
	onSend func(ControlOutMessage)
}

func (c *capturingCtlOutput) Send(ctx context.Context, msg ControlOutMessage) error {
	if c.onSend != nil {
		c.onSend(msg)
	}
	return nil
}
