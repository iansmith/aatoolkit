package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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
