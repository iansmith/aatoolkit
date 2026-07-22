package twilio

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// SOP-152 Phase 0 (rev 3). These tests describe the audio tap's expected
// behavior and fail against the stubs in tap.go.
//
// The tap exists to capture real telephony audio for the probe dataset
// (SOP-153): mu-law over the phone network is what turns "I can't pick up the
// kids" into "I can help you", and no mic recording reproduces that. Its
// governing constraint is that it must be invisible -- off by default, and
// incapable of harming a call when on.
//
// Recordings are keyed by streamSid, not CallSID
// (docs/twilio-media-streams-spec.md:228 -- streamSid uniquely identifies a
// Media Streams connection; a reconnect is a new connection on the same call).

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
// never breaks a call" guarantee (Observable behavior 4). It counts both
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

// --- the two that make the rest matter ---------------------------------------

// TestTap_WiredToDataPlane is the test this suite exists for. Every other test
// here drives a Tap directly, so all of them pass against a flawless Tap that
// production never calls -- which is exactly what the rev-2 suite did, with
// NewTap having no non-test callers at all.
//
// Contract 2's load-bearing word is "delivered": the bytes on disk must be the
// payloads delivered to the session. This drives a real media frame through the
// real handler path -- websocket, DecodeFrame, demux, pumpDataPlane -- and
// requires it to land on disk. Nothing is asserted by waiting: the stop frame
// makes handleStream return, and its defer closes the tap before h.done fires.
//
// This is load-independent because teardown is a structural stop->drain->close
// boundary (AATK-15): the defer closes the data plane, the pump drains every
// buffered frame to the tap and terminates on errPlaneClosed, then tap.Close
// runs. It used to flake -- a frame buffered at teardown was abandoned in a
// 50/50 select between a ready <-ch and a ready <-ctx.Done(). See
// TestHandleStreamDrainsDataBeforeTapClose for the K-frame drain guard and
// design/teardown-protocol.md for the protocol.
func TestTap_WiredToDataPlane(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(tapDirEnv, dir)

	h := newHarness(t)
	h.sendMedia([]byte{0x01, 0x02, 0x03})
	h.sendRaw([]byte(`{"event":"stop","streamSid":"SS` + t.Name() + `"}`))

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after stop")
	}

	streamSID := "SS" + t.Name()
	got, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("no recording for a call that carried media — the tap is not wired to the data plane: %v", err)
	}
	if want := []byte{0x01, 0x02, 0x03}; !bytes.Equal(got, want) {
		t.Errorf("recording = % x, want % x", got, want)
	}

	// The start time has to survive the real path too, not just a direct NewTap
	// call: a wrong instant here means the production chain sampled a clock
	// somewhere instead of passing down the one it was given.
	if side := readSidecar(t, dir, streamSID); !side.StartedAt.Equal(testStreamStartedAt) {
		t.Errorf("sidecar started_at = %s, want %s — handleStream is not passing the stream's start time down to the tap",
			side.StartedAt.Format(time.RFC3339Nano), testStreamStartedAt.Format(time.RFC3339Nano))
	}
}

// TestTap_CreatesMissingTapDir: the tap directory (AATOOLKIT_AUDIO_TAP) may not
// exist yet -- a fresh build/, or one just emptied by `task clean`. handleStream
// must create it, or every capture write (tap AND the decision recorder, which
// share the dir) silently ENOENTs, since capture is best-effort. Regression for
// the live "open build/audio/....in.ulaw: no such file or directory" seen when
// build/audio had never been created.
func TestTap_CreatesMissingTapDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	t.Setenv(tapDirEnv, dir)

	h := newHarness(t)
	h.sendMedia([]byte{0x01, 0x02, 0x03})
	h.sendRaw([]byte(`{"event":"stop","streamSid":"SS` + t.Name() + `"}`))

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after stop")
	}

	streamSID := "SS" + t.Name()
	if _, err := os.ReadFile(inulawPath(dir, streamSID)); err != nil {
		t.Fatalf("no recording — handleStream did not create the missing tap dir %s: %v", dir, err)
	}
}

// TestTap_EnvVarNamesTheDir pins the variable's spelling. Without it, an
// implementation reading AATOOLKIT_TAP (or AATOOLKIT_AUDIO_TAP_DIR, or anything else)
// is green, captures nothing, and looks correct -- the operator simply finds no
// files and has nothing to go on.
func TestTap_EnvVarNamesTheDir(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		t.Setenv(tapDirEnv, "/tmp/some-tap-dir")
		if got := tapDirFromEnv(); got != "/tmp/some-tap-dir" {
			t.Errorf("tapDirFromEnv() = %q, want %q — is the tap reading %s?", got, "/tmp/some-tap-dir", tapDirEnv)
		}
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv(tapDirEnv, "")
		if got := tapDirFromEnv(); got != "" {
			t.Errorf("tapDirFromEnv() = %q, want \"\" — capture must be off by default", got)
		}
	})
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

// TestTap_SidecarRecordsVADConfig: the sidecar's job is to tell a later replay
// which thresholds produced this stream's live log, so SOP-153 can check it is
// comparing like with like.
//
// The comparison is derived, never hardcoded. No literal threshold appears
// here: EndSilenceMS belongs to SOP-154, which takes it 1050 -> 700. f5cad49 is
// the worked example of the cost -- six tests across three packages broke on a
// tuning change because they had hardcoded 22 = 700ms/32ms, failing in packages
// that had nothing to do with the cause.
func TestTap_SidecarRecordsVADConfig(t *testing.T) {
	dir := t.TempDir()
	const streamSID, callSID = "SSsidecar", "CAsidecar"

	tap := NewTap(dir, streamSID, callSID, "", testStreamStartedAt)
	tap.WriteIn([]byte{0x01, 0x02, 0x03})
	tap.Close()

	got := readSidecar(t, dir, streamSID)
	if got.VADConfig != telephony.DefaultVADConfig() {
		t.Errorf("sidecar vad_config = %+v\n                  want %+v", got.VADConfig, telephony.DefaultVADConfig())
	}
	if got.StreamSID != streamSID {
		t.Errorf("sidecar stream_sid = %q, want %q", got.StreamSID, streamSID)
	}
	// Both IDs: the file is keyed by stream, but SOP-153 needs to group a
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

// TestTap_WriteFailureDoesNotKillCall covers Observable behavior 4. A full or
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
// capture broke"), and exactly what Close's own doc says cannot happen. SOP-153
// would find a recording it cannot tell from a silent call, with no sidecar to
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
// broke, and SOP-153 would replay it as a zero-utterance sample.
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

// --- outbound capture (SOP-158) ------------------------------------------
//
// Retroactive Phase 0 verification record: the 5 tests below
// (TestTap_OutboundQueueDrainAligns, TestTap_SilenceOnEmptyQueue,
// TestTap_BargeInOverlap, TestTap_QueueTruncationOnClose,
// TestTap_DisabledAllocatesNothing_Outbound) were confirmed to establish a
// genuine RED state against pre-SOP-158 code: with the WriteOut/DrainOut
// outbound-queue implementation in tap.go reverted, the package failed to
// compile with three undefined-symbol errors (tap.WriteIn, tap.WriteOut,
// tap.DrainOut), and once stubbed to compile, all 5 tests failed on their
// assertions (no .out.ulaw produced, no silence padding, no overlap
// alignment, no out_truncated sidecar flag). With the implementation
// restored, all 5 pass and `go test -race ./internal/telephony/` is clean.
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
// the ticket's own Test Expectations line ("assert both channels have
// energy at the same byte offset"), previously unchecked. Now asserts
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

// TestTap_SilenceMatchesRealFrameSize covers SOP-158's DoD directly: "the engine-
// silent spans are silence (0xFF) of the correct duration" and "in-channel
// and out-channel are byte-for-byte the same length." The 5 pre-existing
// outbound tests above all use synthetic 1-byte payloads on both channels, so
// a DrainOut that falls back to a single 0xFF byte on an empty queue passes
// them even though it desyncs the two files by ~159 bytes on every real,
// 160-byte (20ms @ 8kHz mu-law) silent frame. This test uses realistic
// frame-sized payloads on WriteIn, matching what the real Twilio media-stream
// path (session.go's frame size, SampleRateHz*MuLawFrameMS/1000) delivers, so
// a fixed-1-byte silence fallback fails it while the 5 synthetic tests stay
// green.
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

// --- stream isolation + bounded queue (SOP-158 attempt 6) --------------------
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

// TestTap_OutboundQueueIsBounded pins the ticket's File map requirement that
// outQueue be "thread-safe, bounded": under a stalled drain (DrainOut not
// keeping pace with WriteOut -- e.g. triggered by the isolation bug above,
// or any other inbound-frame stall) the queue must never grow without
// limit. Direct field access is deliberate: this is a white-box test in the
// tap's own package, and the queue has no public accessor by design.
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

// --- ring buffer correctness + complexity (SOP-158 attempt 8) ---------------
//
// Phase 0 (attempt 8) red tests. tap.go's outQueue is currently a slice
// reslice-drop-oldest (t.outQueue = t.outQueue[1:]; append(...)) which is
// O(n) per call once at the bound: reslicing shrinks cap in lockstep with
// len, so every subsequent pop+append forces a full realloc+copy of the
// live queue. Both tests below are confirmed red against that code:
// TestTap_WriteOutSteadyStateIsAllocationFree fails because steady-state
// WriteOut allocates (the realloc+copy) instead of 0 allocs/call;
// TestTap_RingBufferWraparoundPreservesOrder is a correctness pin for the
// ring-buffer replacement (it passes against the reslice implementation
// too, since reslice-drop-oldest is *correct*, just not O(1) — its purpose
// is to catch an off-by-one in the ring buffer's head/tail bookkeeping once
// that replacement lands, not to distinguish the two implementations by
// itself).

// TestTap_WriteOutSteadyStateIsAllocationFree pins the O(1) drop-oldest
// claim directly: once the queue is at its bound, every further WriteOut
// call must not allocate. A slice-reslice queue's cap shrinks with its len
// on every pop, so once full it forces a fresh backing-array alloc+copy of
// the entire live queue on every single call — turning the safety net into
// an allocation storm during the one failure mode (a stalled drain) it
// exists to contain gracefully. A real ring buffer overwrites in place.
func TestTap_WriteOutSteadyStateIsAllocationFree(t *testing.T) {
	dir := t.TempDir()
	tap := NewTap(dir, "SSallocs", "CAallocs", "", testStreamStartedAt)
	defer tap.Close()

	payload := []byte{0x01}
	for i := 0; i < maxOutQueueFrames+10; i++ {
		tap.WriteOut(payload)
	}

	// testing.AllocsPerRun can't see this: it divides total mallocs by runs
	// using integer division, so any average below 1 truncates to exactly
	// 0 -- and a slice-reslice queue's realloc+copy recurs only periodically
	// (every few thousand calls, since cap shrinks by 1 on every pop but a
	// Go append only grows cap when it must), never below 1-per-call. A raw
	// runtime.MemStats delta over a large enough batch has no such floor.
	const steadyStateCalls = 50000
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < steadyStateCalls; i++ {
		tap.WriteOut(payload)
	}
	runtime.ReadMemStats(&after)

	if got := after.Mallocs - before.Mallocs; got > 0 {
		t.Errorf("%d heap allocations across %d steady-state WriteOut calls, want 0 — drop-oldest must overwrite in place (a ring buffer), not periodically realloc+copy the live queue", got, steadyStateCalls)
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
