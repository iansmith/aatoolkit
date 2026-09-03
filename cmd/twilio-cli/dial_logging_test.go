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

	"github.com/iansmith/aatoolkit/telephony"
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

// TestMuLawFrameSizesPlayOutAsExpected checks the CLI's own byte counts
// against telephony.MuLawDuration -- muLawFrame20ms is 20ms of playout and
// telephony.SampleRateHz is one second of it. It does not pin the
// conversion itself: every n here is a multiple of 8, so a truncate-to-
// whole-milliseconds reimplementation would still pass. That contract is
// pinned by TestMuLawDuration in the telephony package, which owns the
// function.
func TestMuLawFrameSizesPlayOutAsExpected(t *testing.T) {
	cases := []struct {
		name  string
		bytes int
		want  time.Duration
	}{
		{"nothing", 0, 0},
		{"one Twilio frame", muLawFrame20ms, 20 * time.Millisecond},
		{"one second", telephony.SampleRateHz, time.Second},
		{"two seconds", 16000, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telephony.MuLawDuration(tc.bytes); got != tc.want {
				t.Errorf("telephony.MuLawDuration(%d) = %s, want %s", tc.bytes, got, tc.want)
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
	audio := &callAudio{markWindowStart: time.Now(), filler: newPlayoutFiller(time.Now(), telephony.MuLawSilence)}

	out := captureLog(t, func() {
		for i := 0; i < frames; i++ {
			// conn is nil: a media frame must never touch the connection.
			handleFrame(twilio.Frame{Event: twilio.EventMedia, Payload: mkFrame(0x7f)},
				player, audio, nil, "MZtest", true)
		}
	})

	if !audio.serverSpoke {
		t.Error("serverSpoke stayed false after 50 media frames — the earcon gate reads this")
	}

	if out != "" {
		t.Errorf("media frames produced log output; want silence:\n%s", out)
	}
	if sink.writes != frames {
		t.Errorf("frames played: got %d, want %d — media must still reach the player", sink.writes, frames)
	}
	if want := frames * muLawFrame20ms; audio.bytesSinceMark != want {
		t.Errorf("bytesSinceMark: got %d, want %d", audio.bytesSinceMark, want)
	}
}

// TestHandleFrame_MarkLogsVolumeAndPlayout: the mark line carries the audio
// volume media itself never logs, and resets the counter for the next mark.
func TestHandleFrame_MarkLogsVolumeAndPlayout(t *testing.T) {
	player := testPlayer(&recordingSink{})
	// One second of audio handed to the player just now, so the whole of it is
	// still queued.
	//
	// The filler has to be TOLD -- it is what tracks outstanding audio now, and
	// setting bytesSinceMark alone no longer implies anything is queued. That is
	// the point of the change: bytes-since-mark counts what arrived, and the
	// mark echo depends on what has not yet played, which are different numbers
	// the moment the stream is not perfectly paced.
	audio := &callAudio{bytesSinceMark: telephony.SampleRateHz, markWindowStart: time.Now(), filler: newPlayoutFiller(time.Now(), telephony.MuLawSilence)}
	audio.filler.fed(make([]byte, telephony.SampleRateHz), time.Now())

	out := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventMark, MarkName: "farewell"},
			player, audio, nil, "MZtest", true) // noEchoMarks: no conn needed
	})

	for _, want := range []string{`mark "farewell"`, "8000 bytes", "1s"} {
		if !strings.Contains(out, want) {
			t.Errorf("mark log missing %q:\n%s", want, out)
		}
	}
	if audio.bytesSinceMark != 0 {
		t.Errorf("bytesSinceMark after mark: got %d, want 0 (reset for the next mark)", audio.bytesSinceMark)
	}
}

// TestHandleFrame_NoEchoMarksIsLoud: suppressing the echo is a deliberate
// test mode, so it says so rather than looking like a dropped mark.
func TestHandleFrame_NoEchoMarksIsLoud(t *testing.T) {
	player := testPlayer(&recordingSink{})
	audio := &callAudio{markWindowStart: time.Now(), filler: newPlayoutFiller(time.Now(), telephony.MuLawSilence)}

	out := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventMark, MarkName: "farewell"},
			player, audio, nil, "MZtest", true)
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

// TestHandleFrame_ClearFlushesQueuedPlayout is barge-in reaching the test
// harness.
//
// The server sends `clear` when the caller starts talking over the reply: stop
// playing it, the rest is abandoned. twilio-cli used to receive that and log
// it, on the premise that it had "no outbound audio buffer to flush" -- but a
// clear is not about the outbound direction at all. It flushes the client's
// playout of server audio, and twilio-cli's model of that playout is the
// filler. Leaving it alone leaves seconds of thrown-away reply charged against
// the next mark echo, so the server sits waiting on audio nobody will hear:
// the harness reports barge-in working when production would not.
//
// ffplay's own stdin pipe cannot be flushed in process (that would mean killing
// the player mid-call), so the filler's timing state is the whole of what
// honoring a clear can mean here -- and it is the part every downstream
// decision reads.
func TestHandleFrame_ClearFlushesQueuedPlayout(t *testing.T) {
	const frames = 50 // one second of reply, delivered in a burst
	sink := &recordingSink{}
	player := testPlayer(sink)
	audio := &callAudio{markWindowStart: time.Now(), filler: newPlayoutFiller(time.Now(), telephony.MuLawSilence)}

	for i := 0; i < frames; i++ {
		handleFrame(twilio.Frame{Event: twilio.EventMedia, Payload: mkFrame(0x7f)},
			player, audio, nil, "MZtest", true)
	}
	if audio.filler.outstanding(time.Now()) == 0 {
		t.Fatal("nothing queued before the clear — the test cannot tell a flush from a no-op")
	}
	playedBeforeClear := sink.writes

	out := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventClear}, player, audio, nil, "MZtest", true)
	})

	if got := audio.filler.outstanding(time.Now()); got != 0 {
		t.Errorf("outstanding after a clear = %s, want 0 — the abandoned reply is still "+
			"queued, so the next mark echo waits it out", got)
	}
	if audio.bytesSinceMark != 0 {
		t.Errorf("bytesSinceMark after a clear: got %d, want 0 — the flushed audio is "+
			"still counted as volume delivered since the last mark", audio.bytesSinceMark)
	}
	if sink.writes != playedBeforeClear {
		t.Errorf("player writes across the clear: got %d, want %d — a clear plays nothing",
			sink.writes, playedBeforeClear)
	}
	if !strings.Contains(out, "clear") {
		t.Errorf("clear frame was not logged:\n%s", out)
	}

	// The consequence the ticket exists for: a mark arriving right behind the
	// clear is echoed at once, because there is nothing left to wait for.
	markOut := captureLog(t, func() {
		handleFrame(twilio.Frame{Event: twilio.EventMark, MarkName: "farewell"},
			player, audio, nil, "MZtest", true)
	})
	if !strings.Contains(markOut, "0s still queued") {
		t.Errorf("mark after a clear did not report an empty queue:\n%s", markOut)
	}
}

// TestHandleFrame_OtherControlEventsAreLogged: clear is handled, and an event
// the server has no business sending back is logged loudly rather than dropped.
//
// The clear case here says only that the frame is reported; what it does to
// the playout is TestHandleFrame_ClearFlushesQueuedPlayout's. It used to want
// the old "no outbound audio buffer to flush" line, which pinned the bug as
// the contract -- and note the wanted substring is "flushed", not "flush",
// since the old line contained the latter and would have passed unchanged.
func TestHandleFrame_OtherControlEventsAreLogged(t *testing.T) {
	cases := []struct {
		name  string
		event twilio.EventType
		want  string
	}{
		{"clear", twilio.EventClear, "flushed"},
		{"unexpected start", twilio.EventStart, "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			player := testPlayer(&recordingSink{})
			audio := &callAudio{markWindowStart: time.Now(), filler: newPlayoutFiller(time.Now(), telephony.MuLawSilence)}

			out := captureLog(t, func() {
				handleFrame(twilio.Frame{Event: tc.event}, player, audio, nil, "MZtest", true)
			})

			if !strings.Contains(out, tc.want) {
				t.Errorf("%s frame: log missing %q:\n%s", tc.event, tc.want, out)
			}
		})
	}
}

// The mark-echo timing test that stood here asserted the wrong formula.
//
// TestMarkEchoDelay_SubtractsTimeAlreadySpent covered
// playout(bytesSinceMark) - elapsed, including a case
// ("slower than real time: still zero") that IS the broken shape: it measured
// elapsed from the previous mark, so silence arriving before the audio was
// charged against the audio and the mark was echoed while the burst was still
// queued. No case in it had idle time PRECEDING the audio, which is the only
// shape that tells the two formulas apart, so the test could not have failed.
//
// The quantity is now playoutFiller.outstanding, tested in filler_test.go by
// TestPlayoutFiller_OutstandingIsWhatHasNotPlayedYet -- including that case.
