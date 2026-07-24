package telephony

import (
	"context"
	"testing"
)

// New (AATK-22 review follow-up): Close must only remove the sink it was
// returned for, never a different, newer registration under the same
// sessionID -- mirrors STTRouter.Deregister's identity check (sttrouter.go),
// which this router's Deregister(sessionID) alone cannot provide since its
// signature is frozen by the Phase 0 contract.
func TestReplySink_CloseOnlyRemovesOwnRegistration(t *testing.T) {
	router := NewReplyRouter()
	sessionID := "CA-shared"

	oldSink := router.Register(sessionID, &captureResponseInput{})
	newSink := router.Register(sessionID, &captureResponseInput{})
	if oldSink == newSink {
		t.Fatal("expected distinct sinks from two Register calls")
	}

	// The old registration's teardown fires after a newer one has already
	// replaced it under the same sessionID.
	oldSink.Close()

	ctx := context.Background()
	frames := [][]byte{{0xAA}}
	if err := router.Route(ctx, sessionID, frames); err != nil {
		t.Fatalf("Route after stale Close: %v -- newer registration was wrongly removed", err)
	}
}

// Close on the currently-registered sink removes it, same as Deregister.
func TestReplySink_CloseRemovesCurrentRegistration(t *testing.T) {
	router := NewReplyRouter()
	sessionID := "CA-solo"

	sink := router.Register(sessionID, &captureResponseInput{})
	sink.Close()

	ctx := context.Background()
	err := router.Route(ctx, sessionID, [][]byte{{0xBB}})
	if err != ErrUnknownSession {
		t.Fatalf("Route after Close: err = %v, want ErrUnknownSession", err)
	}
}
