package twilio

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

const tapDirEnv = "AATOOLKIT_AUDIO_TAP"

// defaultFrameBytes is one MuLawFrameMS frame's worth of mu-law audio (1
// byte/sample at SampleRateHz) -- the same derivation session.go's
// farewellFrameBytes uses. It is only the silence-fallback size before any
// real inbound frame has set the pace; once WriteIn has seen a frame,
// DrainOut's silence matches that frame's actual length instead.
const defaultFrameBytes = telephony.SampleRateHz * telephony.MuLawFrameMS / 1000

// maxOutQueueFrames bounds the outbound recording queue. sendClip enqueues
// an entire clip's frames in one synchronous burst -- exactly the
// burst-dumped, not-real-time-paced behavior this ticket exists to fix --
// so the queue must hold at least one full clip before DrainOut (paced by
// real inbound frame arrival) can catch up. This bound is deliberately NOT
// real-time flow control: it is a safety net against a genuine runaway (a
// bug causing endless WriteOut calls unrelated to any real clip), sized
// generously above every currently known clip (the longest is the ~5s
// llm-thinking bed) so drop-oldest never fires in normal operation. Once
// SOP-157 lands and wires in AATOOLKIT_SIM_TURN_MS -- an operator-configurable
// clip duration with no inherent cap -- this bound should be derived from
// that config instead of a fixed constant; until then, 5 minutes of frames
// is a generous, cheap floor (~2.4MB worst case at ~160 bytes/frame).
const maxOutQueueFrames = 5 * 60 * 1000 / telephony.MuLawFrameMS // same bufferMS/frameMS derivation as telephony.ComputeDepth

func tapDirFromEnv() string {
	return os.Getenv(tapDirEnv)
}

func tapLabelFromEnv() string {
	return os.Getenv("AATOOLKIT_TAP_LABEL")
}

type Tap struct {
	streamSID string
	callSID   string
	dir       string
	label     string
	startedAt time.Time

	mu   sync.Mutex
	w    io.WriteCloser
	wOut io.WriteCloser

	// outQueue is a fixed-size ring buffer: a lazily-allocated
	// maxOutQueueFrames backing array plus outHead/outCount tracking the
	// live window within it. Enqueue and drop-oldest both overwrite in
	// place -- no reslicing, no reallocation -- so both stay O(1) even in
	// the steady-state-dropping case the bound exists to handle.
	outQueue     [][]byte
	outHead      int
	outCount     int
	lastInFrameN int

	inOpenFailed  bool
	outOpenFailed bool
	closed        bool
	frames        int
	bytes         int
	outFrames     int
	outBytes      int
	outDrops      int
	logged        bool
	dropLogged    bool
}

type tapSidecar struct {
	StreamSID    string              `json:"stream_sid"`
	CallSID      string              `json:"call_sid"`
	Label        string              `json:"label,omitempty"`
	StartedAt    time.Time           `json:"started_at"`
	Frames       int                 `json:"frames"`
	Bytes        int                 `json:"bytes"`
	VADConfig    telephony.VADConfig `json:"vad_config"`
	Alignment    string              `json:"alignment,omitempty"`
	Channels     []string            `json:"channels,omitempty"`
	OutTruncated bool                `json:"out_truncated,omitempty"`

	// OutDroppedFrames is the count of outbound frames discarded because
	// the queue hit maxOutQueueFrames -- surfaced so a recording that lost
	// frames to overflow is distinguishable from a clean one downstream.
	OutDroppedFrames int `json:"out_dropped_frames,omitempty"`
}

func NewTap(dir, streamSID, callSID, label string, startedAt time.Time) *Tap {
	if dir == "" {
		return nil
	}
	return &Tap{dir: dir, streamSID: streamSID, callSID: callSID, label: label, startedAt: startedAt}
}

func newTapWithWriter(w io.WriteCloser, dir, streamSID, callSID, label string, startedAt time.Time) *Tap {
	t := NewTap(dir, streamSID, callSID, label, startedAt)
	if t == nil {
		return nil
	}
	t.w = w
	return t
}

func newTapWithOutWriter(wOut io.WriteCloser, dir, streamSID, callSID, label string, startedAt time.Time) *Tap {
	t := NewTap(dir, streamSID, callSID, label, startedAt)
	if t == nil {
		return nil
	}
	t.wOut = wOut
	return t
}

func (t *Tap) inulawPath() string  { return filepath.Join(t.dir, t.streamSID+".in.ulaw") }
func (t *Tap) outulawPath() string { return filepath.Join(t.dir, t.streamSID+".out.ulaw") }
func (t *Tap) sidecarPath() string { return filepath.Join(t.dir, t.streamSID+".json") }

func (t *Tap) logOnce(err error) {
	if t.logged {
		return
	}
	t.logged = true
	log.Printf("twilio: tap %s: %v (capture for this stream is best-effort; further errors suppressed)", t.streamSID, err)
}

// logDropOnce reports the outbound queue hitting its bound just once per
// stream, for the same reason logOnce does: at 50 frames/sec, a stalled
// drain would otherwise flood the log on every subsequent WriteOut.
func (t *Tap) logDropOnce() {
	if t.dropLogged {
		return
	}
	t.dropLogged = true
	log.Printf("twilio: tap %s: outbound queue hit its %d-frame bound, dropping oldest frames (capture for this stream is best-effort; further drops suppressed)", t.streamSID, maxOutQueueFrames)
}

func (t *Tap) WriteIn(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.inOpenFailed {
		return
	}

	if t.w == nil {
		f, err := os.OpenFile(t.inulawPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.inOpenFailed = true
			t.logOnce(err)
			return
		}
		t.w = f
	}

	n, err := t.w.Write(payload)
	if err != nil {
		t.logOnce(err)
	}
	if n > 0 {
		t.frames++
		t.bytes += n
		t.lastInFrameN = len(payload)
	}
}

func (t *Tap) WriteOut(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}

	if t.outQueue == nil {
		t.outQueue = make([][]byte, maxOutQueueFrames)
	}

	if t.outCount == maxOutQueueFrames {
		// Full: overwrite the oldest slot in place and advance head past it
		// -- drop-oldest without ever touching len/cap of the backing array.
		t.outQueue[t.outHead] = payload
		t.outHead = (t.outHead + 1) % maxOutQueueFrames
		t.outDrops++
		t.logDropOnce()
		return
	}

	idx := (t.outHead + t.outCount) % maxOutQueueFrames
	t.outQueue[idx] = payload
	t.outCount++
}

func (t *Tap) DrainOut() {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.outOpenFailed {
		return
	}

	if t.wOut == nil {
		f, err := os.OpenFile(t.outulawPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.outOpenFailed = true
			t.logOnce(err)
			return
		}
		t.wOut = f
	}

	var frame []byte
	if t.outCount > 0 {
		frame = t.outQueue[t.outHead]
		t.outQueue[t.outHead] = nil // don't retain a reference past dequeue
		t.outHead = (t.outHead + 1) % maxOutQueueFrames
		t.outCount--
	} else {
		silenceLen := t.lastInFrameN
		if silenceLen == 0 {
			silenceLen = defaultFrameBytes
		}
		frame = make([]byte, silenceLen)
		for i := range frame {
			frame[i] = 0xFF
		}
	}

	n, err := t.wOut.Write(frame)
	if err != nil {
		t.logOnce(err)
	}
	if n > 0 {
		t.outFrames++
		t.outBytes += n
	}
}

func (t *Tap) Close() {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true

	if t.w != nil {
		if err := t.w.Close(); err != nil {
			t.logOnce(err)
		}
	}

	if t.wOut != nil {
		if err := t.wOut.Close(); err != nil {
			t.logOnce(err)
		}
	}

	if t.frames == 0 {
		if t.w != nil {
			if err := os.Remove(t.inulawPath()); err != nil && !os.IsNotExist(err) {
				t.logOnce(err)
			}
		}
		if t.wOut != nil {
			if err := os.Remove(t.outulawPath()); err != nil && !os.IsNotExist(err) {
				t.logOnce(err)
			}
		}
		return
	}

	raw, err := json.Marshal(tapSidecar{
		StreamSID:        t.streamSID,
		CallSID:          t.callSID,
		Label:            t.label,
		StartedAt:        t.startedAt,
		Frames:           t.frames,
		Bytes:            t.bytes,
		VADConfig:        telephony.DefaultVADConfig(),
		Alignment:        "inbound-frame-clock, 20ms/frame, silence=0xFF",
		Channels:         []string{"in", "out"},
		OutTruncated:     t.outCount > 0,
		OutDroppedFrames: t.outDrops,
	})
	if err != nil {
		t.logOnce(err)
		return
	}
	if err := os.WriteFile(t.sidecarPath(), raw, 0o644); err != nil {
		t.logOnce(err)
	}
}
