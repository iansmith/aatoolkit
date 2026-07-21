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

	opts := []telephony.SessionOption{
		telephony.WithTwilioDataInput(dataIn),
		telephony.WithTwilioControlInput(controlIn),
		telephony.WithTwilioDataOutput(NewDataPlaneOutput(conn, start.StreamSID, tap)),
		telephony.WithTwilioControlOutput(NewControlPlaneOutput(conn, start.StreamSID)),
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

	// pumpCtx (not the outer ctx) bounds the pump goroutines' lifetime to
	// this call: cancelling it and joining pumpWG before Close() guarantees
	// they exit here rather than leaking until the outer ctx (e.g. an HTTP
	// request context with its own, looser lifetime) eventually cancels.
	pumpCtx, cancelPumps := context.WithCancel(ctx)
	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() { defer pumpWG.Done(); pumpDataPlane(pumpCtx, demux.Data, dataIn, tap) }()
	go func() { defer pumpWG.Done(); pumpControlPlane(pumpCtx, demux.Control, controlIn) }()
	defer func() {
		cancelPumps()
		pumpWG.Wait()
		// After pumpWG.Wait(), so no in-flight frame can still be writing.
		tap.Close()
		sess.Close()
	}()

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
// payload. It exits when ctx is cancelled or a Recv fails.
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
