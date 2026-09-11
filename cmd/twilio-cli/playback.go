package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// playerQueueFrames sizes the queue between the read loop and the sink.
//
// It is denominated in frames rather than seconds because that is what the
// queue holds, but the number is chosen from seconds: a single reply can be
// tens of seconds of audio delivered in well under one (78s in ~5.5s, measured
// on the demo call of 2026-09-09 09:37), and the queue's whole purpose is to
// take a burst like that without the read loop waiting on the speaker. 8192
// frames is roughly 160s of 20ms audio -- longer than any single reply
// observed -- at a few hundred KB of []byte headers and payloads, which is not
// a cost worth tuning against a call that lasts minutes.
const playerQueueFrames = 8192

// playerLead is the most audio the writer is allowed to have sitting in the
// sink ahead of real time.
//
// Zero would be the literal answer to "as little buffering as possible" and is
// the wrong one: the writer is a goroutine, and a scheduler that wakes it 5ms
// late with nothing in the pipe is an underrun the listener hears. The lead is
// what the writer is allowed to be late by without the sound stopping.
//
// It is small enough to be the thing it replaces: the kernel pipe into ffplay
// accepts 65,536 bytes -- measured on this machine, 8.19s of 8kHz mu-law --
// and a server that sends faster than real time used to fill it in one burst.
// Everything in that pipe when ffplay's audio device finishes opening is
// audio the listener does not hear (AATK-134), so the lead is also the bound
// on what a slow device open can cost.
const playerLead = 200 * time.Millisecond

// maxPaceCatchUp is the deficit past which the writer resyncs instead of
// writing its way out frame by frame.
//
// Same reasoning as the playout filler's own resync, for the same reason: a
// machine that slept, or a writer descheduled behind something slow, can come
// back seconds behind. Honouring that honestly means writing seconds of audio
// as fast as the pipe accepts -- which is precisely the burst this pacing
// exists to prevent. A resync loses the illusion of continuity once; a burst
// loses the opening of whatever is playing.
const maxPaceCatchUp = 2 * time.Second

// audioPlayer streams μ-law audio frames to a single sink for the lifetime of a
// call. In production the sink is the stdin of one long-lived ffplay process, so
// every received frame plays as one continuous stream instead of spawning a new
// process per 20ms frame.
//
// WRITES ARE ASYNCHRONOUS, and that is the whole of this type's job beyond
// owning the process. ffplay reads its stdin at playback rate, so the pipe into
// it is a rate limiter: measured on this machine, the kernel accepts 65,440
// bytes -- 8.18s of 8kHz mu-law -- before a write blocks. A server that sends
// faster than real time fills that in one burst, and a synchronous write then
// parks whichever goroutine made it for as long as ffplay takes to drain.
//
// In twilio-cli that goroutine is dialReadLoop, which is the ONLY websocket
// reader, the only playout-filler tick, and the only mark echoer. So a long
// reply used to stop the client reading its socket entirely: measured on the
// demo call of 2026-09-09 10:57, a 12.8x burst at t=69s was followed by a
// 16.2-SECOND gap in inbound frames while the outbound stream kept flowing at
// 0.99x without a single gap -- the client was alive and sending, and simply
// not receiving. The server, writing into a socket nobody drained, eventually
// died with a broken pipe.
//
// So play() hands the frame to a queue and returns, and one writer goroutine
// owns the sink. The queue is also flushable, which the pipe was not: see
// flush.
type audioPlayer struct {
	sink io.WriteCloser
	wait func() error // reaps the ffplay process on close; nil when the sink is injected

	// tap is the -record-played recorder, or nil. See attachTap.
	tap *streamRecorder

	// paced releases frames to the sink at real time rather than as fast as it
	// accepts them; see awaitTurn. Off for an injected sink, so a test that
	// queues a hundred frames does not wait two seconds for them.
	paced bool
	now   func() time.Time    // seam: wall clock
	sleep func(time.Duration) // seam: the wait itself

	// writeThrough is the wall time the audio written so far extends to. Owned
	// by writeLoop alone -- no other goroutine reads or writes it.
	writeThrough time.Time

	frames chan playItem
	// writerDone closes when the writer goroutine has returned, so close can
	// wait for the queue to reach the sink before shutting it.
	writerDone chan struct{}

	mu sync.Mutex
	// err is the first sink write failure. Recorded rather than returned,
	// because the write that fails happens on the writer goroutine long after
	// the play() that queued it; play reports it on the next call, which is
	// what lets lazyPlayer keep its "disable permanently on first failure"
	// behavior.
	err     error
	dropped int
}

// newPlayerFunc is a seam for tests to inject a fake player. Default is the real newPlayerImpl.
var newPlayerFunc func(context.Context) (*audioPlayer, error) = newPlayerImpl

// newPlayerImpl starts one ffplay process that reads a raw 8 kHz μ-law stream from
// stdin and plays it through the local speaker. Every frame passed to play is
// written to that single stream, so audio plays continuously and at realtime.
// ffplay is cross-platform, so this needs no per-OS build tags.
func newPlayerImpl(ctx context.Context) (*audioPlayer, error) {
	cmd := exec.CommandContext(ctx, "ffplay",
		"-hide_banner", "-loglevel", "error",
		"-nodisp", "-autoexit",
		"-f", "mulaw", "-ar", "8000", "-i", "-")
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("newPlayer: stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("newPlayer: start ffplay (installed? `brew install ffmpeg`): %w", err)
	}

	p := newAudioPlayer(stdin, cmd.Wait)
	p.paced = true
	return p, nil
}

// attachTap installs the -record-played recorder on this player.
//
// It MUST be called before the first play(), and lazyPlayer.play is the one
// caller that does so. That ordering is what makes the field safe to read from
// writeLoop without a lock: writeLoop is parked on `range p.frames` until an
// item arrives, the first item can only arrive from a play() that happens after
// this call, and the channel send establishes the happens-before edge. Setting
// it after audio has started would be a data race, which is why this is a
// method with a contract rather than an exported field.
//
// A nil recorder is the off state; streamRecorder.write tolerates it, so the
// write path carries no conditional.
func (p *audioPlayer) attachTap(rec *streamRecorder) { p.tap = rec }

// newPlayerWithSink builds a player around an already-open sink. Used by tests.
func newPlayerWithSink(sink io.WriteCloser) *audioPlayer {
	return newAudioPlayer(sink, nil)
}

// newAudioPlayer wires a sink to its writer goroutine. One constructor for both
// the real player and the injected-sink one, so a test exercises the same
// asynchronous path production uses rather than a synchronous stand-in.
func newAudioPlayer(sink io.WriteCloser, wait func() error) *audioPlayer {
	p := &audioPlayer{
		sink:       sink,
		wait:       wait,
		now:        time.Now,
		sleep:      time.Sleep,
		frames:     make(chan playItem, playerQueueFrames),
		writerDone: make(chan struct{}),
	}
	go p.writeLoop()
	return p
}

// playItem is one entry in the player's queue: either audio to write, or a
// barrier to close once everything queued ahead of it has been written.
//
// The barrier exists because "the queue is empty" is not the same question as
// "everything I queued has been written" -- a frame can be inside sink.Write
// when the queue reads empty. Travelling in the queue itself is what makes the
// answer exact, since the writer serves entries in order.
type playItem struct {
	data    []byte
	barrier chan struct{}
}

// writeLoop is the only goroutine that touches the sink. It ends when frames is
// closed, which close does, so a returned writeLoop means every queued frame
// has reached the sink.
func (p *audioPlayer) writeLoop() {
	defer close(p.writerDone)
	for item := range p.frames {
		if item.barrier != nil {
			close(item.barrier)
			continue
		}
		p.awaitTurn(item.data)
		if _, err := p.sink.Write(item.data); err == nil {
			// Recorded HERE, after the write returned, and nowhere else. The
			// tap's question is what reached the player and when; a tap at
			// play() would answer the first half and get the second wrong,
			// because the queue between them is exactly the interval under
			// suspicion -- "queued at t=0, written at t=0" and "queued at t=0,
			// written at t=3" are the two answers it exists to tell apart.
			//
			// A failed write is not recorded: those bytes never reached the
			// player, and a tap that logged them would describe audio nobody
			// could have heard.
			p.tap.write(item.data, time.Now())
		} else {
			p.mu.Lock()
			if p.err == nil {
				p.err = err
			}
			p.mu.Unlock()
			// Keep draining rather than returning: close() closes frames and
			// waits here, and a writer that abandoned the channel would leave
			// it doing so forever.
		}
	}
}

// awaitTurn blocks until this frame is due, so the sink never holds more than
// playerLead of audio ahead of real time.
//
// It runs on writeLoop's goroutine and nowhere else, which is what lets
// writeThrough be an ordinary field: play() hands frames to a channel and never
// touches it. Blocking HERE is the whole point -- it is the one goroutine that
// may wait, because it is the one the queue exists to decouple from the read
// loop. Blocking in play() would restore the exact stall the queue removed.
func (p *audioPlayer) awaitTurn(frame []byte) {
	if !p.paced {
		return
	}
	now := p.now()
	// First frame of the call, or so far behind that catching up would itself
	// be a burst: restart the clock from here.
	if p.writeThrough.IsZero() || now.Sub(p.writeThrough) > maxPaceCatchUp {
		p.writeThrough = now
	}
	if d := p.writeThrough.Sub(now) - playerLead; d > 0 {
		p.sleep(d)
	}
	p.writeThrough = p.writeThrough.Add(telephony.MuLawDuration(len(frame)))
}

// play queues one decoded μ-law frame and returns without waiting for it to be
// written. Empty frames are ignored.
//
// The returned error is the sink's FIRST failure, seen on a later call than the
// one that caused it -- see audioPlayer.err. It is reported once, so a caller
// that disables playback on it does so exactly once.
//
// A full queue DROPS. It means ffplay has stopped consuming for long enough to
// fall 160s behind, which is a dead player rather than a slow one; blocking
// here would put back the exact stall this type exists to remove.
func (p *audioPlayer) play(frame []byte) error {
	if err := p.takeErr(); err != nil {
		return err
	}
	if len(frame) == 0 {
		return nil
	}
	// The frame is a slice of a buffer the caller may reuse, and it now
	// outlives this call. Copy it.
	queued := make([]byte, len(frame))
	copy(queued, frame)
	select {
	case p.frames <- playItem{data: queued}:
	default:
		p.mu.Lock()
		p.dropped++
		n := p.dropped
		p.mu.Unlock()
		if n == 1 {
			log.Printf("twilio-cli: audio queue full -- the player is %d frames behind and frames are being dropped", playerQueueFrames)
		}
	}
	return nil
}

// takeErr returns the first sink error once and then forgets it.
func (p *audioPlayer) takeErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.err
	p.err = nil
	return err
}

// flush discards everything queued and not yet written.
//
// This is what a Twilio `clear` can finally act on. The server sends one when
// the caller barges in: it has abandoned the rest of its reply and is telling
// the client to drop what is queued. Before the queue existed there was
// nothing here to drop -- the audio was already inside ffplay's stdin pipe,
// which cannot be flushed without killing the player mid-call -- so a
// discarded reply kept playing at the caller for as long as the pipe held it,
// up to the 8.18s it accepts. Whatever is still in this queue is now dropped
// with it; the pipe's own contents remain unflushable, which is a smaller
// residue than the reply itself.
func (p *audioPlayer) flush() {
	for {
		select {
		case item := <-p.frames:
			// A barrier is a promise to somebody who is waiting on it, and a
			// flush is not a reason to break it: dropping one silently would
			// park settle for the rest of the call.
			if item.barrier != nil {
				close(item.barrier)
			}
		default:
			return
		}
	}
}

// settle blocks until every frame queued before the call has reached the sink.
//
// It is the synchronisation point an asynchronous player owes its callers.
// Nothing in the call path needs it -- play returns immediately by design and
// close drains on its own -- but a test that queues audio and then reads the
// sink is asking "has it been written yet?", and without this it is really
// asking "has the writer goroutine been scheduled yet?", which is a race
// dressed as an assertion.
func (p *audioPlayer) settle() {
	barrier := make(chan struct{})
	select {
	case p.frames <- playItem{barrier: barrier}:
	default:
		// Queue full: nothing can be promised, and the frames this would have
		// waited on were dropped by play for the same reason.
		return
	}
	select {
	case <-barrier:
	case <-p.writerDone:
	}
}

// close drains the queue to the sink, signals end-of-stream (the EOF that makes
// ffplay drain its buffer and exit) and waits for the process to finish. On
// context cancellation ffplay is killed instead; either way the process is
// reaped.
//
// The queue is drained rather than discarded, so the tail of a call is heard.
// That is a bounded wait: what remains plays out at real time, and the caller
// of close is the call's own teardown.
func (p *audioPlayer) close() error {
	close(p.frames)
	<-p.writerDone
	err := p.sink.Close()
	if p.wait != nil {
		_ = p.wait()
	}
	return err
}

// lazyPlayer starts one audioPlayer on the first non-empty frame and streams
// every subsequent frame into it.
//
// It used to say "so a call with no audio never spawns ffplay". That stopped
// being true when the playout filler started writing silence within 20ms of
// the read loop starting: every call now spawns ffplay, and a machine without
// ffmpeg logs "audio playback disabled" on every call rather than only on
// calls that had audio.
// Playback is disabled permanently after the first failure — whether the ffplay
// process fails to start or dies mid-call — so a broken player is not retried
// (and not error-logged) once per frame. Not safe for concurrent use — drive it
// from one goroutine.
type lazyPlayer struct {
	newPlayer func(context.Context) (*audioPlayer, error) // seam for tests
	ctx       context.Context
	tap       *streamRecorder // -record-played; nil when off
	player    *audioPlayer
	failed    bool
}

func newLazyPlayer(ctx context.Context, tap *streamRecorder) *lazyPlayer {
	return &lazyPlayer{newPlayer: newPlayerFunc, ctx: ctx, tap: tap}
}

// play streams one μ-law frame, starting the player on first use. Errors are
// logged, not returned: a playback failure must not tear down the call.
func (l *lazyPlayer) play(frame []byte) {
	if len(frame) == 0 {
		return
	}
	if l.player == nil {
		if l.failed {
			return
		}
		p, err := l.newPlayer(l.ctx)
		if err != nil {
			log.Printf("twilio-cli: audio playback disabled: %v", err)
			l.failed = true
			return
		}
		// Before any frame reaches it -- see attachTap for why the ordering is
		// the whole of the field's thread safety.
		p.attachTap(l.tap)
		l.player = p
	}
	if err := l.player.play(frame); err != nil {
		// ffplay died mid-call (e.g. broken pipe): reap it and disable playback
		// rather than logging this error for every remaining frame.
		log.Printf("twilio-cli: audio playback disabled: %v", err)
		_ = l.player.close()
		l.player = nil
		l.failed = true
	}
}

// flush drops the queued playout, if a player was ever started. A call with no
// audio yet has nothing to drop, which is why this is not an error.
func (l *lazyPlayer) flush() {
	if l.player != nil {
		l.player.flush()
	}
}

// settle blocks until everything played so far has reached the sink, if a
// player was ever started. See audioPlayer.settle.
func (l *lazyPlayer) settle() {
	if l.player != nil {
		l.player.settle()
	}
}

// close shuts down the underlying player if one was ever started.
func (l *lazyPlayer) close() {
	if l.player != nil {
		_ = l.player.close()
	}
}

// earconDurationMS is how long the capture-live tone sounds for.
//
// It was one 20 ms frame. Eight cycles of 400 Hz is not a tone, it is a click,
// and an operator running a demo call reported never hearing the earcon at
// all -- correctly, because at that length there is nothing to hear. 240 ms is
// long enough to register as a deliberate cue and short enough not to sit on
// top of whatever the server is saying. It is a whole number of 20 ms frames
// so the tone can be written frame-aligned like every other payload here.
const earconDurationMS = 240

// earconRampMS fades the tone in and out.
//
// Starting and ending at full amplitude puts a step discontinuity into the
// stream on both sides of the tone, which is heard as a pop -- so a tone made
// long enough to hear would arrive bracketed by two clicks. The ramp is what
// makes it sound like a cue rather than a dropout.
const earconRampMS = 20

// generateEarcon returns the μ-law capture-live tone: a 400 Hz sine at 8 kHz,
// earconDurationMS long, ramped in and out over earconRampMS at each end.
func generateEarcon() []byte {
	// One definition of the rate. `sampleRate` was a second, float copy used
	// only by the phase term, so a change to the rate would have altered the
	// tone's LENGTH without altering its PITCH -- silently, and only audibly
	// wrong. The rate itself is telephony.SampleRateHz, the module's one
	// declaration of it.
	const frequency = 400.0
	sampleRate := float64(telephony.SampleRateHz)
	samples := telephony.SampleRateHz * earconDurationMS / 1000
	ramp := telephony.SampleRateHz * earconRampMS / 1000

	earcon := make([]byte, samples)
	for i := 0; i < samples; i++ {
		// Generate a 400Hz sine wave in the range [-32767, 32767].
		t := float64(i) / sampleRate
		amplitude := 32767.0 * math.Sin(2*math.Pi*frequency*t)

		// Linear fade over the first and last ramp samples. Applied to the
		// sine rather than to the encoded byte: mu-law is logarithmic, so
		// scaling the code word does not scale the sample it stands for.
		switch {
		case i < ramp:
			amplitude *= float64(i) / float64(ramp)
		case i >= samples-ramp:
			amplitude *= float64(samples-1-i) / float64(ramp)
		}

		// Convert to μ-law.
		earcon[i] = telephony.LinearToMuLaw(int16(amplitude))
	}
	return earcon
}

// playEarcon plays one earcon tone to the given lazy player and returns the
// bytes it wrote, so the caller can account for them. Audio that reaches the
// player without the playout filler knowing is audio the filler will pad on
// top of, permanently offsetting everything after it.
func playEarcon(l *lazyPlayer) []byte {
	tone := generateEarcon()
	l.play(tone)
	return tone
}
