package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AATK-99 Phase 0. These tests are transcribed from the ticket's Test
// expectations; the expected values are the contract and must not be adjusted
// to match the implementation.
//
// They bind to mediaFrameSender -- the one point an outbound frame leaves the
// process -- rather than to either frame source, because the contract is about
// the bytes sent, not about where they came from.

// framePattern builds n distinct 160-byte frames whose bytes are recognizable
// individually: frame i is filled from a rotating sequence, so a recorder that
// dropped, duplicated or reordered a frame cannot still compare equal.
func framePattern(n int) []byte {
	buf := make([]byte, n*muLawFrame20ms)
	for i := range buf {
		buf[i] = byte(i%251 + 1) // never 0xFF-only: leading-silence discard must not apply here
	}
	return buf
}

// recordSentFrom streams src through the path a real call sends on -- the file
// frame source feeding mediaFrameSender -- with -record-sent pointed at path,
// and returns the bytes the recorder wrote.
func recordSentFrom(t *testing.T, path string, src io.Reader) []byte {
	t.Helper()

	rec, err := newStreamRecorder(recordOutbound, path, time.Now())
	if err != nil {
		t.Fatalf("newStreamRecorder(%s): %v", path, err)
	}
	seqNum := 1
	send := mediaFrameSender(newMediaFrameEncoder("MZ_recordsent", &seqNum), rec, nil, func([]byte) error { return nil })
	if err := streamFileFramesFrom(context.Background(), src, send, func(bool) {}); err != nil {
		t.Fatalf("streamFileFramesFrom: %v", err)
	}
	rec.close(time.Now())

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return got
}

// TestRecordSent_WritesSentPayloads is behavior 1: the file holds exactly the
// media-frame payloads sent to the server, concatenated in send order -- raw
// μ-law, not the framed JSON that went on the wire.
func TestRecordSent_WritesSentPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sent.ulaw")

	rec, err := newStreamRecorder(recordOutbound, path, time.Now())
	if err != nil {
		t.Fatalf("newStreamRecorder(%s): %v", path, err)
	}
	seqNum := 1
	var wire [][]byte
	send := mediaFrameSender(newMediaFrameEncoder("MZ_sent", &seqNum), rec, nil, func(msg []byte) error {
		wire = append(wire, append([]byte(nil), msg...))
		return nil
	})

	spoken := framePattern(3)
	for i := 0; i < 3; i++ {
		if err := send(spoken[i*muLawFrame20ms : (i+1)*muLawFrame20ms]); err != nil {
			t.Fatalf("send frame %d: %v", i, err)
		}
	}
	rec.close(time.Now())

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, spoken) {
		t.Errorf("recorded bytes differ from the payloads sent: got %d bytes, want %d", len(got), len(spoken))
	}
	// The wire carried framed JSON; the file must not. Equality above already
	// implies it, but this names the mistake the ticket exists to prevent.
	if len(wire) != 3 {
		t.Fatalf("frames written to the wire: got %d, want 3", len(wire))
	}
	if bytes.Contains(got, []byte(`"event"`)) {
		t.Errorf("recorded file contains framed JSON; -record-sent must write the pre-encode μ-law payload")
	}
}

// TestRecordSent_Off is behavior 3: with no -record-sent the recorder is nil,
// sending still works, and nothing is written.
func TestRecordSent_Off(t *testing.T) {
	dir := t.TempDir()
	seqNum := 1
	sent := 0
	send := mediaFrameSender(newMediaFrameEncoder("MZ_off", &seqNum), nil, nil, func([]byte) error {
		sent++
		return nil
	})

	if err := send(framePattern(1)); err != nil {
		t.Fatalf("send with recording off: %v", err)
	}
	if sent != 1 {
		t.Errorf("frames sent with recording off: got %d, want 1", sent)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("recording off wrote %d file(s); it must write none", len(entries))
	}
}

// TestRecordSent_RoundTripsThroughAudioReplay is behavior 2: a recording is
// directly replayable. Record call A's outbound audio, replay that file as the
// caller, record again -- the second file must equal the first, which is what
// makes "send the exact same audio to a second model" true.
func TestRecordSent_RoundTripsThroughAudioReplay(t *testing.T) {
	dir := t.TempDir()
	spoken := framePattern(5)

	a := recordSentFrom(t, filepath.Join(dir, "a.ulaw"), bytes.NewReader(spoken))
	if !bytes.Equal(a, spoken) {
		t.Fatalf("first recording differs from what was spoken: got %d bytes, want %d", len(a), len(spoken))
	}

	replay, err := os.Open(filepath.Join(dir, "a.ulaw"))
	if err != nil {
		t.Fatalf("open replay source: %v", err)
	}
	defer replay.Close()

	b := recordSentFrom(t, filepath.Join(dir, "b.ulaw"), replay)
	if !bytes.Equal(a, b) {
		t.Errorf("replay round-trip is not byte-identical: first %d bytes, second %d", len(a), len(b))
	}
}

// TestRecordSent_NotRecordedWhenTheSendFails is the other half of behavior 1.
// The file claims to be what went out, so a frame whose write failed must not
// be in it -- an operator comparing the recording against the server's logs is
// entitled to read a missing frame as "the server never got it", not as "the
// tee ran anyway". What makes that true is the tee's position, after the write
// rather than before, and moving it is invisible to every other test here.
func TestRecordSent_NotRecordedWhenTheSendFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sent.ulaw")

	rec, err := newStreamRecorder(recordOutbound, path, time.Now())
	if err != nil {
		t.Fatalf("newStreamRecorder(%s): %v", path, err)
	}
	frames := framePattern(3)
	frame := func(i int) []byte { return frames[i*muLawFrame20ms : (i+1)*muLawFrame20ms] }

	// The middle frame's write fails; the frames on either side succeed, so the
	// assertion is about that one frame and not about recording stopping.
	seqNum := 1
	attempt := 0
	sendErr := errors.New("socket gone")
	send := mediaFrameSender(newMediaFrameEncoder("MZ_failsend", &seqNum), rec, nil, func([]byte) error {
		attempt++
		if attempt == 2 {
			return sendErr
		}
		return nil
	})

	if err := send(frame(0)); err != nil {
		t.Fatalf("send frame 0: %v", err)
	}
	if err := send(frame(1)); !errors.Is(err, sendErr) {
		t.Fatalf("send frame 1: got %v, want the write error propagated to the frame source", err)
	}
	if err := send(frame(2)); err != nil {
		t.Fatalf("send frame 2: %v", err)
	}
	rec.close(time.Now())

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := append(append([]byte(nil), frame(0)...), frame(2)...)
	if !bytes.Equal(got, want) {
		t.Errorf("recorded %d bytes, want %d: only the frames whose write succeeded belong in the file", len(got), len(want))
	}
	if bytes.Contains(got, frame(1)) {
		t.Errorf("the file holds a frame that never went out -- the tee must run after the write, not before")
	}
}

// TestDial_RecordsSentAudioOnlyWhenAsked pins the one link the tests above
// cannot see: dial creating the outbound recorder and wiring it into the send
// it hands the frame source. Everything below mediaFrameSender is already
// covered, and all of it stays green if dial builds a sender with a nil
// recorder -- which would leave -record-sent writing an empty file on every
// real call.
//
// The evidence is the file rather than a non-nil pointer arriving at the frame
// source. Since dial owns the whole outbound path, a source has nothing to
// inspect any more; what it has is a send, and whether that send records is
// exactly what the file answers.
//
// Differential, for the same reason TestCLI_NoEchoMarks is: "records nothing
// without the flag" passes equally well against a build that records nothing
// with it.
func TestDial_RecordsSentAudioOnlyWhenAsked(t *testing.T) {
	spoken := framePattern(2)

	// run dials a server that reads to the stop frame, with a fake mic that
	// pushes spoken through the send dial built and then ends (capture EOF =
	// caller hangup), so dial tears down and closes the recorder before it
	// returns.
	run := func(t *testing.T, opts ...dialOption) {
		t.Helper()
		withFakeMic(t, func(_ context.Context, send func([]byte) error, _ func(bool)) error {
			for i := 0; i*muLawFrame20ms < len(spoken); i++ {
				if err := send(spoken[i*muLawFrame20ms : (i+1)*muLawFrame20ms]); err != nil {
					return err
				}
			}
			return nil
		})

		srv, served := mediaConsumingServer(t)
		defer func() {
			waitServed(t, served)
			srv.Close()
		}()

		addr := "ws" + strings.TrimPrefix(srv.URL, "http")
		if err := dial(dialCtx(t, 5*time.Second), newSID("CA"), addr, opts...); err != nil {
			t.Fatalf("dial: %v", err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sent.ulaw")
	run(t, withSentRecording(path))
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v -- dial must wire its outbound recorder into the send it hands down", path, err)
	}
	if !bytes.Equal(recorded, spoken) {
		t.Errorf("dial -record-sent recorded %d bytes, want the %d that were sent", len(recorded), len(spoken))
	}
	if _, err := os.Stat(path + ".jsonl"); err != nil {
		t.Errorf("timing sidecar missing: %v", err)
	}

	// The other half: no flag, no recording. Same call, same frames.
	off := filepath.Join(dir, "off.ulaw")
	run(t)
	if _, err := os.Stat(off); !os.IsNotExist(err) {
		t.Errorf("no -record-sent, but %s exists (stat err %v): recording must be off unless the flag names a file", off, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("run without -record-sent wrote extra files: %d in the dir, want the 2 from the recorded run", len(entries))
	}
}

// mediaConsumingServer serves one call whose far end reads every frame through
// to the stop event. A frame source that ends on its own is a caller hangup, and
// dial only runs that teardown -- and so only closes the outbound recorder --
// once the media it already wrote has been consumed; a server that stopped
// reading after the handshake would leave the frames in the socket buffer.
//
// The returned channel closes when the handler has finished; a caller that
// closes the server must wait on it first. httptest.Server.Close does not wait
// for a hijacked connection's goroutine, so a test whose dial returns before
// that goroutine is scheduled -- which a fake mic sending two frames and
// hanging up does -- would close the socket underneath it and report the
// handler's read error as a handshake failure. Intermittent, and it says
// "read connected frame" about a connection that was fine.
func mediaConsumingServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		trackConn(t, conn)
		defer conn.Close()
		wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
		readHandshake(t, buf) // connected + start
		for {
			msg, err := readWSFrame(buf)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(msg, &m) == nil && m["event"] == "stop" {
				return
			}
		}
	}))
	return srv, served
}

// waitServed blocks until a mediaConsumingServer handler has finished, or fails
// the test rather than hanging forever on a genuine handler bug.
func waitServed(t *testing.T, served <-chan struct{}) {
	t.Helper()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the media-consuming server handler to finish")
	}
}

// TestDial_RecordSentThroughTheAudioFrameSource pins the -audio half of the
// wiring, which nothing else reaches: streamFileFrames must hand dial's
// outbound recorder to mediaFrameSender.
//
// TestDial_RecordsSentAudioOnlyWhenAsked builds its own sender inside a fake
// mic, so it proves dial passes a recorder down but cannot see what the real
// file source does with one; every other test here drives streamFileFramesFrom,
// which is the level below the wiring. So a build whose streamFileFrames passed
// nil stays green across the whole suite while `-audio in.ulaw -record-sent
// out.ulaw` -- the README's record-then-replay workflow, and the only frame
// source that exists off macOS -- writes an empty file.
//
// Driven --full-duplex on purpose (AATK-127): every call opens with the
// capture-live earcon, which is real bytes into the player, so on the default
// gated path the first half-second of outbound frames is mu-law silence by
// design. This test's claim is about the recorder reaching the file source at
// all, and it says so about the frames as streamed; the gated path's own claim
// -- that the tee records what went on the wire rather than what was captured
// -- is TestDial_RecordSentTeesTheGatedFrames below.
func TestDial_RecordSentThroughTheAudioFrameSource(t *testing.T) {
	dir := t.TempDir()
	spoken := framePattern(3)
	src := filepath.Join(dir, "in.ulaw")
	if err := os.WriteFile(src, spoken, 0o644); err != nil {
		t.Fatalf("write -audio source: %v", err)
	}

	// The real file frame source, installed on the seam -audio installs it on.
	withFakeMic(t, streamFileFrames(src))

	srv, served := mediaConsumingServer(t)
	defer func() {
		waitServed(t, served)
		srv.Close()
	}()

	out := filepath.Join(dir, "sent.ulaw")
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")
	if err := dial(dialCtx(t, 5*time.Second), newSID("CA"), addr, withSentRecording(out), withFullDuplex()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	recorded, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	if !bytes.Equal(recorded, spoken) {
		t.Errorf("-audio %s -record-sent %s recorded %d bytes, want the %d streamed from the file",
			src, out, len(recorded), len(spoken))
	}
}

// TestDial_RecordSentTeesTheGatedFrames is the gated half of the pair above
// (AATK-127): -record-sent claims to be what went out, so it must be teed
// downstream of the mic gate. A recording of what the microphone heard while
// the gate was shut would be a file of the server's own voice labelled as the
// caller's -- the exact confusion the gate exists to end.
//
// The earcon is what holds the gate shut here: it plays into the same player
// as the server's audio, is accounted to the filler, and so gates the mic for
// its own 240 ms plus the hangover -- past the end of the 300 ms clip.
//
// The first frame or two are expected to carry the captured audio, and that is
// the gate being right rather than late: the tone is played by the read loop
// on its own goroutine, signalled by the frame source's first frame, so until
// it has actually been fed to the player there is nothing for the microphone
// to have heard. Everything from the third frame on is over the tone and must
// be silence.
func TestDial_RecordSentTeesTheGatedFrames(t *testing.T) {
	const frames = 15
	const settleFrames = 3 // frames the earcon signal may still be in flight for

	dir := t.TempDir()
	spoken := framePattern(frames)
	src := filepath.Join(dir, "in.ulaw")
	if err := os.WriteFile(src, spoken, 0o644); err != nil {
		t.Fatalf("write -audio source: %v", err)
	}

	withFakeMic(t, streamFileFrames(src))

	srv, served := mediaConsumingServer(t)
	defer func() {
		waitServed(t, served)
		srv.Close()
	}()

	out := filepath.Join(dir, "sent.ulaw")
	addr := "ws" + strings.TrimPrefix(srv.URL, "http")
	if err := dial(dialCtx(t, 5*time.Second), newSID("CA"), addr, withSentRecording(out)); err != nil {
		t.Fatalf("dial: %v", err)
	}

	recorded, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	if len(recorded) != len(spoken) {
		t.Fatalf("-record-sent recorded %d bytes, want %d -- the gate changes content, never cadence", len(recorded), len(spoken))
	}
	for i := settleFrames; i < frames; i++ {
		frame := recorded[i*muLawFrame20ms : (i+1)*muLawFrame20ms]
		if !isSilenceFrame(frame) {
			t.Errorf("-record-sent frame %d, over the earcon: got %x..., want mu-law silence -- the tee must be downstream of the gate", i, head(frame))
			break
		}
	}
}
