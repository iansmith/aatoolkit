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

// streamMic is the mic-capture entry point dial() calls through. Overridable
// in tests to simulate capture completion (EOF) deterministically, since real
// mic capture has no natural EOF to trigger from a test.
var streamMic = streamMicFrames

// dialOptions configures optional dial() behavior.
type dialOptions struct {
	noEchoMarks bool
}

// dialOption configures dialOptions.
type dialOption func(*dialOptions)

// withNoEchoMarks suppresses mark-echo behavior (see --no-echo-marks in
// main.go) so the server's AwaitingMarkEcho state hits its timeout path
// instead of receiving an echo.
func withNoEchoMarks() dialOption {
	return func(o *dialOptions) { o.noEchoMarks = true }
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

	micCtx, cancelMic := context.WithCancel(ctx)
	defer cancelMic() // fires before CloseNow (LIFO); signals goroutine to stop

	streamSID := newSID("MZ")

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
			// seqNum placeholder 0 — real per-call sequenceNumber counter wired by SOP-142.
			stopMsg, err := twilio.EncodeStop(streamSID, callSid, defaultAccountSid, 0)
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
	// micDone fires only when the mic goroutine stops on its own (caller
	// hangup / capture EOF) rather than because something else cancelled
	// micCtx first (Ctrl-C or a server-initiated close, both already handled
	// via their own paths below). This lets caller hangup trigger the same
	// stop-frame send as Ctrl-C without also firing it redundantly when the
	// server already closed the connection.
	micDone := make(chan struct{}, 1)

	go func() {
		<-ctx.Done()
		sendStop()
		cancelRead()
	}()
	go func() {
		select {
		case <-micDone:
			sendStop()
			cancelRead()
		case <-readCtx.Done():
		}
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

	// seqNum placeholder 1 — real per-call sequenceNumber counter wired by SOP-142.
	startMsg, err := twilio.EncodeStart(streamSID, callSid, defaultAccountSid, 1)
	if err != nil {
		return fmt.Errorf("encode start: %w", err)
	}
	if err := writeHandshake(ctx, conn, startMsg); err != nil {
		return ignoreHandshakeHangup(err)
	}
	log.Printf("twilio-cli: connected to %s, streaming mic (Ctrl-C to stop)", addr)

	micErrCh := make(chan error, 1)
	go func() {
		defer cancelMic() // goroutine exit cancels the read loop
		err := streamMic(micCtx, conn, streamSID)
		naturalEnd := micCtx.Err() == nil // stopped on its own, not cancelled
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		micErrCh <- err // always send before cancelMic fires (defer is LIFO)
		if naturalEnd {
			micDone <- struct{}{}
		}
	}()

	// bytesSinceMark estimates the playout duration of the audio Twilio has
	// echoed back to us since the last mark, so a mark can be echoed once
	// that audio has (approximately) finished playing.
	var bytesSinceMark int

	for {
		_, msg, err := conn.Read(readCtx)
		if err != nil {
			if isCallEnded(err) {
				log.Printf("twilio-cli: call ended: %v", err)
				cancelMic()       // unblock goroutine before we wait for its result
				return <-micErrCh // propagate hard mic failures to the caller
			}
			return fmt.Errorf("read: %w", err)
		}
		f, decErr := twilio.DecodeFrame(msg)
		if decErr != nil {
			// Log the bytes, not just the complaint. Every frame we *can*
			// read gets its raw JSON logged below; the one we cannot is
			// where seeing the wire matters most, and the error alone
			// leaves you guessing what actually arrived.
			log.Printf("twilio-cli: decode frame: %v", decErr)
			logCtlFrame("<-", msg)
			continue
		}
		if f.Event != twilio.EventMedia {
			logCtlFrame("<-", msg)
		}
		handleFrame(f, player, conn, streamSID, &bytesSinceMark, cfg.noEchoMarks)
	}
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
			log.Printf("twilio-cli: call ended during handshake: %v", err)
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

func handleFrame(f twilio.Frame, player *lazyPlayer, conn *websocket.Conn, streamSID string, bytesSinceMark *int, noEchoMarks bool) {
	switch f.Event {
	case twilio.EventMedia:
		// Stream media audio into the single player for continuous playback.
		player.play(f.Payload)
		*bytesSinceMark += len(f.Payload)

	case twilio.EventMark:
		// twilio-cli has no way to observe when its playback (piped to
		// ffplay) actually finishes rendering a given frame, so it
		// approximates playout duration from the byte count of the
		// mu-law audio (8 kHz, 1 byte/sample) received since the last
		// mark, and echoes the mark back after that estimated delay
		// (charter R17: approximate, not exact, playout-complete signal).
		delay := mulawPlayoutDuration(*bytesSinceMark)
		log.Printf("twilio-cli: <- mark %q after %d bytes of audio (~%s playout)",
			f.MarkName, *bytesSinceMark, delay.Round(time.Millisecond))
		*bytesSinceMark = 0
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
