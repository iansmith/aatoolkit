package twilio

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// realtimeDialTimeout bounds the backend handshake. A backend that neither
// accepts nor refuses would otherwise hold the call open with silence on the
// line for as long as the carrier tolerates it.
const realtimeDialTimeout = 10 * time.Second

// NewStreamHandler selects the transport for a call.
//
// realtimeURL == "" is the default and returns DefaultHandleStream unchanged:
// the existing VAD/STT/TTS sidecar path, with no realtime client constructed
// and no dial attempted. Non-empty routes the call to an external realtime
// voice backend instead.
//
// The realtime path changes turn-taking, which is the reason this defaults off:
// see HandleStreamRealtime for what is bypassed and why the replay corpus does
// not transfer. Which turn-taking is better is a question for measurement, in
// its own ticket; this switch exists so both stacks can be run and compared
// without a redeploy.
func NewStreamHandler(realtimeURL string) StreamHandler {
	if realtimeURL == "" {
		return DefaultHandleStream
	}
	return func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return HandleStreamRealtime(ctx, conn, start, realtimeURL, 0)
	}
}

// HandleStreamRealtime drives one call over the realtime voice backend. It is
// the realtime peer of HandleStreamWithOpts: same entry shape, called with the
// (ctx, conn, start) a StreamHandler receives, so a consumer that owns its own
// handler body — because its per-call setup and teardown are bound to that
// scope — can select this transport by branching on its own configuration
// rather than going through NewStreamHandler.
//
// Carrier audio crosses to the backend as the base64 string it arrived as, and
// the backend's audio comes back the same way — no transcoding in either
// direction.
//
// TURN-TAKING CONSEQUENCE, stated here rather than at NewStreamHandler because
// a consumer calling this directly never reads that doc: on this path the
// *backend's* VAD and turn logic govern the conversation, so
// telephony/decision.go — this engine's own turn-taking decision function — is
// bypassed entirely for those turns, along with the VAD tuning that feeds it.
// That is a real behavioral change, not a transport swap, and the replay corpus
// was captured against the current decision function, so its labels do NOT
// transfer to calls run this way and cannot be used to judge them.
//
// The call ends when either side does: the carrier hangs up (a stop frame or a
// read error) or the backend goes away. A backend that fails to dial, or drops
// mid-call, ends the call with a logged error rather than leaving the session
// hung.
//
// idleTimeout bounds how long the call may run with the backend producing no
// event. 0 disables it, matching today's behavior exactly: unbounded, no
// timer armed. A positive value is reset on every backend event bridge.Run
// observes, of any type; carrier frames do NOT reset it, so a carrier that is
// streaming continuously cannot mask a backend that has gone silent. If it
// elapses with no backend activity, the call ends with an error naming "idle
// timeout".
//
// A hung backend and a legitimately quiet one look identical on this path —
// there is no local VAD or turn state to tell them apart (see
// telephony.MaxSilenceMS for the order of magnitude the default transport
// uses to make that call). Set idleTimeout longer than the longest
// backend-quiet interval the deployment tolerates, or a normal pause in
// conversation will end the call.
func HandleStreamRealtime(ctx context.Context, conn *websocket.Conn, start Frame, url string, idleTimeout time.Duration) error {
	// CloseNow on every exit path, not Close: there is no local session to
	// drain, and closing the carrier is also what unblocks the carrier pump
	// below if it is still parked in Read, so no goroutine outlives this
	// function however the call ends.
	defer func() { _ = conn.CloseNow() }()

	dialCtx, cancelDial := context.WithTimeout(ctx, realtimeDialTimeout)
	defer cancelDial()

	client, err := realtime.Dial(dialCtx, url)
	if err != nil {
		log.Printf("twilio: realtime: dial: %v", err)
		return err
	}
	defer client.Close()

	bridge := realtime.NewBridge(client, &carrierMediaSink{conn: conn, streamSID: start.StreamSID})

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	backendDone := make(chan error, 1)
	go func() { backendDone <- bridge.Run(runCtx) }()

	// Drain transcripts so a full channel can never wedge the read loop. They
	// are not consumed on this path: the backend owns the conversation, so
	// there is no local turn machinery to feed. The channel closes when Run
	// returns, which ends this goroutine.
	go func() {
		for range bridge.Transcripts() {
		}
	}()

	carrierDone := make(chan error, 1)
	go func() { carrierDone <- pumpCarrierToBridge(ctx, conn, bridge) }()

	idle := newIdleGuard(idleTimeout)
	defer idle.stop()

	// One goroutine, one select: bridge.Activity() resets the guard without any
	// Reset call crossing a goroutine boundary, which is what keeps this
	// race-free under -race. Only backendDone, carrierDone, or the guard firing
	// ends the loop; an activity signal just re-arms and loops again.
	for {
		select {
		case err := <-backendDone:
			log.Printf("twilio: realtime: backend ended the call: %v", err)
			return err
		case err := <-carrierDone:
			return err
		case <-idle.fired():
			return fmt.Errorf("twilio: realtime: idle timeout: no backend activity for %s", idleTimeout)
		case <-bridge.Activity():
			idle.reset()
		}
	}
}

// idleGuard is the idle timeout as a single object, so HandleStreamRealtime's
// loop reads as four things that can end or extend a call rather than as timer
// bookkeeping. A zero duration produces a guard that never fires: fired()
// returns a nil channel, which blocks forever in a select, so the loop reduces
// to the original two-case shape — unbounded, exactly today's behavior.
//
// Not safe for concurrent use, and deliberately so: every method is called from
// the one goroutine that owns the select above.
type idleGuard struct {
	timer *time.Timer
	after time.Duration
}

func newIdleGuard(after time.Duration) *idleGuard {
	if after <= 0 {
		return &idleGuard{}
	}
	return &idleGuard{timer: time.NewTimer(after), after: after}
}

// fired is the channel the guard signals on, or nil when it is disarmed.
func (g *idleGuard) fired() <-chan time.Time {
	if g.timer == nil {
		return nil
	}
	return g.timer.C
}

// reset restarts the guard. Stop-then-drain before Reset is the required idiom:
// a timer that already fired has a value parked in its channel, and resetting
// without draining it would fire again immediately on stale time.
func (g *idleGuard) reset() {
	if g.timer == nil {
		return
	}
	if !g.timer.Stop() {
		select {
		case <-g.timer.C:
		default:
		}
	}
	g.timer.Reset(g.after)
}

func (g *idleGuard) stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
}

// pumpCarrierToBridge reads carrier frames and forwards media to the backend.
// It forwards Frame.EncodedPayload — the base64 exactly as it arrived — never
// re-encoding Frame.Payload, which would spend a decode and an encode per 20 ms
// frame reproducing bytes the carrier already sent.
func pumpCarrierToBridge(ctx context.Context, conn *websocket.Conn, bridge *realtime.Bridge) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			// The carrier went away: a hangup, not a fault.
			return nil
		}

		f, err := DecodeFrame(raw)
		if err != nil {
			log.Printf("twilio: realtime: %v", err)
			continue
		}

		switch f.Event {
		case EventMedia:
			if err := bridge.Forward(ctx, f.EncodedPayload); err != nil {
				log.Printf("twilio: realtime: forward to backend: %v", err)
				return err
			}
		case EventStop:
			return nil
		default:
			// mark/clear/connected carry no meaning on this path: the backend
			// owns playback and barge-in, so there is no local mark-echo or
			// clear handling for them to drive.
		}
	}
}

// carrierMediaSink is the carrier-facing side of the bridge: it writes the
// backend's audio and barge-in signals to the Twilio Media Streams WebSocket.
// Audio arrives base64 and is placed on the wire unchanged.
type carrierMediaSink struct {
	conn      wsWriter
	streamSID string
}

var _ realtime.MediaSink = (*carrierMediaSink)(nil)

func (s *carrierMediaSink) Media(ctx context.Context, payload string) error {
	msg, err := EncodeMediaB64(s.streamSID, payload)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, msg)
}

func (s *carrierMediaSink) Clear(ctx context.Context) error {
	msg, err := EncodeClear(s.streamSID)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, msg)
}
