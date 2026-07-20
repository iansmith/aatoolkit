package twilio

import (
	"context"
	"fmt"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// TwilioDataPlaneOutput is the send side of the channel a Session writes
// outbound media frames to (SOP-125). Twilio-package-local alias, following
// the same cross-package-alias-with-different-T pattern as
// TwilioDataPlaneInput above (avoids an import cycle: this package already
// imports telephony, so telephony cannot import it back).
type TwilioDataPlaneOutput = telephony.ServiceOutput[[]byte]

// TwilioControlPlaneOutput is the send side of the channel a Session writes
// outbound control-plane messages to (mark/clear) (SOP-125).
type TwilioControlPlaneOutput = telephony.ServiceOutput[telephony.ControlOutMessage]

// wsWriter is the subset of *websocket.Conn that dataPlaneOutput/
// controlPlaneOutput need — narrowed to keep tests able to fake it.
type wsWriter interface {
	Write(ctx context.Context, typ websocket.MessageType, data []byte) error
}

// dataPlaneOutput is the concrete TwilioDataPlaneOutput: it encodes each
// outbound payload via EncodeMedia and writes it to the WebSocket. It is a
// send-only ServiceOutput — Channel/Recv are never used by a real Session
// (which only calls Send), but are implemented to satisfy the interface.
type dataPlaneOutput struct {
	conn      wsWriter
	streamSID string
	tap       *Tap
}

var _ TwilioDataPlaneOutput = (*dataPlaneOutput)(nil)

// NewDataPlaneOutput builds a TwilioDataPlaneOutput that writes outbound
// media frames for streamSID to conn. tap may be nil (capture disabled),
// which is a no-op on the WriteOut call in Send below.
func NewDataPlaneOutput(conn *websocket.Conn, streamSID string, tap *Tap) TwilioDataPlaneOutput {
	return &dataPlaneOutput{conn: conn, streamSID: streamSID, tap: tap}
}

func (o *dataPlaneOutput) Channel() <-chan []byte { return nil }

func (o *dataPlaneOutput) Recv(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (o *dataPlaneOutput) Send(ctx context.Context, payload []byte) error {
	msg, err := EncodeMedia(o.streamSID, payload)
	if err != nil {
		return err
	}
	writeErr := o.conn.Write(ctx, websocket.MessageText, msg)
	// Enqueued after the Twilio write regardless of its outcome: the tap
	// records what the engine sent, not just what successfully reached Twilio,
	// and a write failure here already ends the call by a separate path.
	o.tap.WriteOut(payload)
	return writeErr
}

// controlPlaneOutput is the concrete TwilioControlPlaneOutput: it encodes
// each outbound control message via EncodeMark/EncodeClear and writes it to
// the WebSocket. EncodeStop is deliberately never reachable here — per
// stream.go's doc, it's the Twilio stop event a client sends; the server
// ends a call by closing the WebSocket instead.
type controlPlaneOutput struct {
	conn      wsWriter
	streamSID string
}

var _ TwilioControlPlaneOutput = (*controlPlaneOutput)(nil)

// NewControlPlaneOutput builds a TwilioControlPlaneOutput that writes
// outbound control-plane messages for streamSID to conn.
func NewControlPlaneOutput(conn *websocket.Conn, streamSID string) TwilioControlPlaneOutput {
	return &controlPlaneOutput{conn: conn, streamSID: streamSID}
}

func (o *controlPlaneOutput) Channel() <-chan telephony.ControlOutMessage { return nil }

func (o *controlPlaneOutput) Recv(ctx context.Context) (telephony.ControlOutMessage, error) {
	<-ctx.Done()
	return telephony.ControlOutMessage{}, ctx.Err()
}

func (o *controlPlaneOutput) Send(ctx context.Context, msg telephony.ControlOutMessage) error {
	var raw []byte
	var err error
	switch msg.Kind {
	case telephony.ControlOutMark:
		raw, err = EncodeMark(o.streamSID, msg.MarkName)
	case telephony.ControlOutClear:
		raw, err = EncodeClear(o.streamSID)
	default:
		return fmt.Errorf("twilio: unknown control-out kind %q", msg.Kind)
	}
	if err != nil {
		return err
	}
	return o.conn.Write(ctx, websocket.MessageText, raw)
}
