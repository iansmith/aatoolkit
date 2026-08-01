package twilio

import (
	"context"
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
// TURN-TAKING CONSEQUENCE, and the reason this defaults off: on the realtime
// path the *backend's* VAD and turn logic govern the conversation, so
// telephony/decision.go — this engine's own turn-taking decision function — is
// bypassed entirely for those turns, along with the VAD tuning that feeds it.
// That is a real behavioral change, not a transport swap. The replay corpus was
// captured against the current decision function, so its labels do NOT transfer
// to calls run this way and cannot be used to judge them. Which turn-taking is
// better is a question for measurement, in its own ticket; this switch exists so
// both stacks can be run and compared without a redeploy.
func NewStreamHandler(realtimeURL string) StreamHandler {
	if realtimeURL == "" {
		return DefaultHandleStream
	}
	return func(ctx context.Context, conn *websocket.Conn, start Frame) error {
		return handleStreamRealtime(ctx, conn, start, realtimeURL)
	}
}

// handleStreamRealtime drives one call over the realtime voice backend. Carrier
// audio crosses to the backend as the base64 string it arrived as, and the
// backend's audio comes back the same way — no transcoding in either direction.
//
// The call ends when either side does: the carrier hangs up (a stop frame or a
// read error) or the backend goes away. A backend that fails to dial, or drops
// mid-call, ends the call with a logged error rather than leaving the session
// hung.
func handleStreamRealtime(ctx context.Context, conn *websocket.Conn, start Frame, url string) error {
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

	// Whichever side ends first ends the call.
	select {
	case err := <-backendDone:
		log.Printf("twilio: realtime: backend ended the call: %v", err)
		return err
	case err := <-carrierDone:
		return err
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
