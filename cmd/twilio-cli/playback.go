package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"

	"github.com/iansmith/aatoolkit/telephony"
)

// audioPlayer streams μ-law audio frames to a single sink for the lifetime of a
// call. In production the sink is the stdin of one long-lived ffplay process, so
// every received frame plays as one continuous stream instead of spawning a new
// process per 20ms frame.
type audioPlayer struct {
	sink io.WriteCloser
	wait func() error // reaps the ffplay process on close; nil when the sink is injected
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

	return &audioPlayer{sink: stdin, wait: cmd.Wait}, nil
}

// newPlayerWithSink builds a player around an already-open sink. Used by tests.
func newPlayerWithSink(sink io.WriteCloser) *audioPlayer {
	return &audioPlayer{sink: sink}
}

// play writes one decoded μ-law frame to the sink. Empty frames are ignored.
func (p *audioPlayer) play(frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	_, err := p.sink.Write(frame)
	return err
}

// close signals end-of-stream to the sink (the EOF that makes ffplay drain its
// buffer and exit) and waits for the process to finish. On context cancellation
// ffplay is killed instead; either way the process is reaped.
func (p *audioPlayer) close() error {
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
	player    *audioPlayer
	failed    bool
}

func newLazyPlayer(ctx context.Context) *lazyPlayer {
	return &lazyPlayer{newPlayer: newPlayerFunc, ctx: ctx}
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
	// only by the phase term, so a change to sampleRateHz would have altered
	// the tone's LENGTH without altering its PITCH -- silently, and only
	// audibly wrong.
	const frequency = 400.0
	sampleRate := float64(sampleRateHz)
	samples := sampleRateHz * earconDurationMS / 1000
	ramp := sampleRateHz * earconRampMS / 1000

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
