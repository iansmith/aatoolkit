package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// streamMic is the frame-source entry point dial() calls through. It defaults to
// mic capture; main() reassigns it to a file-backed source when -audio is passed
// (capture_file.go). It is also the seam tests override to simulate capture
// completion (EOF) deterministically, since real mic capture has no natural EOF
// to trigger from a test. Receives an onMicWarm callback that fires when the
// first real frame is emitted or the discard cap is hit, with a bool indicating
// whether the cap was hit.
//
// This is the ONE frame-source seam — a second one (a dialOption, say) would
// leave two mechanisms selecting the same thing.
var streamMic func(context.Context, *websocket.Conn, string, *int, func(bool)) error = streamMicFrames

// frameSourceLabel names whatever streamMic currently is, for the connected log
// line. Set alongside streamMic, never independently.
var frameSourceLabel = "mic"

// dialOptions configures optional dial() behavior.
type dialOptions struct {
	noEchoMarks bool
	recordPath  string
}

// dialOption configures dialOptions.
type dialOption func(*dialOptions)

// withNoEchoMarks suppresses mark-echo behavior (see --no-echo-marks in
// main.go) so the server's AwaitingMarkEcho state hits its timeout path
// instead of receiving an echo.
func withNoEchoMarks() dialOption {
	return func(o *dialOptions) { o.noEchoMarks = true }
}

// withRecording records every inbound media payload to path (see -record in
// main.go and inboundRecorder's own doc for why this exists).
func withRecording(path string) dialOption {
	return func(o *dialOptions) { o.recordPath = path }
}

func dial(ctx context.Context, callSid, addr string, opts ...dialOption) error {
	var cfg dialOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.CloseNow()

	// One audio player per call: every media frame streams into the same player
	// so playback is one continuous sound. Bound to ctx — a clean server close
	// lets ffplay drain and finish; Ctrl-C (ctx cancel) kills it.
	player := newLazyPlayer(ctx)
	defer player.close()

	recorder, err := newInboundRecorder(cfg.recordPath, time.Now())
	if err != nil {
		return err
	}
	defer func() { recorder.close(time.Now()) }()

	// earconCh signals the main read loop to play an earcon tone.
	// The mic goroutine sends on this channel; the read loop receives and plays
	// from its own goroutine context to avoid concurrent access to lazyPlayer.
	earconCh := make(chan struct{}, 1)

	micCtx, cancelMic := context.WithCancel(ctx)
	defer cancelMic() // fires before CloseNow (LIFO); signals goroutine to stop

	streamSID := newSID("MZ")

	// seqNum is this call's single, unified Twilio sequenceNumber counter: it
	// starts at 1 for the start frame, the media encoder advances it per media
	// frame, and the stop frame takes the next value. The connected frame is
	// NOT counted. It is only ever read/written from one goroutine at a time —
	// the main goroutine before the mic starts (start), the mic goroutine while
	// streaming (media), and sendStop only after the mic goroutine has fully
	// returned (see micStopped) — so a plain int needs no lock.
	seqNum := 1

	// The blocking read loop below uses readCtx, NOT ctx directly: coder/websocket
	// closes the underlying connection as soon as a Read's context is done, so if
	// the read were bound to ctx, cancelling ctx (SIGINT) would kill the socket
	// before the stop frame below got a chance to write. Instead we watch ctx
	// ourselves, send stop first, then cancel readCtx to unblock the read.
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()

	var sendStopOnce sync.Once
	sendStop := func() {
		sendStopOnce.Do(func() {
			// The stop frame takes the next sequence number after the last media
			// frame. Safe to advance seqNum here: sendStop runs only after the mic
			// goroutine has fully returned (natural end runs it inline after
			// streamMic; the ctx-cancel path waits on micStopped first).
			seqNum++
			stopMsg, err := twilio.EncodeStop(streamSID, callSid, defaultAccountSid, seqNum)
			if err != nil {
				log.Printf("twilio-cli: encode stop: %v", err)
				return
			}
			writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := conn.Write(writeCtx, websocket.MessageText, stopMsg); err != nil {
				log.Printf("twilio-cli: send stop: %v", err)
				return
			}
			logCtlFrame("->", stopMsg)
		})
	}
	// micStopped is closed once the mic goroutine has fully returned — which now
	// includes the graceful drain of ffmpeg's buffered tail on shutdown
	// (capture_darwin.go). The Ctrl-C path below waits on it before sending the stop
	// frame, so the server receives the trailing media BEFORE the stream-stop event
	// (AATK-2). Sending stop first would let the server close the turn while the last
	// ~100-300ms of audio was still arriving.
	micStopped := make(chan struct{})

	// Ctrl-C / ctx-cancel path: cancel the mic to trigger the graceful ffmpeg stop
	// (SIGINT → flush → drain to EOF), wait for that drain to finish, THEN send stop.
	// The readCtx.Done fallback keeps this from blocking forever if ctx is cancelled
	// after an early return (e.g. handshake failure) that never started the mic
	// goroutine, so micStopped is never closed — dial's deferred cancelRead unblocks it.
	//
	// The other two end triggers do NOT send stop from here: a caller hangup sends it
	// synchronously inside the mic goroutine below (gated on naturalEnd), and a
	// server-initiated close needs no stop at all — the socket is already gone, so
	// sending would only draw a broken pipe (AATK-7).
	go func() {
		<-ctx.Done()
		cancelMic()
		select {
		case <-micStopped:
		case <-readCtx.Done():
		}
		sendStop()
		cancelRead()
	}()

	// Twilio opens every Media Stream with a connected frame before start;
	// twilio-cli is a stand-in for Twilio, so it sends one too. the server
	// tolerates its absence (ServeStreams consumes a connected frame only if
	// the first one is), which is why omitting it went unnoticed -- but a
	// fake that skips a frame the real thing always sends cannot be trusted
	// to prove the server speaks the protocol.
	connectedMsg, err := twilio.EncodeConnected()
	if err != nil {
		return fmt.Errorf("encode connected: %w", err)
	}
	if err := writeHandshake(ctx, conn, connectedMsg); err != nil {
		return ignoreHandshakeHangup(err)
	}

	startMsg, err := twilio.EncodeStart(streamSID, callSid, defaultAccountSid, seqNum)
	if err != nil {
		return fmt.Errorf("encode start: %w", err)
	}
	if err := writeHandshake(ctx, conn, startMsg); err != nil {
		return ignoreHandshakeHangup(err)
	}
	log.Printf("twilio-cli: connected to %s, streaming %s (Ctrl-C to stop)", addr, frameSourceLabel)

	micErrCh := make(chan error, 1)
	go func() {
		defer cancelMic() // goroutine exit cancels the read loop
		onMicWarm := func(capHit bool) {
			// Named by source: only the mic can reach the discard cap, but both
			// sources reach "capture live", and calling a file replay "mic" there
			// is just misleading. With no -audio the wording is unchanged.
			if capHit {
				log.Printf("twilio-cli: %s warm (discard cap reached)", frameSourceLabel)
			} else {
				log.Printf("twilio-cli: %s warm (capture live)", frameSourceLabel)
			}
			// Signal the read loop to play an earcon tone. Non-blocking send
			// (buffered channel) so the mic goroutine doesn't wait.
			select {
			case earconCh <- struct{}{}:
			default:
				// Earcon signal already pending; skip this one.
			}
		}
		err := streamMic(micCtx, conn, streamSID, &seqNum, onMicWarm)
		// naturalEnd: streamMic returned on its OWN (mic EOF = caller hangup), not
		// because something cancelled micCtx (Ctrl-C, or a server-initiated close via
		// the read loop's cancelMic).
		naturalEnd := micCtx.Err() == nil
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		// Caller hangup: notify the server with a stop frame, here (synchronously,
		// before micErrCh) so a server-initiated close — where naturalEnd is false and
		// the socket is already closed — never sends one (AATK-7). Ctrl-C's stop is
		// handled by the ctx.Done goroutine above.
		if naturalEnd {
			sendStop()
			cancelRead()
		}
		micErrCh <- err   // always send before cancelMic fires (defer is LIFO)
		close(micStopped) // mic (including its graceful drain) has fully returned
	}()

	// bytesSinceMark estimates the playout duration of the audio Twilio has
	// echoed back to us since the last mark, so a mark can be echoed once
	// that audio has (approximately) finished playing.
	var bytesSinceMark int

	// markWindowStart is when the current mark window opened -- the last mark,
	// or the start of the call. The echo delay is the audio received in the
	// window MINUS the time that window has already taken, because audio that
	// arrived paced at real time has already played by the time it is all in.
	// Without this term the estimate is the raw byte duration, which for a
	// long stream is wrong by the whole length of the stream.
	markWindowStart := time.Now()

	// serverSpoke records whether the server has sent any audio yet. It gates
	// the earcon: the tone means "your microphone is live", and playing it on
	// top of a server that is already talking steps on the server's own words.
	var serverSpoke bool

	// conn.Read blocks, so it cannot be select'd against directly. Pumping it
	// through its own goroutine into readCh lets the loop below select
	// between earconCh and the next inbound message — otherwise a signal sent
	// on earconCh while the loop is already parked in a blocking conn.Read
	// would sit unseen until the next iteration, which may never come.
	readCh := make(chan readResult)
	go func() {
		for {
			_, msg, err := conn.Read(readCtx)
			select {
			case readCh <- readResult{msg, err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	return dialReadLoop(readCtx, earconCh, readCh, player, recorder, conn, streamSID, &bytesSinceMark, &markWindowStart, &serverSpoke, cfg.noEchoMarks, cancelMic, micErrCh)
}

// readResult is one conn.Read outcome, pumped through readCh by dial's reader
// goroutine so dialReadLoop can select between it and earconCh.
type readResult struct {
	msg []byte
	err error
}

// tryPlayEarcon consumes one already-pending earcon signal, if any, without
// blocking, and plays the tone unless the server is speaking. Reports whether
// it consumed a signal -- which it does either way, so a suppressed tone is
// dropped rather than deferred to a moment that is no more appropriate.
func tryPlayEarcon(earconCh chan struct{}, player *lazyPlayer, serverSpoke *bool) bool {
	select {
	case <-earconCh:
		playEarconUnlessServerSpoke(player, serverSpoke)
		return true
	default:
		return false
	}
}

// playEarconUnlessServerSpoke is the one place the suppression rule lives.
//
// The earcon means "your microphone is being captured". Against a server that
// is waiting for the caller that is useful; against one that opens by playing
// something -- a recorded introduction, a greeting -- it is a beep on top of
// the words telling the caller what to do. Observed: an operator heard the
// tone land a second into a hundred-second introduction, which is both the
// wrong moment and the wrong message.
//
// Suppressed, not deferred. There is no later moment at which the tone becomes
// the right thing to play: by then capture has been live for a while, and the
// server's own audio is what tells the caller it is their turn.
func playEarconUnlessServerSpoke(player *lazyPlayer, serverSpoke *bool) {
	if serverSpoke != nil && *serverSpoke {
		log.Printf("twilio-cli: %s warm tone suppressed -- the server is speaking", frameSourceLabel)
		return
	}
	playEarcon(player)
}

// finishCallEnded is the shared teardown for every "call ended" exit:
// unblock the mic goroutine, wait for it to fully finish, then drain any
// earcon signal it sent before returning its error to the caller.
//
// The drain must happen strictly after <-micErrCh, not before: the mic
// goroutine sends on earconCh (if at all) before it ever reaches its
// final micErrCh send, so once micErrCh has unblocked, the mic
// goroutine's entire body — including any earconCh send — has already
// run. Draining earlier would race, because the real per-call socket (as
// opposed to our own readCtx cancellation) can close independently of
// mic-warmup timing, so a pending signal can otherwise still be sitting
// unsent at the moment call-ended is first detected.
func finishCallEnded(cause error, cancelMic context.CancelFunc, micErrCh <-chan error, earconCh chan struct{}, player *lazyPlayer, serverSpoke *bool) error {
	log.Printf("twilio-cli: call ended: %s", callEndReason(cause))
	cancelMic() // unblock goroutine before we wait for its result
	err := <-micErrCh
	tryPlayEarcon(earconCh, player, serverSpoke)
	return err // propagate hard mic failures to the caller
}

// dialReadLoop is dial's inbound frame loop: it plays earcon tones signaled
// by the mic goroutine and dispatches decoded frames until the call ends,
// then runs finishCallEnded to unwind the mic goroutine and report its
// result to the caller.
func dialReadLoop(readCtx context.Context, earconCh chan struct{}, readCh chan readResult, player *lazyPlayer, recorder *inboundRecorder, conn *websocket.Conn, streamSID string, bytesSinceMark *int, markWindowStart *time.Time, serverSpoke *bool, noEchoMarks bool, cancelMic context.CancelFunc, micErrCh <-chan error) error {
	for {
		// Give a pending earcon signal priority over call-ended detection: the
		// mic goroutine always sends on earconCh (if at all) strictly before
		// its naturalEnd path cancels readCtx, but once both are ready at
		// once the select below picks between them pseudo-randomly. Draining
		// earconCh first, in its own non-blocking check, makes that ordering
		// deterministic instead of a coin flip that can drop the tone.
		if tryPlayEarcon(earconCh, player, serverSpoke) {
			continue
		}

		select {
		case <-earconCh:
			// Mic goroutine signaled an earcon tone. Play it from the read loop's
			// goroutine context (the only context that owns lazyPlayer), and only
			// if the server is not already speaking -- see tryPlayEarcon.
			playEarconUnlessServerSpoke(player, serverSpoke)
			continue

		case <-readCtx.Done():
			// readCtx was cancelled out from under the reader goroutine (e.g.
			// the ctx.Done() teardown goroutine sent stop and cancelled readCtx
			// itself) before it could deliver a final result on readCh. Treat
			// this exactly like a call-ended read error.
			return finishCallEnded(context.Canceled, cancelMic, micErrCh, earconCh, player, serverSpoke)

		case r := <-readCh:
			if err, done := dialHandleReadResult(r, player, recorder, conn, streamSID, bytesSinceMark, markWindowStart, serverSpoke, noEchoMarks, cancelMic, micErrCh, earconCh); done {
				return err
			}
		}
	}
}

// dialHandleReadResult decodes and dispatches one inbound read result. done
// reports whether the call has ended (a hard read failure or a clean
// call-ended close, the latter already run through finishCallEnded) — in
// which case err is dialReadLoop's return value; otherwise the loop
// continues and err is always nil.
func dialHandleReadResult(r readResult, player *lazyPlayer, recorder *inboundRecorder, conn *websocket.Conn, streamSID string, bytesSinceMark *int, markWindowStart *time.Time, serverSpoke *bool, noEchoMarks bool, cancelMic context.CancelFunc, micErrCh <-chan error, earconCh chan struct{}) (err error, done bool) {
	if r.err != nil {
		if isCallEnded(r.err) {
			return finishCallEnded(r.err, cancelMic, micErrCh, earconCh, player, serverSpoke), true
		}
		return fmt.Errorf("read: %w", r.err), true
	}
	f, decErr := twilio.DecodeFrame(r.msg)
	if decErr != nil {
		// Log the bytes, not just the complaint. Every frame we *can* read
		// gets its raw JSON logged below; the one we cannot is where seeing
		// the wire matters most, and the error alone leaves you guessing
		// what actually arrived.
		log.Printf("twilio-cli: decode frame: %v", decErr)
		logCtlFrame("<-", r.msg)
		return nil, false
	}
	if f.Event != twilio.EventMedia {
		logCtlFrame("<-", r.msg)
	}
	handleFrame(f, player, recorder, conn, streamSID, bytesSinceMark, markWindowStart, serverSpoke, noEchoMarks)
	return nil, false
}

// isCallEnded reports whether an error means the call is simply over — so
// dial returns cleanly — rather than a failure the caller must hear about:
// a WebSocket close handshake, our own teardown, or the peer dropping the
// connection underneath us. It covers both directions: a read that finds the
// peer gone, and a write to a peer that already left.
//
// ECONNRESET belongs here alongside EOF. the server ends a call by closing its
// socket (CloseNow — see the server's WithCloseFunc) while twilio-cli is
// still writing mic frames at it, and writing to a closed socket draws a RST:
// the OS then reports "connection reset by peer" instead of a clean EOF.
// Which of the two surfaces is a race, so accepting only EOF made "the peer
// hung up" — the single most ordinary way a call ends — intermittently a hard
// error.
//
// EPIPE is the same event seen on a write instead of a read: the first write
// to a socket the peer has closed is accepted by the kernel, and only the
// next one reports the broken pipe. The opening handshake sends two frames
// (connected, then start), so a peer that closes on accept is reported to us
// precisely there — as EPIPE on the start write, not as EOF on a read.
func isCallEnded(err error) bool {
	return websocket.CloseStatus(err) != -1 ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// callEndReason renders an (already isCallEnded-classified) error as a plain
// outcome, so a normal hang-up reads as one rather than as a stack of library
// wording -- e.g. a server that force-stops and drops the socket surfaces as
// "failed to get reader: failed to read frame header: EOF", which looks like a
// failure but is just the peer closing. Unknown causes fall back to the raw
// error so nothing is hidden.
func callEndReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "stopped (Ctrl-C)"
	case websocket.CloseStatus(err) != -1:
		return "peer closed the stream"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, net.ErrClosed):
		return "server closed the connection"
	default:
		return err.Error()
	}
}

// markEchoDelay is how much longer the audio received in a mark window still
// needs in order to finish playing.
//
// A mark says "tell me when everything before this has played". The naive
// answer is the playout duration of the bytes received since the last mark --
// and that was the answer here, and it was wrong by the length of the stream.
// Audio does not arrive instantaneously: a server pacing its send at real time
// delivers a hundred seconds of audio over a hundred seconds, by the end of
// which all but the last moment of it has ALREADY played. Charging the full
// hundred seconds again put the echo a hundred seconds late.
//
// Measured: a server playing a 100 s introduction and then waiting 3.5 s for
// the echo timed out every single time, and read the timeout as the caller's
// player having gone missing.
//
// Subtracting elapsed makes the estimate what it claims to be: audio received
// minus audio already played. It floors at zero -- a window that took longer
// than its own audio has nothing left queued -- and it stays an estimate,
// since twilio-cli cannot see inside ffplay.
func markEchoDelay(bytesSinceMark int, elapsed time.Duration) time.Duration {
	remaining := mulawPlayoutDuration(bytesSinceMark) - elapsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

// mulawPlayoutDuration is how long n bytes of μ-law audio take to play out:
// 1 byte per sample at sampleRateHz.
func mulawPlayoutDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / sampleRateHz
}

// handleFrame dispatches a decoded inbound frame: media plays out and feeds
// the mark-echo delay estimate, mark triggers the delayed echo (unless
// suppressed), and clear is accepted with no further action.
//
// Every control-plane event is logged, inbound (<-) and outbound (->): they
// are rare, and each one says something about the call. Media is the data
// plane and is deliberately never logged per frame — at one frame per
// MuLawFrameMS it buries every line above it, which is exactly what made an
// earlier live debugging session unreadable. Its volume is reported on the
// next mark instead, which is the event that cares about it.
// errHandshakePeerGone reports that the peer hung up during the opening
// handshake. dial turns it into a clean return: a server that closes on
// accept ended the call, it did not fail.
var errHandshakePeerGone = errors.New("peer closed during handshake")

// writeHandshake sends one opening-handshake frame and logs it. A peer that
// has already hung up is not an error -- see isCallEnded, and note the
// handshake is where EPIPE surfaces, since it is the only place twilio-cli
// writes twice in a row with no read between.
func writeHandshake(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		if isCallEnded(err) {
			log.Printf("twilio-cli: call ended during handshake: %s", callEndReason(err))
			return errHandshakePeerGone
		}
		return fmt.Errorf("send handshake frame: %w", err)
	}
	logCtlFrame("->", msg)
	return nil
}

// ignoreHandshakeHangup collapses "the peer hung up mid-handshake" to a nil
// error -- dial's contract is that a call ending is not a failure -- while
// passing every real failure through.
func ignoreHandshakeHangup(err error) error {
	if errors.Is(err, errHandshakePeerGone) {
		return nil
	}
	return err
}

// logCtlFrame logs one control-plane frame exactly as it went over the wire,
// so the log reads as a transcript of the Twilio protocol as actually spoken
// and can be checked against Twilio's published message shapes. dir is "->"
// for a frame twilio-cli sent and "<-" for one it received.
//
// Media never reaches here. At 50 frames/sec each way it would bury the
// control plane, which is the only part worth reading (see
// TestHandleFrame_MediaIsNeverLogged).
func logCtlFrame(dir string, raw []byte) {
	log.Printf("twilio-cli: %s %s", dir, raw)
}

func handleFrame(f twilio.Frame, player *lazyPlayer, recorder *inboundRecorder, conn *websocket.Conn, streamSID string, bytesSinceMark *int, markWindowStart *time.Time, serverSpoke *bool, noEchoMarks bool) {
	switch f.Event {
	case twilio.EventMedia:
		// Stream media audio into the single player for continuous playback.
		// Recorded first: what arrived is a fact independent of whether the
		// player was in any state to render it.
		recorder.writeIn(f.Payload, time.Now())
		player.play(f.Payload)
		*bytesSinceMark += len(f.Payload)
		*serverSpoke = true

	case twilio.EventMark:
		// twilio-cli has no way to observe when its playback (piped to
		// ffplay) actually finishes rendering a given frame, so it
		// approximates playout duration from the byte count of the
		// mu-law audio (8 kHz, 1 byte/sample) received since the last
		// mark, and echoes the mark back after that estimated delay
		// (charter R17: approximate, not exact, playout-complete signal).
		delay := markEchoDelay(*bytesSinceMark, time.Since(*markWindowStart))
		log.Printf("twilio-cli: <- mark %q after %d bytes of audio over %s (echo in ~%s)",
			f.MarkName, *bytesSinceMark, time.Since(*markWindowStart).Round(time.Millisecond),
			delay.Round(time.Millisecond))
		*bytesSinceMark = 0
		*markWindowStart = time.Now()
		if noEchoMarks {
			log.Printf("twilio-cli: -- mark %q echo suppressed (--no-echo-marks)", f.MarkName)
			return
		}
		log.Printf("twilio-cli: -> mark %q echo scheduled in ~%s", f.MarkName, delay.Round(time.Millisecond))
		go echoMark(conn, streamSID, f.MarkName, delay)

	case twilio.EventClear:
		// Twilio buffer-flush signal; twilio-cli has no outbound audio
		// buffer to flush, so this is accepted and logged only.
		log.Printf("twilio-cli: <- clear (no outbound audio buffer to flush)")

	default:
		// start/stop/connected are client->server events; the server has no
		// reason to send one back. Log loudly rather than drop silently.
		log.Printf("twilio-cli: <- %s (unexpected control event, no handler)", f.Event)
	}
}

// echoMark sleeps for the estimated playout delay, then echoes the named
// mark back to conn. Run as its own goroutine so it doesn't block the read
// loop from processing further frames while it waits.
func echoMark(conn *websocket.Conn, streamSID, markName string, delay time.Duration) {
	time.Sleep(delay)
	echoMsg, err := twilio.EncodeMark(streamSID, markName)
	if err != nil {
		log.Printf("twilio-cli: encode mark echo: %v", err)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, echoMsg); err != nil {
		log.Printf("twilio-cli: send mark echo: %v", err)
		return
	}
	logCtlFrame("->", echoMsg)
}

func newSID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%x", prefix, b)
}
