package telephony

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// eventLogEnv is the binary on/off flag that switches decision-event recording
// on. It is a flag, not a path: events are written into the audio-tap directory
// (AATOOLKIT_AUDIO_TAP) so they sit beside the audio they describe.
const eventLogEnv = "AATOOLKIT_EVENT_LOG"

// EventLogEnabled reports whether decision-event recording is switched on via
// AATOOLKIT_EVENT_LOG. Truthy values: 1/true/yes/on (case-insensitive). One
// definition shared by every wiring site (twilio live path, probeset replay).
func EventLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(eventLogEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// DecisionRecorder receives one structured DecisionEvent per parameterized
// choice the voice-input path makes (M1: end-of-utterance only). It exists so
// the parameters we set for dealing with voice input can be evaluated after
// the fact: each event ties a parameter and its value to a position in the
// input audio and the effect the choice had.
//
// Implementations must return promptly from Record: it is called synchronously
// from the session's single sequencer loop and must not block it (same
// contract as TurnSink). Close flushes any buffered state and is idempotent.
type DecisionRecorder interface {
	Record(ev DecisionEvent)
	Close() error
}

// Decision event types and the labels M1 emits. Kept as named constants so the
// producer (state.go) and any consumer read the same strings.
const (
	DecisionTypeVAD = "vad"

	DecisionKindSpeechStart = "speech-start"
	DecisionKindSilence     = "silence"
	DecisionKindEndOfUtter  = "end-of-utterance"
	DecisionKindTurnEnd     = "turn-end"

	DecisionParamSpeechThresh   = "SpeechThresh"
	DecisionParamSilenceThresh  = "SilenceThresh"
	DecisionParamEndSilence     = "EndSilenceMS"
	DecisionParamTurnEndSilence = "TurnEndSilenceMS"
)

// DecisionEvent is one recorded decision. The shape is deliberately flat and
// JSON-friendly (one object per JSONL line); fields not relevant to a given
// event type are omitted. M1 populates only the VAD end-of-utterance shape;
// later milestones add further event types (STT, caps) and any fields they need.
//
// AudioMS is a position in the INPUT audio (derived from the monotonic
// VAD-window clock), never wall-clock, so the record is stable under replay.
type DecisionEvent struct {
	Seq          int     `json:"seq"`
	AudioMS      int     `json:"audio_ms"`
	Type         string  `json:"type"`
	Kind         string  `json:"kind,omitempty"`
	Param        string  `json:"param,omitempty"`
	ParamValue   any     `json:"param_value,omitempty"`
	Prob         float32 `json:"prob,omitempty"`
	SilenceCount int     `json:"silence_count,omitempty"`
	RequestID    int     `json:"request_id,omitempty"`
	Effect       string  `json:"effect,omitempty"`
}

// noopRecorder is the default when no recorder is wired: it drops every event
// and never touches the filesystem, so an unconfigured session behaves exactly
// as before. A session always holds a non-nil recorder (NewSession defaults to
// this), so call sites never nil-check.
type noopRecorder struct{}

func (noopRecorder) Record(DecisionEvent) {}
func (noopRecorder) Close() error         { return nil }

// decisionHeader is written once per recording (its own file) so the JSONL
// stays homogeneous. It anchors the events to the audio and records the VAD
// config that produced them -- the config is a moving target and cannot be
// recovered after the fact, the same reason the audio tap's sidecar keeps it.
type decisionHeader struct {
	StreamSID string    `json:"stream_sid"`
	CallSID   string    `json:"call_sid"`
	Label     string    `json:"label,omitempty"`
	VADConfig VADConfig `json:"vad_config"`
}

// FileDecisionRecorder buffers events in memory, feeds each one live to an
// io.Writer as it arrives, and on Close flushes the buffer to
// <streamSID>.events.jsonl plus a <streamSID>.events.header.json in dir. It
// mirrors the audio tap's lifecycle (per-stream, buffer, write-on-close) and
// is written to the same directory so events sit beside the audio they
// describe.
type FileDecisionRecorder struct {
	dir    string
	header decisionHeader
	live   io.Writer

	mu     sync.Mutex
	seq    int
	buf    []DecisionEvent
	closed bool
}

// NewFileDecisionRecorder builds a recorder that writes into dir. A nil return
// (dir == "") lets the caller decide, per the tap convention, to wire nothing
// rather than a disabled object. live is where each event is echoed as it
// arrives; nil defaults to os.Stderr.
func NewFileDecisionRecorder(dir, streamSID, callSID, label string, cfg VADConfig, live io.Writer) *FileDecisionRecorder {
	if dir == "" {
		return nil
	}
	if live == nil {
		live = os.Stderr
	}
	return &FileDecisionRecorder{
		dir:  dir,
		live: live,
		header: decisionHeader{
			StreamSID: streamSID,
			CallSID:   callSID,
			Label:     label,
			VADConfig: cfg,
		},
	}
}

func (r *FileDecisionRecorder) Record(ev DecisionEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.seq++
	ev.Seq = r.seq
	r.buf = append(r.buf, ev)
	fmt.Fprint(r.live, formatDecisionLine(ev))
}

// Close writes the header and the buffered events, then marks the recorder
// closed so a second Close (or a late Record) is a no-op. Both files are keyed
// by streamSID to line up with the tap's <streamSID>.in.ulaw / .json naming.
func (r *FileDecisionRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true

	sid := r.header.StreamSID
	hdr, err := json.Marshal(r.header)
	if err != nil {
		return fmt.Errorf("decision recorder: marshal header: %w", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, sid+".events.header.json"), hdr, 0o644); err != nil {
		return fmt.Errorf("decision recorder: write header: %w", err)
	}

	// json.Encoder writes one value per line (it appends a newline after each),
	// which is exactly the JSONL shape -- the same idiom cmd/probeset uses.
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, ev := range r.buf {
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("decision recorder: marshal event %d: %w", ev.Seq, err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.dir, sid+".events.jsonl"), body.Bytes(), 0o644); err != nil {
		return fmt.Errorf("decision recorder: write events: %w", err)
	}
	return nil
}

// WithFileDecisionRecorderFromEnv returns a SessionOption that wires a
// FileDecisionRecorder writing into dir when AATOOLKIT_EVENT_LOG is on and dir
// is non-empty; otherwise it is a no-op option. It folds the enable-flag gate
// and the nil-recorder check that the live (twilio) and replay (probeset)
// wiring sites would otherwise each repeat.
func WithFileDecisionRecorderFromEnv(dir, streamSID, callSID, label string, cfg VADConfig, live io.Writer) SessionOption {
	if !EventLogEnabled() {
		return func(*Session) {}
	}
	rec := NewFileDecisionRecorder(dir, streamSID, callSID, label, cfg, live)
	if rec == nil {
		return func(*Session) {}
	}
	return WithDecisionRecorder(rec)
}

// formatDecisionLine renders one event as a single human-readable line for the
// live terminal feed. M1 produces only VAD end-of-utterance events; a later
// milestone that adds event types revisits this alongside them.
func formatDecisionLine(ev DecisionEvent) string {
	return fmt.Sprintf("[%8.3fs] %-16s %s=%v  prob=%.2f silence=%dw  -> %s\n",
		float64(ev.AudioMS)/1000.0, ev.Kind, ev.Param, ev.ParamValue, ev.Prob, ev.SilenceCount, ev.Effect)
}
