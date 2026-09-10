package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// AATK-127. twilio-cli plays inbound audio to the speakers and captures the
// microphone at the same time. On a laptop the mic hears the speaker, so every
// sentence the server says is transcribed back to it as caller speech and it
// barges in on itself -- measured on a demo call where EVERY inbound turn was
// the server's own preceding sentence.
//
// The fix is half-duplex gating, not echo cancellation: while the player still
// has audio to render, the outbound frame carries mu-law silence instead of
// what the mic heard. These tests bind that contract at both levels -- the
// gate on its own, and a real dial() driving a real frame source.

// micGateLoud is a mu-law byte that is not silence, so a frame built from it is
// distinguishable from a gated one by inspection alone.
const micGateLoud = byte(0x00)

// loudFrame is one 20 ms frame of audible mu-law, the thing a gated call must
// never put on the wire while the server is speaking.
func loudFrame() []byte {
	return bytes.Repeat([]byte{micGateLoud}, muLawFrame20ms)
}

// isSilenceFrame reports whether payload is entirely mu-law silence. An empty
// payload is not silence: it is no frame at all, and the gate's contract is
// that cadence is unchanged.
func isSilenceFrame(payload []byte) bool {
	return len(payload) > 0 && bytes.Equal(payload, bytes.Repeat([]byte{telephony.MuLawSilence}, len(payload)))
}

// head is the first few bytes of a payload, for an error message that shows
// what arrived without printing a whole frame.
func head(p []byte) []byte {
	return p[:min(8, len(p))]
}

// --- the gate on its own ---

// TestMicGate_ShutSubstitutesSilenceKeepingFrameCount is observable 1 at the
// unit level: while the gate is shut every outbound frame is mu-law silence,
// and there are exactly as many of them, of exactly the same size, as an
// ungated call would have sent.
//
// Sending silence rather than nothing is load-bearing, not a stylistic choice:
// a server-side prologue paces its writes against inbound frames, so a client
// that simply stops sending stalls the introduction and then trips the
// server's read timeout. Only the content may change; never the cadence.
func TestMicGate_ShutSubstitutesSilenceKeepingFrameCount(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(time.Second))

	var sent [][]byte
	send := gate.wrap(func(p []byte) error {
		sent = append(sent, bytes.Clone(p))
		return nil
	})

	const frames = 5
	for range frames {
		if err := send(loudFrame()); err != nil {
			t.Fatalf("send through a shut gate: %v", err)
		}
	}

	if len(sent) != frames {
		t.Fatalf("frames sent through a shut gate: got %d, want %d -- the gate must change what is sent, never whether", len(sent), frames)
	}
	for i, p := range sent {
		if len(p) != muLawFrame20ms {
			t.Errorf("frame %d size: got %d, want %d", i, len(p), muLawFrame20ms)
		}
		if !isSilenceFrame(p) {
			t.Errorf("frame %d through a shut gate is not mu-law silence: %x", i, head(p))
		}
	}
}

// TestMicGate_OpenPassesCapturedFrames: a gate that has never been shut is not
// in the way. This is the state every call starts in, before the server has
// said anything.
func TestMicGate_OpenPassesCapturedFrames(t *testing.T) {
	gate := newMicGate()

	var got []byte
	send := gate.wrap(func(p []byte) error { got = bytes.Clone(p); return nil })
	if err := send(loudFrame()); err != nil {
		t.Fatalf("send through an open gate: %v", err)
	}
	if !bytes.Equal(got, loudFrame()) {
		t.Errorf("an open gate altered the captured frame: got %x", head(got))
	}
}

// TestMicGate_ReopensWhenTheDeadlinePasses: the gate is a deadline, not a
// latch. Once the player's queued audio plus the hangover has elapsed, the mic
// is live again without anyone having to reopen it.
func TestMicGate_ReopensWhenTheDeadlinePasses(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(-time.Millisecond))

	var got []byte
	send := gate.wrap(func(p []byte) error { got = bytes.Clone(p); return nil })
	if err := send(loudFrame()); err != nil {
		t.Fatalf("send after the deadline: %v", err)
	}
	if !bytes.Equal(got, loudFrame()) {
		t.Errorf("the gate stayed shut past its own deadline: got %x", head(got))
	}
}

// TestMicGate_OpenReopensAtOnce is observable 2 at the unit level: a Twilio
// clear means the reply the queued audio belongs to was abandoned, so the mic
// must come back on the next frame rather than wait out audio nobody will
// hear.
func TestMicGate_OpenReopensAtOnce(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(time.Second))
	gate.open()

	var got []byte
	send := gate.wrap(func(p []byte) error { got = bytes.Clone(p); return nil })
	if err := send(loudFrame()); err != nil {
		t.Fatalf("send after open: %v", err)
	}
	if !bytes.Equal(got, loudFrame()) {
		t.Errorf("open did not reopen the gate: got %x", head(got))
	}
}

// TestMicGate_NilGateIsFullDuplex: --full-duplex is the absence of a gate, not
// a flag the gate consults. An operator wearing headphones, or a run testing
// barge-in, gets the captured frames untouched.
func TestMicGate_NilGateIsFullDuplex(t *testing.T) {
	var gate *micGate

	var got []byte
	send := gate.wrap(func(p []byte) error { got = bytes.Clone(p); return nil })
	if err := send(loudFrame()); err != nil {
		t.Fatalf("send with no gate: %v", err)
	}
	if !bytes.Equal(got, loudFrame()) {
		t.Errorf("a nil gate altered the captured frame: got %x", head(got))
	}
}

// TestMicGate_SendErrorPropagates: the wrapper is a substitution, not a
// swallow. A write failure still ends the drain loop.
func TestMicGate_SendErrorPropagates(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(time.Second))

	want := os.ErrClosed
	send := gate.wrap(func([]byte) error { return want })
	if err := send(loudFrame()); err != want {
		t.Errorf("send error through a shut gate: got %v, want %v", err, want)
	}
}

// --- the gate through a real dial() ---

// loudAudioFile writes seconds of audible mu-law and returns its path, for use
// as the -audio frame source. The file source is the production path, so a
// test driving it exercises the same wrapping a mic call gets.
func loudAudioFile(t *testing.T, seconds float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loud.ulaw")
	n := int(seconds*float64(telephony.SampleRateHz)) / muLawFrame20ms * muLawFrame20ms
	if err := os.WriteFile(path, bytes.Repeat([]byte{micGateLoud}, n), 0o600); err != nil {
		t.Fatalf("write loud audio fixture: %v", err)
	}
	return path
}

// withLoudFrameSource points the streamMic seam at a file of audible mu-law,
// so every frame the client sends is loud unless something silenced it.
func withLoudFrameSource(t *testing.T, seconds float64) {
	t.Helper()
	withFakeMic(t, streamFileFrames(loudAudioFile(t, seconds)))
}

// sentFrame is one outbound media payload with the moment it was read.
type sentFrame struct {
	at      time.Time
	payload []byte
}

// silenceProbeServer is the server half of the dial-level gate tests: it
// completes the handshake, hands the connection to speak, and then reads the
// client's media frames until readFor has elapsed, timestamping each.
//
// speak must not block: reading starts only once it returns, and a read that
// starts late timestamps a backlog of frames at the moment it drained them
// rather than the moment they arrived. Anything that has to happen mid-call
// goes on its own goroutine.
//
// The frames are collected server-side rather than tapped inside the client on
// purpose -- the claim under test is about what goes on the wire.
func silenceProbeServer(t *testing.T, speak func(conn net.Conn), collected chan<- []sentFrame, readFor time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			collected <- nil
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // connected + start

		speak(conn)

		var frames []sentFrame
		deadline := time.Now().Add(readFor)
		_ = conn.SetReadDeadline(deadline.Add(time.Second))
		for time.Now().Before(deadline) {
			msg, err := readWSFrame(buf)
			if err != nil {
				break
			}
			f, err := twilio.DecodeFrame(msg)
			if err != nil || f.Event != twilio.EventMedia {
				continue
			}
			frames = append(frames, sentFrame{at: time.Now(), payload: f.Payload})
		}
		collected <- frames
	}))
	t.Cleanup(srv.Close)
	return srv
}

// framesIn returns the collected frames whose arrival falls in (from, to].
func framesIn(frames []sentFrame, from, to time.Time) []sentFrame {
	var out []sentFrame
	for _, f := range frames {
		if f.at.After(from) && !f.at.After(to) {
			out = append(out, f)
		}
	}
	return out
}

// runGatedDial drives dial() against srv with a fake player, returning once the
// call has ended.
func runGatedDial(t *testing.T, srv *httptest.Server, opts ...dialOption) {
	t.Helper()
	origNewPlayerFunc := newPlayerFunc
	t.Cleanup(func() { newPlayerFunc = origNewPlayerFunc })
	newPlayerFunc = func(context.Context) (*audioPlayer, error) {
		return newPlayerWithSink(&recordingSink{}), nil
	}
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")
	if err := dial(dialCtx(t, 15*time.Second), newSID("CA"), addr, opts...); err != nil {
		t.Errorf("dial: %v", err)
	}
}

// serverSpeaks writes one media frame carrying d of mu-law audio and reports
// when it went out.
func serverSpeaks(t *testing.T, conn net.Conn, streamSID string, d time.Duration) time.Time {
	t.Helper()
	n := int(d.Seconds()*float64(telephony.SampleRateHz)) / muLawFrame20ms * muLawFrame20ms
	msg, err := twilio.EncodeMedia(streamSID, bytes.Repeat([]byte{micGateLoud}, n))
	if err != nil {
		t.Errorf("EncodeMedia: %v", err)
		return time.Now()
	}
	if err := writeWSText(conn, msg); err != nil {
		t.Errorf("write media: %v", err)
	}
	return time.Now()
}

// TestDial_MicIsGatedWhileTheServerIsSpeaking is observable 1 end to end: for
// the second of audio the server hands over, every frame the client puts on
// the wire is mu-law silence -- and the frames keep coming at the same rate, so
// a prologue pacing on them is unaffected. After the audio has played out (plus
// the hangover for room decay) the mic is live again.
func TestDial_MicIsGatedWhileTheServerIsSpeaking(t *testing.T) {
	withLoudFrameSource(t, 5)

	const streamSID = "SS_gate"
	spokeAt := make(chan time.Time, 1)
	collected := make(chan []sentFrame, 1)
	srv := silenceProbeServer(t, func(conn net.Conn) {
		spokeAt <- serverSpeaks(t, conn, streamSID, time.Second)
	}, collected, 2*time.Second)

	runGatedDial(t, srv)

	spoke := <-spokeAt
	frames := <-collected

	// The window starts a beat after the audio was written so a frame already
	// in flight when it arrived is not held against the gate, and ends a beat
	// before playout completes.
	gated := framesIn(frames, spoke.Add(100*time.Millisecond), spoke.Add(900*time.Millisecond))
	if len(gated) < 30 {
		t.Fatalf("frames sent during the server's second of speech: got %d, want >= 30 -- the cadence must be unchanged, only the content", len(gated))
	}
	for _, f := range gated {
		if !isSilenceFrame(f.payload) {
			t.Errorf("the mic was live %s into the server's speech: frame is not silence (%x)",
				f.at.Sub(spoke).Round(time.Millisecond), head(f.payload))
			break
		}
	}

	// Past the audio's playout plus the hangover, the mic comes back on its own.
	after := framesIn(frames, spoke.Add(1400*time.Millisecond), spoke.Add(2*time.Second))
	if len(after) == 0 {
		t.Fatal("no frames after the gate should have reopened")
	}
	for _, f := range after {
		if !isSilenceFrame(f.payload) {
			return
		}
	}
	t.Errorf("the gate never reopened: all %d frames after playout were still silence", len(after))
}

// TestDial_ClearReopensTheMicAtOnce is observable 2: on barge-in the server
// abandons the rest of the reply and sends a clear, so the queued audio will
// never be heard. The mic must come back at once rather than wait out a playout
// that is not going to happen -- otherwise the caller's barge-in is the one
// thing the harness cannot record.
func TestDial_ClearReopensTheMicAtOnce(t *testing.T) {
	withLoudFrameSource(t, 5)

	const streamSID = "SS_gateclear"
	spokeAt := make(chan time.Time, 1)
	clearedAt := make(chan time.Time, 1)
	collected := make(chan []sentFrame, 1)
	srv := silenceProbeServer(t, func(conn net.Conn) {
		spokeAt <- serverSpeaks(t, conn, streamSID, 2*time.Second)
		// On its own goroutine so the frame reader starts now -- see
		// silenceProbeServer on why a late reader mistimes its frames.
		go func() {
			time.Sleep(300 * time.Millisecond)
			msg, err := twilio.EncodeClear(streamSID)
			if err != nil {
				t.Errorf("EncodeClear: %v", err)
			} else if err := writeWSText(conn, msg); err != nil {
				t.Errorf("write clear: %v", err)
			}
			clearedAt <- time.Now()
		}()
	}, collected, time.Second)

	runGatedDial(t, srv)

	spoke := <-spokeAt
	cleared := <-clearedAt
	frames := <-collected

	// Non-vacuity: the gate really was shut before the clear arrived.
	before := framesIn(frames, spoke.Add(100*time.Millisecond), cleared.Add(-40*time.Millisecond))
	if len(before) == 0 {
		t.Fatal("no frames between the server's audio and the clear")
	}
	for _, f := range before {
		if !isSilenceFrame(f.payload) {
			t.Fatalf("the mic was already live %s before the clear -- this test proves nothing about clear",
				cleared.Sub(f.at).Round(time.Millisecond))
		}
	}

	// Two seconds of audio were queued; the clear discards them, so the mic is
	// live again within a few frames rather than 2.25 s later.
	after := framesIn(frames, cleared, cleared.Add(200*time.Millisecond))
	if len(after) == 0 {
		t.Fatal("no frames after the clear")
	}
	for _, f := range after {
		if !isSilenceFrame(f.payload) {
			return
		}
	}
	t.Errorf("the clear did not reopen the mic: all %d frames in the 200ms after it were still silence", len(after))
}

// TestDial_FullDuplexSendsCapturedFrames is observable 3: with --full-duplex
// the captured frames go up throughout, including while the server is
// speaking. Barge-in is a real behaviour the server has to be tested for, and
// with the gate shut it cannot be exercised at all.
func TestDial_FullDuplexSendsCapturedFrames(t *testing.T) {
	withLoudFrameSource(t, 5)

	const streamSID = "SS_fullduplex"
	spokeAt := make(chan time.Time, 1)
	collected := make(chan []sentFrame, 1)
	srv := silenceProbeServer(t, func(conn net.Conn) {
		spokeAt <- serverSpeaks(t, conn, streamSID, time.Second)
	}, collected, time.Second)

	runGatedDial(t, srv, withFullDuplex())

	spoke := <-spokeAt
	frames := <-collected

	during := framesIn(frames, spoke.Add(100*time.Millisecond), spoke.Add(900*time.Millisecond))
	if len(during) < 30 {
		t.Fatalf("frames sent during the server's speech: got %d, want >= 30", len(during))
	}
	for _, f := range during {
		if isSilenceFrame(f.payload) {
			t.Fatalf("--full-duplex silenced a frame %s into the server's speech", f.at.Sub(spoke).Round(time.Millisecond))
		}
	}
}

// TestDial_MarkEchoArrivesWithTheMicGated is observable 4 in miniature: the
// server's paced write followed by a mark still gets its echo with the gate
// shut for the whole clip. The gate changes the content of outbound media and
// nothing else -- not the frame cadence the server paces on, and not the
// control plane.
func TestDial_MarkEchoArrivesWithTheMicGated(t *testing.T) {
	withLoudFrameSource(t, 3)

	const streamSID = "SS_gatemark"
	const markName = "gated-mark"
	echoed := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			close(echoed)
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf)

		serverSpeaks(t, conn, streamSID, 300*time.Millisecond)
		markMsg, err := twilio.EncodeMark(streamSID, markName)
		if err != nil {
			t.Errorf("EncodeMark: %v", err)
			close(echoed)
			return
		}
		if err := writeWSText(conn, markMsg); err != nil {
			t.Errorf("write mark: %v", err)
		}
		waitForMarkEcho(t, buf, echoed)
	}))
	t.Cleanup(srv.Close)

	runGatedDial(t, srv)

	name, ok := <-echoed
	if !ok || name != markName {
		t.Errorf("mark echo: got %q (ok=%v), want %q -- gating outbound media must not stop the control plane", name, ok, markName)
	}
}

// waitForMarkEcho reads client frames until a mark echo arrives or the
// connection is done, publishing the echoed name on echoed exactly once.
func waitForMarkEcho(t *testing.T, buf *bufio.ReadWriter, echoed chan<- string) {
	t.Helper()
	defer close(echoed)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := readWSFrame(buf)
		if err != nil {
			return
		}
		f, err := twilio.DecodeFrame(msg)
		if err != nil {
			continue
		}
		if f.Event == twilio.EventMark {
			echoed <- f.MarkName
			return
		}
	}
}
