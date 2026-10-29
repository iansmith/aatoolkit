package twilio

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// Phase 0 (rev 3). These tests describe the audio tap's expected
// behavior and fail against the stubs in tap.go.
//
// The tap exists to capture real telephony audio for the probe dataset:
// mu-law over the phone network is what makes the transcriber hear a
// similar-sounding but different phrase from the one spoken, and no mic
// recording reproduces that. Its governing constraint is that it must be
// invisible -- off by default, and incapable of harming a call when on.
//
// Recordings are keyed by streamSid, not CallSID: streamSid uniquely
// identifies a Media Streams connection, and a reconnect is a new connection
// on the same call.

// testStreamStartedAt is the stream start time every test in this package
// states, whether it builds a Tap directly or drives one through the harness.
// One definition: it is one concept, and two would drift.
//
// Do not round this value. The odd nanoseconds are the whole sub-second
// precision guarantee: because every assertion compares against it with
// Equal(), this single constant is what fails a tap that truncates the start
// time to seconds, milliseconds, or microseconds on its way to the sidecar --
// and one that substitutes a plausible clock reading of its own. A tidy
// 09:30:00.000000000 silently retires all of that, leaving tests that pass
// against a recording stamped with the wrong moment.
var testStreamStartedAt = time.Date(2026, 7, 17, 9, 30, 15, 123456789, time.UTC)

// errWriter always fails. It is the injected writer behind the "a broken tap
// never breaks a call" guarantee. It counts both
// writes and closes: a tap that leaks its fd degrades a long-running process,
// which is the same harm by a slower route.
type errWriter struct {
	writes int
	closes int
}

func (e *errWriter) Write(p []byte) (int, error) { e.writes++; return 0, errors.New("disk on fire") }
func (e *errWriter) Close() error                { e.closes++; return errors.New("disk still on fire") }

// ulawPath / sidecarPath name the two artifacts a stream produces.
func sidecarPath(dir, streamSID string) string { return filepath.Join(dir, streamSID+".json") }

func readSidecar(t *testing.T, dir, streamSID string) tapSidecar {
	t.Helper()
	raw, err := os.ReadFile(sidecarPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var s tapSidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}
	return s
}

// --- the Tap itself ----------------------------------------------------------

// TestTap_DisabledWritesNothing pins the default. The tap is opt-in: with no
// directory configured NewTap yields nil, and a nil *Tap is a working no-op, so
// the data plane needs no branch and an off tap opens nothing and allocates
// nothing. Calling Write and Close on the nil must be safe -- that is the whole
// mechanism by which "off" costs nothing.
func TestTap_DisabledWritesNothing(t *testing.T) {
	tap := NewTap("", "SSdisabled", "CAdisabled", "", testStreamStartedAt)
	if tap != nil {
		t.Fatalf("NewTap(\"\", ...) = %p, want nil — the tap must be off unless a directory is configured", tap)
	}
	tap.WriteIn([]byte{0x01, 0x02}) // must not panic on the nil receiver
	tap.Close()                     // must not panic on the nil receiver
}

// TestTap_BytesMatchPayloads is the tap's reason to exist: a replay must see
// exactly what production saw. The file is the concatenation of the payloads
// and nothing else -- no header, no framing, no separators, no padding.
func TestTap_BytesMatchPayloads(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSbytes"

	tap := NewTap(dir, streamSID, "CAbytes", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01, 0x02})
	tap.WriteIn([]byte{0x03})
	tap.WriteIn([]byte{0x04, 0x05, 0x06})
	tap.Close()

	got, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading captured audio: %v", err)
	}
	want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if !bytes.Equal(got, want) {
		t.Errorf("captured audio = % x (%d bytes), want % x (%d bytes)", got, len(got), want, len(want))
	}
}

// TestTap_SidecarCountsEveryFrame spans write and read-back in one test,
// deliberately. Split across two tests -- one that writes three payloads and
// never reads the sidecar, one that reads the sidecar after a single write --
// a hardcoded `Frames: 1` and a `Bytes: len(lastPayload)` both pass. The split
// is what hides them.
func TestTap_SidecarCountsEveryFrame(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SScounts"

	tap := NewTap(dir, streamSID, "CAcounts", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01, 0x02})
	tap.WriteIn([]byte{0x03})
	tap.WriteIn([]byte{0x04, 0x05, 0x06})
	tap.Close()

	got := readSidecar(t, dir, streamSID)
	if got.Frames != 3 {
		t.Errorf("sidecar frames = %d, want 3 — the count is not per-frame", got.Frames)
	}
	if got.Bytes != 6 {
		t.Errorf("sidecar bytes = %d, want 6 — the count is not cumulative", got.Bytes)
	}
}

// wantRecordedVAD is the vad_config block every sidecar must carry: the removed
// VAD pipeline's default thresholds, frozen so a sidecar reads as it did before
// the pipeline went (AATK-164). They are no longer a tuning knob, so this is
// deliberately a literal rather than recordedVADConfig itself: a test comparing
// the field with the variable that fills it would pass whatever the variable
// held.
var wantRecordedVAD = tapVADConfig{WindowSize: 256, SpeechThresh: 0.5, SilenceThresh: 0.35, EndSilenceMS: 700, SampleRateHz: 8000, TurnEndSilenceMS: 5000}

// TestTap_SidecarRecordsVADConfig pins the sidecar's identifying fields and its
// vad_config block.
func TestTap_SidecarRecordsVADConfig(t *testing.T) {
	dir := t.TempDir()
	const streamSID, callSID = "SSsidecar", "CAsidecar"

	tap := NewTap(dir, streamSID, callSID, "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01, 0x02, 0x03})
	tap.Close()

	got := readSidecar(t, dir, streamSID)
	if got.VAD != wantRecordedVAD {
		t.Errorf("sidecar vad_config = %+v\n                  want %+v", got.VAD, wantRecordedVAD)
	}
	if got.StreamSID != streamSID {
		t.Errorf("sidecar stream_sid = %q, want %q", got.StreamSID, streamSID)
	}
	// Both IDs: the file is keyed by stream, but the replay harness needs to group a
	// reconnected call's recordings back together.
	if got.CallSID != callSID {
		t.Errorf("sidecar call_sid = %q, want %q", got.CallSID, callSID)
	}
	// Exact, not IsZero(): "not zero" is satisfied by any wrong instant,
	// including a tap quietly reading its own clock. Equal, not ==, because the
	// value has been through a JSON round-trip and carries a different location
	// pointer coming back.
	if !got.StartedAt.Equal(testStreamStartedAt) {
		t.Errorf("sidecar started_at = %s, want %s — the recording's start time is not the one it was given",
			got.StartedAt.Format(time.RFC3339Nano), testStreamStartedAt.Format(time.RFC3339Nano))
	}
}

// --- failure tolerance -------------------------------------------------------

// TestTap_WriteFailureDoesNotKillCall covers the broken-tap guarantee. A full or
// read-only disk must be a non-event: Write reports nothing upward (it has no
// error return by construction), it does not panic, and -- the part that would
// actually hurt -- a failed write must not wedge the tap into dropping the rest
// of the stream. Every subsequent Write is still attempted, and Close still
// closes the writer rather than leaking the fd.
func TestTap_WriteFailureDoesNotKillCall(t *testing.T) {
	dir := t.TempDir()
	w := &errWriter{}

	tap := newTapWithWriter(w, dir, "SSbroken", "CAbroken", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.WriteIn([]byte{0x02})
	tap.WriteIn([]byte{0x03})
	tap.Close()

	if w.writes != 3 {
		t.Errorf("errWriter saw %d writes, want 3 — a failing write must not stop the tap from accepting the rest of the stream", w.writes)
	}
	if w.closes != 1 {
		t.Errorf("errWriter saw %d closes, want 1 — the tap is leaking its file descriptor", w.closes)
	}
}

// TestTap_WriteFailureLogsOnce: Twilio sends ~50 media frames a second. A tap
// that logs every failed write turns a full disk into a log flood, and the
// flood is itself the harm contract 4 exists to prevent -- "log once and
// continue" is load-bearing, not politeness.
func TestTap_WriteFailureLogsOnce(t *testing.T) {
	logs := captureLog(t)

	dir := t.TempDir()
	const streamSID = "SSlog"
	w := &errWriter{}

	tap := newTapWithWriter(w, dir, streamSID, "CAlog", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.WriteIn([]byte{0x02})
	tap.WriteIn([]byte{0x03})
	tap.Close()

	if n := strings.Count(logs.String(), streamSID); n != 1 {
		t.Errorf("tap logged %d lines for 3 failed writes, want 1 — a failing disk floods the log at 50 frames/sec", n)
	}
}

// TestTap_OutboundWriteFailureDoesNotKillCall mirrors
// TestTap_WriteFailureDoesNotKillCall for the outbound side: DrainOut has no
// error return by construction, does not panic on a failing wOut, and a
// failed write must not wedge the tap into dropping the rest of the stream.
// Before this test, the outbound write-failure path (DrainOut's
// t.wOut.Write(frame) failing) had no test seam at all --
// newTapWithWriter only injected a fake writer for the inbound side.
func TestTap_OutboundWriteFailureDoesNotKillCall(t *testing.T) {
	dir := t.TempDir()
	w := &errWriter{}

	tap := newTapWithOutWriter(w, dir, "SSoutbroken", "CAoutbroken", "", testStreamStartedAt)
	tap.WriteOut([]byte{0x01})
	tap.DrainOut()
	tap.WriteOut([]byte{0x02})
	tap.DrainOut()
	tap.WriteOut([]byte{0x03})
	tap.DrainOut()
	tap.Close()

	if w.writes != 3 {
		t.Errorf("errWriter saw %d writes, want 3 — a failing outbound write must not stop DrainOut from accepting the rest of the stream", w.writes)
	}
	if w.closes != 1 {
		t.Errorf("errWriter saw %d closes, want 1 — the tap is leaking its outbound file descriptor", w.closes)
	}
}

// TestTap_OutboundWriteFailureLogsOnce mirrors TestTap_WriteFailureLogsOnce
// for the outbound side: a failing disk on .out.ulaw must log once, not
// flood the log at ~50 frames/sec.
func TestTap_OutboundWriteFailureLogsOnce(t *testing.T) {
	logs := captureLog(t)

	dir := t.TempDir()
	const streamSID = "SSoutlog"
	w := &errWriter{}

	tap := newTapWithOutWriter(w, dir, streamSID, "CAoutlog", "", testStreamStartedAt)
	tap.WriteOut([]byte{0x01})
	tap.DrainOut()
	tap.WriteOut([]byte{0x02})
	tap.DrainOut()
	tap.WriteOut([]byte{0x03})
	tap.DrainOut()
	tap.Close()

	if n := strings.Count(logs.String(), streamSID); n != 1 {
		t.Errorf("tap logged %d lines for 3 failed outbound writes, want 1 — a failing disk floods the log at 50 frames/sec", n)
	}
}

// TestTap_FailedWriteLeavesNoOrphanRecording covers the disk filling mid-call,
// which is the failure the other tolerance tests here cannot see: they all
// inject a writer, and injecting one skips os.OpenFile, so no test ever
// observes a tap that opened a file and then could not write to it.
//
// Write creates the .ulaw before it writes to it, so a full disk yields a
// zero-byte recording with no sidecar beside it -- exactly the artifact
// EmptyPayloadIsNotMedia rejects ("indistinguishable from a recording whose
// capture broke"), and exactly what Close's own doc says cannot happen. The replay
// harness would find a recording it cannot tell from a silent call, with no sidecar to
// say otherwise. The invariant worth having is the one this pins: every .ulaw
// on disk is real audio with a sidecar next to it.
//
// The pre-created file stands in for what OpenFile leaves behind when there is
// room for the inode but not the data. This is an assertion about an action --
// the file exists before Close and must not after -- not about an absence.
func TestTap_FailedWriteLeavesNoOrphanRecording(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSorphan"

	if err := os.WriteFile(inulawPath(dir, streamSID), nil, 0o644); err != nil {
		t.Fatalf("staging the opened-but-empty recording: %v", err)
	}

	tap := newTapWithWriter(&errWriter{}, dir, streamSID, "CAorphan", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.WriteIn([]byte{0x02})
	tap.Close()

	if _, err := os.Stat(inulawPath(dir, streamSID)); !os.IsNotExist(err) {
		t.Errorf("a stream whose every write failed left a zero-byte recording behind (stat err = %v) — nothing distinguishes it from a silent call, and there is no sidecar to say otherwise", err)
	}
	if _, err := os.Stat(sidecarPath(dir, streamSID)); !os.IsNotExist(err) {
		t.Errorf("a stream that captured nothing left a sidecar behind (stat err = %v)", err)
	}
}

// TestTap_MissingDirDoesNotKillCall: the likeliest real-world failure is an
// operator pointing the env at a directory that does not exist. The tap
// tolerates it -- no panic, and no MkdirAll, because creating the directory
// would turn a typo into a real folder somewhere unexpected and let the
// operator believe capture was working.
//
// Asserted as a contrast: "nothing happened" is true of any tap that does
// nothing at all.
func TestTap_MissingDirDoesNotKillCall(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "not-created-by-the-operator")

	tap := NewTap(missing, "SSnodir", "CAnodir", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01}) // must not panic
	tap.WriteIn([]byte{0x02})
	tap.Close() // must not panic

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the tap created its own directory (stat err = %v) — a typo'd env var must not silently produce a real folder", err)
	}

	// The control: the same sequence against a directory that exists records.
	good := NewTap(root, "SSgood", "CAgood", "", testStreamStartedAt)
	good.WriteIn([]byte{0x01})
	good.Close()
	if _, err := os.Stat(inulawPath(root, "SSgood")); err != nil {
		t.Fatalf("a tap over a real directory left no audio file (stat err = %v)", err)
	}
}

// TestTap_WriteAfterCloseIsSafe covers a live sequence, not a hypothetical:
// pumpDataPlane runs in its own goroutine, and handleStream's defer cancels the
// pumps, joins, then closes the tap. A frame in flight arriving at a closed
// file must not panic -- a panic there fires inside the defer and drops the
// call, which is the exact harm contract 4 forbids. Close must also be
// idempotent and final: a late frame cannot reopen a finished recording.
func TestTap_WriteAfterCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSlate"

	tap := NewTap(dir, streamSID, "CAlate", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01, 0x02})
	tap.Close()
	tap.WriteIn([]byte{0x03}) // a frame still in flight in pumpDataPlane
	tap.Close()               // and the double close that follows it

	audio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading captured audio: %v", err)
	}
	if want := []byte{0x01, 0x02}; !bytes.Equal(audio, want) {
		t.Errorf("captured % x, want % x — a post-Close frame reopened a finished recording", audio, want)
	}
	side := readSidecar(t, dir, streamSID)
	if side.Frames != 1 || side.Bytes != 2 {
		t.Errorf("sidecar frames/bytes = %d/%d, want 1/2 — Close is not final", side.Frames, side.Bytes)
	}
}

// --- boundaries --------------------------------------------------------------

// TestTap_EmptyPayloadIsNotMedia decides what contract 3's "first media frame"
// hinges on: Write being called, or bytes actually arriving. It must be the
// latter -- an empty .ulaw is indistinguishable from a recording whose capture
// broke, and the replay harness would replay it as a zero-utterance sample.
//
// Contrast-structured: "no file appeared" is true of any tap that does nothing.
func TestTap_EmptyPayloadIsNotMedia(t *testing.T) {
	dir := t.TempDir()
	const emptySID, realSID = "SSempty", "SSreal"

	empty := NewTap(dir, emptySID, "CAempty", "", testStreamStartedAt)
	empty.WriteIn([]byte{})
	empty.WriteIn(nil)
	empty.Close()

	// The control: a non-empty payload under identical conditions records.
	real := NewTap(dir, realSID, "CAreal", "", testStreamStartedAt)
	real.WriteIn([]byte{0x01})
	real.Close()
	if _, err := os.Stat(inulawPath(dir, realSID)); err != nil {
		t.Fatalf("a stream with real media left no audio file (stat err = %v)", err)
	}

	if _, err := os.Stat(inulawPath(dir, emptySID)); !os.IsNotExist(err) {
		t.Errorf("zero-byte payloads created a recording (stat err = %v)", err)
	}
}

// TestTap_NoMediaNoFile: a stream that carries no audio is not a recording.
//
// Asserted as a contrast, deliberately. "No file appears" is true of any tap
// that does nothing at all, so on its own it is unfalsifiable -- it passed
// against the stub on the rev-2 suite's first run. The behavior only exists
// relative to its opposite: a stream that DID carry media must leave a file in
// the same directory, under the same conditions, in the same test. The pair is
// what pins file creation to the arrival of a payload rather than to the
// opening of a call.
func TestTap_NoMediaNoFile(t *testing.T) {
	dir := t.TempDir()
	const silentSID, talkingSID = "SSsilent", "SStalking"

	silent := NewTap(dir, silentSID, "CAsilent", "", testStreamStartedAt)
	silent.Close() // no Write ever happened

	talking := NewTap(dir, talkingSID, "CAtalking", "", testStreamStartedAt)
	talking.WriteIn([]byte{0x01})
	talking.Close()

	// The control: media arrived, so this stream is a recording.
	if _, err := os.Stat(inulawPath(dir, talkingSID)); err != nil {
		t.Fatalf("a stream WITH media left no audio file — file creation is not tied to payload arrival (stat err = %v)", err)
	}
	if _, err := os.Stat(sidecarPath(dir, talkingSID)); err != nil {
		t.Fatalf("a stream WITH media left no sidecar (stat err = %v)", err)
	}

	// The behavior under test: no media, so nothing at all.
	if _, err := os.Stat(inulawPath(dir, silentSID)); !os.IsNotExist(err) {
		t.Errorf("a stream with no media left an audio file behind (stat err = %v)", err)
	}
	if _, err := os.Stat(sidecarPath(dir, silentSID)); !os.IsNotExist(err) {
		t.Errorf("a stream with no media left a sidecar behind (stat err = %v)", err)
	}
}

// TestTap_ConcurrentWritesRaceFree: the engine takes more than one call at a time,
// and each recording must be exactly one stream.
//
// This replaces rev 2's TestTap_ConcurrentCallsSeparateFiles, which interleaved
// two taps sequentially -- no goroutines, so -race observed nothing and the
// name claimed a property the test could not see. WaitGroup.Wait is a join, not
// a wait-assert: it returns when the work is done, never on a timeout.
func TestTap_ConcurrentWritesRaceFree(t *testing.T) {
	dir := t.TempDir()
	const frames = 100
	cases := []struct {
		streamSID string
		callSID   string
		fill      byte
	}{
		{"SSracea", "CAracea", 0xA1},
		{"SSraceb", "CAraceb", 0xB1},
	}

	var wg sync.WaitGroup
	for _, tc := range cases {
		tap := NewTap(dir, tc.streamSID, tc.callSID, "", testStreamStartedAt)
		wg.Add(1)
		go func(tp *Tap, fill byte) {
			defer wg.Done()
			for i := 0; i < frames; i++ {
				tp.WriteIn([]byte{fill})
			}
			tp.Close()
		}(tap, tc.fill)
	}
	wg.Wait()

	for _, tc := range cases {
		got, err := os.ReadFile(inulawPath(dir, tc.streamSID))
		if err != nil {
			t.Fatalf("reading %s: %v", tc.streamSID, err)
		}
		if len(got) != frames {
			t.Errorf("%s captured %d bytes, want %d", tc.streamSID, len(got), frames)
		}
		for i, b := range got {
			if b != tc.fill {
				t.Fatalf("%s byte %d = %#x, want %#x — streams have bled into each other", tc.streamSID, i, b, tc.fill)
			}
		}
	}
}

// --- outbound capture ------------------------------------------
//
// Retroactive Phase 0 verification record: the 5 tests below
// (TestTap_OutboundQueueDrainAligns, TestTap_SilenceOnEmptyQueue,
// TestTap_BargeInOverlap, TestTap_QueueTruncationOnClose,
// TestTap_DisabledAllocatesNothing_Outbound) were confirmed to establish a
// genuine RED state against pre-outbound-capture code: with the WriteOut/DrainOut
// outbound-queue implementation in tap.go reverted, the package failed to
// compile with three undefined-symbol errors (tap.WriteIn, tap.WriteOut,
// tap.DrainOut), and once stubbed to compile, all 5 tests failed on their
// assertions (no .out.ulaw produced, no silence padding, no overlap
// alignment, no out_truncated sidecar flag). With the implementation
// restored, all 5 pass under the race detector.
// No assertion in these tests was altered to reach this state.

// inulawPath and outulawPath name the two direction-specific .ulaw files.
func inulawPath(dir, streamSID string) string  { return filepath.Join(dir, streamSID+".in.ulaw") }
func outulawPath(dir, streamSID string) string { return filepath.Join(dir, streamSID+".out.ulaw") }

// TestTap_OutboundQueueDrainAligns verifies that DrainOut is paired with WriteIn
// to keep inbound and outbound aligned despite the outbound being burst-dumped.
//
// Strengthened (attempt 8): the original only checked aggregate byte
// lengths, which a DrainOut emitting garbled or reordered frames of the
// right total length would still pass. Every payload here carries a
// distinct byte value, so exact content equality against the recorded
// concatenation pins the drain order, not just the drain count.
func TestTap_OutboundQueueDrainAligns(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSoutbound"
	const nFrames = 5

	tap := NewTap(dir, streamSID, "CAoutbound", "", testStreamStartedAt)
	outPayloads := make([][]byte, nFrames)
	for i := 0; i < nFrames; i++ {
		outPayloads[i] = []byte{byte(0x40 + i)}
	}
	for _, p := range outPayloads {
		tap.WriteOut(p)
	}
	inPayloads := make([][]byte, nFrames)
	for i := 0; i < nFrames; i++ {
		inPayloads[i] = []byte{byte(0x10 + i)}
		tap.WriteIn(inPayloads[i])
		tap.DrainOut()
	}
	tap.Close()

	inAudio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading inbound recording: %v", err)
	}
	outAudio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound recording: %v", err)
	}

	wantIn := bytes.Join(inPayloads, nil)
	wantOut := bytes.Join(outPayloads, nil)
	if !bytes.Equal(inAudio, wantIn) {
		t.Errorf("inbound audio = % x, want % x", inAudio, wantIn)
	}
	if !bytes.Equal(outAudio, wantOut) {
		t.Errorf("drained outbound audio = % x, want % x — DrainOut must emit exactly what was enqueued via WriteOut, in order", outAudio, wantOut)
	}
}

// TestTap_SilenceOnEmptyQueue verifies DrainOut returns 0xFF when queue is empty.
func TestTap_SilenceOnEmptyQueue(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSsilence"
	tap := NewTap(dir, streamSID, "CAsilence", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.DrainOut()
	tap.WriteIn([]byte{0x02})
	tap.DrainOut()
	tap.Close()
	outAudio, _ := os.ReadFile(outulawPath(dir, streamSID))
	want := []byte{0xFF, 0xFF}
	if !bytes.Equal(outAudio, want) {
		t.Errorf("outbound = % x, want % x", outAudio, want)
	}
}

// TestTap_BargeInOverlap verifies barge-in appears in both channels.
//
// Strengthened (attempt 8): the original only checked aggregate byte
// lengths ("both are 4 bytes"), which a DrainOut writing garbled, shifted,
// or zeroed frames of the right total length would still pass -- exactly
// the property this test exists for (both channels have energy at the
// same byte offset), previously unchecked. Now asserts
// pairwise content equality at each overlapping frame's byte offset in
// both channels, not just that the lengths match.
func TestTap_BargeInOverlap(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSbargein"
	tap := NewTap(dir, streamSID, "CAbargein", "", testStreamStartedAt)
	tap.WriteOut([]byte{0x50, 0x51})
	tap.WriteOut([]byte{0x52, 0x53})
	tap.WriteIn([]byte{0x20, 0x21})
	tap.DrainOut()
	tap.WriteIn([]byte{0x22, 0x23})
	tap.DrainOut()
	tap.Close()

	inAudio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading inbound recording: %v", err)
	}
	outAudio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound recording: %v", err)
	}

	wantIn := []byte{0x20, 0x21, 0x22, 0x23}
	wantOut := []byte{0x50, 0x51, 0x52, 0x53}
	if !bytes.Equal(inAudio, wantIn) {
		t.Fatalf("inbound audio = % x, want % x", inAudio, wantIn)
	}
	if !bytes.Equal(outAudio, wantOut) {
		t.Fatalf("outbound audio = % x, want % x", outAudio, wantOut)
	}

	// The barge-in check itself: at each overlapping frame's byte offset,
	// both channels must carry their OWN distinct real content -- not just
	// matching lengths. Frame 1 is bytes [0:2], frame 2 is bytes [2:4].
	for _, off := range []int{0, 2} {
		gotIn, gotOut := inAudio[off:off+2], outAudio[off:off+2]
		wIn, wOut := wantIn[off:off+2], wantOut[off:off+2]
		if !bytes.Equal(gotIn, wIn) || !bytes.Equal(gotOut, wOut) {
			t.Errorf("offset %d: in=% x out=% x, want in=% x out=% x — both channels must show their own energy at the same byte offset", off, gotIn, gotOut, wIn, wOut)
		}
	}
}

// TestTap_QueueTruncationOnClose verifies sidecar records out_truncated.
func TestTap_QueueTruncationOnClose(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SStruncate"
	tap := NewTap(dir, streamSID, "CAtruncate", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.WriteOut([]byte{0x50})
	tap.WriteOut([]byte{0x51})
	tap.Close()
	side := readSidecar(t, dir, streamSID)
	if !side.OutTruncated {
		t.Errorf("sidecar out_truncated = %v, want true", side.OutTruncated)
	}
}

// TestTap_SilenceMatchesRealFrameSize covers two of outbound capture's
// requirements directly: the engine-silent spans are silence (0xFF) of the
// correct duration, and the in-channel and out-channel files are byte-for-byte
// the same length. The 5 pre-existing outbound tests above all use synthetic
// 1-byte payloads on both channels, so a DrainOut that falls back to a single
// 0xFF byte on an empty queue passes them even though it desyncs the two files
// by ~159 bytes on every real, 160-byte (20ms @ 8kHz mu-law) silent frame.
// This test uses realistic frame-sized payloads on WriteIn, matching what the
// real Twilio media-stream path (session.go's frame size,
// SampleRateHz*MuLawFrameMS/1000) delivers, so a fixed-1-byte silence fallback
// fails it while the 5 synthetic tests stay green.
func TestTap_SilenceMatchesRealFrameSize(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSrealframe"
	const frameBytes = telephony.SampleRateHz * telephony.MuLawFrameMS / 1000 // 160

	realFrame := make([]byte, frameBytes)
	for i := range realFrame {
		realFrame[i] = byte(0x10 + i%16)
	}

	tap := NewTap(dir, streamSID, "CArealframe", "", testStreamStartedAt)
	// Frame 1: real audio on both sides.
	tap.WriteIn(realFrame)
	tap.WriteOut(realFrame)
	tap.DrainOut()
	// Frame 2: the engine is silent -- inbound still arrives at the same frame
	// size, but nothing was queued on the outbound side.
	tap.WriteIn(realFrame)
	tap.DrainOut()
	tap.Close()

	inAudio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading inbound recording: %v", err)
	}
	outAudio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound recording: %v", err)
	}

	if len(inAudio) != len(outAudio) {
		t.Fatalf("in/out channel lengths diverged: in=%d, out=%d — the silence fallback is not frame-sized, so a silent span desyncs the two channels", len(inAudio), len(outAudio))
	}

	wantSilence := bytes.Repeat([]byte{0xFF}, frameBytes)
	gotSilence := outAudio[frameBytes:]
	if !bytes.Equal(gotSilence, wantSilence) {
		t.Errorf("silence frame = % x (%d bytes), want %d bytes of 0xFF — DrainOut's empty-queue fallback must be a full frame, not a single byte", gotSilence, len(gotSilence), frameBytes)
	}
}

// TestTap_DisabledAllocatesNothing_Outbound verifies nil tap is a no-op.
func TestTap_DisabledAllocatesNothing_Outbound(t *testing.T) {
	tap := NewTap("", "SSdisabled", "CAdisabled", "", testStreamStartedAt)
	if tap != nil {
		t.Fatalf("NewTap with empty dir should return nil")
	}
	tap.WriteOut([]byte{0x01})
	tap.DrainOut()
}

// --- stream isolation + bounded queue (attempt 6) --------------------
//
// Phase 0 (attempt 6) red tests. tap.go currently shares a single openFailed
// bool between the in- and out-streams, and outQueue has no size cap. Both
// tests below are confirmed red against that code before the fix in this
// same commit series: TestTap_OutOpenFailureDoesNotHaltInboundWrites fails
// because WriteIn's second call sees openFailed=true (set by DrainOut's
// unrelated .out.ulaw open failure) and silently drops the frame;
// TestTap_OutboundQueueIsBounded fails because outQueue grows to exactly n
// with no cap. Neither assertion is to be weakened to match current
// behavior -- they pin the fix.

// TestTap_OutOpenFailureDoesNotHaltInboundWrites covers the cross-
// contamination the requirements-adversary flagged in attempt 5's handoff:
// a single shared openFailed bool means a failure opening .out.ulaw (via
// DrainOut) wrongly halts subsequent .in.ulaw writes, even though the
// in-stream's file handle is healthy. The two streams' failure states must
// be independent.
//
// The .out.ulaw open failure is simulated by pre-creating a directory at
// that exact path, so os.OpenFile(outulawPath, ...) fails with EISDIR --
// without needing to inject a writer, which would bypass the real open path
// entirely.
func TestTap_OutOpenFailureDoesNotHaltInboundWrites(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSisolate"

	if err := os.Mkdir(outulawPath(dir, streamSID), 0o755); err != nil {
		t.Fatalf("staging .out.ulaw collision directory: %v", err)
	}

	tap := NewTap(dir, streamSID, "CAisolate", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01})
	tap.DrainOut() // fails to open .out.ulaw -- must not affect the in-stream
	tap.WriteIn([]byte{0x02})
	tap.Close()

	inAudio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading inbound recording: %v", err)
	}
	want := []byte{0x01, 0x02}
	if !bytes.Equal(inAudio, want) {
		t.Errorf("inbound audio = % x, want % x — an outbound-only open failure must not halt inbound writes (shared openFailed cross-contamination)", inAudio, want)
	}
}

// TestTap_OutboundQueueIsBounded pins that outQueue is bounded: under a
// stalled drain (DrainOut not keeping pace with WriteOut -- e.g. triggered
// by the isolation bug above, or any other inbound-frame stall) the queue
// must never grow without limit. Direct field access is deliberate: this is
// a white-box test in the tap's own package, and the queue has no public
// accessor by design.
func TestTap_OutboundQueueIsBounded(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSqueuebound"
	// n must exceed maxOutQueueFrames (attempt 7 resized it to ~5 minutes of
	// frames) for this loop to actually drive the queue past its bound; the
	// assertion itself (qlen must stay below n) is unchanged from attempt 6.
	const n = maxOutQueueFrames + 1000

	tap := NewTap(dir, streamSID, "CAqueuebound", "", testStreamStartedAt)
	for i := 0; i < n; i++ {
		tap.WriteOut([]byte{0x01})
	}

	tap.mu.Lock()
	qlen := tap.outCount
	tap.mu.Unlock()

	if qlen >= n {
		t.Errorf("outQueue length = %d after %d WriteOut calls with no draining, want an enforced bound well under %d — the queue is unbounded", qlen, n, n)
	}
	tap.Close()
}

// --- ring buffer correctness + complexity (attempt 8) ---------------
//
// The two tests below pin the ring buffer that replaced a slice
// reslice-drop-oldest queue (t.outQueue = t.outQueue[1:]; append(...)). That
// queue was O(n) per call once at the bound: reslicing shrinks cap in
// lockstep with len, so each subsequent pop+append forces a full
// realloc+copy of the live queue.
// TestTap_WriteOutSteadyStateIsAllocationFree fails against it (all three of
// its windows allocate, checked by putting the reslice back into tap.go);
// TestTap_RingBufferWraparoundPreservesOrder is a correctness pin for the
// ring buffer (it passes against the reslice implementation too, since
// reslice-drop-oldest is *correct*, just not O(1) — its purpose is to catch
// an off-by-one in the ring buffer's head/tail bookkeeping, not to
// distinguish the two implementations by itself).

// TestTap_WriteOutSteadyStateIsAllocationFree pins the O(1) drop-oldest
// claim directly: once the queue is at its bound, further WriteOut calls
// must not allocate. A slice-reslice queue's cap shrinks with its len on
// every pop, so once full it forces a fresh backing-array alloc+copy of the
// entire live queue every few thousand calls — turning the safety net into
// an allocation storm during the one failure mode (a stalled drain) it
// exists to contain gracefully. A real ring buffer overwrites in place.
//
// How the measurement is made robust (AATK-144, AATK-95). The Mallocs
// counter is process-wide, so it also counts any other goroutine that
// allocates inside the window: a runtime background allocation (observed
// roughly once per thousand runs at default GOMAXPROCS; its source was not
// established), or a goroutine a sibling test left running. A single window
// therefore fails on noise that has nothing to do with WriteOut. The route
// taken:
//
//   - Up to three independent windows of 50000 calls each, run under
//     GOMAXPROCS(1) (restored by singleProc's cleanup); the test fails only if
//     EVERY window allocates. A real regression allocates in every window, a
//     stray allocation lands in one. The windows stop at the first clean one.
//   - Sibling test goroutines are joined in t.Cleanup (goJoined) where they
//     are free-running tickers, so they cannot allocate into this window.
//     Not joined: goroutines inside hijacked websocket handlers (the fake
//     backend's handler and its emitAudioDeltas, the harness handlers),
//     which httptest.Server.Close does not wait for, and realtimeHarness's
//     countMediaFrames read loop, which ends only when its conn errors.
//
// Rejected:
//
//   - testing.AllocsPerRun: it divides total mallocs by runs with integer
//     division, so any average below 1 truncates to 0, and the reslice
//     queue's realloc recurs only periodically, never below 1-per-call.
//     Run against that queue it reported 0, where the three windows below
//     reported 18, 12 and 12.
//   - A single window, however large: one stray allocation fails it.
//   - runtime.GC() before the window: it can only reduce background work,
//     the ticket's trial of it (0 failures in 4000 runs) is an observation
//     and not an attribution, and it leaves the window open to anything else.
//   - Skipping under -short, or running the test alone: hides the
//     cross-test leak instead of removing it.
//   - Joined teardown alone: it does nothing about runtime allocations,
//     which are independent of any sibling test.
//
// A top-level t.Parallel in this package would break the premise that
// nothing else allocates, so none is used.
func TestTap_WriteOutSteadyStateIsAllocationFree(t *testing.T) {
	const (
		steadyStateCalls   = 50000
		steadyStateWindows = 3
	)

	dir := t.TempDir()
	tap := NewTap(dir, "SSallocs", "CAallocs", "", testStreamStartedAt)
	defer tap.Close()

	// Before warm-up, so the stop-the-world GOMAXPROCS call never lands in a
	// window.
	singleProc(t)

	payload := []byte{0x01}
	for i := 0; i < maxOutQueueFrames+10; i++ {
		tap.WriteOut(payload)
	}

	// Declared once, outside: a MemStats inside the closure would allocate
	// on every call and add one to every window.
	var ms runtime.MemStats
	mallocs := func() uint64 {
		runtime.ReadMemStats(&ms)
		return ms.Mallocs
	}
	deltas, clean := mallocWindows(steadyStateWindows, steadyStateCalls, mallocs, func() { tap.WriteOut(payload) })
	if !clean {
		t.Errorf("%v heap allocations in each of %d windows of %d steady-state WriteOut calls, want 0 in at least one — drop-oldest must overwrite in place (a ring buffer), not periodically realloc+copy the live queue", deltas, len(deltas), steadyStateCalls)
	}
}

// mallocWindows runs body calls times per window, for up to windows windows,
// and records the process-wide Mallocs delta of each. It stops at the first
// window with no allocation and reports clean; clean == false means every
// window allocated. Nothing but the body loop sits between the two mallocs
// reads, so the helper adds no allocation of its own to a window.
func mallocWindows(windows, calls int, mallocs func() uint64, body func()) (deltas []uint64, clean bool) {
	deltas = make([]uint64, 0, windows)
	for w := 0; w < windows; w++ {
		before := mallocs()
		for i := 0; i < calls; i++ {
			body()
		}
		after := mallocs()
		deltas = append(deltas, after-before)
		if after == before {
			return deltas, true
		}
	}
	return deltas, false
}

// singleProc sets GOMAXPROCS to 1 for the rest of the test and restores the
// previous value in t.Cleanup, so it is restored on t.Fatal and panic too.
func singleProc(t *testing.T) {
	t.Helper()
	old := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
}

// fakeMallocs is a deterministic stand-in for the process-wide Mallocs
// counter: it advances by perWindow[w] over window w, and by nothing outside
// the listed windows. It counts body calls so a test can tell which window a
// call belongs to.
type fakeMallocs struct {
	perWindow []uint64
	calls     int
	perCall   int
	counter   uint64
}

func (f *fakeMallocs) body() {
	f.calls++
	w := (f.calls - 1) / f.perCall
	if (f.calls-1)%f.perCall == 0 && w < len(f.perWindow) {
		f.counter += f.perWindow[w]
	}
}

func (f *fakeMallocs) mallocs() uint64 { return f.counter }

func TestMallocWindows_TolerantOfOneAllocatingWindow(t *testing.T) {
	f := &fakeMallocs{perWindow: []uint64{1}, perCall: 10}
	deltas, clean := mallocWindows(3, 10, f.mallocs, f.body)
	if !clean {
		t.Fatalf("clean = false, deltas = %v: one allocating window followed by a quiet one must pass", deltas)
	}
	if len(deltas) != 2 || deltas[0] != 1 || deltas[1] != 0 {
		t.Fatalf("deltas = %v, want [1 0]", deltas)
	}
}

func TestMallocWindows_FailsWhenEveryWindowAllocates(t *testing.T) {
	f := &fakeMallocs{perWindow: []uint64{1, 2, 3}, perCall: 10}
	deltas, clean := mallocWindows(3, 10, f.mallocs, f.body)
	if clean {
		t.Fatalf("clean = true, deltas = %v: every window allocated, want clean = false", deltas)
	}
	if len(deltas) != 3 {
		t.Fatalf("len(deltas) = %d, want 3 (%v)", len(deltas), deltas)
	}
	for i, d := range deltas {
		if d == 0 {
			t.Errorf("deltas[%d] = 0, want > 0", i)
		}
	}
	if f.calls != 30 {
		t.Errorf("body ran %d times, want 30 (3 windows x 10 calls)", f.calls)
	}
}

func TestSingleProc_RestoresGOMAXPROCS(t *testing.T) {
	orig := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(orig) })

	t.Run("inner", func(t *testing.T) {
		singleProc(t)
		if got := runtime.GOMAXPROCS(0); got != 1 {
			t.Errorf("GOMAXPROCS inside = %d, want 1", got)
		}
	})
	if got := runtime.GOMAXPROCS(0); got != 2 {
		t.Errorf("GOMAXPROCS after the subtest = %d, want 2 restored", got)
	}
}

// TestTap_RingBufferWraparoundPreservesOrder pins FIFO correctness across a
// ring buffer's head/tail wraparound -- a classic off-by-one failure mode
// that a pure complexity claim would never catch. Each pushed frame carries
// a distinct 2-byte index, so after pushing well past the bound (forcing
// drop-oldest to wrap the backing array around at least once) and draining
// everything left, the recorded bytes must be exactly the surviving
// (non-dropped) indices, in order -- proving the buffer neither reorders
// nor corrupts frames as head and tail cross the end of the backing array.
func TestTap_RingBufferWraparoundPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSwraparound"
	const overshoot = 500
	const n = maxOutQueueFrames + overshoot // forces wraparound at least once

	tap := NewTap(dir, streamSID, "CAwraparound", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x00}) // keep Close from treating this as a no-media stream
	for i := 0; i < n; i++ {
		tap.WriteOut([]byte{byte(i >> 8), byte(i)})
	}

	// Drop-oldest keeps the newest maxOutQueueFrames pushes: indices
	// [overshoot, n). Draining maxOutQueueFrames times must yield exactly
	// those indices, in order.
	for i := 0; i < maxOutQueueFrames; i++ {
		tap.DrainOut()
	}
	tap.Close()

	outAudio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound recording: %v", err)
	}

	want := make([]byte, 0, maxOutQueueFrames*2)
	for i := overshoot; i < n; i++ {
		want = append(want, byte(i>>8), byte(i))
	}
	if !bytes.Equal(outAudio, want) {
		t.Fatalf("drained outbound bytes do not match the surviving pushed indices in order — a ring buffer head/tail bug reordered or corrupted frames across wraparound (got %d bytes, want %d bytes)", len(outAudio), len(want))
	}
}

// TestTap_SidecarRecordsDropCount pins attempt 6 handoff verification's
// second finding: outDrops is tracked internally but never surfaced, so a
// recording that lost frames to the outbound queue's bound is
// indistinguishable downstream from a clean one. This test forces real
// drops (WriteOut past maxOutQueueFrames with no draining) and asserts the
// sidecar JSON carries a non-zero out_dropped_frames count.
func TestTap_SidecarRecordsDropCount(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSdropcount"

	tap := NewTap(dir, streamSID, "CAdropcount", "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01}) // ensure the sidecar is written (frames > 0)
	for i := 0; i < maxOutQueueFrames+50; i++ {
		tap.WriteOut([]byte{0x02})
	}
	tap.Close()

	side := readSidecar(t, dir, streamSID)
	if side.OutDroppedFrames != 50 {
		t.Errorf("sidecar out_dropped_frames = %d, want 50 — the tracked outDrops count must be surfaced in the sidecar", side.OutDroppedFrames)
	}
}

// --- one-directional taps (AATK-101) -----------------------------------------
//
// Every test above drives a duplex Tap, which is the only kind that existed.
// A consumer that only ever sees one direction of a call -- the realtime path
// hands out outbound audio and never surfaces inbound -- could not use Tap at
// all, and got no error saying so: WriteOut only enqueues, so with no inbound
// frames nothing ever drained and out.ulaw was never opened; and Close deleted
// both recordings whenever t.frames (inbound only) was zero. The tests below
// pin the declared-channel-set behavior that fixes both, and the golden that
// keeps duplex exactly as it was.

// fakeClock is a hand-advanced clock. It needs no mutex: dropOldestPlane reads
// it only under p.mu, and these tests drive Send/Recv from one goroutine.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// outboundOnlyTap builds the Tap under test in this section: outbound-only,
// paced by a hand-advanced clock so the gap fill is deterministic rather than
// a function of how long the test took to run.
func outboundOnlyTap(dir, streamSID, callSID string, clk *fakeClock) *Tap {
	return NewTap(dir, streamSID, callSID, "", testStreamStartedAt, WithChannels(ChannelOut), withNow(clk.now))
}

// realFrame is one 20ms mu-law frame of distinguishable audio. Frame-sized
// payloads matter here for the same reason they do in
// TestTap_SilenceMatchesRealFrameSize: a 1-byte payload cannot tell a
// frame-sized silence fill from a 1-byte one.
func realFrame(seed byte) []byte {
	f := make([]byte, telephony.SampleRateHz*telephony.MuLawFrameMS/1000)
	for i := range f {
		f[i] = seed + byte(i%16)
	}
	return f
}

// TestTap_OutboundOnlyWritesWithoutInboundFrames is behavior 1: a Tap declared
// outbound-only records the payloads it is given without any inbound frame to
// clock it, and its sidecar says what the recording is.
//
// Against the pre-AATK-101 tap this fails by finding no out.ulaw at all --
// WriteOut enqueued into the ring buffer and only DrainOut, called once per
// inbound frame, ever opened the file.
//
// The clock does not advance here, so no gap fill is involved: this test is
// about the bytes reaching disk, and TestTap_OutboundOnlyFillsGapsWithSilence
// is about their timing.
func TestTap_OutboundOnlyWritesWithoutInboundFrames(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSoutonly"
	clk := &fakeClock{t: testStreamStartedAt}

	tap := outboundOnlyTap(dir, streamSID, "CAoutonly", clk)
	payloads := [][]byte{realFrame(0x10), realFrame(0x20), realFrame(0x30)}
	for _, p := range payloads {
		tap.WriteOut(p)
	}
	tap.Close()

	audio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading the outbound recording: %v — an outbound-only tap must open out.ulaw on WriteOut, without waiting for an inbound frame to drain it", err)
	}
	if want := bytes.Join(payloads, nil); !bytes.Equal(audio, want) {
		t.Errorf("outbound audio = %d bytes, want %d — WriteOut must write through on an outbound-only tap", len(audio), len(want))
	}

	side := readSidecar(t, dir, streamSID)
	if want := []string{string(ChannelOut)}; !slices.Equal(side.Channels, want) {
		t.Errorf("sidecar channels = %v, want %v — downstream must be able to tell a one-directional recording from a duplex one without opening the audio", side.Channels, want)
	}
	// Asserted as an equality, not merely as "not the duplex string": every
	// channel set states a different clock, and swapping this recording's for
	// another one's is not caught by a test that only rules out one of them.
	if side.Alignment != outboundWallClockAlignment {
		t.Errorf("sidecar alignment = %q, want %q — there is no inbound frame clock on an outbound-only recording, and a consumer trusting the wrong string misreads the whole timeline", side.Alignment, outboundWallClockAlignment)
	}
}

// TestTap_OutboundOnlyCloseKeepsTheRecording is behavior 4 in the shape that
// bites: Close's deletion rule counted inbound frames only, so a recording with
// nothing but outbound audio was unlinked on the way out -- both ulaw files
// removed and the early return skipping the sidecar too.
//
// Asserted as a contrast, deliberately: "the file is still there" is also true
// of a Close that does nothing at all, so the same test requires the opposite
// case -- an outbound-only tap that recorded nothing leaves nothing behind.
func TestTap_OutboundOnlyCloseKeepsTheRecording(t *testing.T) {
	dir := t.TempDir()
	const recordedSID, silentSID = "SSkept", "SSnothing"

	clk := &fakeClock{t: testStreamStartedAt}
	recorded := outboundOnlyTap(dir, recordedSID, "CAkept", clk)
	recorded.WriteOut(realFrame(0x40))
	recorded.Close()

	if _, err := os.Stat(outulawPath(dir, recordedSID)); err != nil {
		t.Errorf("an outbound-only recording was deleted by Close (stat err = %v) — the frames==0 rule counts inbound frames, and there are none by construction here", err)
	}
	if _, err := os.Stat(sidecarPath(dir, recordedSID)); err != nil {
		t.Errorf("an outbound-only recording got no sidecar (stat err = %v) — a .ulaw with nothing next to it is indistinguishable from capture noise", err)
	}

	// The contrast: nothing recorded, so nothing left behind.
	silent := outboundOnlyTap(dir, silentSID, "CAnothing", clk)
	silent.Close()
	if _, err := os.Stat(outulawPath(dir, silentSID)); !os.IsNotExist(err) {
		t.Errorf("an outbound-only tap that captured nothing left an audio file (stat err = %v)", err)
	}
	if _, err := os.Stat(sidecarPath(dir, silentSID)); !os.IsNotExist(err) {
		t.Errorf("an outbound-only tap that captured nothing left a sidecar (stat err = %v)", err)
	}
}

// TestTap_OutboundOnlyIgnoresDrainOut is behavior 2's first half. A consumer
// that copied the duplex call pattern will call DrainOut as well as WriteOut;
// on an outbound-only tap the payload has already been written through, so a
// drain that did anything would duplicate frames or interleave silence into a
// file that has no inbound clock to justify it.
func TestTap_OutboundOnlyIgnoresDrainOut(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSnodrain"
	clk := &fakeClock{t: testStreamStartedAt}

	tap := outboundOnlyTap(dir, streamSID, "CAnodrain", clk)
	first, second := realFrame(0x50), realFrame(0x60)
	tap.WriteOut(first)
	tap.DrainOut()
	tap.DrainOut()
	tap.WriteOut(second)
	tap.DrainOut()
	tap.Close()

	audio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading the outbound recording: %v", err)
	}
	want := append(append([]byte{}, first...), second...)
	if !bytes.Equal(audio, want) {
		t.Errorf("outbound audio = %d bytes, want %d — DrainOut must be a no-op on an outbound-only tap, so a consumer calling both cannot double-write or pad the file", len(audio), len(want))
	}
}

// TestTap_OutboundOnlyIgnoresInboundWrites is behavior 2's second half: the
// channel set is a declaration, so a direction that was not declared never
// reaches disk. Without this, a consumer that wired WriteIn by mistake would
// get an in.ulaw whose timeline has no relation to out.ulaw's, and a sidecar
// saying channels: ["out"] over two files.
func TestTap_OutboundOnlyIgnoresInboundWrites(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSnoinbound"
	clk := &fakeClock{t: testStreamStartedAt}

	tap := outboundOnlyTap(dir, streamSID, "CAnoinbound", clk)
	tap.WriteOut(realFrame(0x70))
	tap.WriteIn(realFrame(0x80))
	tap.Close()

	if _, err := os.Stat(inulawPath(dir, streamSID)); !os.IsNotExist(err) {
		t.Errorf("an outbound-only tap opened in.ulaw (stat err = %v) — an undeclared direction must not reach disk", err)
	}
	if side := readSidecar(t, dir, streamSID); side.Frames != 0 || side.Bytes != 0 {
		t.Errorf("sidecar frames/bytes = %d/%d, want 0/0 — an undeclared direction must not be counted either", side.Frames, side.Bytes)
	}
}

// TestTap_OutboundOnlyFillsGapsWithSilence is behavior 5, the clock. A gapless
// concatenation of what the engine said would turn a 3-minute call with 40
// seconds of speech into 40 seconds of audio with every pause removed -- the
// same failure cmd/twilio-cli/filler.go exists to prevent on the playout side.
// So an outbound-only recording is paced against the wall clock: the gap
// between two WriteOut calls appears as silence of that length, and a call that
// outlives its last frame is padded to its end at Close.
//
// Driven through the injected clock, never time.Sleep: the assertion is an
// exact byte count, and a real clock would make it a race.
func TestTap_OutboundOnlyFillsGapsWithSilence(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSgaps"
	frameBytes := telephony.SampleRateHz * telephony.MuLawFrameMS / 1000
	clk := &fakeClock{t: testStreamStartedAt}

	tap := outboundOnlyTap(dir, streamSID, "CAgaps", clk)
	first, second := realFrame(0x11), realFrame(0x22)

	tap.WriteOut(first) // t=0: the recording starts at the first outbound frame
	clk.advance(120 * time.Millisecond)
	tap.WriteOut(second) // 20ms of that gap is `first` playing out; 100ms is silence
	clk.advance(60 * time.Millisecond)
	tap.Close() // 20ms of that is `second`; 40ms is trailing silence

	audio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading the outbound recording: %v", err)
	}

	silence := bytes.Repeat([]byte{0xFF}, frameBytes)
	var want []byte
	want = append(want, first...)
	want = append(want, bytes.Repeat(silence, 5)...) // the 100ms gap
	want = append(want, second...)
	want = append(want, bytes.Repeat(silence, 2)...) // the 40ms tail
	if !bytes.Equal(audio, want) {
		t.Fatalf("outbound audio = %d bytes, want %d — the recording must carry the call's silence, not just its speech", len(audio), len(want))
	}

	// The point of the byte count, stated as what it means: the recording is
	// as long as the span of the call it covers, not as long as its payloads.
	gotMS := len(audio) * 1000 / telephony.SampleRateHz
	if wantMS := 180; gotMS != wantMS {
		t.Errorf("recording duration = %dms, want %dms (payload total is only %dms)", gotMS, wantMS, 2*telephony.MuLawFrameMS)
	}
}

// TestTap_DuplexOutputUnchanged is behavior 3, and it is a golden rather than a
// red test: it must pass both before and after AATK-101. Duplex recordings are
// what probeset build already consumes, so the load-bearing safety property of
// the channel set is that adding it changed nothing for them -- the queue, the
// inbound frame clock, the empty-queue silence, the truncation flag and the
// sidecar strings all as they were. Reasoning that "duplex is untouched" is not
// the same as a fixed script asserting it byte for byte.
func TestTap_DuplexOutputUnchanged(t *testing.T) {
	dir := t.TempDir()
	const streamSID, callSID = "SSduplexgolden", "CAduplexgolden"
	frameBytes := telephony.SampleRateHz * telephony.MuLawFrameMS / 1000

	in1, in2, in3 := realFrame(0x01), realFrame(0x02), realFrame(0x03)
	out1, out2, out3 := realFrame(0xA0), realFrame(0xB0), realFrame(0xC0)

	tap := NewTap(dir, streamSID, callSID, "golden", testStreamStartedAt)
	tap.WriteOut(out1)
	tap.WriteOut(out2)
	tap.WriteIn(in1)
	tap.DrainOut() // dequeues out1
	tap.WriteIn(in2)
	tap.DrainOut() // dequeues out2
	tap.WriteIn(in3)
	tap.DrainOut() // queue empty: one frame of 0xFF, sized to the inbound frame
	tap.WriteOut(out3)
	tap.Close() // out3 is still queued: truncated

	gotIn, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading inbound recording: %v", err)
	}
	gotOut, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading outbound recording: %v", err)
	}

	wantIn := bytes.Join([][]byte{in1, in2, in3}, nil)
	wantOut := bytes.Join([][]byte{out1, out2, bytes.Repeat([]byte{0xFF}, frameBytes)}, nil)
	if !bytes.Equal(gotIn, wantIn) {
		t.Errorf("duplex in.ulaw changed: %d bytes, want %d", len(gotIn), len(wantIn))
	}
	if !bytes.Equal(gotOut, wantOut) {
		t.Errorf("duplex out.ulaw changed: %d bytes, want %d — the inbound frame clock and its 0xFF fill are what make the two files time-aligned", len(gotOut), len(wantOut))
	}
	if len(gotIn) != len(gotOut) {
		t.Errorf("duplex channels desynced: in=%d out=%d", len(gotIn), len(gotOut))
	}

	side := readSidecar(t, dir, streamSID)
	want := tapSidecar{
		StreamSID:    streamSID,
		CallSID:      callSID,
		Label:        "golden",
		StartedAt:    testStreamStartedAt,
		Frames:       3,
		Bytes:        3 * frameBytes,
		VAD:          wantRecordedVAD,
		Alignment:    duplexAlignment,
		Channels:     []string{string(ChannelIn), string(ChannelOut)},
		OutTruncated: true,
	}
	if !reflect.DeepEqual(side, want) {
		t.Errorf("duplex sidecar = %+v, want %+v", side, want)
	}
}

// TestTap_SidecarBytesUnchanged pins the sidecar file byte for byte, for both
// shapes of Tap a depending project can build: duplex, and outbound-only as on
// the realtime path. The other sidecar tests decode the file and compare
// values, which cannot see how the bytes are laid out: a field moved within the
// sidecar struct, or within its vad_config block, decodes equal and is written
// in another order.
//
// The literals are the sidecars this suite wrote before AATK-164 removed the
// VAD pipeline, captured from a run rather than written by hand, so they are an
// oracle independent of whatever struct produces the file.
func TestTap_SidecarBytesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streamSID string
		record    func(dir, streamSID string)
		want      string
	}{
		{
			name:      "duplex",
			streamSID: "SSsidecarbytesduplex",
			record: func(dir, streamSID string) {
				tap := NewTap(dir, streamSID, "CAsidecarbytesduplex", "golden", testStreamStartedAt)
				tap.WriteOut(realFrame(0xA0))
				tap.WriteOut(realFrame(0xB0))
				tap.WriteIn(realFrame(0x01))
				tap.DrainOut()
				tap.WriteIn(realFrame(0x02))
				tap.DrainOut()
				tap.WriteIn(realFrame(0x03))
				tap.DrainOut()
				tap.WriteOut(realFrame(0xC0))
				tap.Close()
			},
			want: `{"stream_sid":"SSsidecarbytesduplex","call_sid":"CAsidecarbytesduplex","label":"golden","started_at":"2026-07-17T09:30:15.123456789Z","frames":3,"bytes":480,"vad_config":{"WindowSize":256,"SpeechThresh":0.5,"SilenceThresh":0.35,"EndSilenceMS":700,"SampleRateHz":8000,"TurnEndSilenceMS":5000},"alignment":"inbound-frame-clock, 20ms/frame, silence=0xFF","channels":["in","out"],"out_truncated":true}`,
		},
		{
			name:      "outbound-only",
			streamSID: "SSsidecarbytesoutonly",
			record: func(dir, streamSID string) {
				clk := &fakeClock{t: testStreamStartedAt}
				tap := outboundOnlyTap(dir, streamSID, "CAsidecarbytesoutonly", clk)
				tap.WriteOut(realFrame(0x10))
				clk.advance(40 * time.Millisecond)
				tap.Close()
			},
			want: `{"stream_sid":"SSsidecarbytesoutonly","call_sid":"CAsidecarbytesoutonly","started_at":"2026-07-17T09:30:15.123456789Z","frames":0,"bytes":0,"vad_config":{"WindowSize":256,"SpeechThresh":0.5,"SilenceThresh":0.35,"EndSilenceMS":700,"SampleRateHz":8000,"TurnEndSilenceMS":5000},"alignment":"wall-clock, 20ms/frame, silence=0xFF, t0=first outbound frame","channels":["out"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.record(dir, tc.streamSID)

			got, err := os.ReadFile(sidecarPath(dir, tc.streamSID))
			if err != nil {
				t.Fatalf("reading sidecar: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("sidecar bytes changed:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestTap_DuplexKeepsSidecarWhenOnlyOneDirectionRecorded is behavior 4 for a
// duplex tap: a real call where the engine never spoke opens no out.ulaw, and
// that must not cost the recording its sidecar. Green before AATK-101 as well
// -- it pins the half of the per-file rule that already held, so the
// generalization in Close cannot quietly drop it.
func TestTap_DuplexKeepsSidecarWhenOnlyOneDirectionRecorded(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSinboundonly"

	tap := NewTap(dir, streamSID, "CAinboundonly", "", testStreamStartedAt)
	tap.WriteIn(realFrame(0x05))
	tap.Close()

	if _, err := os.Stat(inulawPath(dir, streamSID)); err != nil {
		t.Errorf("the direction that recorded left no file (stat err = %v)", err)
	}
	if _, err := os.Stat(outulawPath(dir, streamSID)); !os.IsNotExist(err) {
		t.Errorf("the direction that recorded nothing left a file (stat err = %v)", err)
	}
	if _, err := os.Stat(sidecarPath(dir, streamSID)); err != nil {
		t.Errorf("a recording with content on one channel got no sidecar (stat err = %v)", err)
	}
}

// countingWriter accepts everything and counts the Write calls. It is the
// assertion behind TestTap_OutboundOnlyBatchesLongGapFills: what the
// silence fill puts on disk is already pinned byte for byte by
// TestTap_OutboundOnlyFillsGapsWithSilence, so what is left to pin is how many
// syscalls it takes to get there.
type countingWriter struct {
	writes int
	bytes  int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	c.bytes += len(p)
	return len(p), nil
}
func (c *countingWriter) Close() error { return nil }

// TestTap_OutboundOnlyBatchesLongGapFills pins the silence fill's cost, not
// its bytes. A gap on a one-directional tap is legitimately call-length -- the
// engine is quiet while the caller talks -- and the fill runs inside WriteOut,
// under t.mu, on the outbound send path. A syscall per 20ms frame makes a quiet
// half hour ~90,000 writes issued in one burst there; batching makes it ~900
// and changes nothing about the recording.
func TestTap_OutboundOnlyBatchesLongGapFills(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: testStreamStartedAt}
	w := &countingWriter{}

	tap := outboundOnlyTap(dir, "SSchunked", "CAchunked", clk)
	tap.wOut = w // as newTapWithOutWriter does, but this tap needs the options too

	const gapFrames = 1500 // 30 seconds of silence
	frameDur := telephony.MuLawDuration(defaultFrameBytes)

	tap.WriteOut(realFrame(0x01))
	// The first frame put fedThrough one frame ahead, so advancing by one more
	// than the gap leaves exactly gapFrames to fill.
	clk.advance(time.Duration(gapFrames+1) * frameDur)
	tap.WriteOut(realFrame(0x02))
	tap.Close()

	if want := (gapFrames + 2) * defaultFrameBytes; w.bytes != want {
		t.Fatalf("outbound bytes = %d, want %d — batching must not change what the recording holds", w.bytes, want)
	}
	// Two payload writes plus one per chunk, rounded up.
	maxWrites := 2 + (gapFrames+silenceChunkFrames-1)/silenceChunkFrames
	if w.writes > maxWrites {
		t.Errorf("silence fill issued %d writes for a %d-frame gap, want at most %d — one write per frame stalls the send path it holds the lock on", w.writes, gapFrames, maxWrites)
	}
}

// TestTap_InboundOnlyRecordsWithoutOutbound is the third channel set. It has no
// out.ulaw to align against, so it claims neither the duplex inbound-frame
// clock nor the outbound wall clock, and an outbound frame handed to it is
// dropped the way an inbound one is dropped by an outbound-only tap.
func TestTap_InboundOnlyRecordsWithoutOutbound(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSinonly"

	tap := NewTap(dir, streamSID, "CAinonly", "", testStreamStartedAt, WithChannels(ChannelIn))
	in1, in2 := realFrame(0x0A), realFrame(0x0B)
	tap.WriteIn(in1)
	tap.WriteOut(realFrame(0xF0))
	tap.WriteIn(in2)
	tap.DrainOut()
	tap.Close()

	audio, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading the inbound recording: %v", err)
	}
	if want := bytes.Join([][]byte{in1, in2}, nil); !bytes.Equal(audio, want) {
		t.Errorf("inbound audio = %d bytes, want %d", len(audio), len(want))
	}
	if _, err := os.Stat(outulawPath(dir, streamSID)); !os.IsNotExist(err) {
		t.Errorf("an inbound-only tap left an outbound recording (stat err = %v) — an undeclared direction must not reach disk, and DrainOut must not open a file for it either", err)
	}

	side := readSidecar(t, dir, streamSID)
	if want := []string{string(ChannelIn)}; !slices.Equal(side.Channels, want) {
		t.Errorf("sidecar channels = %v, want %v", side.Channels, want)
	}
	if side.Alignment != inboundArrivalAlignment {
		t.Errorf("sidecar alignment = %q, want %q — with no second file there is no cross-channel alignment to claim", side.Alignment, inboundArrivalAlignment)
	}
}

// TestTap_WithChannelsIgnoresRepeats pins the channel set against a caller
// naming a direction twice. The set is rendered straight into the sidecar, so
// without this a duplex tap declared as (in, out, out) writes channels:
// ["in","out","out"] -- a duplex recording that no longer looks like one to the
// consumer reading that field.
func TestTap_WithChannelsIgnoresRepeats(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSrepeats"

	tap := NewTap(dir, streamSID, "CArepeats", "", testStreamStartedAt, WithChannels(ChannelIn, ChannelOut, ChannelOut))
	tap.WriteIn(realFrame(0x0C))
	tap.DrainOut()
	tap.Close()

	side := readSidecar(t, dir, streamSID)
	want := []string{string(ChannelIn), string(ChannelOut)}
	if !slices.Equal(side.Channels, want) {
		t.Errorf("sidecar channels = %v, want %v — a repeated direction is one direction", side.Channels, want)
	}
	if side.Alignment != duplexAlignment {
		t.Errorf("sidecar alignment = %q, want %q", side.Alignment, duplexAlignment)
	}
}

// TestTap_OutboundOnlyAbsorbsFasterThanRealTimeBursts pins the half of the
// wall-clock pacing fillOutGap's comment claims and nothing asserted: a backend
// that hands over a clip faster than it plays leaves fedThrough ahead of the
// clock, and the surplus is absorbed by the next real gap rather than fought
// over. A fill that treated a negative gap as anything but zero frames would
// either pad silence in between a burst's frames or, by resyncing fedThrough to
// now, swallow the part of the following gap the burst already covers.
func TestTap_OutboundOnlyAbsorbsFasterThanRealTimeBursts(t *testing.T) {
	dir := t.TempDir()
	const streamSID = "SSburst"
	clk := &fakeClock{t: testStreamStartedAt}

	tap := outboundOnlyTap(dir, streamSID, "CAburst", clk)
	burst := [][]byte{realFrame(0x31), realFrame(0x32), realFrame(0x33)}
	for _, p := range burst { // 60ms of audio handed over at one instant
		tap.WriteOut(p)
	}
	clk.advance(100 * time.Millisecond) // 60ms of which the burst is still playing
	last := realFrame(0x34)
	tap.WriteOut(last)
	tap.Close() // fedThrough is 20ms past now again, so no tail

	audio, err := os.ReadFile(outulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("reading the outbound recording: %v", err)
	}
	silence := bytes.Repeat([]byte{0xFF}, defaultFrameBytes)
	want := bytes.Join([][]byte{burst[0], burst[1], burst[2], silence, silence, last}, nil)
	if !bytes.Equal(audio, want) {
		t.Fatalf("outbound audio = %d bytes, want %d — a burst must not be padded apart, and only the 40ms it did not cover may become silence", len(audio), len(want))
	}
}
