package twilio

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
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
// generously above every currently known clip (farewell, forced-stop, and a
// generated response bounded by MaxResponseMS) so drop-oldest never fires in
// normal operation. 5 minutes of frames is a generous, cheap floor (~2.4MB
// worst case at ~160 bytes/frame).
const maxOutQueueFrames = 5 * 60 * 1000 / telephony.MuLawFrameMS // same bufferMS/frameMS derivation as telephony.ComputeDepth

// mulawDuration is how long n bytes of mu-law audio take to play out: one byte
// per sample at telephony.SampleRateHz. It is the pacing an outbound-only
// recording is written against, so a gap in the writes becomes silence of the
// same length rather than nothing at all.
//
// cmd/twilio-cli carries the same arithmetic as mulawPlayoutDuration for its
// playout side. Collapsing the two means an exported helper on the telephony
// package, which is a public-API change this ticket does not carry.
func mulawDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / telephony.SampleRateHz
}

// silenceChunkFrames is how many silence frames fillOutGap hands to one Write.
//
// A gap in a one-directional recording is legitimately call-length: the engine
// is simply quiet while the caller talks, and the whole point of the fill is
// that those minutes appear in the recording. One 160-byte syscall per 20ms
// frame would make a quiet half hour ~90,000 writes issued in a single burst,
// all of them under t.mu and so all of them on the outbound send path WriteOut
// is called from. Batching costs one 16KB buffer and changes neither the bytes
// on disk nor the timeline they describe.
const silenceChunkFrames = 100

func tapDirFromEnv() string {
	return os.Getenv(tapDirEnv)
}

func tapLabelFromEnv() string {
	return os.Getenv("AATOOLKIT_TAP_LABEL")
}

// agentLabelFromEnv is the transcript summary's response role label (SOP-168),
// injected by the consumer (empty = the engine's generic "agent" default). The
// engine never embeds a product name; the deployment sets this env var.
func agentLabelFromEnv() string {
	return os.Getenv("AATOOLKIT_AGENT_LABEL")
}

// Tap records a call's audio to a pair of raw mu-law files plus a JSON
// sidecar. It is best-effort throughout: every failure is logged once and
// absorbed, because a broken recording must never cost a call.
//
// Which directions a Tap carries is declared at construction (see
// WithChannels) and decides how the outbound side works, because the two
// shapes have different clocks:
//
// Duplex -- the default, and the pipeline session's tap. WriteOut only
// ENQUEUES; nothing reaches disk until DrainOut dequeues a frame, and the
// pipeline calls DrainOut once per inbound frame. That inbound frame clock,
// with nextOutFrame synthesizing 0xFF silence when the queue is empty, is what
// makes out.ulaw time-aligned with in.ulaw byte for byte -- the property the
// sidecar's alignment promises and probeset build consumes. A caller that
// writes outbound frames and never drains gets no error and no file.
//
// One-directional -- WriteOut writes through, paced against the wall clock
// (fillOutGap), and DrainOut is a no-op. This exists because the two rules
// above made a one-directional recording impossible and said nothing about it:
// with no inbound frames nothing ever drained, and Close's deletion rule keyed
// on t.frames, which counts INBOUND frames only, so even a written-through
// out.ulaw was unlinked on the way out along with the sidecar. Close now
// decides per file.
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

	// channels is the declared direction set (see WithChannels) and now is
	// the clock the non-duplex gap fill is paced against. Both are set at
	// construction and never mutated, so neither is guarded by t.mu.
	channels []Channel
	now      func() time.Time

	// fedThrough is the wall-clock instant up to which out.ulaw holds audio,
	// used only when the outbound side has no inbound frame clock. Zero until
	// the first WriteOut, which is where the recording starts -- byte 0 is
	// the first outbound frame, the same convention duplex uses for its first
	// inbound one. Named after cmd/twilio-cli's playoutFiller.fedThrough,
	// which is the same quantity for the same reason.
	fedThrough time.Time

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

// duplexAlignment is the sidecar "alignment" string a duplex recording
// carries: out.ulaw is written one frame per inbound frame, so byte offset is
// the same instant in both files. It is named rather than inlined because a
// one-directional recording must be able to say it is NOT this.
const duplexAlignment = "inbound-frame-clock, 20ms/frame, silence=0xFF"

// outboundWallClockAlignment is what a recording with no inbound stream to
// clock it carries instead. There is no second file to align against, so the
// claim is weaker and different in kind: byte offset is elapsed wall-clock
// time since the first outbound frame, with the gaps between frames written
// out as real silence. A consumer cannot recover which of the two produced a
// file from its bytes, so the sidecar has to say.
const outboundWallClockAlignment = "wall-clock, 20ms/frame, silence=0xFF, t0=first outbound frame"

// inboundArrivalAlignment is the inbound-only case: in.ulaw is the inbound
// frames concatenated in arrival order, exactly as in a duplex recording, but
// with no out.ulaw beside it there is no alignment between channels to claim.
const inboundArrivalAlignment = "inbound-frame-arrival, 20ms/frame"

// Channel names one direction of a call. The values are the strings the
// sidecar's "channels" field carries, so there is one definition of each name
// rather than a constant here and a literal in writeSidecar.
type Channel string

const (
	ChannelIn  Channel = "in"
	ChannelOut Channel = "out"
)

// TapOption configures a Tap at construction, matching the RealtimeOption /
// SessionOption vocabulary the rest of this package uses.
type TapOption func(*Tap)

// WithChannels declares which directions of the call this Tap will be given.
//
// The default -- no option at all -- is duplex, which is the pipeline
// session's tap and is unchanged in every respect: the outbound ring buffer,
// the inbound frame clock that drains it, the 0xFF silence fill, and the
// sidecar it writes. Declaring a single channel is what makes a Tap usable to
// a consumer that only ever sees one direction of a call, and it changes three
// things for that Tap; see hasChannel's callers in WriteIn, WriteOut, DrainOut
// and Close.
//
// A direction that is not declared never reaches disk and is never counted:
// writing one is a caller mistake, and a tap's whole ethos is to absorb those
// rather than fail a call over them.
//
// Unrecognized values and repeats are dropped -- the set is rendered straight
// into the sidecar's "channels", so a caller naming a direction twice would
// otherwise publish a duplex recording that no longer reads as one. An empty
// result is treated as duplex. There is no error return on this path -- NewTap
// cannot fail today, and a silently-off tap is exactly the failure mode this
// ticket exists to remove.
func WithChannels(channels ...Channel) TapOption {
	return func(t *Tap) {
		var set []Channel
		for _, ch := range channels {
			if ch != ChannelIn && ch != ChannelOut {
				continue
			}
			if slices.Contains(set, ch) {
				continue
			}
			set = append(set, ch)
		}
		if len(set) > 0 {
			t.channels = set
		}
	}
}

// withNow injects the clock an outbound-only Tap paces its gap fill against.
// Test-only, like newTapWithWriter and newTapWithOutWriter: a byte-exact
// assertion about how much silence covers a gap is a race against a real
// clock.
func withNow(now func() time.Time) TapOption {
	return func(t *Tap) {
		if now != nil {
			t.now = now
		}
	}
}

func NewTap(dir, streamSID, callSID, label string, startedAt time.Time, opts ...TapOption) *Tap {
	if dir == "" {
		return nil
	}
	t := &Tap{
		dir:       dir,
		streamSID: streamSID,
		callSID:   callSID,
		label:     label,
		startedAt: startedAt,
		channels:  []Channel{ChannelIn, ChannelOut},
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// hasChannel reports whether this Tap was declared to carry a direction.
func (t *Tap) hasChannel(ch Channel) bool {
	for _, c := range t.channels {
		if c == ch {
			return true
		}
	}
	return false
}

// duplex reports whether both directions were declared -- the pipeline
// session's tap, and the only shape that has an inbound frame clock to align
// the two files against.
func (t *Tap) duplex() bool {
	return t.hasChannel(ChannelIn) && t.hasChannel(ChannelOut)
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

// WriteIn records one frame of audio the caller sent. It is dropped, and not
// counted, on a tap that did not declare the inbound direction.
func (t *Tap) WriteIn(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.inOpenFailed || !t.hasChannel(ChannelIn) {
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

// WriteOut records one frame of audio the engine sent to the caller.
//
// On a duplex tap this only enqueues -- see the type comment; DrainOut is what
// puts it on disk, and a tap that is never drained records nothing. On a tap
// that declared the outbound direction alone it writes through, first padding
// the gap since the previous frame with real silence.
func (t *Tap) WriteOut(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || !t.hasChannel(ChannelOut) {
		return
	}

	// With no inbound stream there is no DrainOut to move this to disk, so
	// write through instead of enqueuing -- and pace it against the wall
	// clock, because a gapless concatenation of what the engine said turns a
	// 3-minute call with 40 seconds of speech into 40 seconds of audio with
	// every pause removed. None of the queue's machinery applies here: the
	// bound, the drop-oldest and the OutTruncated flag all exist to bound a
	// queue that only exists because the drain is elsewhere.
	if !t.duplex() {
		if !t.ensureOutWriter() {
			return
		}
		t.fillOutGap(t.now())
		t.appendOut(payload)
		t.fedThrough = t.fedThrough.Add(mulawDuration(len(payload)))
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

// nextOutFrame dequeues the oldest queued outbound frame, or -- when the
// queue is empty -- synthesizes one frame of silence paced to the last
// inbound frame's length (falling back to defaultFrameBytes before any
// inbound frame has set the pace). Caller holds t.mu.
func (t *Tap) nextOutFrame() []byte {
	if t.outCount > 0 {
		frame := t.outQueue[t.outHead]
		t.outQueue[t.outHead] = nil // don't retain a reference past dequeue
		t.outHead = (t.outHead + 1) % maxOutQueueFrames
		t.outCount--
		return frame
	}

	silenceLen := t.lastInFrameN
	if silenceLen == 0 {
		silenceLen = defaultFrameBytes
	}
	frame := make([]byte, silenceLen)
	for i := range frame {
		frame[i] = 0xFF
	}
	return frame
}

// ensureOutWriter opens out.ulaw on first use and reports whether it is
// writable. A failed open is latched, so a tap over a bad directory logs once
// and then costs a branch per frame. Caller holds t.mu.
func (t *Tap) ensureOutWriter() bool {
	if t.outOpenFailed {
		return false
	}
	if t.wOut != nil {
		return true
	}
	f, err := os.OpenFile(t.outulawPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.outOpenFailed = true
		t.logOnce(err)
		return false
	}
	t.wOut = f
	return true
}

// appendOut writes one frame to out.ulaw. Caller holds t.mu and has already
// called ensureOutWriter.
func (t *Tap) appendOut(frame []byte) {
	t.appendOutFrames(frame, 1)
}

// appendOutFrames writes one Write's worth of outbound audio -- frames whole
// mu-law frames, laid end to end. outBytes counts what landed; outFrames counts
// what was offered, so a short write over-counts by up to frames-1, where
// appendOut's single frame could only ever be off by one. That is a rounding
// this can afford rather than one it hides: outFrames is read only as a zero
// test (removeEmptyRecordings, and Close deciding whether to write a sidecar),
// and any short write with n > 0 answers that test the same way whatever it is
// counted as. Caller holds t.mu and has already called ensureOutWriter.
func (t *Tap) appendOutFrames(buf []byte, frames int) {
	n, err := t.wOut.Write(buf)
	if err != nil {
		t.logOnce(err)
	}
	if n > 0 {
		t.outFrames += frames
		t.outBytes += n
	}
}

// fillOutGap writes 0xFF silence frames until out.ulaw holds audio up to now.
// It is the outbound-only pacing: the file's duration tracks the span of the
// call rather than the sum of its payloads. Caller holds t.mu and has already
// called ensureOutWriter.
//
// The recording starts at the first outbound frame -- before it there is
// nothing to be silent between, and startedAt is a caller-supplied stamp, not
// a reading of this clock. Byte 0 being the first frame of the declared
// channel is also what duplex does with its first inbound one.
//
// Past that first frame fedThrough advances only by whole frames handed to the
// writer, never by jumping to now, so len(out.ulaw) and fedThrough stay two
// views of one quantity -- with the one exception every path here shares: a
// write that failed still advances it, because a fill that could not make
// progress would loop forever. A backend sending faster than real time leaves
// fedThrough ahead of now and this loop simply does not run; the surplus is
// absorbed by the next real gap.
//
// Whole frames only, for the reason cmd/twilio-cli's playoutFiller.fill gives:
// a partial frame would put the file off the 20ms grid, and the rounding error
// is at most one frame and is recovered on the next fill.
//
// Unlike that filler this has no catch-up bound, deliberately: there a deficit
// past maxFillCatchUp is resynced away because filling it honestly would block
// on the player, while here a minutes-long gap is the ordinary case and
// dropping it would be exactly the lie the fill exists to prevent. The price is
// paid under t.mu, on the send path WriteOut is called from: a gap costs its own
// length in bytes on disk, plus one Write call per silenceChunkFrames of it --
// that constant bounds the buffer handed to each Write, never the total.
//
// A gap here can only be real elapsed time, never a clock correction: in
// production both now and fedThrough descend from a time.Now reading, Add
// preserves the monotonic reading, and time.Time.Sub then uses the monotonic
// readings alone. An NTP step moves the wall clock and does not appear here at
// all. (withNow's injected clock carries no monotonic reading, so a test's gaps
// are exactly the durations it advances by -- which is the point of it.)
func (t *Tap) fillOutGap(now time.Time) {
	if t.fedThrough.IsZero() {
		t.fedThrough = now
		return
	}

	frameDur := mulawDuration(defaultFrameBytes)
	// Truncating division is the same "whole frames only" rule the loop below
	// used to spell out one frame at a time, and it is <= 0 for both the
	// sub-frame gap and a clock that went backwards.
	frames := int64(now.Sub(t.fedThrough) / frameDur)
	if frames <= 0 {
		return
	}

	chunk := frames
	if chunk > silenceChunkFrames {
		chunk = silenceChunkFrames
	}
	silence := bytes.Repeat([]byte{0xFF}, int(chunk)*defaultFrameBytes)

	for frames > 0 {
		n := min(frames, chunk)
		t.appendOutFrames(silence[:int(n)*defaultFrameBytes], int(n))
		t.fedThrough = t.fedThrough.Add(time.Duration(n) * frameDur)
		frames -= n
	}
}

// DrainOut moves one queued outbound frame to disk, paced by the caller -- the
// pipeline session calls it once per inbound frame, which is what makes
// out.ulaw time-aligned with in.ulaw.
//
// It is a no-op on a tap that did not declare both directions: there is no
// inbound clock to pace it, WriteOut has already written the payload through,
// and a drain that did anything would duplicate frames or interleave silence
// the wall clock did not call for. A consumer that copies the duplex call
// pattern is therefore safe.
func (t *Tap) DrainOut() {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || !t.duplex() {
		return
	}
	if !t.ensureOutWriter() {
		return
	}
	t.appendOut(t.nextOutFrame())
}

// closeWriters closes whichever of w/wOut were opened, logging (not
// aggregating) any error via logOnce -- one line about the tap's first
// failure is the whole guarantee, not a report of every failing writer.
// Caller holds t.mu.
func (t *Tap) closeWriters() {
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
}

// removeEmptyRecordings deletes whichever ulaw files were opened but never
// received a byte -- an empty recording is capture noise, not data.
//
// The decision is per file. It used to be global on the inbound count, which
// is why an outbound-only recording was unlinked on the way out: t.frames
// counts inbound frames, and there are none by construction. Per file is the
// honest form of the same rule rather than a special case for one channel set.
//
// Two duplex sequences change because of this, both of them a call where one
// direction landed bytes and the other landed none:
//
//   - Outbound content with an inbound count of zero used to lose both files
//     and the sidecar, and now keeps out.ulaw with a sidecar saying frames: 0.
//     On the pipeline path DrainOut is called only after WriteIn, but that does
//     not make the case unreachable: t.frames stays zero whenever the inbound
//     side never landed a byte, and in.ulaw failing to open latches
//     inOpenFailed so every WriteIn after it returns early. What the change
//     costs there is keeping the recording of a call whose inbound capture
//     broke, which beats the nothing it used to leave behind.
//   - Inbound content with an outbound count of zero used to keep an empty
//     out.ulaw and now removes it. That needs every outbound write to fail,
//     since the drain writes a frame -- real or silence -- on every call, so it
//     is a broken-disk path either way and an absent file states what a
//     zero-byte one only implies.
//
// Caller holds t.mu.
func (t *Tap) removeEmptyRecordings() {
	if t.w != nil && t.frames == 0 {
		if err := os.Remove(t.inulawPath()); err != nil && !os.IsNotExist(err) {
			t.logOnce(err)
		}
	}
	if t.wOut != nil && t.outFrames == 0 {
		if err := os.Remove(t.outulawPath()); err != nil && !os.IsNotExist(err) {
			t.logOnce(err)
		}
	}
}

// alignment names the clock that produced this recording's bytes. A consumer
// cannot recover it from them, and guessing wrong misreads the whole timeline
// -- so it is stated, and the three channel sets state three different things.
// Caller holds t.mu.
func (t *Tap) alignment() string {
	switch {
	case t.duplex():
		return duplexAlignment
	case t.hasChannel(ChannelOut):
		return outboundWallClockAlignment
	default:
		return inboundArrivalAlignment
	}
}

// declaredChannels renders the channel set for the sidecar. It is what stops a
// one-directional recording being fed to probeset build, which needs caller
// utterances an outbound-only file does not contain. Caller holds t.mu.
func (t *Tap) declaredChannels() []string {
	out := make([]string, 0, len(t.channels))
	for _, ch := range t.channels {
		out = append(out, string(ch))
	}
	return out
}

// writeSidecar marshals and writes the tap's JSON sidecar. Caller holds t.mu.
//
// Frames and Bytes are the INBOUND counts, and always have been; on an
// outbound-only recording they are honestly zero. Adding outbound counters
// here would change the duplex sidecar, which this ticket holds byte-identical
// on purpose, so they stay out.
func (t *Tap) writeSidecar() {
	raw, err := json.Marshal(tapSidecar{
		StreamSID:        t.streamSID,
		CallSID:          t.callSID,
		Label:            t.label,
		StartedAt:        t.startedAt,
		Frames:           t.frames,
		Bytes:            t.bytes,
		VADConfig:        telephony.DefaultVADConfig(),
		Alignment:        t.alignment(),
		Channels:         t.declaredChannels(),
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

// Close finishes the recording: it pads a wall-clock-paced tail if there is
// one, closes both writers, deletes whichever file never received a byte, and
// writes the sidecar if either direction has content.
//
// It is idempotent and final -- a frame still in flight when the pumps are
// torn down must not reopen a finished recording.
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

	// A recording paced by the wall clock ends when the call does, not at its
	// last payload: pad the tail before the writer goes away.
	if !t.duplex() && t.hasChannel(ChannelOut) && !t.fedThrough.IsZero() && t.ensureOutWriter() {
		t.fillOutGap(t.now())
	}

	t.closeWriters()
	t.removeEmptyRecordings()

	if t.frames == 0 && t.outFrames == 0 {
		return
	}

	t.writeSidecar()
}
