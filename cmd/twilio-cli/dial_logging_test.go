package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// captureLog redirects the standard logger into a buffer for the duration of
// fn and returns everything logged. log's output is process-global, so these
// tests must not run in parallel with anything else that logs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	origOut, origFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	fn()
	return buf.String()
}

// testPlayer builds a lazyPlayer that writes into sink instead of spawning a
// real ffplay process.
func testPlayer(sink io.WriteCloser) *lazyPlayer {
	return &lazyPlayer{
		newPlayer: func(context.Context) (*audioPlayer, error) { return newPlayerWithSink(sink), nil },
		ctx:       context.Background(),
	}
}

// TestIsCallEnded pins which read errors mean "the call is over" (dial
// returns nil) versus a real failure. The reset cases matter most: the server
// ends a call by closing its socket while twilio-cli is still writing mic
// frames at it, so the peer's close surfaces as ECONNRESET rather than EOF
// depending on timing — and treating that as an error made an ordinary
// hangup fail intermittently.
func TestIsCallEnded(t *testing.T) {
	// The real shape coder/websocket produces on a reset: the syscall error
	// wrapped by net.OpError, wrapped again by the library's own context.
	wrappedReset := fmt.Errorf("failed to get reader: failed to read frame header: %w",
		&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)})

	// The shape a write to a hung-up peer produces: the handshake's second
	// frame (start) is where this lands, since the kernel accepts the first
	// write to a closed socket and only reports EPIPE on the next one.
	wrappedPipe := fmt.Errorf("failed to write msg: failed to write frame: failed to flush: %w",
		&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"clean EOF", io.EOF, true},
		{"truncated read", io.ErrUnexpectedEOF, true},
		{"our own teardown", context.Canceled, true},
		{"peer reset the connection", syscall.ECONNRESET, true},
		{"peer reset, as the ws library reports it", wrappedReset, true},
		{"our conn already closed", net.ErrClosed, true},
		{"peer closed before our next write", syscall.EPIPE, true},
		{"broken pipe, as the ws library reports it", wrappedPipe, true},
		{"a genuine failure", errors.New("protocol violation"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCallEnded(tc.err); got != tc.want {
				t.Errorf("isCallEnded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMulawPlayoutDuration(t *testing.T) {
	cases := []struct {
		name  string
		bytes int
		want  time.Duration
	}{
		{"nothing", 0, 0},
		{"one Twilio frame", muLawFrame20ms, 20 * time.Millisecond},
		{"one second", sampleRateHz, time.Second},
		{"two seconds", 16000, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mulawPlayoutDuration(tc.bytes); got != tc.want {
				t.Errorf("mulawPlayoutDuration(%d) = %s, want %s", tc.bytes, got, tc.want)
			}
		})
	}
}

// TestHandleFrame_MediaIsNeverLogged pins the control-plane-only logging
// rule. Media arrives once per 20ms — 50 lines/sec — and logging it per frame
// buried every control event behind a wall of identical lines during a live
// debugging session. Media must still play, and its volume is reported on the
// next mark instead.
func TestHandleFrame_MediaIsNeverLogged(t *testing.T) {
	const frames = 50
	sink := &recordingSink{}
	player := testPlayer(sink)
	var bytesSinceMark int

	out := captureLog(t, func() {
		for i := 0; i < frames; i++ {
			// conn is nil: a media frame must never touch the connection.
			handleFrame(twilio.Frame{Event: twilio.EventMedia, Payload: mkFrame(0x7f)},
				player, nil, "MZtest", &bytesSinceMark, true)
		}
	})

	if out != "" {
		t.Errorf("media frames produced log output; want silence:\n%s", out)
	}
	if sink.writes != frames {
		t.Errorf("frames played: got %d, want %d — media must still reach the player", sink.writes, frames)
	}
	if want := frames * muLawFrame20ms; bytesSinceMark != want {
		t.Errorf("bytesSinceMark: got %d, want %d", bytesSinceMark, want)
	}
}

// TestHandleFrame_MarkLogsVolumeAndPlayout: the mark line carries the audio
// volume media itself never logs, and resets the counter for the next mark.
func TestHandleFrame_MarkLogsVolumeAndPlayout(t *testing.T) {
	player := testPlayer(&recordingSink{})
	bytesSinceMark := sampleRateHz // exactly 1s of μ-law audio

	out := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventMark, MarkName: "farewell"},
			player, nil, "MZtest", &bytesSinceMark, true) // noEchoMarks: no conn needed
	})

	for _, want := range []string{`mark "farewell"`, "8000 bytes", "1s"} {
		if !strings.Contains(out, want) {
			t.Errorf("mark log missing %q:\n%s", want, out)
		}
	}
	if bytesSinceMark != 0 {
		t.Errorf("bytesSinceMark after mark: got %d, want 0 (reset for the next mark)", bytesSinceMark)
	}
}

// TestHandleFrame_NoEchoMarksIsLoud: suppressing the echo is a deliberate
// test mode, so it says so rather than looking like a dropped mark.
func TestHandleFrame_NoEchoMarksIsLoud(t *testing.T) {
	player := testPlayer(&recordingSink{})
	var bytesSinceMark int

	out := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventMark, MarkName: "farewell"},
			player, nil, "MZtest", &bytesSinceMark, true)
	})

	if !strings.Contains(out, "suppressed") {
		t.Errorf("--no-echo-marks did not log that the echo was suppressed:\n%s", out)
	}
}

// TestLogCtlFrame_EmitsRawJSONVerbatim: the control-plane transcript is only
// useful for checking conformance if it shows the bytes that actually went
// over the wire, unsummarized and unmangled — a paraphrase can agree with a
// wrong frame. Both directions are marked so a send is never read as a
// receive.
func TestLogCtlFrame_EmitsRawJSONVerbatim(t *testing.T) {
	start, err := twilio.EncodeStart("MZtest", "CAtest", "ACtest", 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}

	out := captureLog(t, func() {
		logCtlFrame("->", start)
		logCtlFrame("<-", []byte(`{"event":"mark","streamSid":"MZtest","mark":{"name":"farewell"}}`))
	})

	if !strings.Contains(out, string(start)) {
		t.Errorf("sent frame's raw JSON not logged verbatim.\nwant substring: %s\ngot:\n%s", start, out)
	}
	if !strings.Contains(out, `-> {"event":"start"`) {
		t.Errorf("sent frame not marked as outbound:\n%s", out)
	}
	if !strings.Contains(out, `<- {"event":"mark"`) {
		t.Errorf("received frame not marked as inbound:\n%s", out)
	}
}

// TestHandleFrame_OtherControlEventsAreLogged: clear is handled, and an event
// the server has no business sending back is logged loudly rather than dropped.
func TestHandleFrame_OtherControlEventsAreLogged(t *testing.T) {
	cases := []struct {
		name  string
		event twilio.EventType
		want  string
	}{
		{"clear", twilio.EventClear, "clear"},
		{"unexpected start", twilio.EventStart, "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			player := testPlayer(&recordingSink{})
			var bytesSinceMark int

			out := captureLog(t, func() {
				handleFrame(twilio.Frame{Event: tc.event}, player, nil, "MZtest", &bytesSinceMark, true)
			})

			if !strings.Contains(out, tc.want) {
				t.Errorf("%s frame: log missing %q:\n%s", tc.event, tc.want, out)
			}
		})
	}
}
