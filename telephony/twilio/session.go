package twilio

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// stopDrainTimeout bounds how long handleStream waits, after routing a
// "stop" frame, for it to travel pumpControlPlane -> the session's
// controlIn -> run()'s transition table and land the session in
// StateClosed, before falling back to Close(). Without this wait, returning
// immediately races Close()'s ctx cancellation against that async delivery,
// and the transition table's stop handling (state.go's handleControlEvent)
// can be skipped entirely on the real Twilio path.
type replyRouterContextKey struct{}

var replyRouterKey replyRouterContextKey

// ContextWithReplyRouter returns a copy of ctx carrying router, so that a
// later handleStream call on that ctx registers its session's ReplySink with
// it. Server.ReplyRouter is the production entry point (AATK-22); this is
// exported for callers driving handleStream/HandleStreamWithOpts directly
// (e.g. tests, or a consumer not going through Server).
func ContextWithReplyRouter(ctx context.Context, router *telephony.ReplyRouter) context.Context {
	return context.WithValue(ctx, replyRouterKey, router)
}

const stopDrainTimeout = 500 * time.Millisecond

// DefaultHandleStream is the default session handler. It drives a live
// telephony.Session for the call: media frames are pumped into the session
// for VAD processing, and the session is closed exactly once when the call
// ends (a stop frame, a read error, or context cancellation).
func DefaultHandleStream(ctx context.Context, conn *websocket.Conn, start Frame) error {
	return handleStream(ctx, conn, start, time.Now(), func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
		return telephony.NewSession(ctx, callSID, opts...)
	})
}

// HandleStreamWithOpts is like DefaultHandleStream but accepts additional
// SessionOptions to be passed to the session factory.
func HandleStreamWithOpts(ctx context.Context, conn *websocket.Conn, start Frame, extraOpts ...telephony.SessionOption) error {
	return handleStream(ctx, conn, start, time.Now(), func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
		opts = append(opts, extraOpts...)
		return telephony.NewSession(ctx, callSID, opts...)
	})
}

// handleStream is the unexported implementation behind DefaultHandleStream.
// newSession is a factory seam: production uses telephony.NewSession (via
// DefaultHandleStream's wrapper above), tests inject a factory that adds
// telephony.WithVADFactory and telephony.WithTurnSink on top of the
// data/control SessionOptions handleStream itself supplies, so a session's
// VAD/turn-taking behavior is deterministic and observable in tests while
// its inbound wiring always goes through a real Demux (SOP-120).
//
// startedAt is this stream's wall-clock start, read once by the exported
// wrappers above and passed down rather than sampled here -- see NewTap for
// why the tap is handed the instant instead of taking it.
func handleStream(ctx context.Context, conn *websocket.Conn, start Frame, startedAt time.Time, newSession func(context.Context, string, ...telephony.SessionOption) *telephony.Session) error {
	demux := NewDemux()

	dataIn := telephony.NewBufferedChan[[]byte](telephony.ComputeDepth(telephony.DataPlaneBufferMS, telephony.MuLawFrameMS))
	controlIn := telephony.NewBufferedChan[telephony.ControlEvent](controlPlaneDepth)

	// The tap and the decision recorder both write files into the capture dir
	// but neither creates it, so make it once here (before either is wired) --
	// a fresh or just-cleaned build/audio would otherwise silently ENOENT every
	// capture write, since both are best-effort. Empty dir = capture off: skip.
	tapDir := tapDirFromEnv()
	if tapDir != "" {
		if err := os.MkdirAll(tapDir, 0o755); err != nil {
			log.Printf("twilio: handleStream: create capture dir %s: %v", tapDir, err)
		}
	}

	// tap is nil unless AATOOLKIT_AUDIO_TAP names a directory, and a nil *Tap is
	// a no-op, so the disabled path costs nothing and needs no branch below.
	// Built before the session so its reference can be threaded into both
	// pumpDataPlane (inbound) and dataPlaneOutput.Send (outbound) below --
	// the one place holding the same payload the session is about to
	// receive/send, so what gets recorded and what the session heard/said
	// cannot diverge (SOP-152 Observable behavior 2 -- "byte-identical to
	// the payloads delivered to the session").
	tap := NewTap(tapDir, start.StreamSID, start.CallSID, tapLabelFromEnv(), startedAt)

	dataOut := NewDataPlaneOutput(conn, start.StreamSID, tap)
	responseIn := telephony.NewBufferedChan[telephony.ResponseEvent](responseInputDepth)
	opts := []telephony.SessionOption{
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioControlInput(controlIn),
		telephony.WithTwilioDataOutput(dataOut),
		telephony.WithTwilioControlOutput(NewControlPlaneOutput(conn, start.StreamSID)),
		telephony.WithResponseInput(responseIn),
		telephony.WithCloseFunc(func() {
			// CloseNow, not Close: Close performs a close handshake and
			// blocks up to ~10s waiting for the peer's close frame ack,
			// which the termination flow (already past its own bounded
			// mark-echo wait) has no reason to wait out again.
			_ = conn.CloseNow()
		}),
	}
	// Decision-event recording rides the same directory and label as the audio
	// tap, gated by AATOOLKIT_EVENT_LOG; the option is a no-op when recording is
	// off or no tap dir is set. The session flushes the recorder on Close.
	opts = append(opts, telephony.WithFileDecisionRecorderFromEnv(
		tapDir, start.StreamSID, start.CallSID, tapLabelFromEnv(), telephony.DefaultVADConfig(), os.Stderr))
	// Conversation transcript summary (SOP-168): printed to stderr at end of
	// call, and written to <streamSID>.transcript.txt when a tap dir is set. The
	// agent role label is consumer-injected via AATOOLKIT_AGENT_LABEL.
	opts = append(opts,
		telephony.WithTranscriptOutput(tapDir, start.StreamSID, os.Stderr),
		telephony.WithTranscriptAgentLabel(agentLabelFromEnv()))

	sess := newSession(ctx, start.CallSID, opts...)
	if err := sess.Start(); err != nil {
		log.Printf("twilio: handleStream: session start: %v", err)
		return err
	}

	// Register with ReplyRouter if one is available in the context.
	replyRouter, ok := ctx.Value(replyRouterKey).(*telephony.ReplyRouter)
	var replySink *telephony.ReplySink
	if ok && replyRouter != nil {
		replySink = replyRouter.Register(start.CallSID, responseIn)
	}

	// The data and control pumps terminate differently at teardown.
	//
	// The DATA pump's graceful termination is structural, not context-driven:
	// closing the data plane (demux.CloseData below) makes its Recv drain every
	// buffered frame and then return errPlaneClosed, so pumpWG.Wait() implies a
	// fully drained plane with no goroutine left to leak. It therefore takes the
	// outer ctx directly — the one context graceful teardown does NOT cancel,
	// leaving ctx cancellation as purely the hard-abort escape that unblocks a
	// pump wedged on a torn-down downstream. This is the AATK-15 fix: under the
	// old cancel-then-join, a frame still buffered at cancel time was abandoned
	// in a 50/50 select between a ready <-ch and a ready <-ctx.Done(), losing it
	// from both the tap and the session. See design/teardown-protocol.md.
	//
	// The CONTROL pump keeps context-cancel termination (its own cancellable
	// ctrlCtx): control frames carry no completeness requirement (the stop that
	// triggered teardown is already consumed), so abort semantics are correct
	// for it, and there is no control-plane close to drive it structurally.
	ctrlCtx, cancelCtrl := context.WithCancel(ctx)
	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() { defer pumpWG.Done(); pumpDataPlane(ctx, demux.Data, dataIn, tap) }()
	go func() { defer pumpWG.Done(); pumpControlPlane(ctrlCtx, demux.Control, controlIn) }()
	defer func() {
		// Stop input structurally, on every exit path (stop frame, read error,
		// outer-context cancel): close the data plane so the data pump drains to
		// errPlaneClosed, then cancel the control pump. After pumpWG.Wait() the
		// data plane is fully drained and no in-flight frame can still be
		// writing, so it is safe to close the tap and session.
		demux.CloseData()
		cancelCtrl()
		pumpWG.Wait()
		tap.Close()
		sess.Close()
	}()
	// Deferred after (so it runs before, LIFO) the teardown above: a
	// registered session must stop being routable the instant teardown
	// begins, not after the data plane has drained and the session/tap have
	// closed. Deferred the other way around, a Route call arriving during
	// that drain/close window would still find the session "live" and
	// attempt a real send into a connection already being torn down, instead
	// of the clean unknown-session path Route takes once a session is gone.
	//
	// Close, not Deregister(start.CallSID): Close only removes replySink if
	// it is still the sink registered under start.CallSID, so a duplicate
	// registration racing on the same CallSID can't have its still-live
	// session torn out from under it by this one's teardown.
	if replySink != nil {
		defer replySink.Close()
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return nil
		}

		f, err := DecodeFrame(raw)
		if err != nil {
			log.Printf("twilio: handleStream: %v", err)
			continue
		}

		// demux.RouteFrame itself decides data vs. control plane by event
		// type (EventMedia -> data, everything else -> control), so every
		// decoded frame is routed here — mark/clear must reach the control
		// plane for the session's AwaitingMarkEcho/clear handling to ever
		// fire on a real call, not just in tests that inject ControlEvents
		// directly.
		if err := demux.RouteFrame(ctx, f); err != nil {
			log.Printf("twilio: handleStream: route %s frame: %v", f.Event, err)
		}
		if f.Event == EventStop {
			waitForClosed(sess, stopDrainTimeout)
			return nil
		}
	}
}

// waitForClosed waits for the session to reach StateClosed or timeout elapses,
// giving an already-enqueued "stop" ControlEvent a bounded window to reach the
// transition table before the caller proceeds to Close().
func waitForClosed(sess *telephony.Session, timeout time.Duration) {
	select {
	case <-sess.Closed():
	case <-time.After(timeout):
	}
}

// pumpDataPlane relays decoded media frames from the demux's data plane into
// the session's TwilioDataPlaneInput, translating Frame to its raw mu-law
// payload. Its graceful terminator is errPlaneClosed: once handleStream closes
// the data plane, Recv drains every buffered frame (each written to the tap
// here) and then returns errPlaneClosed, so pumpWG.Wait() implies a fully
// drained plane. A ctx-cancelled Recv is the hard-abort escape. Both surface as
// a non-nil err and end the pump (AATK-15).
//
// tap may be nil (capture disabled), which is a no-op. It is written before
// Send: the recording is of what arrived on the wire, and a Send failure that
// ends this pump must not silently make the last frame vanish from the
// recording as well.
func pumpDataPlane(ctx context.Context, data TwilioDataPlaneInput, dataIn telephony.TwilioDataPlaneInput, tap *Tap) {
	for {
		f, err := data.Recv(ctx)
		if err != nil {
			return
		}
		tap.WriteIn(f.Payload)
		tap.DrainOut()
		if err := dataIn.Send(ctx, f.Payload); err != nil {
			return
		}
	}
}

// pumpControlPlane relays decoded control frames (stop/mark/clear) from the
// demux's control plane into the session's TwilioControlPlaneInput,
// translating Frame to the transport-agnostic ControlEvent. It exits when
// ctx is cancelled or a Recv fails.
func pumpControlPlane(ctx context.Context, control TwilioControlPlaneInput, controlIn telephony.TwilioControlPlaneInput) {
	for {
		f, err := control.Recv(ctx)
		if err != nil {
			return
		}
		ev := telephony.ControlEvent{
			Kind:     string(f.Event),
			MarkName: f.MarkName,
			CallSID:  f.CallSID,
		}
		if err := controlIn.Send(ctx, ev); err != nil {
			return
		}
	}
}
