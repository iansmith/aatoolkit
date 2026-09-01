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

	"github.com/coder/websocket"
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
	send := mediaFrameSender(newMediaFrameEncoder("MZ_recordsent", &seqNum), rec, func([]byte) error { return nil })
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
	send := mediaFrameSender(newMediaFrameEncoder("MZ_sent", &seqNum), rec, func(msg []byte) error {
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
	send := mediaFrameSender(newMediaFrameEncoder("MZ_off", &seqNum), nil, func([]byte) error {
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
	send := mediaFrameSender(newMediaFrameEncoder("MZ_failsend", &seqNum), rec, func([]byte) error {
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
// cannot see: dial creating the outbound recorder and handing it down to the
// frame source. Everything below mediaFrameSender is already covered, and all
// of it stays green if dial passes nil to streamMic instead -- which would
// leave -record-sent writing an empty file on every real call.
//
// Differential, for the same reason TestCLI_NoEchoMarks is: "records nothing
// without the flag" passes equally well against a build that records nothing
// with it.
func TestDial_RecordsSentAudioOnlyWhenAsked(t *testing.T) {
	spoken := framePattern(2)

	// run dials a server that reads to the stop frame, with a fake mic that
	// sends spoken through the real send seam and then ends (capture EOF =
	// caller hangup), so dial tears down and closes the recorder before it
	// returns. Reports whether the frame source was handed a recorder at all.
	run := func(t *testing.T, opts ...dialOption) (gotRecorder bool) {
		t.Helper()
		withFakeMic(t, func(_ context.Context, conn *websocket.Conn, streamSID string, seqNum *int, rec *streamRecorder, _ func(bool)) error {
			gotRecorder = rec != nil
			send := mediaFrameSender(newMediaFrameEncoder(streamSID, seqNum), rec, connFrameWriter(conn))
			for i := 0; i*muLawFrame20ms < len(spoken); i++ {
				if err := send(spoken[i*muLawFrame20ms : (i+1)*muLawFrame20ms]); err != nil {
					return err
				}
			}
			return nil
		})

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			trackConn(t, conn)
			defer conn.Close()
			wsHandshake(conn, r.Header.Get("Sec-Websocket-Key"))
			readHandshake(t, buf) // connected + start
			// The media frames arrive before stop; the server has to consume
			// them for the caller-hangup teardown to run.
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
		defer srv.Close()

		addr := "ws" + strings.TrimPrefix(srv.URL, "http")
		if err := dial(dialCtx(t, 5*time.Second), newSID("CA"), addr, opts...); err != nil {
			t.Fatalf("dial: %v", err)
		}
		return gotRecorder
	}

	path := filepath.Join(t.TempDir(), "sent.ulaw")
	if !run(t, withSentRecording(path)) {
		t.Fatalf("-record-sent was passed but the frame source got a nil recorder: dial must hand its outbound recorder to streamMic")
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(recorded, spoken) {
		t.Errorf("dial -record-sent recorded %d bytes, want the %d that were sent", len(recorded), len(spoken))
	}
	if _, err := os.Stat(path + ".jsonl"); err != nil {
		t.Errorf("timing sidecar missing: %v", err)
	}

	if run(t) {
		t.Errorf("no -record-sent, but the frame source was handed a recorder: recording must be off unless the flag names a file")
	}
}
