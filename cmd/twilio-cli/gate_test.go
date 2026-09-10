package main

import (
	"bufio"
	"bytes"
	"net"
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
	return loudMuLaw(telephony.MuLawDuration(muLawFrame20ms))
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

	// Counted at the socket, not at the gate: `append` runs once per iteration
	// whatever gated returns, so counting the slice would pass against a gate
	// that returned nil. What the cadence claim is about is frames reaching
	// write, which is what mediaFrameSender puts them through.
	const frames = 5
	var sent [][]byte
	written := 0
	seqNum := 1
	send := mediaFrameSender(newMediaFrameEncoder("MZ_cadence", &seqNum), nil, gate, func([]byte) error {
		written++
		return nil
	})
	for range frames {
		payload := loudFrame()
		sent = append(sent, gate.gated(payload))
		if err := send(payload); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	if written != frames {
		t.Fatalf("frames written through a shut gate: got %d, want %d -- the gate must change what is sent, never whether", written, frames)
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

	if got := gate.gated(loudFrame()); !bytes.Equal(got, loudFrame()) {
		t.Errorf("an open gate altered the captured frame: got %x", head(got))
	}
}

// TestMicGate_ReopensWhenTheDeadlinePasses: the gate is a deadline, not a
// latch. Once the player's queued audio plus the hangover has elapsed, the mic
// is live again without anyone having to reopen it.
//
// The horizon is two hangovers in the past, not one millisecond: shutUntil
// adds the hangover to whatever horizon it is given, so a horizon that has
// only just passed is still a gate that is legitimately shut.
func TestMicGate_ReopensWhenTheDeadlinePasses(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(-2 * micGateHangover))

	if got := gate.gated(loudFrame()); !bytes.Equal(got, loudFrame()) {
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

	if got := gate.gated(loudFrame()); !bytes.Equal(got, loudFrame()) {
		t.Errorf("open did not reopen the gate: got %x", head(got))
	}
}

// TestMicGate_NilGateIsFullDuplex: --full-duplex is the absence of a gate, not
// a flag the gate consults. An operator wearing headphones, or a run testing
// barge-in, gets the captured frames untouched.
func TestMicGate_NilGateIsFullDuplex(t *testing.T) {
	var gate *micGate

	if got := gate.gated(loudFrame()); !bytes.Equal(got, loudFrame()) {
		t.Errorf("a nil gate altered the captured frame: got %x", head(got))
	}
}

// TestMicGate_NilGateTakesTheWholeProtocol: --full-duplex is a nil gate, and
// the feed path publishes a deadline on every frame the player is handed --
// including the earcon's. Every method has to tolerate nil, not just the two
// the frame source calls, or the flag turns a demo call into a panic on the
// first thing the server says.
func TestMicGate_NilGateTakesTheWholeProtocol(t *testing.T) {
	var gate *micGate

	gate.shutUntil(time.Now().Add(time.Second))
	gate.open()
	if gate.shut() {
		t.Error("a nil gate reported itself shut -- there is no gate to shut")
	}
}

// TestMediaFrameSender_GatedSendErrorPropagates: gating substitutes a payload,
// it does not swallow a failure. A write that fails on a gated frame still
// ends the drain loop, exactly as an ungated one does.
func TestMediaFrameSender_GatedSendErrorPropagates(t *testing.T) {
	gate := newMicGate()
	gate.shutUntil(time.Now().Add(time.Second))

	want := os.ErrClosed
	seqNum := 1
	send := mediaFrameSender(newMediaFrameEncoder("MZ_gatederr", &seqNum), nil, gate, func([]byte) error { return want })
	if err := send(loudFrame()); err != want {
		t.Errorf("write error on a gated frame: got %v, want %v", err, want)
	}
}

// TestCallAudio_FedPublishesThePlayoutHorizon pins the join between the filler
// and the gate in microseconds, from a supplied clock.
//
// Everything else about this decision is only observable through the
// dial-level tests below, which take seconds of wall clock to say it and say
// it about the whole call. The specific claims here are the ones dial.go's
// callAudio.fed argues for at length and nothing asserts: the horizon is the
// end of the queued playout plus exactly one hangover; a burst that arrives
// faster than real time queues behind what is already there rather than
// restarting from now; and an idle stretch is not charged against the gate,
// because filler.fed clamps forward before the horizon is read.
func TestCallAudio_FedPublishesThePlayoutHorizon(t *testing.T) {
	// One second of playout, so the arithmetic below is readable.
	const playout = time.Second
	audio := &callAudio{gate: newMicGate()}

	start := time.Now()
	audio.filler = newPlayoutFiller(start, telephony.MuLawSilence)
	audio.fed(loudMuLaw(playout), start)

	if got, want := gateDeadline(audio.gate), start.Add(playout+micGateHangover); !got.Equal(want) {
		t.Errorf("horizon after one feed: got %s, want %s (playout + one hangover)",
			got.Sub(start).Round(time.Millisecond), want.Sub(start).Round(time.Millisecond))
	}

	// A second burst arriving mid-playout queues behind the first: the horizon
	// moves by the new audio's length, not to now plus its length. A gate that
	// took `now` as its base would reopen while the first burst was still
	// playing -- the exact double-count filler.fed's clamp exists to prevent.
	mid := start.Add(playout / 2)
	audio.fed(loudMuLaw(playout), mid)
	if got, want := gateDeadline(audio.gate), start.Add(2*playout+micGateHangover); !got.Equal(want) {
		t.Errorf("horizon after a burst mid-playout: got %s, want %s (both bursts, then one hangover)",
			got.Sub(start).Round(time.Millisecond), want.Sub(start).Round(time.Millisecond))
	}

	// A clear abandons all of it, whatever was standing.
	audio.flush(mid)
	if audio.gate.shut() {
		t.Error("the gate was still shut after a clear: a flush must reopen the mic at once")
	}

	// And an idle stretch is not gated from an instant already past: after
	// silence, the horizon is measured from the feed, not from the stale
	// fedThrough the filler was carrying.
	late := start.Add(10 * playout)
	audio.fed(loudMuLaw(playout), late)
	if got, want := gateDeadline(audio.gate), late.Add(playout+micGateHangover); !got.Equal(want) {
		t.Errorf("horizon after an idle stretch: got %s, want %s (measured from the feed, not from fedThrough)",
			got.Sub(late).Round(time.Millisecond), want.Sub(late).Round(time.Millisecond))
	}
}

// gateDeadline reads back the instant a gate is shut until, so a test can
// assert on the published horizon rather than on whether time.Now happens to
// have passed it.
func gateDeadline(g *micGate) time.Time {
	return time.Unix(0, g.shutUntilNanos.Load())
}

// --- the gate through a real dial() ---

// loudMuLaw is d of audible mu-law, truncated to whole 20 ms frames. Both
// directions of these tests want the same thing -- the caller's fixture file
// and the server's spoken burst -- so the frame arithmetic has one spelling.
func loudMuLaw(d time.Duration) []byte {
	frames := int(d / telephony.MuLawDuration(muLawFrame20ms))
	return bytes.Repeat([]byte{micGateLoud}, frames*muLawFrame20ms)
}

// withLoudFrameSource points the streamMic seam at a file of audible mu-law,
// so every frame the client sends is loud unless something silenced it. The
// file source is the production path, so a test driving it gates exactly as a
// mic call does.
func withLoudFrameSource(t *testing.T, d time.Duration) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loud.ulaw")
	if err := os.WriteFile(path, loudMuLaw(d), 0o600); err != nil {
		t.Fatalf("write loud audio fixture: %v", err)
	}
	withFakeMic(t, streamFileFrames(path))
}

// earconPlayout is how long the capture-live tone occupies the player, and so
// how long it holds the gate shut. Derived from the tone itself rather than
// restating earconDurationMS, which is its one definition.
func earconPlayout() time.Duration {
	return telephony.MuLawDuration(len(generateEarcon()))
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
	return hijackedWSServer(t, func(conn net.Conn, buf *bufio.ReadWriter) {
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
	})
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
	withFakePlayer(t)
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")
	if err := dial(dialCtx(t, 15*time.Second), newSID("CA"), addr, opts...); err != nil {
		t.Errorf("dial: %v", err)
	}
}

// serverSpeaks writes one media frame carrying d of mu-law audio and reports
// when it went out.
func serverSpeaks(t *testing.T, conn net.Conn, streamSID string, d time.Duration) time.Time {
	t.Helper()
	msg, err := twilio.EncodeMedia(streamSID, loudMuLaw(d))
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
	// Every window below is derived from these two and from micGateHangover,
	// never written out as an absolute. A test that hard-codes "silence until
	// 1250ms" passes or fails on arithmetic done by hand at authoring time:
	// retune the hangover and it goes red while the mechanism is perfectly
	// correct, which is a test asserting on its own margins rather than on the
	// gate.
	const speech = time.Second
	// margin is the one free knob here, and what it is covering is the client's
	// own latency: the server timestamps the instant it *wrote* the media,
	// while the gate shuts only once the read loop has been scheduled, played
	// the frame and published the deadline. Nothing in the mechanism bounds
	// that, so the windows below are pulled in by margin at both ends. 100 ms
	// was too thin -- a heavily contended machine was measured letting a frame
	// through 112 ms in -- and the sibling test's non-vacuity check declines to
	// assume any figure at all for the same latency (see
	// TestDial_ClearReopensTheMicAtOnce). 250 ms is that measurement with room,
	// and it costs only a shorter assertion window.
	const margin = 250 * time.Millisecond

	withLoudFrameSource(t, 5*time.Second)

	const streamSID = "SS_gate"
	spokeAt := make(chan time.Time, 1)
	collected := make(chan []sentFrame, 1)
	srv := silenceProbeServer(t, func(conn net.Conn) {
		// Speak from a goroutine, and not immediately. This call opens with
		// the capture-live earcon -- the server is quiet here, so nothing
		// suppresses it -- which is real bytes into the same player
		// and so gates the mic for its own tone plus the hangover; waiting
		// that out first leaves the deadline this test measures derived from
		// the speech alone. The delay runs on its own goroutine so the frame
		// reader starts now -- see silenceProbeServer.
		go func() {
			time.Sleep(earconPlayout() + micGateHangover + margin)
			spokeAt <- serverSpeaks(t, conn, streamSID, speech)
		}()
	}, collected, earconPlayout()+micGateHangover+speech+2*micGateHangover+4*margin)

	runGatedDial(t, srv)

	spoke := <-spokeAt
	frames := <-collected

	// playoutEnd is when the handed-over audio runs out; reopenAt is when the
	// gate is due to open. Everything else is one of the three intervals those
	// two define.
	playoutEnd := spoke.Add(speech)
	reopenAt := playoutEnd.Add(micGateHangover)

	// Shut for the speech. The window starts a beat after the audio was
	// written, so a frame already in flight when it arrived is not held
	// against the gate, and ends a beat before playout completes.
	gated := framesIn(frames, spoke.Add(margin), playoutEnd.Add(-margin))
	wantGated := int((speech - 2*margin) / telephony.MuLawDuration(muLawFrame20ms) * 3 / 4)
	if len(gated) < wantGated {
		t.Fatalf("frames sent during the server's speech: got %d, want >= %d -- the cadence must be unchanged, only the content", len(gated), wantGated)
	}
	for _, f := range gated {
		if !isSilenceFrame(f.payload) {
			t.Errorf("the mic was live %s into the server's speech: frame is not silence (%x)",
				f.at.Sub(spoke).Round(time.Millisecond), head(f.payload))
			break
		}
	}

	// Still shut through the hangover. The audio has finished playing, but the
	// room has not finished ringing and the player's own pipe is still behind.
	// A gate that reopened the moment the queue emptied would let the tail of
	// the last word through, which is enough for the server's STT to produce a
	// turn -- the whole defect, quieter.
	decaying := framesIn(frames, playoutEnd, reopenAt.Add(-margin/2))
	if len(decaying) == 0 {
		t.Fatalf("no frames in the %s hangover window", micGateHangover)
	}
	for _, f := range decaying {
		if !isSilenceFrame(f.payload) {
			t.Errorf("the mic reopened %s after playout ended, inside the %s hangover",
				f.at.Sub(playoutEnd).Round(time.Millisecond), micGateHangover)
			break
		}
	}

	// Open once the hangover has run, and promptly: bounded on both sides, so
	// a gate that reopened far too late fails here rather than passing because
	// some later frame eventually came through.
	after := framesIn(frames, reopenAt.Add(margin/2), reopenAt.Add(2*margin+2*micGateHangover))
	if len(after) == 0 {
		t.Fatal("no frames after the gate should have reopened")
	}
	for _, f := range after {
		if !isSilenceFrame(f.payload) {
			return
		}
	}
	t.Errorf("the gate did not reopen within %s of the hangover running out: all %d frames were still silence",
		(2*margin + 2*micGateHangover).Round(time.Millisecond), len(after))
}

// TestDial_ClearReopensTheMicAtOnce is observable 2: on barge-in the server
// abandons the rest of the reply and sends a clear, so the queued audio will
// never be heard. The mic must come back at once rather than wait out a playout
// that is not going to happen -- otherwise the caller's barge-in is the one
// thing the harness cannot record.
func TestDial_ClearReopensTheMicAtOnce(t *testing.T) {
	withLoudFrameSource(t, 5*time.Second)

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
	}, collected, 700*time.Millisecond)

	runGatedDial(t, srv)

	spoke := <-spokeAt
	cleared := <-clearedAt
	frames := <-collected

	// Non-vacuity: the gate really was shut before the clear arrived.
	//
	// On the shape, not on a margin. How soon after the server's write the
	// read loop plays the audio and publishes the deadline is scheduling --
	// measured at ~20 ms, but a window that demanded it inside 100 ms would
	// fail on a loaded machine and blame the gate for the scheduler. What is a
	// contract is that once the gate shuts it is still shut when the clear
	// lands, which is the only thing the assertion after it needs.
	before := framesIn(frames, spoke, cleared.Add(-2*frameInterval))
	if len(before) == 0 {
		t.Fatal("no frames between the server's audio and the clear")
	}
	shut := false
	for _, f := range before {
		switch {
		case isSilenceFrame(f.payload):
			shut = true
		case shut:
			t.Fatalf("the mic went live again %s before the clear, after the gate had shut -- this test proves nothing about clear",
				cleared.Sub(f.at).Round(time.Millisecond))
		}
	}
	if !shut {
		t.Fatalf("the gate never shut in the %s between the server's audio and the clear -- this test proves nothing about clear",
			cleared.Sub(spoke).Round(time.Millisecond))
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
	withLoudFrameSource(t, 5*time.Second)

	const streamSID = "SS_fullduplex"
	spokeAt := make(chan time.Time, 1)
	collected := make(chan []sentFrame, 1)
	// The window sits inside the speech by margin at each end, and the reader
	// outlasts it by another margin -- a reader that stops level with the
	// window would charge its own last-frame lag against the count.
	const speech = time.Second
	const margin = 100 * time.Millisecond
	srv := silenceProbeServer(t, func(conn net.Conn) {
		spokeAt <- serverSpeaks(t, conn, streamSID, speech)
	}, collected, speech+2*margin)

	runGatedDial(t, srv, withFullDuplex())

	spoke := <-spokeAt
	frames := <-collected

	// Derived, not written out: the count is the window's own length in
	// frames, less a quarter for scheduling -- the same arithmetic the gated
	// test's wantGated does, for the same reason a hard-coded 30 would be an
	// assertion about authoring-time margins rather than about full duplex.
	during := framesIn(frames, spoke.Add(margin), spoke.Add(speech-margin))
	want := int((speech - 2*margin) / telephony.MuLawDuration(muLawFrame20ms) * 3 / 4)
	if len(during) < want {
		t.Fatalf("frames sent during the server's speech: got %d, want >= %d", len(during), want)
	}
	for _, f := range during {
		if isSilenceFrame(f.payload) {
			t.Fatalf("--full-duplex silenced a frame %s into the server's speech", f.at.Sub(spoke).Round(time.Millisecond))
		}
	}
}

// TestDial_ConnectedLineNamesTheDuplexMode: a call's log has to say which mode
// produced it. A gated call cannot exercise barge-in and an ungated one on
// speakers is the server talking to itself, so a recording or transcript read
// afterwards means different things in the two modes -- and the only place
// that distinction is recorded is this line.
func TestDial_ConnectedLineNamesTheDuplexMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []dialOption
		want string
	}{
		{"default", nil, "half duplex"},
		{"--full-duplex", []dialOption{withFullDuplex()}, "full duplex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withLoudFrameSource(t, time.Second)
			srv := hijackedWSServer(t, func(net.Conn, *bufio.ReadWriter) {})

			out := captureLog(t, func() { runGatedDial(t, srv, tc.opts...) })

			connected := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "connected to") {
					connected = line
					break
				}
			}
			if connected == "" {
				t.Fatalf("no connected line in the log:\n%s", out)
			}
			if !strings.Contains(connected, tc.want) {
				t.Errorf("connected line does not name the duplex mode: got %q, want it to contain %q", connected, tc.want)
			}
		})
	}
}

// TestDial_MarkEchoArrivesWithTheMicGated is observable 4 in miniature: the
// server's paced write followed by a mark still gets its echo with the gate
// shut for the whole clip. The gate changes the content of outbound media and
// nothing else -- not the frame cadence the server paces on, and not the
// control plane.
//
// The media count is not incidental to that claim, it is what makes it one.
// "The echo arrived" is equally true of a build with no gate at all, so this
// test also carries the media frames that went out alongside the mark, and
// they have to be silence -- otherwise what it proves is only that mark echo
// works.
//
// "Once it shuts", not "from frame one", for the same reason
// TestDial_RecordSentTeesTheGatedFrames asserts on the shape: the server's
// audio is played by the read loop on its own goroutine, so the opening frames
// go out before there is anything for a microphone to have heard. How many
// that takes is scheduling. What is a contract is that the gate does not let
// captured audio back through afterwards.
func TestDial_MarkEchoArrivesWithTheMicGated(t *testing.T) {
	withLoudFrameSource(t, 3*time.Second)

	const streamSID = "SS_gatemark"
	const markName = "gated-mark"
	echoed := make(chan markEcho, 1)
	srv := hijackedWSServer(t, func(conn net.Conn, buf *bufio.ReadWriter) {
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
	})

	runGatedDial(t, srv)

	got, ok := <-echoed
	if !ok || got.name != markName {
		t.Errorf("mark echo: got %q (ok=%v), want %q -- gating outbound media must not stop the control plane", got.name, ok, markName)
	}
	if got.silent == 0 {
		t.Errorf("no silent media frame reached the server before the echo: the gate was not shut, so this test proves nothing about gating")
	}
	if got.loudAfterShut != 0 {
		t.Errorf("%d media frames went out as captured audio after the gate had shut: it must stay shut for the whole clip", got.loudAfterShut)
	}
}

// markEcho is what waitForMarkEcho saw: the echoed mark name, how many gated
// media frames preceded it, and how many carried captured audio after the
// first gated one -- the count that has to be zero.
type markEcho struct {
	name          string
	silent        int
	loudAfterShut int
}

// waitForMarkEcho reads client frames until a mark echo arrives or the
// connection is done, publishing the result on echoed exactly once. Media
// frames seen on the way are tallied rather than dropped -- the caller's claim
// is about them as much as about the echo.
func waitForMarkEcho(t *testing.T, buf *bufio.ReadWriter, echoed chan<- markEcho) {
	t.Helper()
	defer close(echoed)
	var seen markEcho
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
		switch {
		case f.Event == twilio.EventMedia && isSilenceFrame(f.Payload):
			seen.silent++
		case f.Event == twilio.EventMedia && seen.silent > 0:
			seen.loudAfterShut++
		case f.Event == twilio.EventMedia:
			// Before the gate shut: the read loop has not played the server's
			// audio yet, so there is nothing to echo. Not a finding.
		case f.Event == twilio.EventMark:
			seen.name = f.MarkName
			echoed <- seen
			return
		}
	}
}
