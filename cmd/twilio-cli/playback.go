package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
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
// every subsequent frame into it, so a call with no audio never spawns ffplay.
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

// generateEarcon returns a 160-byte μ-law encoded earcon tone (400Hz sine wave at 8kHz).
func generateEarcon() []byte {
	const (
		sampleRate = 8000.0
		frequency  = 400.0
		samples    = muLawFrame20ms
	)

	earcon := make([]byte, samples)
	for i := 0; i < samples; i++ {
		// Generate a 400Hz sine wave in the range [-32767, 32767].
		t := float64(i) / sampleRate
		amplitude := 32767.0 * math.Sin(2*math.Pi*frequency*t)
		// Convert to μ-law.
		earcon[i] = linearToMulaw(int16(amplitude))
	}
	return earcon
}

// linearToMulaw converts a 16-bit signed PCM sample to 8-bit μ-law.
// μ-law encoding is the standard used by Twilio and telephony systems.
// Implements ITU-T G.711: sign bit, 3 segment (exponent) bits, and 4 mantissa
// bits. The exponent is found by repeatedly halving the biased magnitude, but
// the mantissa is extracted from that ORIGINAL biased value shifted by
// (exponent+3) — not from the already-halved value — since the halving loop
// destroys the low bits the mantissa needs. Only the segment+mantissa bits are
// complemented; the sign bit passes through as computed. Zero is treated as
// positive (bit 7 set to 0x80) by default, producing 0xFF (silence) via the
// standard's "negative zero" convention.
func linearToMulaw(sample int16) byte {
	const bias = 0x84
	const clip = 32635 // standard ITU-T G.711 clip: CLIP+BIAS caps at 0x7fff
	const segMask = 0x7f

	// Extract sign bit and get absolute value. Bit 7 in the encoded output
	// indicates sign: 1 for positive (per ITU-T G.711 and ffmpeg convention),
	// 0 for negative. Zero encodes as 0xFF (silence) because it's treated as
	// positive for bit-level purposes (sign=0x80 pre-OR), but the compression
	// logic produces segment|mantissa=0, so 0x80|(~0&0x7f)=0xFF.
	s := int32(sample)
	sign := byte(0x80) // Default to positive
	if s < 0 {
		sign = 0x00 // Negative: clear bit 7
		s = -s
	}

	// Clip to valid range.
	if s > clip {
		s = clip
	}

	// Add bias for compression.
	s += bias
	biased := s

	// Find the exponent (segment) by halving until the value fits in 8 bits.
	var exponent uint
	for s > 0xff {
		exponent++
		s >>= 1
	}

	// Mantissa comes from the original biased value, not the halved one.
	// Only segment+mantissa bits are complemented; sign passes through.
	mantissa := byte((biased >> (exponent + 3)) & 0x0f)
	segment := byte((exponent & 0x07) << 4)
	return sign | (^(segment | mantissa) & segMask)
}

// playEarcon plays one earcon tone to the given lazy player.
func playEarcon(l *lazyPlayer) {
	tone := generateEarcon()
	l.play(tone)
}
