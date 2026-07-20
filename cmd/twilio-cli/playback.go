package main

import (
	"context"
	"fmt"
	"io"
	"log"
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

// newPlayer starts one ffplay process that reads a raw 8 kHz μ-law stream from
// stdin and plays it through the local speaker. Every frame passed to play is
// written to that single stream, so audio plays continuously and at realtime.
// ffplay is cross-platform, so this needs no per-OS build tags.
func newPlayer(ctx context.Context) (*audioPlayer, error) {
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
	return &lazyPlayer{newPlayer: newPlayer, ctx: ctx}
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
