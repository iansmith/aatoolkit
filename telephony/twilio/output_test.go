package twilio

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// fakeWSWriter captures every Write call instead of touching a real
// WebSocket, so these tests can assert exactly what output.go encodes.
type fakeWSWriter struct {
	writes []struct {
		typ  websocket.MessageType
		data []byte
	}
}

func (f *fakeWSWriter) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	f.writes = append(f.writes, struct {
		typ  websocket.MessageType
		data []byte
	}{typ, data})
	return nil
}

func TestDataPlaneOutputSendEncodesMedia(t *testing.T) {
	fake := &fakeWSWriter{}
	out := &dataPlaneOutput{conn: fake, streamSID: "SS123"}

	payload := []byte{1, 2, 3, 4}
	if err := out.Send(context.Background(), payload); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes: got %d, want 1", len(fake.writes))
	}

	f, err := DecodeFrame(fake.writes[0].data)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != EventMedia {
		t.Fatalf("Event: got %s, want %s", f.Event, EventMedia)
	}
	if string(f.Payload) != string(payload) {
		t.Fatalf("Payload: got %v, want %v", f.Payload, payload)
	}
	if f.StreamSID != "SS123" {
		t.Fatalf("StreamSID: got %q, want %q", f.StreamSID, "SS123")
	}
}

// TestDataPlaneOutputSendWritesToTap covers the gap flagged by PR #115's
// review: the 5 outbound-queue tests in tap_test.go all call tap.WriteOut
// directly, bypassing Send entirely, so a Send that never enqueues onto the
// tap is invisible to that suite. This drives the real Send() method (the
// path a live call actually takes) with a real *Tap wired in, and asserts
// the tap's outbound recording -- not a direct WriteOut call -- reflects
// what was sent.
func TestDataPlaneOutputSendWritesToTap(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SPsendtap"
	tap := NewTap(dir, streamSID, "CAsendtap", "", testStreamStartedAt)

	fake := &fakeWSWriter{}
	out := &dataPlaneOutput{conn: fake, streamSID: streamSID, tap: tap}

	payload := []byte{0x11, 0x22, 0x33}
	if err := out.Send(context.Background(), payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// WriteIn+DrainOut mirrors pumpDataPlane's real cadence, and gives this
	// stream an inbound frame -- Close discards the outbound file entirely
	// for a stream with zero inbound frames (tap.go's Close), so an all-
	// outbound stream needs this pairing to have anything to assert on.
	tap.WriteIn([]byte{0x99})
	tap.DrainOut()
	tap.Close()

	got, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound tap recording: %v — Send is not wired to tap.WriteOut", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("outbound tap recording = % x, want % x — Send() did not enqueue the sent payload onto the tap's outbound queue", got, payload)
	}
}

func TestControlPlaneOutputSendEncodesMark(t *testing.T) {
	fake := &fakeWSWriter{}
	out := &controlPlaneOutput{conn: fake, streamSID: "SS123"}

	msg := telephony.ControlOutMessage{Kind: telephony.ControlOutMark, MarkName: "farewell"}
	if err := out.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes: got %d, want 1", len(fake.writes))
	}

	f, err := DecodeFrame(fake.writes[0].data)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != EventMark {
		t.Fatalf("Event: got %s, want %s", f.Event, EventMark)
	}
	if f.MarkName != "farewell" {
		t.Fatalf("MarkName: got %q, want %q", f.MarkName, "farewell")
	}
}

func TestControlPlaneOutputSendEncodesClear(t *testing.T) {
	fake := &fakeWSWriter{}
	out := &controlPlaneOutput{conn: fake, streamSID: "SS123"}

	msg := telephony.ControlOutMessage{Kind: telephony.ControlOutClear}
	if err := out.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes: got %d, want 1", len(fake.writes))
	}

	f, err := DecodeFrame(fake.writes[0].data)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Event != EventClear {
		t.Fatalf("Event: got %s, want %s", f.Event, EventClear)
	}
}

// TestControlPlaneOutputNeverEncodesStop guards Observable behavior #1:
// EncodeStop is not part of the server's outbound vocabulary. There is no
// ControlOutKind that maps to it, so an unknown/zero-value Kind must be
// rejected rather than silently falling through to some default encoding.
func TestControlPlaneOutputNeverEncodesStop(t *testing.T) {
	fake := &fakeWSWriter{}
	out := &controlPlaneOutput{conn: fake, streamSID: "SS123"}

	msg := telephony.ControlOutMessage{Kind: telephony.ControlOutKind("stop")}
	if err := out.Send(context.Background(), msg); err == nil {
		t.Fatalf("Send with kind %q: got nil error, want a rejection", msg.Kind)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("writes: got %d, want 0 -- an unknown/stop kind must never reach the WebSocket", len(fake.writes))
	}
}
