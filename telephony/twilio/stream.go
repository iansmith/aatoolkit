package twilio

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

// EventType is the Twilio Media Streams WebSocket event type.
type EventType string

const (
	EventStart     EventType = "start"
	EventMedia     EventType = "media"
	EventStop      EventType = "stop"
	EventMark      EventType = "mark"
	EventClear     EventType = "clear"
	EventConnected EventType = "connected"
)

// Frame is a decoded Twilio Media Streams WebSocket message.
type Frame struct {
	Event     EventType
	StreamSID string
	Payload   []byte // decoded μ-law audio; non-nil only for EventMedia
	Chunk     int    // monotonic frame sequence number; only set for EventMedia
	Timestamp string // ms offset from stream start; only set for EventMedia
	MarkName  string // non-empty only for EventMark
	CallSID   string // non-empty only for EventStart
	From      string // caller From threaded in by ServeStreams from the voice webhook; "" if unknown, only set for EventStart
}

// inbound is the union of all Twilio Media Streams message shapes. Only the
// fields relevant to a given event's type are populated after unmarshaling.
type inbound struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
	Media     struct {
		Payload string `json:"payload"`
		// Chunk is a raw message rather than int because Twilio's wire format
		// is inconsistent about whether it's a quoted string or a bare
		// number — see parseChunk.
		Chunk     json.RawMessage `json:"chunk"`
		Timestamp string          `json:"timestamp"`
	} `json:"media"`
	Mark struct {
		Name string `json:"name"`
	} `json:"mark"`
	Start struct {
		CallSID string `json:"callSid"`
	} `json:"start"`
}

// parseChunk decodes a Twilio media.chunk value, which is observed on the
// wire as either a bare JSON number or a quoted numeric string.
func parseChunk(raw json.RawMessage) (int, error) {
	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return asInt, nil
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return strconv.Atoi(asStr)
	}
	return 0, fmt.Errorf("chunk must be a number or numeric string, got %s", raw)
}

func DecodeFrame(raw []byte) (Frame, error) {
	var in inbound
	if err := json.Unmarshal(raw, &in); err != nil {
		return Frame{}, fmt.Errorf("twilio: decode frame: %w", err)
	}

	f := Frame{
		Event:     EventType(in.Event),
		StreamSID: in.StreamSID,
	}

	switch f.Event {
	case EventMedia:
		payload, err := base64.StdEncoding.DecodeString(in.Media.Payload)
		if err != nil {
			return Frame{}, fmt.Errorf("twilio: decode media payload: %w", err)
		}
		f.Payload = payload

		if len(in.Media.Chunk) > 0 {
			chunk, err := parseChunk(in.Media.Chunk)
			if err != nil {
				return Frame{}, fmt.Errorf("twilio: decode media chunk: %w", err)
			}
			f.Chunk = chunk
		}
		f.Timestamp = in.Media.Timestamp

	case EventMark:
		f.MarkName = in.Mark.Name

	case EventStart:
		f.CallSID = in.Start.CallSID

	case EventStop, EventClear, EventConnected:
		// no extra fields

	default:
		return Frame{}, fmt.Errorf("twilio: unknown event type %q", in.Event)
	}

	return f, nil
}

type outboundBase struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
}

func EncodeMedia(streamSID string, payload []byte) ([]byte, error) {
	msg := struct {
		outboundBase
		Media struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}{
		outboundBase: outboundBase{Event: string(EventMedia), StreamSID: streamSID},
		Media: struct {
			Payload string `json:"payload"`
		}{Payload: base64.StdEncoding.EncodeToString(payload)},
	}
	return json.Marshal(msg)
}

// EncodeMediaWithMetadata encodes an outgoing media frame with monotonic
// chunk numbering and its ms-offset timestamp from stream start. chunk is
// 1-based; timestamp is the frame's start offset, so chunk 1 → "0", chunk 2
// → "20", etc. (frames are muLawFrame20ms wide). seqNum is the per-message
// sequence number for this Media Streams WebSocket session. Per the Twilio
// spec, sequenceNumber, chunk, and timestamp are all emitted as JSON strings.
func EncodeMediaWithMetadata(streamSID string, payload []byte, chunk, seqNum int) ([]byte, error) {
	msg := struct {
		outboundBase
		SequenceNumber string `json:"sequenceNumber"`
		Media          struct {
			Track     string `json:"track"`
			Payload   string `json:"payload"`
			Chunk     string `json:"chunk"`
			Timestamp string `json:"timestamp"`
		} `json:"media"`
	}{
		outboundBase:   outboundBase{Event: string(EventMedia), StreamSID: streamSID},
		SequenceNumber: strconv.Itoa(seqNum),
		Media: struct {
			Track     string `json:"track"`
			Payload   string `json:"payload"`
			Chunk     string `json:"chunk"`
			Timestamp string `json:"timestamp"`
		}{
			Track:     "inbound",
			Payload:   base64.StdEncoding.EncodeToString(payload),
			Chunk:     strconv.Itoa(chunk),
			Timestamp: strconv.Itoa((chunk - 1) * 20),
		},
	}
	return json.Marshal(msg)
}

func EncodeMark(streamSID, name string) ([]byte, error) {
	msg := struct {
		outboundBase
		Mark struct {
			Name string `json:"name"`
		} `json:"mark"`
	}{
		outboundBase: outboundBase{Event: string(EventMark), StreamSID: streamSID},
		Mark: struct {
			Name string `json:"name"`
		}{Name: name},
	}
	return json.Marshal(msg)
}

// EncodeClear encodes a Twilio clear event (flushes Twilio's audio buffer).
func EncodeClear(streamSID string) ([]byte, error) {
	return json.Marshal(outboundBase{Event: string(EventClear), StreamSID: streamSID})
}

// EncodeStop encodes the Twilio stop event that a client sends at the end of
// a Media Streams WebSocket session. seqNum is the per-message sequence
// number for this Media Streams WebSocket session, emitted as a JSON string.
func EncodeStop(streamSID, callSID, accountSID string, seqNum int) ([]byte, error) {
	type stopPayload struct {
		AccountSID string `json:"accountSid"`
		CallSID    string `json:"callSid"`
	}
	msg := struct {
		outboundBase
		SequenceNumber string      `json:"sequenceNumber"`
		Stop           stopPayload `json:"stop"`
	}{
		outboundBase:   outboundBase{Event: string(EventStop), StreamSID: streamSID},
		SequenceNumber: strconv.Itoa(seqNum),
		Stop:           stopPayload{AccountSID: accountSID, CallSID: callSID},
	}
	return json.Marshal(msg)
}

// mediaFormat describes the fixed audio encoding the engine's inbound Media
// Streams sessions use — mirrored verbatim into every EncodeStart frame.
type mediaFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

// EncodeStart encodes the Twilio start event that a client sends at the
// beginning of a Media Streams WebSocket session. seqNum is the per-message
// sequence number for this session, emitted as a JSON string.
func EncodeStart(streamSID, callSID, accountSID string, seqNum int) ([]byte, error) {
	type startPayload struct {
		StreamSID        string            `json:"streamSid"`
		AccountSID       string            `json:"accountSid"`
		CallSID          string            `json:"callSid"`
		Tracks           []string          `json:"tracks"`
		CustomParameters map[string]string `json:"customParameters"`
		MediaFormat      mediaFormat       `json:"mediaFormat"`
	}
	msg := struct {
		outboundBase
		SequenceNumber string       `json:"sequenceNumber"`
		Start          startPayload `json:"start"`
	}{
		outboundBase:   outboundBase{Event: string(EventStart), StreamSID: streamSID},
		SequenceNumber: strconv.Itoa(seqNum),
		Start: startPayload{
			StreamSID:        streamSID,
			AccountSID:       accountSID,
			CallSID:          callSID,
			Tracks:           []string{"inbound"},
			CustomParameters: map[string]string{},
			MediaFormat: mediaFormat{
				Encoding:   "audio/x-mulaw",
				SampleRate: 8000,
				Channels:   1,
			},
		},
	}
	return json.Marshal(msg)
}

// EncodeConnected encodes a Twilio connected event (sent by client to acknowledge connection).
func EncodeConnected() ([]byte, error) {
	msg := struct {
		Event    string `json:"event"`
		Protocol string `json:"protocol"`
		Version  string `json:"version"`
	}{
		Event:    "connected",
		Protocol: "Call",
		Version:  "1.0.0",
	}
	return json.Marshal(msg)
}

// EncodeDTMF is a panic-stub for DTMF encoding (not yet implemented).
func EncodeDTMF(streamSID, digit string) ([]byte, error) {
	panic("twilio: dtmf encoding not implemented yet")
}
